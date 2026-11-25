package scenarios

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/vmtest"
)

// An operator reinstalls a node onto a new disk and leaves the disk of the
// earlier install attached. That disk carries every Katl GPT partition label
// and filesystem label, so the installer and the installed system must find
// their partitions by the disk they bound rather than by label.
func TestInstallerReinstallBesideOldKatlDisk(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run the reinstall-beside-old-disk journey")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-old-katl-disk")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	fail := func(err error) {
		t.Helper()
		_ = worldScenario.WriteResult(vmtest.WorldStatusFailed, err.Error())
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	repo := katlRepoRoot(t)
	katlctl := buildKatlctlCommand(t, ctx, repo, filepath.Join(worldScenario.Dir, "katlctl.yaml"))
	privateKey, _, err := vmtest.EnsureWorldSSHKey(world)
	if err != nil {
		fail(err)
	}
	installerUKI, err := worldArtifactPath(world, "installer-uki")
	if err != nil {
		fail(err)
	}
	image, err := worldArtifactPath(world, "katlos-install-image")
	if err != nil {
		fail(err)
	}
	node, err := worldScenario.AddNode(vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		fail(err)
	}

	disk := filepath.Join(worldScenario.Dir, "root.raw")
	if err := writeInstallerESPDisk(ctx, worldScenario.Dir, disk, installerUKI); err != nil {
		fail(err)
	}
	oldDisk := filepath.Join(worldScenario.Dir, "old-katl.raw")
	if err := vmtest.WriteOldKatlDisk(ctx, worldScenario.Dir, oldDisk); err != nil {
		fail(err)
	}
	imageURL, _, stopImageServer, err := serveInstallImage(worldScenario.World.Network.Gateway, image)
	if err != nil {
		fail(err)
	}
	defer stopImageServer()

	runner := vmtest.NewRunner(options)
	scenario := vmtest.Scenario{
		Name:  "installer-old-katl-disk",
		Disks: []vmtest.DiskFixture{vmtest.SnapshotDisk("old", oldDisk, vmtest.DiskRaw)},
	}
	result, err := runner.Plan(scenario)
	if err != nil {
		fail(err)
	}
	if err := vmtest.CreateDisks(ctx, vmtest.ExecDiskRunner{}, result.Disks); err != nil {
		fail(err)
	}
	vm := vmtest.VMConfig{
		Boot:          vmtest.VMBoot{Image: disk, ImageFormat: vmtest.DiskRaw, ImageWithoutSerial: true},
		Network:       node.VMNetwork(),
		PreserveNVRAM: true,
		KVM:           options.KVM,
		RAMMiB:        2048,
		CPUs:          2,
		Timeout:       30 * time.Minute,
	}
	journey := oldDiskJourney{installerESPJourney: installerESPJourney{
		katlctl:    katlctl,
		privateKey: privateKey,
		publicKey:  privateKey + ".pub",
		dir:        worldScenario.Dir,
		imageURL:   imageURL,
		imageJSON:  image + ".json",
	}}
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(handle *vmtest.VMHandle) error {
		return journey.installAndPowerOff(ctx, handle)
	})
	if result.Status != vmtest.StatusPassed {
		_ = runner.Write(scenario, result)
		fail(fmt.Errorf("install journey: %s (%s)", result.FailureSummary, result.RunDir))
	}

	vm.OVMFVars = filepath.Join(result.VMDir, "OVMF_VARS.fd")
	vm.Timeout = 10 * time.Minute
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(*vmtest.VMHandle) error {
		if _, err := journey.waitInstalledBoot(ctx, ""); err != nil {
			return err
		}
		if err := journey.requireGeneration0(ctx, filepath.Join(journey.dir, "cluster.yaml")); err != nil {
			return err
		}
		return journey.requireSystemDiskMounts(ctx)
	})
	if err := runner.Write(scenario, result); err != nil {
		fail(err)
	}
	if result.Status != vmtest.StatusPassed {
		fail(fmt.Errorf("cold boot: %s (%s)", result.FailureSummary, result.RunDir))
	}

	// A disk that shares the installed disk's partition GUIDs, such as a copy
	// of it, makes the booted disk uncertain: the node must stop before
	// mounting /var and say how to recover, then boot normally once the copy
	// is detached.
	copyDisk := filepath.Join(worldScenario.Dir, "copy.raw")
	if output, err := exec.CommandContext(ctx, "cp", "--sparse=always", disk, copyDisk).CombinedOutput(); err != nil {
		fail(fmt.Errorf("copy installed disk: %w: %s", err, output))
	}
	copyScenario := vmtest.Scenario{
		Name:  "installer-old-katl-disk-copy",
		Disks: []vmtest.DiskFixture{vmtest.SnapshotDisk("old", oldDisk, vmtest.DiskRaw), vmtest.SnapshotDisk("copy", copyDisk, vmtest.DiskRaw)},
	}
	copyResult, err := runner.Plan(copyScenario)
	if err != nil {
		fail(err)
	}
	if err := vmtest.CreateDisks(ctx, vmtest.ExecDiskRunner{}, copyResult.Disks); err != nil {
		fail(err)
	}
	copyVM := vm
	copyVM.OVMFVars = filepath.Join(result.VMDir, "OVMF_VARS.fd")
	copyVM.Timeout = 5 * time.Minute
	copyResult = vmtest.VMRunner{}.RunWithVM(ctx, copyResult, copyVM, func(handle *vmtest.VMHandle) error {
		// Kernel messages can interleave with long console lines, so match
		// short markers; the guidance text itself is unit tested.
		for _, line := range []string{"could not identify its system disk", "emergency.target"} {
			if _, err := handle.WaitForSerialSignal(line, time.Second); err != nil {
				return fmt.Errorf("boot with a copied disk did not report %q: %w", line, err)
			}
		}
		if _, err := journey.ssh(ctx, "true"); err == nil {
			return errors.New("node with a copied disk attached reached multi-user mode")
		}
		return nil
	})
	if err := runner.Write(copyScenario, copyResult); err != nil {
		fail(err)
	}
	if copyResult.Status != vmtest.StatusPassed {
		fail(fmt.Errorf("boot with a copied disk: %s (%s)", copyResult.FailureSummary, copyResult.RunDir))
	}
	if err := vmtest.CleanupDisks(copyResult); err != nil {
		fail(err)
	}
	if err := os.Remove(copyDisk); err != nil {
		fail(err)
	}

	vm.OVMFVars = filepath.Join(copyResult.VMDir, "OVMF_VARS.fd")
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(*vmtest.VMHandle) error {
		if _, err := journey.waitInstalledBoot(ctx, ""); err != nil {
			return err
		}
		return journey.requireSystemDiskMounts(ctx)
	})
	if err := runner.Write(scenario, result); err != nil {
		fail(err)
	}
	if result.Status != vmtest.StatusPassed {
		fail(fmt.Errorf("boot after detaching the copy: %s (%s)", result.FailureSummary, result.RunDir))
	}
	if err := vmtest.CleanupDisks(result); err != nil {
		fail(err)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

type oldDiskJourney struct {
	installerESPJourney
}

func (j *oldDiskJourney) installAndPowerOff(ctx context.Context, handle *vmtest.VMHandle) error {
	base, err := handle.WaitForHandoffAPI(time.Second)
	if err != nil {
		return err
	}
	j.address = handle.Config.Network.Address
	if j.byPath, err = bootDiskPath(ctx, base); err != nil {
		return err
	}

	config := filepath.Join(j.dir, "cluster.yaml")
	if _, err := j.cli(ctx, "config", "init", config, "--management-authentication", "trusted-network", "--node", "cp-1=control-plane,"+j.address+","+j.byPath, "--ssh-authorized-key", j.publicKey); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "ssh", "--config", config, "--node", "cp-1"); err != nil {
		return err
	}
	installerBoot, err := j.ssh(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	bundle := filepath.Join(j.dir, "cluster.katlcfg")
	if _, err := j.cli(ctx, "config", "bundle", config, "--output", bundle, "--katlos-image-url", j.imageURL, "--katlos-image-metadata", j.imageJSON); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "apply", "--config", bundle, "--node", "cp-1", "--timeout", "20m"); err != nil {
		return err
	}

	if _, err := j.waitInstalledBoot(ctx, installerBoot); err != nil {
		return fmt.Errorf("automatic reboot: %w", err)
	}
	if err := j.requireGeneration0(ctx, config); err != nil {
		return fmt.Errorf("after automatic reboot: %w", err)
	}
	if err := j.requireSystemDiskMounts(ctx); err != nil {
		return fmt.Errorf("after automatic reboot: %w", err)
	}

	command := exec.CommandContext(ctx, handle.Plan.VirshPath, "-c", handle.Plan.LibvirtURI, "shutdown", handle.Plan.DomainName, "--mode", "acpi")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("power off installed node: %w: %s", err, output)
	}
	return handle.Wait()
}

