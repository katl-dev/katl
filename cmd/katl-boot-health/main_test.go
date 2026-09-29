package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
)

func TestRunPromotesGenerationFromCommandLine(t *testing.T) {
	stubManagementNetwork(t)
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
	pending := filepath.Join(root, "run/katl/boot-health/pending")
	if err := os.MkdirAll(filepath.Dir(pending), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, nil, 0o644); err != nil {
		t.Fatal(err)
	}

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
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("boot deadline marker remains after success: %v", err)
	}
}

func TestRunPromotesTrialAndSetsBootDefault(t *testing.T) {
	stubManagementNetwork(t)
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-2*time.Hour))
	writeCommandGeneration(t, root, "gen1", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-90*time.Minute))
	configStatus, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID: "gen1", PreviousGeneration: "gen0",
		RequestedApplyMode: generation.ApplyModeAuto, AcceptedApplyMode: generation.ApplyModeNextBoot,
		ChangedDomains: []string{"host-configuration"}, HealthState: generation.HealthStateUnknown, UpdatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	configStatus, err = generation.MarkConfigApplyPhase(configStatus, generation.ConfigApplyPhaseNextBoot, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	configStatusPath, err := generation.ConfigApplyStatusPath(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteConfigApplyStatus(configStatusPath, configStatus); err != nil {
		t.Fatal(err)
	}
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
	configStatus, err = generation.ReadConfigApplyStatus(configStatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if configStatus.Phase != generation.ConfigApplyPhaseActive || configStatus.HealthState != generation.HealthStateHealthy {
		t.Fatalf("config status = %#v", configStatus)
	}
}

func TestRunDoesNotPromoteWithoutManagementNetwork(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 30, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
	oldWait := waitForManagementNetwork
	waitForManagementNetwork = func(context.Context) error { return errors.New("no usable address") }
	t.Cleanup(func() { waitForManagementNetwork = oldWait })

	var stdout bytes.Buffer
	err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "no usable address") {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "result=failure") || !strings.Contains(stdout.String(), "promoted=false") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	_, status, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if status.BootState != generation.BootStateFailed {
		t.Fatalf("boot state = %q, want failed", status.BootState)
	}
}

func TestManagementNetworkReadyUsesConfiguredRequiredLinks(t *testing.T) {
	for _, test := range []struct {
		name       string
		links      string
		ready      bool
		wantReason string
	}{
		{
			name:  "required management link",
			links: `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable"}]}`,
			ready: true,
		},
		{
			name:       "workload address cannot mask offline management",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier"},{"Name":"cni0","OnlineState":null,"OperationalState":"routable"}]}`,
			wantReason: "enp1s0",
		},
		{
			name:       "workload address cannot supply required route",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"degraded"},{"Name":"cni0","OnlineState":null,"OperationalState":"routable"}]}`,
			wantReason: "no required link is routable",
		},
		{
			name:  "optional secondary link is ignored",
			links: `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable"},{"Name":"enp2s0","NetworkFile":"/etc/systemd/network/20-secondary.network","RequiredForOnline":false,"OnlineState":"offline","OperationalState":"no-carrier"}]}`,
			ready: true,
		},
		{
			name:       "required secondary link blocks readiness",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable"},{"Name":"enp2s0","NetworkFile":"/etc/systemd/network/20-secondary.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier"}]}`,
			wantReason: "enp2s0",
		},
		{
			name:       "no configured management link",
			links:      `{"Interfaces":[{"Name":"cni0","OnlineState":null,"OperationalState":"routable"}]}`,
			wantReason: "no systemd-networkd link",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, reason, err := managementNetworkReady([]byte(test.links))
			if err != nil {
				t.Fatalf("managementNetworkReady() error = %v", err)
			}
			if ready != test.ready || !strings.Contains(reason, test.wantReason) {
				t.Fatalf("managementNetworkReady() = %t, %q, want %t, reason containing %q", ready, reason, test.ready, test.wantReason)
			}
		})
	}
}

func TestRunKeepsKnownGoodGenerationOnNetworkOutage(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 45, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-30*time.Minute))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
	oldWait := waitForManagementNetwork
	waitForManagementNetwork = func(context.Context) error { return errors.New("no usable address") }
	t.Cleanup(func() { waitForManagementNetwork = oldWait })

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
		t.Fatalf("known-good boot failed during network outage: %v", err)
	}
	_, status, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if !generation.IsKnownGood(status) || !strings.Contains(stdout.String(), "result=success") {
		t.Fatalf("known-good generation was failed: status=%#v output=%q", status, stdout.String())
	}
}

func TestRunDeadmanArmsRebootForTrialFailure(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 18, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-45*time.Minute))
	writeCommandGeneration(t, root, "gen1", now.Add(-30*time.Minute))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		BootedBootEntry:               "loader/entries/katl-gen1.conf",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    generation.DefaultPromotionPending,
		UpdatedAt:                     now.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	if armed, err := generation.ArmBootRecovery(root, "gen1"); err != nil || !armed {
		t.Fatalf("ArmBootRecovery(gen1) = %t, %v", armed, err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen1\n")
	oldClock := bootHealthClock
	bootHealthClock = func() time.Time { return now }
	t.Cleanup(func() { bootHealthClock = oldClock })

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=timeout", "--reason=katl-boot-health-deadline-expired", "--force-failure", "--request-reboot"}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "rebootRequested=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.FailedBootGenerationID != "gen1" || selection.RecoveryRequired {
		t.Fatalf("selection = %#v, want bounded fallback to gen0", selection)
	}
}

func TestRunDeadmanRefusesRebootWithoutFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 18, 30, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
	oldClock := bootHealthClock
	bootHealthClock = func() time.Time { return now }
	t.Cleanup(func() { bootHealthClock = oldClock })

	var stdout bytes.Buffer
	err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=timeout", "--force-failure", "--request-reboot"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "not an armed trial") {
		t.Fatalf("run() error = %v, want manual recovery guidance", err)
	}
	if !strings.Contains(stdout.String(), "recoveryRequired=true") || !strings.Contains(stdout.String(), "rebootRequested=false") {
		t.Fatalf("stdout = %q", stdout.String())
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
	for _, path := range []string{spec.Boot.UKIPath, filepath.Join("/efi", spec.Boot.LoaderEntryPath)} {
		path = filepath.Join(root, strings.TrimPrefix(path, "/"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
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

func stubManagementNetwork(t *testing.T) {
	t.Helper()
	oldWait := waitForManagementNetwork
	waitForManagementNetwork = func(context.Context) error { return nil }
	t.Cleanup(func() { waitForManagementNetwork = oldWait })
}
