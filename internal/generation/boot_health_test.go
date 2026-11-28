package generation

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
	"golang.org/x/sys/unix"
)

var testBootTime = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

// newMover returns the owner for one boot of running over root and efi.
func newMover(root string, efi *fakeEFI, bootID, running string) *BootMover {
	return &BootMover{
		root: root, bootID: bootID, commandLine: bootHealthCommandLine(running),
		now:        func() time.Time { return testBootTime },
		readEFI:    efi.read,
		setDefault: efi.set(&efi.entries.Default),
		setOneShot: efi.set(&efi.entries.OneShot),
	}
}

// moverOn returns the owner for the first boot of running over root, with
// firmware that boots the published default as an installed node's does.
func moverOn(t *testing.T, root, running string) (*BootMover, *fakeEFI) {
	t.Helper()
	writeBootDisk(t, root)
	efi := &fakeEFI{}
	if selection, err := ReadBootSelection(root); err == nil {
		efi.entries.Default = filepath.Base(selection.DefaultBootEntry)
	}
	return newMover(root, efi, "boot-1", running), efi
}

func selectionOf(t *testing.T, root string) BootSelectionRecord {
	t.Helper()
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func statusOf(t *testing.T, root, id string) GenerationStatus {
	t.Helper()
	_, status, err := ReadGeneration(root, id)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// keepsBootState snapshots the durable boot state and the firmware variables.
// Calling the returned function fails the test if either changed.
func keepsBootState(t *testing.T, root string, efi *fakeEFI) func() {
	t.Helper()
	// Take the lock file out of the comparison.
	if err := withBootStateLock(root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	efi.written = false
	before := snapshotBootFiles(t, filepath.Join(root, "var"))
	entries := efi.entries
	return func() {
		t.Helper()
		if after := snapshotBootFiles(t, filepath.Join(root, "var")); !reflect.DeepEqual(before, after) {
			t.Fatal("boot state files changed")
		}
		if efi.entries != entries || efi.written {
			t.Fatalf("firmware changed: entries %+v, want %+v, written %t", efi.entries, entries, efi.written)
		}
	}
}

// writeTrial writes a known-good gen0 and a candidate gen1 that gen0
// precedes, with the selection a runtime without the owner leaves for a trial.
func writeTrial(t *testing.T, root string, now time.Time, booted string) BootSelectionRecord {
	t.Helper()
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	selection := BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", TargetBootGenerationID: "gen1", TrialGenerationID: "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		TargetBootEntry:               "loader/entries/katl-gen1.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		BootedGenerationID:            booted,
		BootedBootEntry:               "loader/entries/katl-" + booted + ".conf",
		PendingTransactionID:          "txn-gen1",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	}
	writeBootHealthSelection(t, root, selection)
	return selection
}

func TestPromoteLiveMakesHealthyPersistentDefault(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthFallback(t, root, "gen1")
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, Revision: 7,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0", Generation0FallbackID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		TargetBootGenerationID: "gen0", TargetBootEntry: "loader/entries/katl-gen0.conf", OneShot: true,
		UpdatedAt: now.Add(-time.Hour),
	})
	m, efi := moverOn(t, root, "gen0")
	efi.entries.OneShot = "katl-gen0.conf"

	if err := m.PromoteLive(LivePromotionRequest{GenerationID: "gen1", OperationID: "upgrade-1", Now: now}); err != nil {
		t.Fatal(err)
	}

	if efi.entries.Default != "katl-gen1.conf" || efi.entries.OneShot != "" {
		t.Fatalf("firmware = %+v, want default katl-gen1.conf and no one-shot", efi.entries)
	}
	if status := statusOf(t, root, "gen1"); status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy || status.CommittedByOperation != "upgrade-1" {
		t.Fatalf("promoted status = %#v", status)
	}
	if status := statusOf(t, root, "gen0"); status.CommitState != CommitStateSuperseded {
		t.Fatalf("previous commit state = %q", status.CommitState)
	}
	selection := selectionOf(t, root)
	if selection.Revision != 8 || selection.DefaultGenerationID != "gen1" || selection.ActiveGenerationID != "gen1" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen1.conf" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" || selection.PreviousKnownGoodGenerationID != "gen0" || selection.PendingHealthValidation || selection.OneShot || selection.PersistentDefaultPromotion != DefaultPromotionDone {
		t.Fatalf("promoted selection = %#v", selection)
	}

	// The next boot runs the promoted generation, which health confirms.
	running := restart(t, root, efi)
	if running != "gen1" {
		t.Fatalf("firmware boots %q, want gen1", running)
	}
	if _, err := newMover(root, efi, "boot-2", running).ConfirmBoot(""); err != nil {
		t.Fatalf("ConfirmBoot after live promotion: %v", err)
	}
	selection = selectionOf(t, root)
	if selection.ActiveGenerationID != "gen1" || selection.BootedGenerationID != "gen1" || selection.BootedBootEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("post-reboot selection = %#v", selection)
	}
}

