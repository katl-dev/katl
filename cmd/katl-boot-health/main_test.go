package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	waitForManagementNetwork = func(context.Context, bool, io.Writer, string) error { return errors.New("no usable address") }
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
	waitForManagementNetwork = func(context.Context, bool, io.Writer, string) error { return errors.New("no usable address") }
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

func TestRunRecoveryKeepsKnownGood(t *testing.T) {
	for _, tc := range []struct {
		name, running, next string
	}{
		{name: "default", running: "gen2", next: "gen2"},
		{name: "one-shot", running: "gen1", next: "gen2"},
		{name: "queued trial", running: "gen2", next: "gen3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running := tc.running
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			for _, id := range []string{"gen1", "gen2"} {
				writeCommandGeneration(t, root, id, now.Add(-time.Hour))
				markCommandGenerationHealthy(t, root, id, now.Add(-time.Minute))
			}
			selection := generation.BootSelectionRecord{
				APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind, UpdatedAt: now,
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodGenerationID: "gen1", PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
				BootedGenerationID: running, BootedBootEntry: "loader/entries/katl-" + running + ".conf",
			}
			if running == "gen1" {
				selection.OneShot = true
				selection.TargetBootGenerationID = running
				selection.TargetBootEntry = selection.BootedBootEntry
			}
			if tc.next == "gen3" {
				writeCommandGeneration(t, root, tc.next, now)
				selection.PendingHealthValidation = true
				selection.PersistentDefaultPromotion = generation.DefaultPromotionPending
				selection.TrialGenerationID = tc.next
				selection.TrialBootEntry = "loader/entries/katl-gen3.conf"
				selection.TargetBootGenerationID = tc.next
				selection.TargetBootEntry = selection.TrialBootEntry
			}
			if err := generation.WriteBootSelection(root, selection); err != nil {
				t.Fatal(err)
			}
			cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation="+running)
			oldDefault := bootDefaultCommand
			t.Cleanup(func() { bootDefaultCommand = oldDefault })
			bootDefaultCommand = func(_, entry string) error {
				t.Fatalf("unexpected EFI default write to %s", entry)
				return nil
			}

			var stdout bytes.Buffer
			err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=failure", "--reason=katl-boot-health.service-failed", "--force-failure", "--request-reboot"}, &stdout)
			if err == nil {
				t.Fatal("successful exit would trigger the recovery service's SuccessAction=reboot")
			}
			for _, want := range []string{
				"running generation " + running + " stays known-good", "boot default is generation gen2",
				"Generation " + tc.next + " is selected for the next boot", "No automatic reboot",
				"katlctl node status <node>", "systemctl --failed", "console or out-of-band",
				"katlctl cluster apply", "katlctl node reboot <node>",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("recovery error = %q, want %q", err, want)
				}
			}
			if !strings.Contains(stdout.String(), "failed=false recoveryRequired=false rebootRequested=false") {
				t.Fatalf("boot-health result = %q, want kept generation and no reboot", stdout.String())
			}
			if tc.next == "gen3" {
				got, err := generation.ReadBootSelection(root)
				if err != nil {
					t.Fatal(err)
				}
				if got != selection {
					t.Fatalf("recovery altered the queued trial: %#v", got)
				}
			}
		})
	}
}

