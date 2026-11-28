package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
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
	useTestOwner(t, "katl-gen0.conf")

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
	efi := useTestOwner(t, "katl-gen0.conf")

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
	if efi.entries.Default != "katl-gen1.conf" {
		t.Fatalf("EFI default = %q, want katl-gen1.conf", efi.entries.Default)
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
			before, err := os.ReadFile(filepath.Join(root, "var/lib/katl/boot/selection.json"))
			if err != nil {
				t.Fatal(err)
			}
			cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 quiet katl.generation=gen0\n")
			// Only katl-boot-recovery.service may decide a failed boot.
			oldOwner := newBootOwner
			newBootOwner = func(string, string) (bootOwner, error) {
				t.Fatal("a failed link wait called the boot-state owner")
				return nil, nil
			}
			t.Cleanup(func() { newBootOwner = oldOwner })
			oldWait := waitForManagementNetwork
			waitForManagementNetwork = func(context.Context) error { return tc.wait }
			t.Cleanup(func() { waitForManagementNetwork = oldWait })

			var stdout bytes.Buffer
			err = run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("run() error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(stdout.String(), "generation=gen0 result=failure") {
				t.Fatalf("stdout = %q", stdout.String())
			}
			after, err := os.ReadFile(filepath.Join(root, "var/lib/katl/boot/selection.json"))
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("selection changed after a failed link wait: %s, %v", after, err)
			}
			_, status, err := generation.ReadGeneration(root, "gen0")
			if err != nil {
				t.Fatal(err)
			}
			if status.BootState != generation.BootStatePending {
				t.Fatalf("boot state = %q, want pending", status.BootState)
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
	useTestOwner(t, "katl-gen0.conf")
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

func TestRunFailureWithoutTrialIsReportOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		knownGood bool
		want      []string
	}{
		{name: "known-good", knownGood: true, want: []string{
			"running generation gen0 stays known-good", "boot default is generation gen0",
			"Generation gen0 is selected for the next boot", "No automatic reboot", "katlctl node reboot <node>",
		}},
		{name: "unproven", want: []string{
			"generation gen0 has not passed boot health and no boot trial is pending", "boot state is unchanged",
			"No automatic reboot", "katlctl node reboot <node>",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
			if tc.knownGood {
				markCommandGenerationHealthy(t, root, "gen0", now.Add(-time.Minute))
			}
			if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
				APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind, UpdatedAt: now,
				DefaultGenerationID: "gen0", DefaultBootEntry: "loader/entries/katl-gen0.conf",
				BootedGenerationID: "gen0", BootedBootEntry: "loader/entries/katl-gen0.conf",
			}); err != nil {
				t.Fatal(err)
			}
			selectionPath := filepath.Join(root, "var/lib/katl/boot/selection.json")
			before, err := os.ReadFile(selectionPath)
			if err != nil {
				t.Fatal(err)
			}
			_, statusBefore, err := generation.ReadGeneration(root, "gen0")
			if err != nil {
				t.Fatal(err)
			}
			cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen0")
			efi := useTestOwner(t, "katl-gen0.conf")
			disarmed := stubDisarm(t)

			var stdout bytes.Buffer
			err = run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=failure", "--reason=katl-boot-health.service-failed"}, &stdout)
			// A successful exit would fire the recovery unit's SuccessAction=reboot.
			if err == nil {
				t.Fatal("report-only failure exited successfully")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}

			after, readErr := os.ReadFile(selectionPath)
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("report-only failure changed the selection: %s, %v", after, readErr)
			}
			_, statusAfter, err := generation.ReadGeneration(root, "gen0")
			if err != nil || !reflect.DeepEqual(statusAfter, statusBefore) {
				t.Fatalf("report-only failure changed gen0 status: %#v, %v", statusAfter, err)
			}
			if efi.entries != (efiboot.Entries{Default: "katl-gen0.conf"}) {
				t.Fatalf("report-only failure wrote EFI entries %#v", efi.entries)
			}
			if *disarmed {
				t.Fatal("report-only failure stopped the boot trial deadline")
			}
		})
	}
}

