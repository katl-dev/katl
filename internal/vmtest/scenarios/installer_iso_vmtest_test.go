package scenarios

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func TestInstallerISOFirstInstallSmoke(t *testing.T) {
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
`, installerISOTestSSHKey))
	vm := vmtest.VMConfig{
		KVM:     options.KVM,
		RAMMiB:  2048,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := vmtest.RunFirstInstall(ctx, vmtest.NewRunner(options), vmtest.Scenario{Name: "installer-iso-first-install"}, vmtest.FirstInstallConfig{
		Installer: vmtest.InstallerBootConfig{InstallerISO: iso, VM: vm},
		Runtime: vmtest.InstalledRuntimeConfig{
			Expect: "katl-boot-health generation=0 result=success",
			VM:     vm,
		},
		Manifest:        manifest,
		PreseedManifest: true,
		UseInstalledESP: true,
		TargetDisk:      vmtest.TargetDisk("root", string(vmtest.DiskRaw), "32G"),
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