func TestRunDeadmanRetriesTrialFailure(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 18, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-45*time.Minute))
	writeCommandGeneration(t, root, "gen1", now.Add(-30*time.Minute))
	const fallbackEntry = "loader/entries/katl-gen0.conf"
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           "gen1",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    fallbackEntry,
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

	selectionPath, err := generation.BootSelectionPath(root)
	if err != nil {
		t.Fatal(err)
	}
	// Block the selection's atomic rename after the EFI setter succeeds.
	oldDefault := bootDefaultCommand
	t.Cleanup(func() { bootDefaultCommand = oldDefault })
	savedSelection := selectionPath + ".saved"
	firmwareDefault := "loader/entries/katl-gen1.conf"
	bootDefaultCommand = func(_, entry string) error {
		firmwareDefault = entry
		if err := os.Rename(selectionPath, savedSelection); err != nil {
			t.Fatal(err)
		}
		return os.Mkdir(selectionPath, 0o755)
	}
	args := []string{"--root", root, "--cmdline", cmdline, "--result=timeout", "--reason=katl-boot-health-deadline-expired", "--force-failure", "--request-reboot"}
	if err := run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "write boot selection") {
		t.Fatalf("run() error = %v, want the injected selection write failure", err)
	}
	if err := os.Remove(selectionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedSelection, selectionPath); err != nil {
		t.Fatal(err)
	}
	bootDefaultCommand = func(_, entry string) error {
		firmwareDefault = entry
		return nil
	}

	var stdout bytes.Buffer
	// The recovery unit's SuccessAction=reboot depends on this exit status.
	if err := run(t.Context(), args, &stdout); err != nil {
		t.Fatalf("run() after restoring selection storage = %v", err)
	}
	if !strings.Contains(stdout.String(), "rebootRequested=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.NextBootGenerationID() != "gen0" || selection.DefaultBootEntry != fallbackEntry ||
		selection.FailedBootGenerationID != "gen1" || selection.RecoveryRequired || selection.PendingHealthValidation || firmwareDefault != fallbackEntry {
		t.Fatalf("selection = %#v and EFI default = %q, want settled fallback to gen0", selection, firmwareDefault)
	}
	_, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if status.BootState != generation.BootStateFailed || status.HealthState != generation.HealthStateUnhealthy {
		t.Fatalf("trial status = %s/%s, want failed/unhealthy", status.BootState, status.HealthState)
	}
	if err := run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "automatic reboot refused") {
		t.Fatalf("run() after recovery completed = %v, want no further automatic reboot", err)
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
	waitForManagementNetwork = func(context.Context, bool, io.Writer, string) error { return nil }
	t.Cleanup(func() { waitForManagementNetwork = oldWait })
}

const (
	linkNoCarrier = `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier"}]}`
	linkRoutable  = `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable"}]}`
)

// stubNetworkctl replaces networkctl with state, shortens the bounded wait to
// deadline, and records the systemd status text boot health reports.
func stubNetworkctl(t *testing.T, deadline time.Duration, state func() (string, error)) *[]string {
	t.Helper()
	oldRead, oldDeadline, oldPoll, oldNotify := readNetworkctlState, managementNetworkDeadline, managementNetworkPoll, notifyStatus
	t.Cleanup(func() {
		readNetworkctlState, managementNetworkDeadline, managementNetworkPoll, notifyStatus = oldRead, oldDeadline, oldPoll, oldNotify
	})
	readNetworkctlState = func(context.Context) ([]byte, error) {
		data, err := state()
		return []byte(data), err
	}
	managementNetworkDeadline = deadline
	managementNetworkPoll = time.Millisecond
	var statuses []string
	notifyStatus = func(status string) { statuses = append(statuses, status) }
	return &statuses
}

// writeFirstInstall publishes gen0 the way the installer does, so it has no
// known-good generation to fall back to.
func writeFirstInstall(t *testing.T, root string, now time.Time) string {
	t.Helper()
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	spec, _, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.Initialize(root, spec); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	return writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
}

func TestRunWaitsForManagementNetworkWithoutFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC)
	cmdline := writeFirstInstall(t, root, now)
	// The cable is connected well after the bounded deadline would have expired.
	deadline := 10 * time.Millisecond
	start := time.Now()
	statuses := stubNetworkctl(t, deadline, func() (string, error) {
		if time.Since(start) < 20*deadline {
			return linkNoCarrier, nil
		}
		return linkRoutable, nil
	})

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
		t.Fatalf("run() error = %v\n%s", err, stdout.String())
	}

	waiting := "waiting for the management network: required link enp1s0: no carrier; connect a cable or check the switch port"
	if !strings.Contains(stdout.String(), "katl-boot-health: "+waiting) || !strings.Contains(stdout.String(), "promoted=true") {
		t.Fatalf("stdout = %q, want the link it waited for and a promotion", stdout.String())
	}
	if len(*statuses) != 2 || (*statuses)[0] != waiting || (*statuses)[1] != "" {
		t.Fatalf("systemd status = %q, want %q then cleared", *statuses, waiting)
	}
	_, status, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if !generation.IsKnownGood(status) {
		t.Fatalf("gen0 status = %s/%s, want good/healthy", status.BootState, status.HealthState)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.RecoveryRequired || selection.PendingHealthValidation || selection.FailedBootGenerationID != "" {
		t.Fatalf("selection = %#v, want gen0 promoted without recovery", selection)
	}
}