func TestConfirmBootPromotesTrial(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	writeTrial(t, root, now, "gen0")
	m, efi := moverOn(t, root, "gen1")

	result, err := m.ConfirmBoot("test success")
	if err != nil {
		t.Fatalf("ConfirmBoot: %v", err)
	}

	if !result.Promoted || result.DefaultGeneration != "gen1" || result.BootDefaultEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("result = %#v, want promoted gen1", result)
	}
	if efi.entries.Default != "katl-gen1.conf" {
		t.Fatalf("firmware default = %q, want katl-gen1.conf", efi.entries.Default)
	}
	if status := statusOf(t, root, "gen1"); status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("gen1 status = %#v, want good/healthy", status)
	}
	if status := statusOf(t, root, "gen0"); status.CommitState != CommitStateSuperseded {
		t.Fatalf("gen0 commitState = %s, want superseded", status.CommitState)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "gen1" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" ||
		selection.PendingHealthValidation || selection.PersistentDefaultPromotion != DefaultPromotionDone ||
		selection.PreviousKnownGoodGenerationID != "gen0" ||
		selection.BootedGenerationID != "gen1" || selection.BootedBootEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("selection after promotion = %#v", selection)
	}
}

func TestConfirmBootRecommitsSupersededRollbackTarget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen1", TrialGenerationID: "gen0", PreviousKnownGoodGenerationID: "gen1",
		DefaultBootEntry: "loader/entries/katl-gen1.conf", TrialBootEntry: "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
		PendingTransactionID:       "rollback-gen0", PendingHealthValidation: true,
		PersistentDefaultPromotion: DefaultPromotionPending, UpdatedAt: now.Add(-time.Minute),
	})
	m, _ := moverOn(t, root, "gen0")

	if _, err := m.ConfirmBoot(""); err != nil {
		t.Fatalf("ConfirmBoot(rollback): %v", err)
	}

	if status := statusOf(t, root, "gen0"); status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("rollback target status = %#v, want committed/good/healthy", status)
	}
}

func TestConfirmBootReconcilesManualKnownGoodFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 8, 17, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen1", PreviousKnownGoodGenerationID: "gen0", BootedGenerationID: "gen1",
		DefaultBootEntry: "loader/entries/katl-gen1.conf", PreviousKnownGoodBootEntry: "loader/entries/katl-gen0.conf",
		BootedBootEntry: "loader/entries/katl-gen1.conf", PersistentDefaultPromotion: DefaultPromotionDone,
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, efi := moverOn(t, root, "gen0")

	result, err := m.ConfirmBoot("manual known-good fallback booted")
	if err != nil {
		t.Fatalf("ConfirmBoot(manual fallback): %v", err)
	}

	if !result.Promoted || result.DefaultGeneration != "gen0" || result.BootDefaultEntry != "loader/entries/katl-gen0.conf" || efi.entries.Default != "katl-gen0.conf" {
		t.Fatalf("result = %#v, firmware default %q, want reconciled gen0 default", result, efi.entries.Default)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "gen0" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen0.conf" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" {
		t.Fatalf("selection after manual fallback = %#v, want coherent gen0 boot evidence", selection)
	}
	if status := statusOf(t, root, "gen0"); status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("gen0 status = %#v, want committed/good/healthy", status)
	}
	if status := statusOf(t, root, "gen1"); status.CommitState != CommitStateSuperseded {
		t.Fatalf("gen1 commitState = %s, want superseded", status.CommitState)
	}
}

func TestConfirmBootRevalidatesFailedFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateFailed, HealthStateUnhealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen1", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen1.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		FailedBootGenerationID: "gen0", RecoveryRequired: true, UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, efi := moverOn(t, root, "gen0")

	result, err := m.ConfirmBoot("failed unit cleared")
	if err != nil {
		t.Fatalf("ConfirmBoot(revalidated fallback): %v", err)
	}

	if !result.Promoted || result.DefaultGeneration != "gen0" || result.BootDefaultEntry != "loader/entries/katl-gen0.conf" || efi.entries.Default != "katl-gen0.conf" {
		t.Fatalf("recovery result = %#v, firmware default %q, want durable gen0", result, efi.entries.Default)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "gen0" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen0.conf" || selection.PreviousKnownGoodGenerationID != "" {
		t.Fatalf("selection after revalidation = %#v, want coherent recovered gen0", selection)
	}
	// Recovery ends with a healthy boot of the published default, which gen0 now is.
	if !selection.RecoveryRequired {
		t.Fatalf("recovery cleared by a boot of %s, which was not the published default", "gen0")
	}
	restart(t, root, efi)
	if _, err := newMover(root, efi, "boot-2", "gen0").ConfirmBoot(""); err != nil {
		t.Fatal(err)
	}
	if selectionOf(t, root).RecoveryRequired {
		t.Fatal("a healthy boot of the published default left recovery required")
	}
}

