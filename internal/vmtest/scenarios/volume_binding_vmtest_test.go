package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
)

// A data volume is bound to the partition and filesystem it was provisioned
// on. A copy of its disk attached beside it, or a detached disk, must leave
// the volume unmounted with guidance while the node stays manageable through
// katlctl. A copy that replaces the detached original mounts. After the
// volume is replaced, an older generation bound to the old storage cannot be
// selected.
func TestVolumeBindingGate(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run the volume binding journey")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "volume-binding-gate")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	fail := func(err error) {
		t.Helper()
		_ = worldScenario.WriteResult(vmtest.WorldStatusFailed, err.Error())
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	repo := katlRepoRoot(t)
	katlctl := buildKatlctlCommand(t, ctx, repo, filepath.Join(worldScenario.Dir, "katlctl.yaml"))
	privateKey, _, err := ensureWorldSSHKey(world)
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
	imageURL, _, stopImageServer, err := serveInstallImage(world.Network.Gateway, image)
	if err != nil {
		fail(err)
	}
	defer stopImageServer()

	runner := vmtest.NewRunner(options)
	vm := vmtest.VMConfig{
		Boot:          vmtest.VMBoot{Image: disk, ImageFormat: vmtest.DiskRaw, ImageWithoutSerial: true},
		Network:       node.VMNetwork(),
		PreserveNVRAM: true,
		KVM:           options.KVM,
		RAMMiB:        2048,
		CPUs:          2,
		Timeout:       30 * time.Minute,
	}
	j := &volumeJourney{installerESPJourney: installerESPJourney{
		katlctl:    katlctl,
		privateKey: privateKey,
		publicKey:  privateKey + ".pub",
		dir:        worldScenario.Dir,
		imageURL:   imageURL,
		imageJSON:  image + ".json",
	}}
	plan := func(scenario vmtest.Scenario) vmtest.Result {
		t.Helper()
		result, err := runner.Plan(scenario)
		if err != nil {
			fail(err)
		}
		if err := vmtest.CreateDisks(ctx, vmtest.ExecDiskRunner{}, result.Disks); err != nil {
			fail(err)
		}
		return result
	}
	// boot runs one boot of the node with the planned disks; journey must
	// power the node off before it returns.
	boot := func(label string, scenario vmtest.Scenario, result vmtest.Result, journey func(*vmtest.VMHandle) error) vmtest.Result {
		t.Helper()
		result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, journey)
		if err := runner.Write(scenario, result); err != nil {
			fail(err)
		}
		if result.Status != vmtest.StatusPassed {
			fail(fmt.Errorf("%s: %s (%s)", label, result.FailureSummary, result.RunDir))
		}
		vm.OVMFVars = filepath.Join(result.VMDir, "OVMF_VARS.fd")
		vm.Timeout = 15 * time.Minute
		return result
	}

	original := vmtest.Scenario{Name: "volume-binding-gate", Disks: []vmtest.DiskFixture{vmtest.ExtraDisk("data", string(vmtest.DiskRaw), "4G")}}
	installed := boot("install with a data volume", original, plan(original), func(handle *vmtest.VMHandle) error {
		if err := j.install(ctx, handle); err != nil {
			return err
		}
		if err := j.requireMounted(ctx); err != nil {
			return err
		}
		if _, err := j.ssh(ctx, "echo original > /var/mnt/data/marker && sync"); err != nil {
			return err
		}
		return j.powerOff(ctx, handle)
	})
	dataDisk := ""
	for _, planned := range installed.Disks {
		if planned.Name == "data" {
			dataDisk = planned.HostPath
		}
	}
	if dataDisk == "" {
		fail(errors.New("installed scenario has no data disk"))
	}

	copyDisk := filepath.Join(worldScenario.Dir, "data-copy.raw")
	if output, err := exec.CommandContext(ctx, "cp", "--sparse=always", dataDisk, copyDisk).CombinedOutput(); err != nil {
		fail(fmt.Errorf("copy data disk: %w: %s", err, output))
	}
	beside := vmtest.Scenario{Name: "volume-binding-gate-copy-beside", Disks: []vmtest.DiskFixture{
		vmtest.SnapshotDisk("data", dataDisk, vmtest.DiskRaw), vmtest.SnapshotDisk("copy", copyDisk, vmtest.DiskRaw),
	}}
	besideResult := boot("boot with a copy beside the data disk", beside, plan(beside), func(handle *vmtest.VMHandle) error {
		if err := j.requireUnmanagedVolume(ctx, "Detach the copy"); err != nil {
			return err
		}
		return j.powerOff(ctx, handle)
	})
	cleanup(t, besideResult)

	replaced := vmtest.Scenario{Name: "volume-binding-gate-copy-only", Disks: []vmtest.DiskFixture{vmtest.SnapshotDisk("copy", copyDisk, vmtest.DiskRaw)}}
	replacedResult := boot("boot with the copy in place of the detached original", replaced, plan(replaced), func(handle *vmtest.VMHandle) error {
		if _, err := j.waitManageable(ctx); err != nil {
			return err
		}
		if err := j.requireMounted(ctx); err != nil {
			return err
		}
		if marker, err := j.ssh(ctx, "cat /var/mnt/data/marker"); err != nil || marker != "original" {
			return fmt.Errorf("copy mounted without the original's data: %q, %v", marker, err)
		}
		return j.powerOff(ctx, handle)
	})
	cleanup(t, replacedResult)
	if err := os.Remove(copyDisk); err != nil {
		fail(err)
	}

	// The original disk failed and is replaced by a blank one: the volume
	// stays unmounted until the operator rebinds it, and generation 0, bound
	// to the old disk, can no longer be selected.
	replacement := vmtest.Scenario{Name: "volume-binding-gate-replacement", Disks: []vmtest.DiskFixture{vmtest.ExtraDisk("data2", string(vmtest.DiskRaw), "4G")}}
	replacementResult := boot("replace the data disk", replacement, plan(replacement), func(handle *vmtest.VMHandle) error {
		if err := j.requireUnmanagedVolume(ctx, "Attach the disk"); err != nil {
			return err
		}
		previous, err := j.defaultGeneration(ctx)
		if err != nil {
			return err
		}
		if err := j.rebind(ctx, "/dev/disk/by-id/virtio-katl-data2"); err != nil {
			return err
		}
		if err := j.requireMounted(ctx); err != nil {
			return err
		}
		if err := j.requireSelectRefused(ctx, previous); err != nil {
			return err
		}
		return j.powerOff(ctx, handle)
	})
	cleanup(t, replacementResult)
	cleanup(t, installed)
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func cleanup(t *testing.T, result vmtest.Result) {
	t.Helper()
	if err := vmtest.CleanupDisks(result); err != nil {
		t.Fatal(err)
	}
}