func TestRunFailsTrialWithoutManagementNetwork(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 19, 30, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-45*time.Minute))
	writeCommandGeneration(t, root, "gen1", now.Add(-30*time.Minute))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", TargetBootGenerationID: "gen1", TrialGenerationID: "gen1",
		PreviousKnownGoodGenerationID: "gen0", BootedGenerationID: "gen1",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", PreviousKnownGoodBootEntry: "loader/entries/katl-gen0.conf",
		TrialBootEntry: "loader/entries/katl-gen1.conf", BootedBootEntry: "loader/entries/katl-gen1.conf",
		PendingHealthValidation: true, PersistentDefaultPromotion: generation.DefaultPromotionPending,
		UpdatedAt: now.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if armed, err := generation.ArmBootRecovery(root, "gen1"); err != nil || !armed {
		t.Fatalf("ArmBootRecovery(gen1) = %t, %v", armed, err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen1\n")
	stubNetworkctl(t, 20*time.Millisecond, func() (string, error) { return linkNoCarrier, nil })
	// An unbounded wait would run until this context ends instead.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	err := run(ctx, []string{"--root", root, "--cmdline", cmdline}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "not ready after 20ms: required link enp1s0: no carrier") {
		t.Fatalf("run() error = %v, want the bounded management-network failure", err)
	}

	_, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if status.BootState != generation.BootStateFailed {
		t.Fatalf("gen1 boot state = %q, want failed", status.BootState)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.RecoveryRequired {
		t.Fatalf("selection = %#v, want fallback to gen0", selection)
	}
}

func TestRunBoundsNetworkWaitWhenFallbackCannotBeRead(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 19, 45, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-2*time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-time.Hour))
	writeCommandGeneration(t, root, "gen1", now.Add(-30*time.Minute))
	fallbackDir, err := generation.GenerationDir(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fallbackDir, "spec.json"), []byte("invalid JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	selection := generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen1",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen1.conf",
		UpdatedAt: now.Add(-time.Minute),
	}
	if err := generation.WriteBootSelection(root, selection); err != nil {
		t.Fatal(err)
	}
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen1\n")
	statuses := stubNetworkctl(t, 20*time.Millisecond, func() (string, error) { return linkNoCarrier, nil })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	err = run(ctx, []string{"--root", root, "--cmdline", cmdline}, &stdout)
	const fallbackError = "read rollback target gen0"
	if err == nil || !strings.Contains(err.Error(), fallbackError) || ctx.Err() != nil {
		t.Fatalf("run() error = %v, context = %v, want bounded wait followed by the rollback target error", err, ctx.Err())
	}
	if !strings.Contains(stdout.String(), fallbackError) {
		t.Fatalf("stdout = %q, want the fallback error", stdout.String())
	}
	if len(*statuses) == 0 {
		t.Fatal("no systemd status was reported")
	}
	lastStatus := (*statuses)[len(*statuses)-1]
	if !strings.Contains(lastStatus, "enp1s0: no carrier") || !strings.Contains(lastStatus, fallbackError) {
		t.Fatalf("last systemd status = %q, want the link and fallback errors", lastStatus)
	}

	_, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.BootState != generation.BootStatePending || persisted != selection {
		t.Fatalf("indeterminate fallback changed durable state: boot state %s, selection %#v", status.BootState, persisted)
	}
}

