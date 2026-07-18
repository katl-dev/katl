package vmtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
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

func TestVMTestServicesDoNotDependOnBootEntryInjection(t *testing.T) {
	for _, name := range []string{"katl-vmtest-agent.service", "katl-vmtest-debug-shell.service"} {
		path := filepath.Join(repoRoot(t), "internal", "vmtest", "image", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "ConditionKernelCommandLine=") {
			t.Fatalf("%s still depends on boot-entry injection:\n%s", name, data)
		}
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
	vmConfig.Expect = runtimeBootSignal
	runner := VMRunner{
		Executor: vmExec{write: runtimeBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
	if serial, err := os.ReadFile(result.Artifacts.RuntimeSerial); err != nil || !strings.Contains(string(serial), runtimeBootSignal) {
		t.Fatalf("runtime serial = %q, err = %v", serial, err)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.Disk != disk || input.DiskFormat != string(DiskRaw) || input.ESPArtifacts != esp || input.RequireVMTestAgent {
		t.Fatalf("installed runtime input = %#v", input)
	}
	domainXML := readDomainXML(t, result)
	if strings.Contains(domainXML, "katl-efi") || !strings.Contains(domainXML, `<source file="`+filepath.Join(result.VMDir, "vda.snapshot.qcow2")+`"></source>`) {
		t.Fatalf("default runtime boot did not use the installed disk snapshot:\n%s", domainXML)
	}
	entry, err := os.ReadFile(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if strings.Contains(string(entry), "katl.vmtest_agent=1") {
		t.Fatalf("default runtime boot injected vmtest agent flag: %s", entry)
	}
	source, err := os.ReadFile(loaderEntry(t, esp))
	if err != nil {
		t.Fatalf("read source loader entry: %v", err)
	}
	if string(entry) != string(source) {
		t.Fatalf("copied loader entry was mutated:\n%s", entry)
	}
}

func TestInstalledRuntimeWithVMTestAgent(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	fixtureManifest := writeInstalledFixtureManifest(t, root, disk, esp)
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{write: runtimeKernelBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:               disk,
		DiskFormat:         DiskRaw,
		ESPArtifacts:       esp,
		FixtureManifest:    fixtureManifest,
		RequireVMTestAgent: true,
		VM:                 vmConfig,
	}, runner)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if result.VSock.GuestCID == 0 || result.VSock.Port != DefaultAgentPort {
		t.Fatalf("vsock = %#v", result.VSock)
	}
	domainXML := readDomainXML(t, result)
	if strings.Contains(domainXML, "katl-efi") || !strings.Contains(domainXML, `<source file="`+filepath.Join(result.VMDir, "vda.snapshot.qcow2")+`"></source>`) {
		t.Fatalf("runtime domain XML did not boot the installed disk snapshot:\n%s", domainXML)
	}
	entry, err := os.ReadFile(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if strings.Contains(string(entry), "katl.vmtest_agent=1") {
		t.Fatalf("installed runtime mutated the copied boot entry for VM support: %s", entry)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.Disk != disk || input.DiskFormat != string(DiskRaw) || input.ESPArtifacts != esp || !input.RequireVMTestAgent {
		t.Fatalf("installed runtime input = %#v", input)
	}
	if input.FixtureManifest != fixtureManifest {
		t.Fatalf("fixture manifest = %q, want %q", input.FixtureManifest, fixtureManifest)
	}
	diskSHA, err := fileSHA256(disk)
	if err != nil {
		t.Fatalf("hash disk: %v", err)
	}
	espSHA, err := espTreeSHA256(esp)
	if err != nil {
		t.Fatalf("hash ESP: %v", err)
	}
	if input.Fixture == nil || input.Fixture.Disk.SHA256 != diskSHA || input.Fixture.ESPArtifacts.TreeSHA256 != espSHA {
		t.Fatalf("fixture binding = %#v", input.Fixture)
	}
	source, err := os.ReadFile(loaderEntry(t, esp))
	if err != nil {
		t.Fatalf("read source loader entry: %v", err)
	}
	if strings.Contains(string(source), "katl.vmtest_agent=1") {
		t.Fatalf("source ESP artifact was mutated: %s", source)
	}
}

func TestInstalledRuntimeRejectsMalformedFixtureManifest(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	manifest := filepath.Join(root, "installed-runtime-fixture.json")
	if err := os.WriteFile(manifest, []byte(`{"kind":"Wrong"}`), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result.start(time.Now().UTC())
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:            disk,
		DiskFormat:      DiskRaw,
		ESPArtifacts:    espFixture(t),
		FixtureManifest: manifest,
	}, VMRunner{})
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "installed runtime fixture manifest has") {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestInstalledRuntimeDoesNotInjectDebugShellOption(t *testing.T) {
	t.Setenv("KATL_VMTEST_DEBUG_ON_FAILURE", "1")
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-debug-shell",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	esp := espFixture(t)
	if err := PrepareInstalledRuntime(result, InstalledRuntimeConfig{
		Disk:         disk,
		DiskFormat:   DiskRaw,
		ESPArtifacts: esp,
	}); err != nil {
		t.Fatalf("PrepareInstalledRuntime() error = %v", err)
	}
	entry, err := os.ReadFile(filepath.Join(result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if strings.Contains(string(entry), runtimeDebugShellOption) {
		t.Fatalf("debug support mutated the copied loader entry: %s", entry)
	}
}

func TestInstalledRuntimeRejectsFixtureDrift(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, disk string, esp string, metadata string)
		wantErr string
	}{
		{
			name: "disk",
			mutate: func(t *testing.T, disk string, _ string, _ string) {
				t.Helper()
				if err := os.WriteFile(disk, []byte("changed"), 0o644); err != nil {
					t.Fatalf("mutate disk: %v", err)
				}
			},
			wantErr: "disk sha256 does not match",
		},
		{
			name: "esp",
			mutate: func(t *testing.T, _ string, esp string, _ string) {
				t.Helper()
				entry := loaderEntry(t, esp)
				data, err := os.ReadFile(entry)
				if err != nil {
					t.Fatalf("read entry: %v", err)
				}
				if err := os.WriteFile(entry, append(data, []byte("# drift\n")...), 0o644); err != nil {
					t.Fatalf("mutate ESP: %v", err)
				}
			},
			wantErr: "ESP treeSHA256 does not match",
		},
		{
			name: "metadata",
			mutate: func(t *testing.T, _ string, _ string, metadata string) {
				t.Helper()
				if err := os.WriteFile(metadata, []byte(`{"kind":"NodeMetadata","changed":true}`), 0o644); err != nil {
					t.Fatalf("mutate metadata: %v", err)
				}
			},
			wantErr: "node metadata sha256 does not match",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			disk := filepath.Join(root, "installed.raw")
			if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
				t.Fatalf("write disk: %v", err)
			}
			esp := espFixture(t)
			metadata := filepath.Join(root, "node.json")
			if err := os.WriteFile(metadata, []byte(`{"kind":"NodeMetadata"}`), 0o644); err != nil {
				t.Fatalf("write node metadata: %v", err)
			}
			fixtureManifest := writeInstalledFixtureManifest(t, root, disk, esp, metadata)
			tc.mutate(t, disk, esp, metadata)
			result, err := NewRunner(Options{
				StateRoot: root,
				RunID:     "run-" + tc.name,
			}).Plan(Scenario{Name: "runtime"})
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			result.start(time.Now().UTC())
			result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
				Disk:            disk,
				DiskFormat:      DiskRaw,
				ESPArtifacts:    esp,
				FixtureManifest: fixtureManifest,
				NodeMetadata:    metadata,
			}, VMRunner{})
			if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, tc.wantErr) {
				t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
			}
		})
	}
}

func TestInstalledRuntimeRejectsFixtureSymlinkESP(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	if err := os.Symlink(loaderEntry(t, esp), filepath.Join(esp, "loader", "entries", "linked.conf")); err != nil {
		t.Fatalf("symlink ESP entry: %v", err)
	}
	fixtureManifest := writeInstalledFixtureManifestWithESPHash(t, root, disk, esp, strings.Repeat("2", 64))
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-symlink",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result.start(time.Now().UTC())
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:            disk,
		DiskFormat:      DiskRaw,
		ESPArtifacts:    esp,
		FixtureManifest: fixtureManifest,
	}, VMRunner{})
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "unsupported non-regular entry") {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestInstalledRuntimeAcceptsRelativeFixturePaths(t *testing.T) {
	root := t.TempDir()
	fixtureDir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	disk := filepath.Join(fixtureDir, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := filepath.Join(fixtureDir, "esp")
	if err := copyDir(espFixture(t), esp); err != nil {
		t.Fatalf("copy ESP: %v", err)
	}
	metadata := filepath.Join(fixtureDir, "node.json")
	if err := os.WriteFile(metadata, []byte(`{"kind":"NodeMetadata"}`), 0o644); err != nil {
		t.Fatalf("write node metadata: %v", err)
	}
	diskSHA, err := fileSHA256(disk)
	if err != nil {
		t.Fatalf("hash disk: %v", err)
	}
	espSHA, err := espTreeSHA256(esp)
	if err != nil {
		t.Fatalf("hash ESP: %v", err)
	}
	metadataSHA, err := fileSHA256(metadata)
	if err != nil {
		t.Fatalf("hash metadata: %v", err)
	}
	fixtureManifest := filepath.Join(fixtureDir, "installed-runtime-fixture.json")
	content, err := json.MarshalIndent(installedRuntimeFixtureRecord{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "InstalledRuntimeVMTestFixture",
		NodeName:   "node-1",
		SystemRole: "control-plane",
		Disk: installedRuntimeFixtureDisk{
			Path:   "installed.raw",
			Format: "raw",
			SHA256: diskSHA,
		},
		ESPArtifacts: installedRuntimeFixtureESP{
			Path:       "esp",
			TreeSHA256: espSHA,
		},
		NodeMetadata: &installedRuntimeFixtureFile{
			Path:   "node.json",
			SHA256: metadataSHA,
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(fixtureManifest, content, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-relative",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = runtimeBootSignal
	runner := VMRunner{
		Executor: vmExec{write: runtimeBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = RunInstalledRuntime(context.Background(), result, InstalledRuntimeConfig{
		Disk:            disk,
		DiskFormat:      DiskRaw,
		ESPArtifacts:    esp,
		FixtureManifest: fixtureManifest,
		VM:              vmConfig,
	}, runner)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.NodeMetadata != metadata {
		t.Fatalf("node metadata = %q, want %q", input.NodeMetadata, metadata)
	}
}

func TestInstalledRuntimeRecordIgnoresAmbientNodeMetadata(t *testing.T) {
	root := t.TempDir()
	ambientMetadata := writeFixtureFile(t, filepath.Join(root, "ambient-node.json"), `{"kind":"NodeMetadata"}`)
	t.Setenv("KATL_INSTALLED_NODE_METADATA", ambientMetadata)

	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-no-env-metadata",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if err := writeInstalledRuntimeRecord(result, InstalledRuntimeConfig{
		Disk:         filepath.Join(root, "disk.raw"),
		DiskFormat:   DiskRaw,
		ESPArtifacts: filepath.Join(root, "esp"),
	}); err != nil {
		t.Fatalf("writeInstalledRuntimeRecord() error = %v", err)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.NodeMetadata != "" {
		t.Fatalf("node metadata = %q, want empty", input.NodeMetadata)
	}
}

func TestInstalledRuntimeRecordIgnoresAmbientFixtureManifest(t *testing.T) {
	root := t.TempDir()
	disk := writeFixtureFile(t, filepath.Join(root, "disk.raw"), "disk")
	esp := espFixture(t)
	ambientManifest := writeInstalledFixtureManifest(t, root, disk, esp)
	t.Setenv("KATL_INSTALLED_FIXTURE_MANIFEST", ambientManifest)

	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-no-env-fixture",
	}).Plan(Scenario{Name: "runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if err := writeInstalledRuntimeRecord(result, InstalledRuntimeConfig{
		Disk:         disk,
		DiskFormat:   DiskRaw,
		ESPArtifacts: esp,
	}); err != nil {
		t.Fatalf("writeInstalledRuntimeRecord() error = %v", err)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.FixtureManifest != "" || input.Fixture != nil {
		t.Fatalf("installed runtime input used ambient fixture manifest: %#v", input)
	}
}

func readInstalledRuntimeInput(t *testing.T, path string) installedRuntimeRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed runtime input: %v", err)
	}
	var record installedRuntimeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode installed runtime input: %v", err)
	}
	return record
}

func writeInstalledFixtureManifest(t *testing.T, root string, disk string, esp string, metadata ...string) string {
	t.Helper()
	espSHA, err := espTreeSHA256(esp)
	if err != nil {
		t.Fatalf("hash ESP: %v", err)
	}
	return writeInstalledFixtureManifestWithESPHash(t, root, disk, esp, espSHA, metadata...)
}

func writeInstalledFixtureManifestWithESPHash(t *testing.T, root string, disk string, esp string, espSHA string, metadata ...string) string {
	t.Helper()
	path := filepath.Join(root, "installed-runtime-fixture.json")
	diskSHA, err := fileSHA256(disk)
	if err != nil {
		t.Fatalf("hash disk: %v", err)
	}
	record := installedRuntimeFixtureRecord{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "InstalledRuntimeVMTestFixture",
		NodeName:   "node-1",
		SystemRole: "control-plane",
		Disk: installedRuntimeFixtureDisk{
			Path:   disk,
			Format: "raw",
			SHA256: diskSHA,
		},
		ESPArtifacts: installedRuntimeFixtureESP{
			Path:       esp,
			TreeSHA256: espSHA,
		},
	}
	if len(metadata) > 0 && metadata[0] != "" {
		metadataSHA, err := fileSHA256(metadata[0])
		if err != nil {
			t.Fatalf("hash node metadata: %v", err)
		}
		record.NodeMetadata = &installedRuntimeFixtureFile{
			Path:   metadata[0],
			SHA256: metadataSHA,
		}
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture manifest: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	return path
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
				ID:           "katlos",
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
