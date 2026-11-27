package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
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

func TestRunPromotesTrialAfterManagementReadError(t *testing.T) {
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
	synctest.Test(t, func(t *testing.T) {
		isolateLinkRecoveryRecords(t)
		oldRead := readNetworkctlState
		t.Cleanup(func() { readNetworkctlState = oldRead })
		oldWait := waitForManagementNetwork
		t.Cleanup(func() { waitForManagementNetwork = oldWait })
		// The timeout stands in for the management-link deadline timer.
		waitForManagementNetwork = func(ctx context.Context) error {
			ctx, stop := context.WithTimeout(ctx, 90*time.Second)
			defer stop()
			return awaitManagementNetwork(ctx)
		}
		started := time.Now()
		readNetworkctlState = func(context.Context) ([]byte, error) {
			if time.Since(started) < time.Minute {
				return nil, errors.New("networkctl temporarily unavailable")
			}
			return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","CarrierState":"carrier","AddressState":"routable","OperationalState":"routable"}]}`), nil
		}

		if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
			t.Fatalf("run() after %s: %v (%s)", time.Since(started), err, &stdout)
		}
	})
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
	for _, tc := range []struct {
		name, want string
		wait       error
	}{
		{"offline", "management network unavailable: no usable address", errors.New("no usable address")},
		// An unarmed deadline still rejects the trial, but must not be
		// reported as an offline link.
		{"deadline not armed", "management network deadline not armed: systemctl start failed",
			fmt.Errorf("%w: %w", errManagementDeadlineNotArmed, errors.New("systemctl start failed"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			waitForManagementNetwork = func(context.Context) error { return tc.wait }
			t.Cleanup(func() { waitForManagementNetwork = oldWait })

			var stdout bytes.Buffer
			err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("run() error = %v, want %q", err, tc.want)
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
		})
	}
}

func TestManagementNetworkAcceptsOnlineReadAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateLinkRecoveryRecords(t)
		oldRead := readNetworkctlState
		t.Cleanup(func() { readNetworkctlState = oldRead })
		reads := 0
		readNetworkctlState = func(ctx context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline"}]}`), nil
			}
			// Deliver the completed online observation after the deadline is visible.
			<-ctx.Done()
			return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`), nil
		}

		ctx, stop := context.WithTimeout(t.Context(), 2*time.Second)
		defer stop()
		if err := awaitManagementNetwork(ctx); err != nil {
			t.Fatalf("management network wait rejected a successful online read: %v", err)
		}
	})
}

func TestManagementNetworkReportsLastOfflineLinkAtExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateLinkRecoveryRecords(t)
		oldRead := readNetworkctlState
		t.Cleanup(func() { readNetworkctlState = oldRead })
		readNetworkctlState = func(ctx context.Context) ([]byte, error) {
			return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline"}]}`), nil
		}
		ctx, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		err := awaitManagementNetwork(ctx)
		if err == nil || !strings.Contains(err.Error(), "systemd-networkd reports required link management0 not online") {
			t.Fatalf("management network expiry = %v, want named offline link", err)
		}
	})
}

// The failure reason reports the link-recovery owner's record as it stands,
// so a failed reconfiguration is not reported as a successful one.
func TestManagementNetworkExpiryReportsLinkRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := isolateLinkRecoveryRecords(t)
		oldRead := readNetworkctlState
		t.Cleanup(func() { readNetworkctlState = oldRead })
		readNetworkctlState = func(ctx context.Context) ([]byte, error) {
			return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier"}]}`), nil
		}
		record := `{"link":"management0","at":"2026-11-27T06:00:00Z","outcome":"failed","error":"networkctl reconfigure: exit status 1: access denied"}`
		if err := os.WriteFile(filepath.Join(dir, "management0"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}

		snapshot := func() map[string]string {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			files := make(map[string]string, len(entries))
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				files[entry.Name()] = string(data)
			}
			return files
		}
		before := snapshot()
		ctx, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		err := awaitManagementNetwork(ctx)
		if err == nil || !strings.Contains(err.Error(), "Required link management0 could not be reconfigured") ||
			!strings.Contains(err.Error(), "access denied") || strings.Contains(err.Error(), "was reconfigured") {
			t.Fatalf("management network expiry = %v, want the failed reconfiguration of management0", err)
		}
		if after := snapshot(); !maps.Equal(before, after) {
			t.Fatalf("management network expiry changed the record directory: %v -> %v", before, after)
		}
	})
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
			if tc.name == "one-shot" {
				if !strings.Contains(err.Error(), "mount efivarfs") {
					t.Fatalf("recovery with unreadable EFI = %v, want mount guidance", err)
				}
				got, err := generation.ReadBootSelection(root)
				if err != nil || got != selection {
					t.Fatalf("recovery with unreadable EFI changed one-shot selection: %#v, %v", got, err)
				}
				return
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
		PendingTransactionID:          "next-boot-apply",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    generation.DefaultPromotionPending,
		UpdatedAt:                     now.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	authorization := filepath.Join(root, "run/katl/boot-health/pending")
	if err := os.MkdirAll(filepath.Dir(authorization), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorization, []byte(`{"trialGeneration":"gen1","fallbackGeneration":"gen0"}`), 0o600); err != nil {
		t.Fatal(err)
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

// isolateLinkRecoveryRecords points the record reader at an empty directory,
// so no test reads the host's /run/katl/link-recovery.
func isolateLinkRecoveryRecords(t *testing.T) string {
	t.Helper()
	old := linkRecoveryDir
	linkRecoveryDir = t.TempDir()
	t.Cleanup(func() { linkRecoveryDir = old })
	return linkRecoveryDir
}
