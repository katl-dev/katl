package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

// Release qualification uses public SSH and the shipped CLI. Neither the
// installer nor the runtime is rebuilt or augmented with test support.
func TestReleaseLifecycle(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	candidate := os.Getenv("KATL_VMTEST_RELEASE_DIR")
	if candidate == "" {
		t.Skip("set KATL_VMTEST_RELEASE_DIR to a verified production bundle")
	}
	previous := os.Getenv("KATL_VMTEST_PREVIOUS_RELEASE_DIR")
	if previous == "" {
		t.Fatal("KATL_VMTEST_PREVIOUS_RELEASE_DIR is required for release qualification")
	}
	for _, journey := range []struct {
		name, source string
		upgrade      bool
	}{
		{"install", candidate, false}, {"upgrade", previous, true},
	} {
		t.Run(journey.name, func(t *testing.T) { runReleaseLifecycle(t, journey.source, candidate, journey.upgrade) })
	}
}

func releaseFile(t *testing.T, dir, pattern string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected one release file %s: %v, %v", pattern, paths, err)
	}
	return paths[0]
}

func runReleaseLifecycle(t *testing.T, source, candidate string, upgrade bool) {
	t.Helper()
	world := RequireWorld(t)
	suffix, imagePrefix := "", "katlos-"
	if os.Getenv("KATL_FLAVOUR") == "lts" {
		suffix, imagePrefix = "-lts", "katlos-lts-"
	}
	image := releaseFile(t, source, imagePrefix+"install-*.squashfs")
	metadata, err := katlosimage.ReadArtifactMetadata(image+".json", katlosimage.RoleInstall)
	if err != nil {
		t.Fatal(err)
	}
	target := releaseFile(t, candidate, imagePrefix+"upgrade-*.squashfs")
	targetMetadata, err := katlosimage.ReadArtifactMetadata(target+".json", katlosimage.RoleUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	katlctl := releaseFile(t, candidate, "katlctl-*-linux-amd64")
	if err := os.Chmod(katlctl, 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("SSH key: %v: %s", err, output)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), "artifacts.json")
	if err := writeJSON(indexPath, mkosiArtifactIndex{Artifacts: []mkosiArtifact{{Kind: "katlos-install-image", Path: image, MetadataPath: image + ".json"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATL_MKOSI_ARTIFACT_INDEX", indexPath)
	name := "release-install"
	if upgrade {
		name = "release-upgrade"
	}
	run, err := PlanFirstInstallWorldRun(world, name, repoRoot(t), NodeSpec{Name: "cp-1", Role: ControlPlane}, FirstInstallWorldInput{
		Installer: InstallerBootConfig{InstallerISO: releaseFile(t, source, "katl-installer"+suffix+".iso")},
		Mode:      FirstInstallWorldPreseed, SSHAuthorizedKey: strings.TrimSpace(string(public)), UseInstalledESP: true,
	}, KVMOn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	vm := VMConfig{KVM: KVMOn, RAMMiB: 2048, CPUs: 2, Timeout: 35 * time.Minute, SerialIdleTimeout: -1, PreserveNVRAM: true, Expect: runtimeKernelBootSignal}
	vm.SerialHooks = []SerialHook{{Name: "release-lifecycle", Signal: runtimeKernelBootSignal, Run: func(ctx context.Context, event SerialHookEvent) error {
		lease, err := WaitLibvirtLease(ctx, event.Plan.VirshPath, event.Plan.LibvirtURI, event.Plan.LibvirtNetwork, event.Plan.MACAddress, 2*time.Minute)
		if err != nil {
			return err
		}
		result := event.Result
		result.IPAddress = lease.IPAddress
		observe := func(command string) (string, error) { return releaseSSH(ctx, key, lease.IPAddress, command) }
		healthy := func(expected katlosimage.ArtifactMetadata) error {
			if expected.ExtensionRelease == nil {
				return fmt.Errorf("release %s has no kernel identity", expected.Version)
			}
			want := expected.Version + "\n" + expected.ExtensionRelease.Target.Kernel.Release
			deadline, stop := context.WithTimeout(ctx, 3*time.Minute)
			defer stop()
			var last error
			for {
				text, err := releaseSSH(deadline, key, lease.IPAddress, "systemctl is-active --quiet katl-boot-complete.target sshd.service katlc-agent.service && test \"$(systemctl show katl-boot-health.service -p Result --value)\" = success && test ! -e /usr/lib/katl/vmtest/katl-vmtest-agent && . /usr/lib/os-release && printf '%s\\n' \"$VERSION_ID\" && uname -r")
				if err == nil && text == want {
					return nil
				}
				last = fmt.Errorf("version/health: %q, expected %q: %v", text, want, err)
				select {
				case <-deadline.Done():
					return last
				case <-time.After(time.Second):
				}
			}
		}
		if err := healthy(metadata); err != nil {
			return err
		}
		// Reuse the installation's management identity through the public CLI.
		enrollConfigApplyNode(t, ctx, result, katlctl, lease.IPAddress+":9443", strings.TrimSpace(string(public)))
		cli := func(label string, args ...string) { runKatlctl(t, ctx, result, katlctl, label, args...) }
		cli("initial-status", "node", "status", "cp-1", "--output", "json")
		selection := func() (generation.BootSelectionRecord, error) {
			data, err := observe("cat /var/lib/katl/boot/selection.json")
			if err != nil {
				return generation.BootSelectionRecord{}, err
			}
			var envelope persistedrecord.Envelope
			if err := json.Unmarshal([]byte(data), &envelope); err != nil {
				return generation.BootSelectionRecord{}, err
			}
			value, err := persistedrecord.DecodePayload[generation.BootSelectionRecord](envelope)
			if err != nil {
				return value, err
			}
			if err := generation.ValidateBootSelection(value); err != nil {
				return value, err
			}
			if value.PendingHealthValidation || value.RecoveryRequired || value.DefaultGenerationID != value.BootedGenerationID {
				return value, fmt.Errorf("unhealthy boot selection: %+v", value)
			}
			return value, nil
		}
		original, err := selection()
		if err != nil {
			return err
		}
		machine, err := observe("cat /etc/machine-id")
		if err != nil {
			return err
		}
		boot, err := observe("cat /proc/sys/kernel/random/boot_id")
		if err != nil {
			return err
		}
		cli("reboot", "node", "reboot", "cp-1", "--timeout", "5m")
		if err := healthy(metadata); err != nil {
			return err
		}
		rebooted, err := observe("cat /proc/sys/kernel/random/boot_id")
		if err != nil || rebooted == boot {
			return fmt.Errorf("public reboot did not change boot identity: %v", err)
		}
		if upgrade {
			config := filepath.Join(result.RunDir, "katlctl", "enrollment", "cluster.yaml")
			cli("upgrade-plan", "node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", first(targetMetadata.Flavour, "standard"), "--plan")
			planned, err := selection()
			if err != nil || planned.BootedGenerationID != original.BootedGenerationID || planned.DefaultGenerationID != original.DefaultGenerationID || planned.TargetBootGenerationID != "" || planned.TrialGenerationID != "" {
				return fmt.Errorf("plan changed current generation: %v", err)
			}
			plannedBoot, err := observe("cat /proc/sys/kernel/random/boot_id")
			if err != nil || plannedBoot != rebooted {
				return fmt.Errorf("plan rebooted the node: %v", err)
			}
			cli("upgrade", "node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", first(targetMetadata.Flavour, "standard"), "--timeout", "10m")
			if err := healthy(targetMetadata); err != nil {
				return err
			}
			active, err := selection()
			if err != nil || active.BootedGenerationID == original.BootedGenerationID {
				return fmt.Errorf("upgrade did not promote a new generation: %v", err)
			}
			// Verify the booted kernel independently of management success.
			kernel, err := observe("uname -r")
			if err != nil {
				return err
			}
			if targetMetadata.ExtensionRelease == nil || kernel != targetMetadata.ExtensionRelease.Target.Kernel.Release {
				return fmt.Errorf("booted kernel %q does not match release metadata", kernel)
			}
			cli("rollback-select", "node", "generations", "select", original.BootedGenerationID, "cp-1")
			cli("rollback-reboot", "node", "reboot", "cp-1", "--timeout", "5m")
			if err := healthy(metadata); err != nil {
				return err
			}
			rolledBack, err := selection()
			if err != nil || rolledBack.BootedGenerationID != original.BootedGenerationID {
				return fmt.Errorf("rollback did not restore source generation: %v", err)
			}
		}
		finalSelection, err := selection()
		if err != nil || finalSelection.BootedGenerationID != original.BootedGenerationID {
			return fmt.Errorf("final boot selection differs from the source: %v", err)
		}
		persisted, err := observe("cat /etc/machine-id")
		if err != nil || persisted != machine {
			return fmt.Errorf("machine identity changed: %v", err)
		}
		cli("final-status", "node", "status", "cp-1", "--output", "json")
		return nil
	}}}
	run.Config.Installer.VM = vm
	run.Config.Runtime.VM = vm
	run.Config.RebootIntoInstalled = true
	result, err := RunFirstInstall(ctx, run.Runner, Scenario{Name: name}, run.Config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("release journey: %s (%s)", result.FailureSummary, result.RunDir)
	}
	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func releaseSSH(ctx context.Context, key, address, command string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "ssh", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-i", key, "root@"+address, command).Output()
	return strings.TrimSpace(string(output)), err
}