func TestConfirmBootResolvesTrialOnFallbackBoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	writeTrial(t, root, now, "gen0")
	writeBootHealthFallback(t, root, "gen0")
	m, efi := moverOn(t, root, "gen0")

	if _, err := m.ConfirmBoot(""); err != nil {
		t.Fatal(err)
	}

	// The fallback never promotes a trial it did not boot; the trial ended
	// without a health result, so it is failed and the default stays.
	if status := statusOf(t, root, "gen1"); status.BootState != BootStateFailed || status.HealthState != HealthStateUnhealthy {
		t.Fatalf("candidate status = %#v, want failed/unhealthy", status)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "gen0" || selection.BootedGenerationID != "gen0" || selection.PendingHealthValidation || selection.TrialGenerationID != "" || selection.RecoveryRequired {
		t.Fatalf("selection = %#v, want gen0 default after the failed trial", selection)
	}
	if efi.entries.Default != "katl-gen0.conf" {
		t.Fatalf("firmware default = %q, want katl-gen0.conf", efi.entries.Default)
	}
}

func TestConfirmBootPromotesFirstBootGeneration(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, _ := moverOn(t, root, "gen0")

	if _, err := m.ConfirmBoot(""); err != nil {
		t.Fatalf("ConfirmBoot(first boot): %v", err)
	}

	if status := statusOf(t, root, "gen0"); status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("first boot status = %#v, want good/healthy", status)
	}
}

func TestConfirmBootIsIdempotent(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, efi := moverOn(t, root, "gen0")
	if _, err := m.ConfirmBoot(""); err != nil {
		t.Fatalf("first ConfirmBoot: %v", err)
	}

	kept := keepsBootState(t, root, efi)
	second, err := m.ConfirmBoot("")
	if err != nil {
		t.Fatalf("second ConfirmBoot: %v", err)
	}
	if second.DefaultGeneration != "gen0" {
		t.Fatalf("second result = %#v", second)
	}
	kept()

	// A deadline after success has no trial to fail and changes nothing.
	timeout, err := m.FailTrial("deadline")
	if err != nil {
		t.Fatalf("FailTrial after success: %v", err)
	}
	if timeout.Failed || timeout.RebootRequested || !timeout.KnownGood {
		t.Fatalf("FailTrial after success = %#v, want a known-good report", timeout)
	}
	kept()
}

func TestFailTrialThenFallbackBoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeTrial(t, root, now, "gen1")
	writeBootHealthFallback(t, root, "gen0")
	m, efi := moverOn(t, root, "gen1")

	result, err := m.FailTrial("deadline")
	if err != nil {
		t.Fatalf("FailTrial: %v", err)
	}

	if !result.Failed || !result.RebootRequested || result.RecoveryRequired || result.DefaultGeneration != "gen0" {
		t.Fatalf("trial failure = %#v, want one reboot to gen0", result)
	}
	if efi.entries.Default != "katl-gen0.conf" {
		t.Fatalf("firmware default = %q, want katl-gen0.conf", efi.entries.Default)
	}
	if status := statusOf(t, root, "gen1"); status.BootState != BootStateFailed || status.HealthState != HealthStateUnhealthy {
		t.Fatalf("gen1 status = %#v, want failed/unhealthy", status)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "gen0" || selection.FailedBootGenerationID != "gen1" || selection.RecoveryRequired {
		t.Fatalf("selection after failure = %#v", selection)
	}

	// The fallback boot keeps the failed trial's identity until health passes.
	running := restart(t, root, efi)
	if running != "gen0" {
		t.Fatalf("firmware boots %q, want gen0", running)
	}
	fallback := newMover(root, efi, "boot-2", running)
	if _, _, err := fallback.Activate(running); err != nil {
		t.Fatal(err)
	}
	selection = selectionOf(t, root)
	if selection.BootedGenerationID != "gen0" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" || selection.FailedBootGenerationID != "gen1" {
		t.Fatalf("fallback identity before health = %#v", selection)
	}
	if _, err := fallback.ConfirmBoot(""); err != nil {
		t.Fatal(err)
	}
	selection = selectionOf(t, root)
	if selection.BootedGenerationID != "gen0" || selection.DefaultGenerationID != "gen0" || selection.PreviousKnownGoodGenerationID != "" || selection.FailedBootGenerationID != "" {
		t.Fatalf("healthy fallback selection = %#v", selection)
	}
}

func TestActivateRejectsMismatchedRoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeTrial(t, root, now, "gen1")
	writeBootHealthFallback(t, root, "gen0")
	m, efi := moverOn(t, root, "gen1")
	if _, err := m.FailTrial("deadline"); err != nil {
		t.Fatal(err)
	}
	restart(t, root, efi)
	kept := keepsBootState(t, root, efi)

	wrong := newMover(root, efi, "boot-2", "gen0")
	wrong.commandLine = "root=PARTUUID=99999999-9999-9999-9999-999999999999 katl.generation=gen0"
	if _, _, err := wrong.Activate("gen0"); err == nil {
		t.Fatal("mismatched root recorded a fallback boot")
	}

	kept()
}

func TestActivateReplacesPreviousBootActiveGeneration(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		selection BootSelectionRecord
	}{
		{
			name: "persistent rollback",
			selection: BootSelectionRecord{
				DefaultGenerationID:           "gen0",
				TargetBootGenerationID:        "gen0",
				PreviousKnownGoodGenerationID: "gen1",
				ActiveGenerationID:            "gen1",
				BootedGenerationID:            "gen1",
				DefaultBootEntry:              "loader/entries/katl-gen0.conf",
				TargetBootEntry:               "loader/entries/katl-gen0.conf",
				PreviousKnownGoodBootEntry:    "loader/entries/katl-gen1.conf",
				BootedBootEntry:               "loader/entries/katl-gen1.conf",
			},
		},
		{
			name: "one-shot rollback",
			selection: BootSelectionRecord{
				DefaultGenerationID:           "gen1",
				TargetBootGenerationID:        "gen0",
				OneShot:                       true,
				PreviousKnownGoodGenerationID: "gen0",
				ActiveGenerationID:            "gen1",
				BootedGenerationID:            "gen1",
				DefaultBootEntry:              "loader/entries/katl-gen1.conf",
				TargetBootEntry:               "loader/entries/katl-gen0.conf",
				PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
				BootedBootEntry:               "loader/entries/katl-gen1.conf",
			},
		},
		{
			name: "booted generation unchanged after live promotion",
			selection: BootSelectionRecord{
				DefaultGenerationID:           "gen1",
				TargetBootGenerationID:        "gen0",
				OneShot:                       true,
				PreviousKnownGoodGenerationID: "gen0",
				ActiveGenerationID:            "gen1",
				BootedGenerationID:            "gen0",
				DefaultBootEntry:              "loader/entries/katl-gen1.conf",
				TargetBootEntry:               "loader/entries/katl-gen0.conf",
				PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
				BootedBootEntry:               "loader/entries/katl-gen0.conf",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthFallback(t, root, "gen0")
			selection := tc.selection
			selection.APIVersion = APIVersion
			selection.Kind = BootSelectionKind
			selection.UpdatedAt = now.Add(-time.Minute)
			writeBootHealthSelection(t, root, selection)
			m, _ := moverOn(t, root, "gen0")

			if _, _, err := m.Activate("gen0"); err != nil {
				t.Fatal(err)
			}

			// The management agent starts before boot health and reports the
			// active generation as the running one.
			got := selectionOf(t, root)
			if got.ActiveGenerationID != "gen0" || got.BootedGenerationID != "gen0" {
				t.Fatalf("selection before boot health = active %q booted %q, want gen0", got.ActiveGenerationID, got.BootedGenerationID)
			}
			if got.Revision != selection.Revision+1 {
				t.Fatalf("revision = %d, want %d", got.Revision, selection.Revision+1)
			}
		})
	}
}

func TestActivateKeepsLivePromotionWithinBoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthFallback(t, root, "gen0")
	writeBootHealthFallback(t, root, "gen1")
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", ActiveGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-time.Minute),
	})
	m, _ := moverOn(t, root, "gen0")
	if _, _, err := m.Activate("gen0"); err != nil {
		t.Fatal(err)
	}
	if err := m.PromoteLive(LivePromotionRequest{GenerationID: "gen1", OperationID: "live", Now: now}); err != nil {
		t.Fatal(err)
	}

	// Restarting systemd-sysext.service reruns activation in the same boot.
	if _, _, err := m.Activate("gen0"); err != nil {
		t.Fatal(err)
	}

	got := selectionOf(t, root)
	if got.ActiveGenerationID != "gen1" || got.BootedGenerationID != "gen0" {
		t.Fatalf("selection after repeated activation = active %q booted %q, want live gen1 on booted gen0", got.ActiveGenerationID, got.BootedGenerationID)
	}
}

