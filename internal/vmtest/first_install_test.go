package vmtest

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/handoff"
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

func TestFirstInstallGuestHandoff(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	server, err := handoff.NewHandoffServer("guest-token", nil)
	if err != nil {
		t.Fatalf("NewHandoffServer() error = %v", err)
	}
	handoffPosted := make(chan struct{})
	installerSerial := stagedVMExec{
		first: server.Announcement("http://10.0.2.15:8080") + "\n",
		wait:  handoffPosted,
		then:  guestHandoffAcceptedSignal + "/run/katl/install-manifest.json\n",
	}
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff"}, FirstInstallConfig{
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
		GuestHandoff:    true,
		HandoffHostPort: 18080,
		HandoffPoster: func(ctx context.Context, url, token string, manifest []byte) (int, string, error) {
			status, body, err := postLocal(ctx, server.Handler(), url, token, manifest)
			if err == nil {
				close(handoffPosted)
			}
			return status, body, err
		},
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVMWithExecutor(installerSerial),
		RuntimeRunner:   fakeVM("Katl state projection ready"),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	request := readLog(t, result.Artifacts.HandoffRequest)
	if request.URL != "http://10.0.2.15:8080/v1/install" || request.PostURL != "http://127.0.0.1:18080/v1/install" {
		t.Fatalf("handoff request = %#v", request)
	}
	response := readLog(t, result.Artifacts.HandoffResponse)
	if response.StatusCode != 200 || !strings.Contains(response.Body, "install-starting") {
		t.Fatalf("handoff response = %#v", response)
	}
	if command, err := os.ReadFile(result.Artifacts.InstallerQEMUCommand); err != nil || !strings.Contains(string(command), "hostfwd=tcp:127.0.0.1:18080-:8080") {
		t.Fatalf("installer command = %q, err = %v", command, err)
	}
}

func TestFirstInstallUsesInstalledESPExtractor(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	sourceESP := espFixture(t)
	_, vmConfig := vmFixture(t)
	var extractedDisk DiskPlan
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
		Keep:      KeepAlways,
	}), Scenario{Name: "first-install-installed-esp"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			VM: vmConfig,
		},
		UseInstalledESP: true,
		ESPExtractor: func(_ context.Context, disk DiskPlan, outputDir string) (string, error) {
			extractedDisk = disk
			if err := copyDir(sourceESP, outputDir); err != nil {
				return "", err
			}
			return outputDir, nil
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
	if extractedDisk.Kind != DiskTarget || extractedDisk.HostPath == "" {
		t.Fatalf("extractor disk = %#v", extractedDisk)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.InstalledESP, "loader", "entries")); err != nil {
		t.Fatalf("installed ESP artifacts missing: %v", err)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.ESPArtifacts != result.Artifacts.InstalledESP {
		t.Fatalf("runtime ESP artifacts = %q, want %q", input.ESPArtifacts, result.Artifacts.InstalledESP)
	}
	if !hasPhase(result, "installed-esp") {
		t.Fatalf("installed-esp phase missing: %#v", result.Phases)
	}
}

func TestFirstInstallGuestHandoffRequiresHook(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff-missing"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			Expect:          "Katl installer ready",
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		GuestHandoff:    true,
		HandoffHostPort: 18080,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM("Katl state projection ready"),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "guest handoff response artifact is missing") {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func hasPhase(result Result, name string) bool {
	for _, phase := range result.Phases {
		if phase.Name == name {
			return true
		}
	}
	return false
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
	return fakeVMWithExecutor(vmExec{write: signal})
}

func fakeVMWithExecutor(executor VMExecutor) VMRunner {
	return VMRunner{
		Executor: executor,
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
}

type stagedVMExec struct {
	first string
	wait  <-chan struct{}
	then  string
}

func (e stagedVMExec) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	if e.first != "" {
		_, _ = io.WriteString(serial, e.first)
	}
	if syncer, ok := serial.(interface{ Sync() error }); ok {
		_ = syncer.Sync()
	}
	select {
	case <-e.wait:
	case <-ctx.Done():
		return ctx.Err()
	}
	if e.then != "" {
		_, _ = io.WriteString(serial, e.then)
	}
	return nil
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
			},
			"systemRole": "control-plane"
		},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/virtio-katl-root", "minSizeMiB": 32}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`
}