// The owner decides a failed trial; the command maps its result to the exit
// status the unit's SuccessAction=reboot acts on.
func TestRunFailedTrialExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  generation.BootHealthResult
		wantErr string
	}{
		{name: "reboot", result: generation.BootHealthResult{
			GenerationID: "gen1", Failed: true, RebootRequested: true, DefaultGeneration: "gen0", NextBootGeneration: "gen0",
		}},
		{name: "reboot spent", result: generation.BootHealthResult{
			GenerationID: "gen1", Failed: true, DefaultGeneration: "gen0", NextBootGeneration: "gen0",
		}, wantErr: "automatic reboot was already used"},
		{name: "recovery required", result: generation.BootHealthResult{
			GenerationID: "gen1", Failed: true, RecoveryRequired: true, DefaultGeneration: "gen1", NextBootGeneration: "gen1",
		}, wantErr: "recovery is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen1")
			oldOwner := newBootOwner
			newBootOwner = func(string, string) (bootOwner, error) { return fixedOwner{tc.result}, nil }
			t.Cleanup(func() { newBootOwner = oldOwner })
			disarmed := stubDisarm(t)

			var stdout bytes.Buffer
			err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline, "--result=timeout", "--reason=katl-boot-health-deadline-expired"}, &stdout)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("run() error = %v, want success so the unit reboots", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("run() error = %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(stdout.String(), "generation=gen1 result=timeout") {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if !*disarmed {
				t.Fatal("a decided trial left the boot trial deadline running")
			}
		})
	}
}

type fixedOwner struct{ result generation.BootHealthResult }

func (o fixedOwner) ConfirmBoot(string) (generation.BootHealthResult, error) { return o.result, nil }
func (o fixedOwner) FailTrial(string) (generation.BootHealthResult, error)   { return o.result, nil }

type testEFI struct{ entries efiboot.Entries }

// useTestOwner routes the command to the real owner with EFI variables held in
// memory, starting from the given default entry.
func useTestOwner(t *testing.T, defaultEntry string) *testEFI {
	t.Helper()
	efi := &testEFI{entries: efiboot.Entries{Default: defaultEntry}}
	oldOwner := newBootOwner
	newBootOwner = func(root, commandLine string) (bootOwner, error) {
		return generation.NewBootMover(root, generation.BootMoverOptions{
			BootID:      "boot-1",
			CommandLine: commandLine,
			Now:         bootHealthClock,
			ReadEFI:     func(string) (efiboot.Entries, error) { return efi.entries, nil },
			SetDefault:  func(_, entry string) error { efi.entries.Default = filepath.Base(entry); return nil },
			SetOneShot: func(_, entry string) error {
				efi.entries.OneShot = ""
				if entry != "" {
					efi.entries.OneShot = filepath.Base(entry)
				}
				return nil
			},
		})
	}
	t.Cleanup(func() { newBootOwner = oldOwner })
	stubDisarm(t)
	return efi
}

func stubDisarm(t *testing.T) *bool {
	t.Helper()
	called := new(bool)
	old := disarmBootTrial
	disarmBootTrial = func(context.Context) error { *called = true; return nil }
	t.Cleanup(func() { disarmBootTrial = old })
	return called
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

func TestRunSuccessStopsTrialDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		disarmErr error
	}{
		{name: "stopped"},
		// The boot is decided, so a timer that will not stop is only logged.
		{name: "stop fails", disarmErr: errors.New("unit not loaded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubManagementNetwork(t)
			root := t.TempDir()
			now := time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)
			writeCommandGeneration(t, root, "gen0", now.Add(-time.Hour))
			if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
				APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
				DefaultGenerationID: "gen0", DefaultBootEntry: "loader/entries/katl-gen0.conf",
				BootedGenerationID: "gen0", BootedBootEntry: "loader/entries/katl-gen0.conf",
				UpdatedAt: now.Add(-30 * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen0")
			useTestOwner(t, "katl-gen0.conf")
			stopped := false
			old := disarmBootTrial
			disarmBootTrial = func(context.Context) error { stopped = true; return tc.disarmErr }
			t.Cleanup(func() { disarmBootTrial = old })

			var stdout bytes.Buffer
			if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if !stopped {
				t.Fatal("a confirmed boot left the trial deadline running")
			}
			if !strings.Contains(stdout.String(), "generation=gen0 result=success") {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

// A success that arrives after the deadline failed this boot's trial is
// reported as a failure and leaves the deadline running.
func TestRunLateSuccessChangesNothing(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 30, 0, 0, time.UTC)
	writeCommandGeneration(t, root, "gen1", now.Add(-time.Hour))
	cmdline := writeCommandLine(t, root, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen1")
	oldOwner := newBootOwner
	newBootOwner = func(string, string) (bootOwner, error) {
		return fixedOwner{generation.BootHealthResult{
			GenerationID: "gen1", Result: generation.BootHealthSuccess, ReportOnly: true, DefaultGeneration: "gen0", NextBootGeneration: "gen0",
		}}, nil
	}
	t.Cleanup(func() { newBootOwner = oldOwner })
	oldWait := waitForManagementNetwork
	waitForManagementNetwork = func(context.Context) error { return nil }
	t.Cleanup(func() { waitForManagementNetwork = oldWait })
	disarmed := stubDisarm(t)

	var stdout bytes.Buffer
	err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "already failed its boot trial on this boot") {
		t.Fatalf("run() error = %v, want the standing failure", err)
	}
	if *disarmed {
		t.Fatal("a late success stopped the boot trial deadline")
	}
}
