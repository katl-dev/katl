package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
)

func TestRunPromotesGenerationFromCommandLine(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "gen0",
		BootedGenerationID:  "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:     "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	cmdline := filepath.Join(root, "proc/cmdline")
	if err := os.MkdirAll(filepath.Dir(cmdline), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cmdline, []byte("root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldClock := bootHealthClock
	bootHealthClock = func() time.Time { return now }
	t.Cleanup(func() { bootHealthClock = oldClock })

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result", generation.BootHealthSuccess}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "generation=gen0") || !strings.Contains(stdout.String(), "promoted=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	_, status, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if status.BootState != generation.BootStateGood || status.HealthState != generation.HealthStateHealthy {
		t.Fatalf("status = %#v, want good/healthy", status)
	}
}

func TestRunPromotesTrialAndSetsBootDefault(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-2*time.Hour))
	writeCommandGeneration(t, root, "gen1", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-90*time.Minute))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:             generation.APIVersion,
		Kind:                   generation.BootSelectionKind,
		DefaultGenerationID:    "gen0",
		TargetBootGenerationID: "gen1",
		TrialGenerationID:      "gen1",
		BootedGenerationID:     "gen1",
		DefaultBootEntry:       "loader/entries/katl-gen0.conf",
		TargetBootEntry:        "loader/entries/katl-gen1.conf",
		TrialBootEntry:         "loader/entries/katl-gen1.conf",
		BootedBootEntry:        "loader/entries/katl-gen1.conf",
		UpdatedAt:              now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen1\n")
	oldClock := bootHealthClock
	bootHealthClock = func() time.Time { return now }
	t.Cleanup(func() { bootHealthClock = oldClock })
	oldBootDefault := bootDefaultCommand
	var gotRoot, gotEntry string
	bootDefaultCommand = func(root string, bootEntry string) error {
		gotRoot = root
		gotEntry = bootEntry
		return nil
	}
	t.Cleanup(func() { bootDefaultCommand = oldBootDefault })

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result", generation.BootHealthSuccess}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if gotRoot != root || gotEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("boot default call = (%q, %q), want (%q, loader/entries/katl-gen1.conf)", gotRoot, gotEntry, root)
	}
	if !strings.Contains(stdout.String(), "generation=gen1") || !strings.Contains(stdout.String(), "promoted=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunDeadmanRequestsReboot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 18, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "gen0",
		BootedGenerationID:  "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:     "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
	oldClock := bootHealthClock
	bootHealthClock = func() time.Time { return now }
	t.Cleanup(func() { bootHealthClock = oldClock })

	marker := filepath.Join(root, "run/katl/boot-health/reboot-requested")
	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=timeout", "--reason=katl-boot-health-deadline-expired", "--request-reboot"}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "rebootRequested=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "result=timeout") {
		t.Fatalf("reboot marker = %q, %v", data, err)
	}
}

func writeCommandGeneration(t *testing.T, root string, id string, created time.Time) {
	t.Helper()
	spec := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   id,
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl-" + id + ".efi",
			LoaderEntryPath: "loader/entries/katl-" + id + ".conf",
		},
		CreatedAt: created,
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, created)
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
}

func markCommandGenerationHealthy(t *testing.T, root string, id string, at time.Time) {
	t.Helper()
	spec, status, err := generation.ReadGeneration(root, id)
	if err != nil {
		t.Fatalf("ReadGeneration(%s) error = %v", id, err)
	}
	status.BootState = generation.BootStateGood
	status.HealthState = generation.HealthStateHealthy
	status.UpdatedAt = at
	if err := generation.WriteGenerationStatus(root, spec, status); err != nil {
		t.Fatalf("WriteGenerationStatus(%s) error = %v", id, err)
	}
}

func writeCommandLine(t *testing.T, root string, commandLine string) string {
	t.Helper()
	cmdline := filepath.Join(root, "proc/cmdline")
	if err := os.MkdirAll(filepath.Dir(cmdline), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cmdline, []byte(commandLine), 0o644); err != nil {
		t.Fatal(err)
	}
	return cmdline
}