type volumeJourney struct {
	installerESPJourney
	config string
	boot   string
}

// install configures the node with a data volume on the disk with serial
// katl-data and installs it through the public CLI.
func (j *volumeJourney) install(ctx context.Context, handle *vmtest.VMHandle) error {
	const announcement = "katlos-install waiting for config at "
	if _, err := handle.WaitForSerialSignal(announcement, time.Second); err != nil {
		return err
	}
	serial, err := os.ReadFile(handle.Plan.SerialLog)
	if err != nil {
		return err
	}
	match := regexp.MustCompile(`katlos-install waiting for config at (http://\S+)/v1/config-bundle`).FindSubmatch(serial)
	if match == nil {
		return errors.New("installer did not announce its handoff endpoint")
	}
	endpoint, err := url.Parse(string(match[1]))
	if err != nil {
		return err
	}
	j.address = endpoint.Hostname()
	if j.byPath, err = bootDiskPath(ctx, endpoint.String()); err != nil {
		return err
	}
	j.config = filepath.Join(j.dir, "cluster.yaml")
	if _, err := j.cli(ctx, "config", "init", j.config, "--node", "cp-1=control-plane,"+j.address+","+j.byPath, "--ssh-authorized-key", j.publicKey); err != nil {
		return err
	}
	if err := j.selectDataDisk("/dev/disk/by-id/virtio-katl-data"); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "ssh", "--config", j.config, "--node", "cp-1"); err != nil {
		return err
	}
	installerBoot, err := j.ssh(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	bundle := filepath.Join(j.dir, "cluster.katlcfg")
	if _, err := j.cli(ctx, "config", "bundle", j.config, "--output", bundle, "--katlos-image-url", j.imageURL, "--katlos-image-metadata", j.imageJSON); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "apply", "--config", bundle, "--node", "cp-1", "--timeout", "20m"); err != nil {
		return err
	}
	if j.boot, err = j.waitInstalledBoot(ctx, installerBoot); err != nil {
		return fmt.Errorf("automatic reboot: %w", err)
	}
	return j.requireGeneration0(ctx, j.config)
}

// selectDataDisk sets the node's data volume to the given disk, formatting it
// when it is first provisioned.
func (j *volumeJourney) selectDataDisk(byID string) error {
	data, err := os.ReadFile(j.config)
	if err != nil {
		return err
	}
	text := string(data)
	volume := "          storage:\n            volumes:\n              - name: data\n                selector:\n                  disk:\n                    byID: " + byID + "\n                filesystem: xfs\n                wipe: true\n"
	if start := strings.Index(text, "          storage:\n"); start >= 0 {
		end := strings.Index(text[start:], "          management:\n")
		if end < 0 {
			return fmt.Errorf("cluster config has storage but no management section:\n%s", text)
		}
		text = text[:start] + text[start+end:]
	}
	if !strings.Contains(text, "          management:\n") {
		return fmt.Errorf("cluster config has no node management section:\n%s", text)
	}
	text = strings.Replace(text, "          management:\n", volume+"          management:\n", 1)
	return os.WriteFile(j.config, []byte(text), 0o644)
}

