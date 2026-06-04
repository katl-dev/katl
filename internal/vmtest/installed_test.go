package vmtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
)

func TestESPCheck(t *testing.T) {
	esp := espFixture(t)
	if err := CheckESP(esp); err != nil {
		t.Fatalf("CheckESP() error = %v", err)
	}
	entry := loaderEntry(t, esp)
	data, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	data = []byte(strings.ReplaceAll(string(data), "root=PARTUUID=11111111-2222-3333-4444-555555555555 ", "root=UUID=11111111-2222-3333-4444-555555555555 "))
	if err := os.WriteFile(entry, data, 0o644); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := CheckESP(esp); err == nil {
		t.Fatal("CheckESP() succeeded with root auto-discovery")
	}
}

func TestInstalledRuntime(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "Katl state projection ready"
	runner := VMRunner{
		Executor: vmExec{write: "Katl state projection ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:         disk,
		DiskFormat:   DiskRaw,
		ESPArtifacts: esp,
		VM:           vmConfig,
	}, runner)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if _, err := os.Stat(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp)))); err != nil {
		t.Fatalf("ESP copy missing: %v", err)
	}
	if serial, err := os.ReadFile(result.Artifacts.RuntimeSerial); err != nil || !strings.Contains(string(serial), "Katl state projection ready") {
		t.Fatalf("runtime serial = %q, err = %v", serial, err)
	}
	command, err := os.ReadFile(result.Artifacts.QEMUCommand)
	if err != nil {
		t.Fatalf("read qemu command: %v", err)
	}
	if strings.Contains(string(command), "fat:rw:") {
		t.Fatalf("default runtime boot used injected ESP tree: %s", command)
	}
	entry, err := os.ReadFile(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if strings.Contains(string(entry), "katl.vmtest_agent=1") {
		t.Fatalf("default runtime boot injected vmtest agent flag: %s", entry)
	}
}

func TestInstalledRuntimeWithVMTestAgent(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "Katl state projection ready"
	runner := VMRunner{
		Executor: vmExec{write: "Katl state projection ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:               disk,
		DiskFormat:         DiskRaw,
		ESPArtifacts:       esp,
		RequireVMTestAgent: true,
		VM:                 vmConfig,
	}, runner)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	command, err := os.ReadFile(result.Artifacts.QEMUCommand)
	if err != nil {
		t.Fatalf("read qemu command: %v", err)
	}
	if !strings.Contains(string(command), "fat:rw:"+filepath.Join(result.RunDir, "esp")) {
		t.Fatalf("runtime command did not boot injected ESP tree: %s", command)
	}
	entry, err := os.ReadFile(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if !strings.Contains(string(entry), "katl.vmtest_agent=1") {
		t.Fatalf("vmtest agent flag missing from copied loader entry: %s", entry)
	}
	source, err := os.ReadFile(loaderEntry(t, esp))
	if err != nil {
		t.Fatalf("read source loader entry: %v", err)
	}
	if strings.Contains(string(source), "katl.vmtest_agent=1") {
		t.Fatalf("source ESP artifact was mutated: %s", source)
	}
}

func espFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "2026.06.03-001",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-2026.06.03-001.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/2026.06.03-001/confext/katl-node.raw",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katl",
				VersionID:    "44",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	if _, err := generation.WriteEntry(root, generation.LoaderRequest{
		Record:    record,
		MachineID: "0123456789abcdef0123456789abcdef",
	}); err != nil {
		t.Fatalf("WriteEntry() error = %v", err)
	}
	return root
}

func loaderEntry(t *testing.T, esp string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(esp, "loader", "entries", "*.conf"))
	if err != nil {
		t.Fatalf("glob loader entry: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("loader entries = %#v", matches)
	}
	return matches[0]
}
