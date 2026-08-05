package scenarios

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
)

const installerISOTestSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func TestInstallerISOBootSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer ISO boot smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-iso-boot")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	iso := os.Getenv("KATL_INSTALLER_ISO")
	if iso == "" {
		iso = filepath.Join(katlRepoRoot(t), "_build", "mkosi", "katl-installer.iso")
	}
	runner := vmtest.NewRunner(options)
	result, err := vmtest.RunInstallerBoot(
		context.Background(),
		runner,
		vmtest.Scenario{Name: "installer-iso-boot"},
		vmtest.InstallerBootConfig{
			InstallerISO: iso,
			Expect:       "katlos-install progress: waiting for configuration at",
			VM: vmtest.VMConfig{
				KVM:     options.KVM,
				RAMMiB:  2048,
				CPUs:    2,
				Timeout: 3 * time.Minute,
			},
		},
		vmtest.VMRunner{},
	)
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer ISO boot status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

func TestInstallerPXEBootSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer PXE boot smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-pxe-boot")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	repo := katlRepoRoot(t)
	kernel := os.Getenv("KATL_INSTALLER_KERNEL")
	if kernel == "" {
		kernel = filepath.Join(repo, "_build", "mkosi", "katl-installer.vmlinuz")
	}
	initrd := os.Getenv("KATL_INSTALLER_INITRD")
	if initrd == "" {
		initrd = filepath.Join(repo, "_build", "mkosi", "katl-installer.initrd")
	}
	result, err := vmtest.RunInstallerBoot(
		context.Background(),
		vmtest.NewRunner(options),
		vmtest.Scenario{Name: "installer-pxe-boot"},
		vmtest.InstallerBootConfig{
			InstallerKernel: kernel,
			InstallerInitrd: initrd,
			CommandLine: []string{
				"console=ttyS0,115200n8",
				"systemd.log_target=console",
				"loglevel=6",
			},
			Expect: "Katl installer ready",
			VM: vmtest.VMConfig{
				KVM:     options.KVM,
				RAMMiB:  2048,
				CPUs:    2,
				Timeout: 3 * time.Minute,
			},
		},
		vmtest.VMRunner{},
	)
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer PXE boot status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

func TestInstallerISOFirstInstallStorageAuthority(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer ISO first-install smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-iso-first-install")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	iso := os.Getenv("KATL_INSTALLER_ISO")
	if iso == "" {
		iso = filepath.Join(katlRepoRoot(t), "_build", "mkosi", "katl-installer.iso")
	}
	manifest := []byte(fmt.Sprintf(`apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: iso-node
    ssh:
      authorizedKeys:
        - %s
  systemRole: control-plane
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/virtio-katl-root
  volumes:
    - name: data
      selector:
        disk:
          byID: /dev/disk/by-id/virtio-katl-data
      filesystem: xfs
      wipe: true
`, installerISOTestSSHKey))
	vm := vmtest.VMConfig{
		KVM:     options.KVM,
		RAMMiB:  2048,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var dataDisk string
	diskRunner := nonBlankDataDiskRunner{dataPath: &dataDisk}
	result, err := vmtest.RunFirstInstall(ctx, vmtest.NewRunner(options), vmtest.Scenario{
		Name: "installer-iso-first-install-storage-authority",
		Disks: []vmtest.DiskFixture{
			vmtest.TargetDisk("root", string(vmtest.DiskRaw), "32G"),
			vmtest.ExtraDisk("data", string(vmtest.DiskRaw), "4G"),
		},
	}, vmtest.FirstInstallConfig{
		Installer: vmtest.InstallerBootConfig{InstallerISO: iso, VM: vm},
		Runtime: vmtest.InstalledRuntimeConfig{
			Expect: "katl-boot-health generation=0 result=success",
			VM:     vm,
		},
		Manifest:            manifest,
		GuestHandoff:        true,
		RebootIntoInstalled: true,
		TargetDisk:          vmtest.TargetDisk("root", string(vmtest.DiskRaw), "32G"),
		DiskRunner:          diskRunner,
		HandoffPoster: func(ctx context.Context, endpoint string, payload []byte) (int, string, error) {
			status, body, err := postInstallerManifest(ctx, endpoint, payload)
			if err != nil {
				return 0, "", err
			}
			if status != http.StatusPreconditionRequired || !strings.Contains(body, "--acknowledge-storage-wipe iso-node/data") {
				return 0, "", fmt.Errorf("unacknowledged handoff status=%d body=%s", status, body)
			}
			if dataDisk == "" {
				return 0, "", fmt.Errorf("non-blank data disk path was not recorded")
			}
			filesystem, err := exec.CommandContext(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", dataDisk).CombinedOutput()
			if err != nil || strings.TrimSpace(string(filesystem)) != "ext4" {
				return 0, "", fmt.Errorf("refused handoff changed existing data disk signature: type=%q err=%v", filesystem, err)
			}
			acknowledged, err := url.Parse(endpoint)
			if err != nil {
				return 0, "", err
			}
			query := acknowledged.Query()
			query.Add("acknowledgeStorageWipe", "iso-node/data")
			acknowledged.RawQuery = query.Encode()
			return postInstallerManifest(ctx, acknowledged.String(), payload)
		},
	})
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer ISO first-install status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

type nonBlankDataDiskRunner struct {
	dataPath *string
}

func (r nonBlankDataDiskRunner) Run(ctx context.Context, name string, args ...string) error {
	if err := (vmtest.ExecDiskRunner{}).Run(ctx, name, args...); err != nil {
		return err
	}
	if name != "qemu-img" || len(args) < 2 {
		return nil
	}
	path := args[len(args)-2]
	if !strings.Contains(filepath.Base(path), "-data.") {
		return nil
	}
	if err := exec.CommandContext(ctx, "mkfs.ext4", "-F", path).Run(); err != nil {
		return fmt.Errorf("create existing ext4 data signature: %w", err)
	}
	*r.dataPath = path
	return nil
}

func postInstallerManifest(ctx context.Context, endpoint string, payload []byte) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, string(body), err
}