// bootDiskPath returns the attachment path of the installer's boot disk, the
// one disk without a hardware identity; the old Katl disk has a serial.
func bootDiskPath(ctx context.Context, endpoint string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/status", nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var status handoff.HandoffStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("decode installer status: %w", err)
	}
	if len(status.Disks) != 2 {
		return "", fmt.Errorf("installer reports %d disks, want the boot disk and the old Katl disk: %+v", len(status.Disks), status.Disks)
	}
	for _, disk := range status.Disks {
		if strings.EqualFold(disk.GPTGUID, installerESPDiskGUID) && len(disk.ByPath) > 0 {
			return disk.ByPath[0], nil
		}
	}
	return "", fmt.Errorf("installer does not report the boot disk: %+v", status.Disks)
}

// requireSystemDiskMounts checks through the kernel's mount table that /, /efi
// and /var all come from the installed disk, and that the old Katl disk is
// neither mounted nor modified.
func (j *oldDiskJourney) requireSystemDiskMounts(ctx context.Context) error {
	disk, err := j.ssh(ctx, "readlink -f "+j.byPath)
	if err != nil {
		return err
	}
	report, err := j.ssh(ctx, "echo "+base64.StdEncoding.EncodeToString([]byte(vmtest.SystemDiskMountsScript))+" | base64 -d | sh")
	if err != nil {
		return err
	}
	if report != "system disk "+disk {
		return fmt.Errorf("system disk = %q, want the installed disk %s", report, disk)
	}
	return nil
}