func TestFailTrialReportsHealth(t *testing.T) {
	for _, tc := range []struct {
		name, running, bootDefault, target, next string
		oneShot, queued, trial                   bool
	}{
		{name: "ordinary", running: "gen2", bootDefault: "gen2", next: "gen2"},
		{name: "superseded fallback", running: "gen1", bootDefault: "gen2", next: "gen2"},
		{name: "future one-shot to running generation", running: "gen2", bootDefault: "gen2", target: "gen2", next: "gen2", oneShot: true, queued: true},
		{name: "future persistent to running generation", running: "gen2", bootDefault: "gen2", target: "gen2", next: "gen2", queued: true},
		{name: "future one-shot to another generation", running: "gen2", bootDefault: "gen2", target: "gen1", next: "gen1", oneShot: true, queued: true},
		{name: "future persistent to another generation", running: "gen2", bootDefault: "gen1", target: "gen1", next: "gen1", queued: true},
		{name: "trial without a transaction", running: "gen2", bootDefault: "gen2", next: "gen2", trial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			entry := func(id string) string { return "loader/entries/katl-" + id + ".conf" }
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			previous := "gen1"
			if tc.bootDefault == "gen1" {
				previous = "gen2"
			}
			selection := BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now.Add(-time.Minute),
				DefaultGenerationID: tc.bootDefault, DefaultBootEntry: entry(tc.bootDefault),
				PreviousKnownGoodGenerationID: previous, PreviousKnownGoodBootEntry: entry(previous),
				BootedGenerationID: tc.running, BootedBootEntry: entry(tc.running),
				ActiveGenerationID: tc.running, PersistentDefaultPromotion: DefaultPromotionDone,
				TargetBootGenerationID: tc.target, OneShot: tc.oneShot,
			}
			if tc.target != "" {
				selection.TargetBootEntry = entry(tc.target)
			}
			if tc.trial {
				selection.TrialGenerationID, selection.TrialBootEntry = "gen2", entry("gen2")
				selection.PendingHealthValidation, selection.PersistentDefaultPromotion = true, DefaultPromotionPending
			}
			writeBootHealthSelection(t, root, selection)
			m, efi := moverOn(t, root, tc.running)
			if tc.queued {
				// The loader has yet to consume the one-shot.
				efi.entries.OneShot = "katl-" + tc.target + ".conf"
			}
			kept := keepsBootState(t, root, efi)

			result, err := m.FailTrial("health failed")
			if err != nil {
				t.Fatal(err)
			}

			if !result.ReportOnly || !result.KnownGood || result.Failed || result.RebootRequested || result.RecoveryRequired ||
				result.DefaultGeneration != tc.bootDefault || result.NextBootGeneration != tc.next {
				t.Fatalf("boot health = %#v, want a report for %s with default %s and next boot %s", result, tc.running, tc.bootDefault, tc.next)
			}
			kept()
		})
	}
}

func TestFailTrialReportsOnFirstBoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", TargetBootGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", TargetBootEntry: "loader/entries/katl-gen0.conf",
		BootedBootEntry:         "loader/entries/katl-gen0.conf",
		PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, efi := moverOn(t, root, "gen0")
	kept := keepsBootState(t, root, efi)

	result, err := m.FailTrial("health failed")
	if err != nil {
		t.Fatalf("FailTrial: %v", err)
	}

	if !result.ReportOnly || result.Failed || result.RecoveryRequired || result.RebootRequested || result.KnownGood {
		t.Fatalf("failure result = %#v, want a report for the unconfirmed first boot", result)
	}
	kept()
}