// waitManageable waits for a new boot whose boot health has settled, with
// any verdict, and returns its boot ID.
func (j *volumeJourney) waitManageable(ctx context.Context) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	var last error
	for {
		// systemctl is-active succeeds when any one of several units is active,
		// so each unit is checked on its own.
		boot, err := j.ssh(deadline, "systemctl is-active --quiet katl-boot-health-settled.target && systemctl is-active --quiet katlc-agent.service && cat /proc/sys/kernel/random/boot_id")
		if err == nil && boot != j.boot {
			j.boot = boot
			return boot, nil
		}
		last = err
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("node did not settle boot health: %v", last)
		case <-time.After(5 * time.Second):
		}
	}
}

// requireMounted checks through the kernel's mount table that the data
// volume is mounted.
func (j *volumeJourney) requireMounted(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		source, err := j.ssh(deadline, "findmnt --noheadings --output SOURCE --mountpoint /var/mnt/data")
		if err == nil && source != "" {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("data volume is not mounted: %v", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// requireUnmanagedVolume checks that the volume stayed unmounted because the
// activation check refused it, that boot health failed, and that katlctl
// reaches the node and shows the guidance.
func (j *volumeJourney) requireUnmanagedVolume(ctx context.Context, guidance string) error {
	if _, err := j.waitManageable(ctx); err != nil {
		return err
	}
	mounted, err := j.ssh(ctx, "if findmnt --mountpoint /var/mnt/data >/dev/null; then echo mounted; else echo unmounted; fi; systemctl show katl-volumes-activate.service -p Result --value")
	if err != nil {
		return err
	}
	if mounted != "unmounted\nexit-code" {
		return fmt.Errorf("data volume state and activation result = %q, want unmounted after a refused check", mounted)
	}
	output, err := j.cli(ctx, "node", "status", "cp-1", "--config", j.config, "--output", "json")
	if err != nil {
		return err
	}
	var status struct {
		Health  string `json:"health"`
		Volumes []struct {
			Name              string `json:"name"`
			FailureDiagnostic string `json:"failureDiagnostic"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return fmt.Errorf("decode node status: %w: %s", err, output)
	}
	if strings.EqualFold(status.Health, "ok") {
		return fmt.Errorf("node status reports healthy boot with an unmounted volume: %s", output)
	}
	for _, volume := range status.Volumes {
		if volume.Name == "data" && strings.Contains(volume.FailureDiagnostic, guidance) {
			return nil
		}
	}
	return fmt.Errorf("node status does not explain the unmounted volume with %q: %s", guidance, output)
}

func (j *volumeJourney) defaultGeneration(ctx context.Context) (string, error) {
	output, err := j.cli(ctx, "node", "generations", "list", "cp-1", "--config", j.config, "--output", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		DefaultGenerationID string `json:"defaultGenerationId"`
	}
	if err := json.Unmarshal(output, &list); err != nil || list.DefaultGenerationID == "" {
		return "", fmt.Errorf("decode generation list: %v: %s", err, output)
	}
	return list.DefaultGenerationID, nil
}

// rebind selects the replacement disk and authorizes the new binding.
func (j *volumeJourney) rebind(ctx context.Context, byID string) error {
	if err := j.selectDataDisk(byID); err != nil {
		return err
	}
	_, err := j.cli(ctx, "cluster", "apply", "--config", j.config, "--node", "cp-1", "--rebind-volume", "cp-1/data", "--timeout", "10m")
	return err
}

// requireSelectRefused checks that selecting the generation is refused and
// leaves the boot selection unchanged.
func (j *volumeJourney) requireSelectRefused(ctx context.Context, generationID string) error {
	before, err := j.ssh(ctx, "cat /var/lib/katl/boot/selection.json")
	if err != nil {
		return err
	}
	_, err = j.cli(ctx, "node", "generations", "select", generationID, "cp-1", "--config", j.config)
	if err == nil || !strings.Contains(err.Error(), "would boot with its volumes unmounted") {
		return fmt.Errorf("select generation %s = %v, want a refusal naming the unresolved volume", generationID, err)
	}
	after, err := j.ssh(ctx, "cat /var/lib/katl/boot/selection.json")
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("refused selection changed the boot selection:\n%s\nwas:\n%s", after, before)
	}
	return nil
}

func (j *volumeJourney) powerOff(ctx context.Context, handle *vmtest.VMHandle) error {
	command := exec.CommandContext(ctx, handle.Plan.VirshPath, "-c", handle.Plan.LibvirtURI, "shutdown", handle.Plan.DomainName, "--mode", "acpi")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("power off node: %w: %s", err, output)
	}
	return handle.Wait()
}