func TestRunRecoveryNamesRebootForManagementNetworkFailure(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 20, 0, 0, 0, time.UTC)
	cmdline := writeFirstInstall(t, root, now)
	stubNetworkctl(t, time.Minute, func() (string, error) { return "", errors.New("networkctl list: exit status 1") })

	// Boot health fails, then katl-boot-recovery.service runs as its OnFailure unit.
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, io.Discard); err == nil {
		t.Fatal("boot health succeeded without network state")
	}
	err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=failure", "--reason=katl-boot-health.service-failed", "--force-failure", "--request-reboot"}, io.Discard)

	if err == nil || !strings.Contains(err.Error(), "automatic reboot refused") || !strings.Contains(err.Error(), "katlctl node reboot <node>") {
		t.Fatalf("recovery error = %v, want reboot guidance", err)
	}
	if strings.Contains(err.Error(), "recover from the console") {
		t.Fatalf("recovery error = %v, want reboot guidance instead of console recovery", err)
	}
}

func TestRunRetainsBootableTrialWhenDefaultUKIIsMissing(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 20, 0, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
	markCommandGenerationHealthy(t, root, "gen0", now.Add(-45*time.Minute))
	writeCommandGeneration(t, root, "gen1", now.Add(-30*time.Minute))
	const defaultEntry = "loader/entries/katl-gen0.conf"
	const trialEntry = "loader/entries/katl-gen1.conf"
	// Upgrade uses an EFI one-shot while keeping the persistent default.
	selection := generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind, UpdatedAt: now,
		DefaultGenerationID: "gen0", DefaultBootEntry: defaultEntry,
		PreviousKnownGoodGenerationID: "gen0", PreviousKnownGoodBootEntry: defaultEntry,
		TargetBootGenerationID: "gen1", TargetBootEntry: trialEntry,
		TrialGenerationID: "gen1", TrialBootEntry: trialEntry,
		BootedGenerationID: "gen1", BootedBootEntry: trialEntry,
		PendingHealthValidation: true, PersistentDefaultPromotion: generation.DefaultPromotionPending,
	}
	if err := generation.WriteBootSelection(root, selection); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "efi/EFI/Linux/katl-gen0.efi")); err != nil {
		t.Fatal(err)
	}
	if armed, err := generation.ArmBootRecovery(root, "gen1"); err != nil || armed {
		t.Fatalf("ArmBootRecovery(gen1) = %t, %v, want disarmed", armed, err)
	}
	const commandLine = "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen1"
	cmdline := writeCommandLine(t, root, commandLine)
	networkErr := errors.New("networkctl list: exit status 1")
	stubNetworkctl(t, time.Minute, func() (string, error) {
		if networkErr != nil {
			return "", networkErr
		}
		return linkRoutable, nil
	})
	oldDefault := bootDefaultCommand
	t.Cleanup(func() { bootDefaultCommand = oldDefault })
	firmwareDefault := defaultEntry
	bootDefaultCommand = func(_, entry string) error {
		firmwareDefault = entry
		return nil
	}

	args := []string{"--root", root, "--cmdline", cmdline}
	if err := run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "networkctl list: exit status 1") {
		t.Fatalf("boot health = %v, want the network read failure", err)
	}
	err := run(t.Context(), append(args, "--result=failure", "--force-failure", "--request-reboot"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "automatic reboot refused") ||
		!strings.Contains(err.Error(), "run boot health on generation gen1") ||
		!strings.Contains(err.Error(), "katlctl node reboot <node>") {
		t.Fatalf("recovery error = %v, want manual reboot guidance naming gen1", err)
	}
	selection, err = generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.NextBootGenerationID() != "gen1" || selection.DefaultBootEntry != trialEntry || firmwareDefault != trialEntry ||
		!selection.RecoveryRequired || selection.PendingHealthValidation || selection.FailedBootGenerationID != "gen1" {
		t.Fatalf("selection = %#v and EFI default = %q, want gen1 selected for recovery", selection, firmwareDefault)
	}

	networkErr = nil
	if err := generation.RecordBootedGeneration(root, commandLine); err != nil {
		t.Fatalf("record reboot into gen1: %v", err)
	}
	if err := run(t.Context(), args, io.Discard); err != nil {
		t.Fatalf("boot health after network repair = %v", err)
	}
	_, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil || !generation.IsKnownGood(status) {
		t.Fatalf("gen1 after network repair = %s/%s, %v, want known-good", status.BootState, status.HealthState, err)
	}
}
