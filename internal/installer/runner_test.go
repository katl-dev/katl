package installer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/generation"
)

func TestDefaultPlanOrder(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		WaitForLocalConfig,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := DefaultPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultPlan IDs = %#v, want %#v", got, want)
	}
}

func TestPreseededManifestPlanSkipsLocalConfigWait(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := PreseededManifestPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PreseededManifestPlan IDs = %#v, want %#v", got, want)
	}
}

func TestRunnerRecordsCheckpointsWithoutCommands(t *testing.T) {
	store := &MemoryStateStore{}
	commands := &NoopCommandRunner{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     t.TempDir(),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       commands,
		Store:          store,
	}

	if err := NewRunner(DefaultPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := DefaultPlan().IDs()
	if !reflect.DeepEqual(install.Completed, want) {
		t.Fatalf("completed steps = %#v, want %#v", install.Completed, want)
	}
	if len(store.Checkpoints) != len(want) {
		t.Fatalf("checkpoint count = %d, want %d", len(store.Checkpoints), len(want))
	}
	if got := store.Checkpoints[len(store.Checkpoints)-1].CompletedSteps; !reflect.DeepEqual(got, want) {
		t.Fatalf("final checkpoint completed steps = %#v, want %#v", got, want)
	}
	if len(commands.Calls) != 0 {
		t.Fatalf("command runner was called during scaffold run: %#v", commands.Calls)
	}
}

func TestRunnerInstallsIdentity(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	bootRoot := t.TempDir()
	record := generation.Record{
		GenerationID:   "2026.06.01-005",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:          "root-a",
			PartitionUUID: "11111111-2222-3333-4444-555555555555",
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl.efi",
		},
	}
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     targetRoot,
		BootRoot:       bootRoot,
		LoaderRecord:   &record,
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          store,
	}

	if err := NewRunner(PreseededManifestPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	machineID := "30313233343536373839616263646566"
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/identity/machine-id"), machineID+"\n")
	assertText(t, filepath.Join(targetRoot, "etc/ssh/authorized_keys/katl"), sshKey+"\n")
	assertContains(t, filepath.Join(targetRoot, "etc/ssh/sshd_config.d/10-katl.conf"), "AllowUsers katl")
	assertContains(t, filepath.Join(bootRoot, "loader/entries/katl-2026.06.01-005.conf"), "systemd.machine_id="+machineID)
}

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"

func writeManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "install.json")
	data := `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"` + sshKey + `"
					]
				}
			}
		},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"artifacts": {
			"runtimeRoot": {
				"url": "https://example.invalid/root.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			"uki": {
				"url": "https://example.invalid/katl.efi",
				"sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
			"sysexts": [
				{
					"name": "kubelet",
					"url": "https://example.invalid/kubelet.sysext.raw",
					"sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
				}
			]
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func assertText(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s missing %q:\n%s", path, want, data)
	}
}
