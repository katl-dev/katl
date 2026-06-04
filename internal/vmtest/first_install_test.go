package vmtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstInstall(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		HandoffToken:    "test-token",
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM("Katl state projection ready"),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if manifest, err := os.ReadFile(result.Artifacts.InstallManifest); err != nil || !strings.Contains(string(manifest), "lab-node-01") {
		t.Fatalf("install manifest = %q, err = %v", manifest, err)
	}
	response := readLog(t, result.Artifacts.HandoffResponse)
	if response.StatusCode != 200 || !strings.Contains(response.Body, "install-starting") {
		t.Fatalf("handoff response = %#v", response)
	}
	if command, err := os.ReadFile(result.Artifacts.InstallerQEMUCommand); err != nil || !strings.Contains(string(command), "fat:rw:") {
		t.Fatalf("installer command = %q, err = %v", command, err)
	}
	if command, err := os.ReadFile(result.Artifacts.RuntimeQEMUCommand); err != nil || !strings.Contains(string(command), "format=raw,file="+result.Disks[0].HostPath) {
		t.Fatalf("runtime command = %q, err = %v", command, err)
	}
	if _, err := os.Stat(result.Disks[0].HostPath); !os.IsNotExist(err) {
		t.Fatalf("successful target disk kept: %v", err)
	}
}

func TestFirstInstallFailure(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI: uki,
			VM:           vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		HandoffToken:    "test-token",
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM(""),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if _, err := os.Stat(result.Disks[0].HostPath); err != nil {
		t.Fatalf("failed target disk missing: %v", err)
	}
}

type fileDiskRunner struct{}

func (fileDiskRunner) Run(_ context.Context, _ string, args ...string) error {
	path := args[len(args)-2]
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("disk"), 0o644)
}

func fakeVM(signal string) VMRunner {
	return VMRunner{
		Executor: vmExec{write: signal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
}

func readLog(t *testing.T, path string) handoffLog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read handoff log: %v", err)
	}
	var log handoffLog
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("decode handoff log: %v", err)
	}
	return log
}

func firstManifest() string {
	return `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"
					]
				}
			}
		},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/virtio-katl-root", "minSizeMiB": 32}
		},
		"artifacts": {
			"runtimeRoot": {
				"url": "https://example.invalid/root.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
		}
	}`
}