func TestFailTrialConsumesManualSelection(t *testing.T) {
	for _, next := range []string{"reboot", "health retry"} {
		t.Run(next, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthFallback(t, root, "gen1")
			writeBootHealthFallback(t, root, "gen2")
			writeBootHealthSelection(t, root, BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now.Add(-time.Minute),
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodGenerationID: "gen1", PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
				BootedGenerationID: "gen2", BootedBootEntry: "loader/entries/katl-gen2.conf", ActiveGenerationID: "gen2",
				OneShot: true, TargetBootGenerationID: "gen1", TargetBootEntry: "loader/entries/katl-gen1.conf",
			})
			efi := &fakeEFI{entries: efiboot.Entries{Default: "katl-gen2.conf", OneShot: "katl-gen1.conf"}}
			running := restart(t, root, efi)
			if running != "gen1" {
				t.Fatalf("firmware boots %q, want the one-shot gen1", running)
			}
			m := newMover(root, efi, "boot-2", running)
			if _, _, err := m.Activate(running); err != nil {
				t.Fatal(err)
			}

			result, err := m.FailTrial("health failed")
			if err != nil {
				t.Fatal(err)
			}

			if !result.ReportOnly || result.RebootRequested || result.NextBootGeneration != "gen2" {
				t.Fatalf("failed one-shot = %#v, want a report with next boot gen2", result)
			}
			selection := selectionOf(t, root)
			if selection.OneShot || selection.TargetBootGenerationID != "" || selection.TargetBootEntry != "" || selection.DefaultGenerationID != "gen2" {
				t.Fatalf("consumed selection remains: %#v", selection)
			}
			if status := statusOf(t, root, "gen1"); !IsKnownGood(status) {
				t.Fatalf("one-shot generation lost known-good status: %#v", status)
			}

			if next == "reboot" {
				restart(t, root, efi)
				if _, _, err := newMover(root, efi, "boot-3", "gen2").Activate("gen2"); err != nil {
					t.Fatalf("activate default after one-shot: %v", err)
				}
				selection = selectionOf(t, root)
				if selection.BootedGenerationID != "gen2" || selection.ActiveGenerationID != "gen2" {
					t.Fatalf("default boot did not become active: %#v", selection)
				}
				return
			}

			// Health retries keep the one-shot boot temporary.
			efi.written = false
			for range 2 {
				result, err := m.ConfirmBoot("retry")
				if err != nil || result.Promoted || result.DefaultGeneration != "gen2" {
					t.Fatalf("health retry = %#v, %v, want gen1 to stay temporary", result, err)
				}
				if got := selectionOf(t, root); got.DefaultGenerationID != "gen2" || got.DefaultBootEntry != "loader/entries/katl-gen2.conf" {
					t.Fatalf("health retry changed the boot default: %#v", got)
				}
			}
			if efi.written {
				t.Fatal("health retry wrote firmware variables")
			}
		})
	}
}

func TestFailTrialSelectsFallback(t *testing.T) {
	for _, missingEntry := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing fallback entry", false: "recorded default"}[missingEntry], func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthSelection(t, root, BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now.Add(-time.Minute),
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodGenerationID: "gen1", PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
				BootedGenerationID: "gen2", BootedBootEntry: "loader/entries/katl-gen2.conf",
				TrialGenerationID: "gen2", TrialBootEntry: "loader/entries/katl-gen2.conf",
				PendingTransactionID: "next-boot-apply", PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
			})
			writeBootHealthFallback(t, root, "gen1")
			if missingEntry {
				removeBootFixture(t, filepath.Join(root, "efi/loader/entries/katl-gen1.conf"))
			}
			m, efi := moverOn(t, root, "gen2")

			result, err := m.FailTrial("health failed")
			if err != nil {
				t.Fatal(err)
			}

			if missingEntry {
				if !result.Failed || !result.RecoveryRequired || result.RebootRequested || !strings.Contains(result.FallbackProblem, "katl-gen1.conf") {
					t.Fatalf("boot health = %+v, want recovery required with missing entry", result)
				}
				if selection := selectionOf(t, root); selection.DefaultGenerationID != "gen2" || efi.entries.Default != "katl-gen2.conf" {
					t.Fatalf("default changed to %q, firmware %q", selection.DefaultGenerationID, efi.entries.Default)
				}
				return
			}

			if !result.Failed || result.DefaultGeneration != "gen1" || !result.RebootRequested || result.RecoveryRequired {
				t.Fatalf("boot health = %#v, want demotion, selected fallback and one reboot", result)
			}
			if status := statusOf(t, root, "gen2"); status.BootState != BootStateFailed || status.HealthState != HealthStateUnhealthy {
				t.Fatalf("generation status = %#v, want failed/unhealthy", status)
			}

			// A replay finishes nothing more and never asks for a second reboot.
			kept := keepsBootState(t, root, efi)
			replay, err := m.FailTrial("health failed")
			if err != nil || replay.RebootRequested || replay.DefaultGeneration != "gen1" {
				t.Fatalf("failure replay = %+v, %v", replay, err)
			}
			kept()
		})
	}
}

func TestFailTrialRequiresKnownGoodFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", TrialGenerationID: "gen1", PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry: "loader/entries/katl-gen0.conf", TrialBootEntry: "loader/entries/katl-gen1.conf",
		BootedBootEntry: "loader/entries/katl-gen1.conf", PendingTransactionID: "next-boot-apply",
		PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, _ := moverOn(t, root, "gen1")

	result, err := m.FailTrial("health failed")
	if err != nil {
		t.Fatalf("FailTrial: %v", err)
	}

	if !result.Failed || !result.RecoveryRequired || result.RebootRequested {
		t.Fatalf("failure result = %#v, want recovery required without a reboot", result)
	}
	selection := selectionOf(t, root)
	if !selection.RecoveryRequired || selection.DefaultGenerationID != "gen1" || selection.PreviousKnownGoodGenerationID != "" {
		t.Fatalf("selection after invalid rollback target = %#v, want the running candidate as default", selection)
	}
}

func TestFailTrialWithUnreadableFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "status read", true: "legacy metadata read"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
			writeBootHealthSelection(t, root, BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now,
				DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
				TrialGenerationID: "gen1", TrialBootEntry: "loader/entries/katl-gen1.conf",
				PreviousKnownGoodGenerationID: "gen0", PreviousKnownGoodBootEntry: "loader/entries/katl-gen0.conf",
				PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending, PendingTransactionID: "next-boot-apply",
			})
			path := filepath.Join(root, "var/lib/katl/generations/gen0/status.json")
			wantErr := unix.ELOOP
			if legacy {
				removeBootFixture(t, filepath.Join(filepath.Dir(path), "spec.json"))
				metadata := filepath.Join(filepath.Dir(path), "metadata.json")
				// Linux rejects reads from the unmapped offset zero of /proc/self/mem.
				linkBootFixture(t, metadata, "/proc/self/mem")
				wantErr = unix.EIO
			} else {
				removeBootFixture(t, path)
				linkBootFixture(t, path, path)
			}
			m, efi := moverOn(t, root, "gen1")
			before := selectionOf(t, root)

			_, err := m.FailTrial("health failed")

			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "bootability indeterminate") {
				t.Fatalf("failure outcome = %v, want indeterminate with %v", err, wantErr)
			}
			if after := selectionOf(t, root); after != before || efi.written {
				t.Fatalf("selection changed to %+v or firmware written (%t)", after, efi.written)
			}
			if status := statusOf(t, root, "gen1"); status.BootState != BootStateTrying {
				t.Fatalf("candidate status changed to %+v", status)
			}
		})
	}
}

func TestConfirmBootRejectsCorruptSelection(t *testing.T) {
	root := t.TempDir()
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC))
	path, err := BootSelectionPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := moverOn(t, root, "gen0")

	_, err = m.ConfirmBoot("")

	if err == nil || !strings.Contains(err.Error(), "decode boot selection") {
		t.Fatalf("ConfirmBoot(corrupt) error = %v, want decode failure", err)
	}
}

func TestConfirmBootRejectsMissingBootEvidence(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", DefaultBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now.Add(-30 * time.Minute),
	})
	m, _ := moverOn(t, root, "gen0")

	_, err := m.ConfirmBoot("")

	if err == nil || !strings.Contains(err.Error(), "bootedGenerationID is required") {
		t.Fatalf("ConfirmBoot(missing boot evidence) error = %v, want bootedGenerationID failure", err)
	}
}

func TestConfirmBootRejectsMismatchedBootEvidence(t *testing.T) {
	tests := []struct {
		name                   string
		commandLine            string
		selection              func(BootSelectionRecord) BootSelectionRecord
		missingSpecLoaderEntry bool
		want                   string
	}{
		{
			name:        "root partuuid",
			commandLine: "root=PARTUUID=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee katl.generation=gen0",
			want:        "does not match generation root PARTUUID",
		},
		{
			name:        "booted loader entry",
			commandLine: bootHealthCommandLine("gen0"),
			selection: func(selection BootSelectionRecord) BootSelectionRecord {
				selection.BootedBootEntry = "loader/entries/other.conf"
				return selection
			},
			want: "does not match generation loader entry",
		},
		{
			name:                   "missing spec loader entry",
			commandLine:            bootHealthCommandLine("gen0"),
			missingSpecLoaderEntry: true,
			want:                   "loaderEntryPath is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 6, 15, 18, 0, 0, 0, time.UTC)
			if tt.missingSpecLoaderEntry {
				writeBootHealthGenerationWithoutLoaderEntry(t, root, "gen0", now.Add(-time.Hour))
			} else {
				writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
			}
			selection := BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind,
				DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
				DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
				UpdatedAt: now.Add(-30 * time.Minute),
			}
			if tt.selection != nil {
				selection = tt.selection(selection)
			}
			writeBootHealthSelection(t, root, selection)
			m, _ := moverOn(t, root, "gen0")
			m.commandLine = tt.commandLine

			_, err := m.ConfirmBoot("")

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ConfirmBoot(%s) error = %v, want %q", tt.name, err, tt.want)
			}
		})
	}
}

func TestConfirmBootRetriesAfterFirmwareWriteFault(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC)
	before := writeTrial(t, root, now, "gen1")
	m, efi := moverOn(t, root, "gen1")
	efi.fault = "write"

	if _, err := m.ConfirmBoot(""); !errors.Is(err, errEFIFault) {
		t.Fatalf("ConfirmBoot(firmware fault) error = %v, want the injected fault", err)
	}

	// The published selection still names the old default until the move completes.
	if got := selectionOf(t, root); got != before {
		t.Fatalf("selection after fault = %#v, want it unpublished", got)
	}

	efi.fault = ""
	result, err := m.ConfirmBoot("")
	if err != nil {
		t.Fatalf("ConfirmBoot(retry): %v", err)
	}

	if !result.Promoted || efi.entries.Default != "katl-gen1.conf" {
		t.Fatalf("retry = %#v, firmware default %q, want gen1 promoted", result, efi.entries.Default)
	}
	if selection := selectionOf(t, root); selection.DefaultGenerationID != "gen1" || selection.Revision != before.Revision+1 {
		t.Fatalf("selection after retry = %#v, want gen1 at one revision bump", selection)
	}
	if _, err := os.Stat(rootedPathUnchecked(root, bootMovePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains after the retry: %v", err)
	}
}

func writeBootHealthGeneration(t *testing.T, root string, id string, previous string, commitState string, bootState string, healthState string, updatedAt time.Time) {
	t.Helper()
	record := abRecord(t, id, "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", updatedAt)
	spec := SpecFromRecord(record)
	spec.Root.RuntimeArtifactSHA256 = fmt.Sprintf("%x", sha256.Sum256(bootRootImage("boot health runtime")))
	spec.PreviousGenerationID = previous
	setExtensionDigests(t, &spec, "sysext")
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	status, err := NewGenerationStatus(spec, commitState, bootState, healthState, updatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus(%s) error = %v", id, err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration(%s) error = %v", id, err)
	}
	for _, path := range []string{spec.Boot.UKIPath, filepath.Join("/efi", spec.Boot.LoaderEntryPath)} {
		path = rootedPathUnchecked(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeBootHealthFallback(t *testing.T, root, id string) {
	t.Helper()
	spec, _, err := ReadGeneration(root, id)
	if err != nil {
		t.Fatal(err)
	}
	writeBootDisk(t, root)
	writeBootFixture(t, bootDevicePath(root, spec.Root.Slot), bootRootImage("boot health runtime"))
	for _, ref := range spec.Sysexts {
		writeBootFixture(t, rootedPathUnchecked(root, ref.Path), []byte("sysext"))
	}
	for _, ref := range spec.Confexts {
		if err := os.MkdirAll(rootedPathUnchecked(root, ref.Path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeBootHealthGenerationWithoutLoaderEntry(t *testing.T, root string, id string, updatedAt time.Time) {
	t.Helper()
	record := abRecord(t, id, "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", updatedAt)
	spec := SpecFromRecord(record)
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStatePending, HealthStateUnknown, updatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus(%s) error = %v", id, err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration(%s) error = %v", id, err)
	}
}

func bootHealthCommandLine(generationID string) string {
	return "root=PARTUUID=11111111-2222-3333-4444-555555555555 rootfstype=squashfs ro katl.generation=" + generationID
}

func writeBootHealthSelection(t *testing.T, root string, selection BootSelectionRecord) {
	t.Helper()
	if err := WriteBootSelection(root, selection); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
}

// setExtensionDigests binds a spec to the artifacts the fixtures write, so
// that activation verifies them: the sysext holds content and the confext
// directory is empty.
func setExtensionDigests(t *testing.T, spec *GenerationSpec, content string) {
	t.Helper()
	empty, err := DigestDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range spec.Sysexts {
		spec.Sysexts[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	}
	for i := range spec.Confexts {
		spec.Confexts[i].SHA256 = empty
	}
}

// restart models a reboot: firmware consumes the one-shot and picks the
// generation to boot, and the fresh /run holds no activation record.
func restart(t *testing.T, root string, efi *fakeEFI) string {
	t.Helper()
	if err := os.Remove(rootedPathUnchecked(root, currentBootPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return efi.boot()
}
