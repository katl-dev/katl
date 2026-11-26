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

	"golang.org/x/sys/unix"
)

func TestPromoteLiveGenerationMakesHealthyPersistentDefault(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:             APIVersion,
		Kind:                   BootSelectionKind,
		DefaultGenerationID:    "gen0",
		BootedGenerationID:     "gen0",
		Generation0FallbackID:  "gen0",
		DefaultBootEntry:       "loader/entries/katl-gen0.conf",
		BootedBootEntry:        "loader/entries/katl-gen0.conf",
		TargetBootGenerationID: "gen0",
		TargetBootEntry:        "loader/entries/katl-gen0.conf",
		OneShot:                true,
		UpdatedAt:              now.Add(-time.Hour),
	})
	var nextBoot string
	if err := PromoteLiveGeneration(LivePromotionRequest{
		Root: root, GenerationID: "gen1", OperationID: "upgrade-1", Now: now,
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen1.conf"),
		SetBootOneshot: func(_ string, entry string) error { nextBoot = entry; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if nextBoot != "loader/entries/katl-gen1.conf" {
		t.Fatalf("firmware next boot = %q", nextBoot)
	}
	_, status, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy || status.CommittedByOperation != "upgrade-1" {
		t.Fatalf("promoted status = %#v", status)
	}
	_, previous, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	if previous.CommitState != CommitStateSuperseded {
		t.Fatalf("previous commit state = %q", previous.CommitState)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "gen1" || selection.ActiveGenerationID != "gen1" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen1.conf" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" || selection.PreviousKnownGoodGenerationID != "gen0" || selection.PendingHealthValidation || selection.PersistentDefaultPromotion != DefaultPromotionDone {
		t.Fatalf("promoted selection = %#v", selection)
	}
	if _, err := RecordBootHealth(BootHealthRequest{
		Root: root, GenerationID: "gen1", CommandLine: bootHealthCommandLine("gen1"), Result: BootHealthSuccess, Now: now.Add(time.Minute),
		SetBootDefault: bootHealthDefaultRecorder(t, ""),
	}); err != nil {
		t.Fatalf("RecordBootHealth(after live promotion) error = %v", err)
	}
	selection, err = ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.ActiveGenerationID != "gen1" || selection.BootedGenerationID != "gen1" || selection.BootedBootEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("post-reboot selection = %#v", selection)
	}
}

func TestPromoteLiveGenerationPersistsPromotionBeforeBootDefault(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf", UpdatedAt: now.Add(-time.Hour),
	})
	inspected := false
	err := PromoteLiveGeneration(LivePromotionRequest{
		Root: root, GenerationID: "gen1", OperationID: "upgrade-1", Now: now,
		SetBootOneshot: func(string, string) error { return nil },
		SetBootDefault: func(_ string, entry string) error {
			if entry != "loader/entries/katl-gen1.conf" {
				t.Fatalf("boot default entry = %q", entry)
			}
			_, candidate, err := ReadGeneration(root, "gen1")
			if err != nil {
				t.Fatal(err)
			}
			_, previous, err := ReadGeneration(root, "gen0")
			if err != nil {
				t.Fatal(err)
			}
			selection, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.CommitState != CommitStateCommitted || candidate.BootState != BootStateGood || candidate.HealthState != HealthStateHealthy || previous.CommitState != CommitStateSuperseded || selection.DefaultGenerationID != "gen1" || selection.ActiveGenerationID != "gen1" {
				t.Fatalf("durable state before boot default: candidate=%#v previous=%#v selection=%#v", candidate, previous, selection)
			}
			inspected = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inspected {
		t.Fatal("boot default updater was not called")
	}
}

func TestPromoteLiveGenerationRestoresDurableStateWhenBootDefaultFails(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf", UpdatedAt: now.Add(-time.Hour),
	})
	calls := 0
	err := PromoteLiveGeneration(LivePromotionRequest{
		Root: root, GenerationID: "gen1", OperationID: "upgrade-1", Now: now,
		SetBootOneshot: func(string, string) error { return nil },
		SetBootDefault: func(_ string, entry string) error {
			calls++
			if calls == 1 {
				if entry != "loader/entries/katl-gen1.conf" {
					t.Fatalf("new boot default = %q", entry)
				}
				return os.ErrPermission
			}
			if entry != "loader/entries/katl-gen0.conf" {
				t.Fatalf("restored boot default = %q", entry)
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "set boot default") {
		t.Fatalf("PromoteLiveGeneration() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("boot default calls = %d, want update and restore", calls)
	}
	_, candidate, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	_, previous, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.CommitState != CommitStateSuperseded || candidate.BootState != BootStateGood || candidate.HealthState != HealthStateHealthy || previous.CommitState != CommitStateCommitted || selection.DefaultGenerationID != "gen0" || selection.ActiveGenerationID != "" || selection.BootedGenerationID != "gen0" {
		t.Fatalf("restored state: candidate=%#v previous=%#v selection=%#v", candidate, previous, selection)
	}
}

func TestPromoteLiveGenerationRestoresBootSelectionWhenNextBootFails(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf", UpdatedAt: now.Add(-time.Hour),
	})
	defaultEntry := "loader/entries/katl-gen0.conf"
	var nextBoot string
	err := PromoteLiveGeneration(LivePromotionRequest{
		Root: root, GenerationID: "gen1", OperationID: "upgrade-1", Now: now,
		SetBootDefault: func(_ string, entry string) error { defaultEntry = entry; return nil },
		SetBootOneshot: func(_ string, entry string) error {
			nextBoot = entry
			if entry == "loader/entries/katl-gen1.conf" {
				return os.ErrPermission
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "set boot one-shot") {
		t.Fatalf("PromoteLiveGeneration() error = %v", err)
	}
	if defaultEntry != "loader/entries/katl-gen0.conf" || nextBoot != "" {
		t.Fatalf("firmware default=%q next=%q, want previous default without a one-shot", defaultEntry, nextBoot)
	}
	_, previous, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if previous.CommitState != CommitStateCommitted || selection.DefaultGenerationID != "gen0" {
		t.Fatalf("previous state=%#v selection=%#v", previous, selection)
	}
}

func TestPromoteLiveGenerationRestoresDurableStateWhenBootDefaultRestoreFails(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf", UpdatedAt: now.Add(-time.Hour),
	})
	calls := 0
	err := PromoteLiveGeneration(LivePromotionRequest{
		Root: root, GenerationID: "gen1", OperationID: "upgrade-1", Now: now,
		SetBootOneshot: func(string, string) error { return nil },
		SetBootDefault: func(_ string, _ string) error {
			calls++
			return os.ErrPermission
		},
	})
	if err == nil || !strings.Contains(err.Error(), "restore boot default") {
		t.Fatalf("PromoteLiveGeneration() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("boot default calls = %d, want update and restore attempts", calls)
	}
	_, candidate, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	_, previous, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.CommitState != CommitStateSuperseded || previous.CommitState != CommitStateCommitted || selection.DefaultGenerationID != "gen0" || selection.ActiveGenerationID != "" {
		t.Fatalf("restored durable state: candidate=%#v previous=%#v selection=%#v", candidate, previous, selection)
	}
}

func TestRecordBootHealthPromotesArmedSelection(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-1*time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		TargetBootEntry:               "loader/entries/katl-gen1.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:               "loader/entries/katl-gen1.conf",
		PendingTransactionID:          "txn-gen1",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	})

	result, err := RecordBootHealth(BootHealthRequest{
		Root:           root,
		GenerationID:   "gen1",
		CommandLine:    bootHealthCommandLine("gen1"),
		Result:         BootHealthSuccess,
		Reason:         "test success",
		Now:            now,
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen1.conf"),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(success) error = %v", err)
	}
	if !result.Promoted || result.DefaultGeneration != "gen1" || !result.BootDefaultSet || result.BootDefaultEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("result = %#v, want promoted gen1", result)
	}
	_, gen1Status, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatalf("ReadGeneration(gen1) error = %v", err)
	}
	if gen1Status.BootState != BootStateGood || gen1Status.HealthState != HealthStateHealthy {
		t.Fatalf("gen1 status = %#v, want good/healthy", gen1Status)
	}
	_, gen0Status, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if gen0Status.CommitState != CommitStateSuperseded {
		t.Fatalf("gen0 commitState = %s, want superseded", gen0Status.CommitState)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen1" ||
		selection.TargetBootGenerationID != "" ||
		selection.TrialGenerationID != "" ||
		selection.PendingHealthValidation ||
		selection.PersistentDefaultPromotion != DefaultPromotionDone ||
		selection.PreviousKnownGoodGenerationID != "gen0" {
		t.Fatalf("selection after promotion = %#v", selection)
	}
}

func TestRecordBootHealthRecommitsSupersededRollbackTarget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen1",
		TrialGenerationID:             "gen0",
		PreviousKnownGoodGenerationID: "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen1.conf",
		TrialBootEntry:                "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen1.conf",
		PendingTransactionID:          "rollback-gen0",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-time.Minute),
	})
	if _, err := RecordBootHealth(BootHealthRequest{
		Root:           root,
		GenerationID:   "gen0",
		CommandLine:    bootHealthCommandLine("gen0"),
		Result:         BootHealthSuccess,
		Now:            now,
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen0.conf"),
	}); err != nil {
		t.Fatalf("RecordBootHealth(rollback) error = %v", err)
	}
	_, status, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("rollback target status = %#v, want committed/good/healthy", status)
	}
}

func TestRecordBootHealthReconcilesManualKnownGoodFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 8, 17, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:               "loader/entries/katl-gen1.conf",
		PersistentDefaultPromotion:    DefaultPromotionDone,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	})

	result, err := RecordBootHealth(BootHealthRequest{
		Root:           root,
		GenerationID:   "gen0",
		CommandLine:    bootHealthCommandLine("gen0"),
		Result:         BootHealthSuccess,
		Reason:         "manual known-good fallback booted",
		Now:            now,
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen0.conf"),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(manual fallback) error = %v", err)
	}
	if !result.Promoted || !result.BootDefaultSet || result.DefaultGeneration != "gen0" || result.BootDefaultEntry != "loader/entries/katl-gen0.conf" {
		t.Fatalf("result = %#v, want reconciled gen0 default", result)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen0.conf" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" {
		t.Fatalf("selection after manual fallback = %#v, want coherent gen0 boot evidence", selection)
	}
	_, gen0Status, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if gen0Status.CommitState != CommitStateCommitted || gen0Status.BootState != BootStateGood || gen0Status.HealthState != HealthStateHealthy {
		t.Fatalf("gen0 status = %#v, want committed/good/healthy", gen0Status)
	}
	_, gen1Status, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatalf("ReadGeneration(gen1) error = %v", err)
	}
	if gen1Status.CommitState != CommitStateSuperseded {
		t.Fatalf("gen1 commitState = %s, want superseded", gen1Status.CommitState)
	}
}

func TestRecordBootHealthRevalidatesFailedManualFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateSuperseded, BootStateFailed, HealthStateUnhealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:             APIVersion,
		Kind:                   BootSelectionKind,
		DefaultGenerationID:    "gen1",
		BootedGenerationID:     "gen0",
		DefaultBootEntry:       "loader/entries/katl-gen1.conf",
		BootedBootEntry:        "loader/entries/katl-gen0.conf",
		FailedBootGenerationID: "gen0",
		RecoveryRequired:       true,
		UpdatedAt:              now.Add(-30 * time.Minute),
	})

	recovered, err := RecordBootHealth(BootHealthRequest{
		Root:           root,
		GenerationID:   "gen0",
		CommandLine:    bootHealthCommandLine("gen0"),
		Result:         BootHealthSuccess,
		Reason:         "failed unit cleared",
		Now:            now.Add(time.Minute),
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen0.conf"),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(revalidated fallback) error = %v", err)
	}
	if !recovered.Promoted || !recovered.BootDefaultSet || recovered.DefaultGeneration != "gen0" || recovered.BootDefaultEntry != "loader/entries/katl-gen0.conf" {
		t.Fatalf("recovery result = %#v, want durable gen0", recovered)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.BootedGenerationID != "gen0" || selection.DefaultBootEntry != "loader/entries/katl-gen0.conf" || selection.PreviousKnownGoodGenerationID != "" || selection.RecoveryRequired {
		t.Fatalf("selection after revalidation = %#v, want coherent recovered gen0", selection)
	}
}

func TestRecordBootHealthInfersBootedTrialFromCommandLine(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen0",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		TargetBootEntry:               "loader/entries/katl-gen1.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:               "loader/entries/katl-gen0.conf",
		PendingTransactionID:          "txn-gen1",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	})

	result, err := RecordBootHealth(BootHealthRequest{
		Root:           root,
		GenerationID:   "gen1",
		CommandLine:    bootHealthCommandLine("gen1"),
		Result:         BootHealthSuccess,
		Reason:         "trial booted",
		Now:            now,
		SetBootDefault: bootHealthDefaultRecorder(t, "loader/entries/katl-gen1.conf"),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(success) error = %v", err)
	}
	if !result.Promoted || result.DefaultGeneration != "gen1" {
		t.Fatalf("result = %#v, want promoted gen1", result)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.BootedGenerationID != "gen1" || selection.BootedBootEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("selection boot evidence = %#v, want inferred gen1 trial", selection)
	}
}

func TestRecordBootHealthRejectsHealthyFallbackDuringPendingTrial(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	want := BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen0",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		TargetBootEntry:               "loader/entries/katl-gen1.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:               "loader/entries/katl-gen0.conf",
		PendingTransactionID:          "txn-gen1",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	}
	writeBootHealthSelection(t, root, want)

	_, err := RecordBootHealth(BootHealthRequest{
		Root:         root,
		GenerationID: "gen0",
		CommandLine:  bootHealthCommandLine("gen0"),
		Result:       BootHealthSuccess,
		Now:          now,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match pending boot target gen1") {
		t.Fatalf("RecordBootHealth(fallback) error = %v, want pending target mismatch", err)
	}
	selection, readErr := ReadBootSelection(root)
	if readErr != nil {
		t.Fatalf("ReadBootSelection() error = %v", readErr)
	}
	if !selection.PendingHealthValidation || selection.TargetBootGenerationID != "gen1" || selection.DefaultGenerationID != "gen0" {
		t.Fatalf("selection after rejected fallback = %#v, want pending gen1 preserved", selection)
	}
}

func TestRecordBootHealthPromotesFirstBootPendingGeneration(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:          APIVersion,
		Kind:                BootSelectionKind,
		DefaultGenerationID: "gen0",
		BootedGenerationID:  "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:     "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-30 * time.Minute),
	})

	if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now}); err != nil {
		t.Fatalf("RecordBootHealth(first boot success) error = %v", err)
	}
	_, status, err := ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("first boot status = %#v, want good/healthy", status)
	}
}

func TestRecordBootHealthTimeoutRestoresPreviousAndRequestsReboot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TargetBootGenerationID:        "gen1",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		BootedBootEntry:               "loader/entries/katl-gen1.conf",
		PendingTransactionID:          "txn-gen1",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	})
	writeBootHealthFallback(t, root, "gen0")
	if armed, err := ArmBootRecovery(root, "gen1"); err != nil || !armed {
		t.Fatalf("ArmBootRecovery(gen1) = %t, %v", armed, err)
	}
	result, err := RecordBootHealth(BootHealthRequest{
		Root:          root,
		GenerationID:  "gen1",
		CommandLine:   bootHealthCommandLine("gen1"),
		Result:        BootHealthTimeout,
		Reason:        "deadline",
		Now:           now,
		RequestReboot: true,
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(timeout) error = %v", err)
	}
	if !result.Failed || !result.RebootRequested || result.DefaultGeneration != "gen0" {
		t.Fatalf("timeout result = %#v, want failed/reboot/default gen0", result)
	}
	_, gen1Status, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatalf("ReadGeneration(gen1) error = %v", err)
	}
	if gen1Status.BootState != BootStateFailed || gen1Status.HealthState != HealthStateUnhealthy {
		t.Fatalf("gen1 status = %#v, want failed/unhealthy", gen1Status)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.FailedBootGenerationID != "gen1" || selection.RecoveryRequired {
		t.Fatalf("selection after timeout = %#v", selection)
	}
	if selection.BootedGenerationID != "gen1" || selection.BootedBootEntry != "loader/entries/katl-gen1.conf" {
		t.Fatalf("failed trial boot evidence = %#v, want gen1 until the rollback boot occurs", selection)
	}
	if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now.Add(time.Minute)}); err != nil {
		t.Fatalf("RecordBootHealth(recovered gen0 success) error = %v", err)
	}
}

func TestRecordBootedGenerationAfterTrialFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
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
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-time.Minute),
	})
	if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen1", CommandLine: bootHealthCommandLine("gen1"), Result: BootHealthFailure, Now: now}); err != nil {
		t.Fatal(err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.BootedGenerationID != "gen1" || selection.FailedBootGenerationID != "gen1" {
		t.Fatalf("failed trial identity = %#v", selection)
	}
	if err := RecordBootedGeneration(root, "root=PARTUUID=99999999-9999-9999-9999-999999999999 katl.generation=gen0"); err == nil {
		t.Fatal("mismatched root recorded a fallback boot")
	}
	selection, err = ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.BootedGenerationID != "gen1" {
		t.Fatalf("invalid boot evidence changed identity: %#v", selection)
	}

	if err := RecordBootedGeneration(root, bootHealthCommandLine("gen0")); err != nil {
		t.Fatal(err)
	}
	selection, err = ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.BootedGenerationID != "gen0" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" || selection.FailedBootGenerationID != "gen1" {
		t.Fatalf("fallback identity before health = %#v", selection)
	}
	if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	selection, err = ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.BootedGenerationID != "gen0" || selection.DefaultGenerationID != "gen0" || selection.PreviousKnownGoodGenerationID != "" || selection.FailedBootGenerationID != "" {
		t.Fatalf("healthy fallback selection = %#v", selection)
	}
}

func TestRecordBootedGenerationReplacesPreviousBootActiveGeneration(t *testing.T) {
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
			selection := tc.selection
			selection.APIVersion = APIVersion
			selection.Kind = BootSelectionKind
			selection.UpdatedAt = now.Add(-time.Minute)
			writeBootHealthSelection(t, root, selection)

			if err := RecordBootedGeneration(root, bootHealthCommandLine("gen0")); err != nil {
				t.Fatal(err)
			}

			// The management agent starts before boot health and reports the
			// active generation as the running one.
			got, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if got.ActiveGenerationID != "gen0" || got.BootedGenerationID != "gen0" {
				t.Fatalf("selection before boot health = active %q booted %q, want gen0", got.ActiveGenerationID, got.BootedGenerationID)
			}
		})
	}
}

func TestRecordBootedGenerationKeepsLivePromotionWithinBoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:          APIVersion,
		Kind:                BootSelectionKind,
		DefaultGenerationID: "gen0",
		ActiveGenerationID:  "gen0",
		BootedGenerationID:  "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:     "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-time.Minute),
	})
	if err := RecordBootedGeneration(root, bootHealthCommandLine("gen0")); err != nil {
		t.Fatal(err)
	}
	setter := func(string, string) error { return nil }
	if err := PromoteLiveGeneration(LivePromotionRequest{Root: root, GenerationID: "gen1", OperationID: "live", Now: now, SetBootDefault: setter, SetBootOneshot: setter}); err != nil {
		t.Fatal(err)
	}

	// Restarting systemd-sysext.service reruns activation in the same boot.
	if err := RecordBootedGeneration(root, bootHealthCommandLine("gen0")); err != nil {
		t.Fatal(err)
	}

	got, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveGenerationID != "gen1" || got.BootedGenerationID != "gen0" {
		t.Fatalf("selection after repeated activation = active %q booted %q, want live gen1 on booted gen0", got.ActiveGenerationID, got.BootedGenerationID)
	}
}

func TestRecordBootHealthFailureWithoutPreviousRequiresRecovery(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                 APIVersion,
		Kind:                       BootSelectionKind,
		DefaultGenerationID:        "gen0",
		TargetBootGenerationID:     "gen0",
		BootedGenerationID:         "gen0",
		DefaultBootEntry:           "loader/entries/katl-gen0.conf",
		TargetBootEntry:            "loader/entries/katl-gen0.conf",
		BootedBootEntry:            "loader/entries/katl-gen0.conf",
		PendingHealthValidation:    true,
		PersistentDefaultPromotion: DefaultPromotionPending,
		UpdatedAt:                  now.Add(-30 * time.Minute),
	})

	result, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthFailure, Now: now, RequestReboot: true})
	if err != nil {
		t.Fatalf("RecordBootHealth(failure) error = %v", err)
	}
	if !result.Failed || !result.RecoveryRequired || result.RebootRequested {
		t.Fatalf("failure result = %#v, want manual recovery without reboot", result)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if !selection.RecoveryRequired || selection.FailedBootGenerationID != "gen0" {
		t.Fatalf("selection after failure = %#v", selection)
	}
}

func TestRecordBootHealthRejectsCorruptSelection(t *testing.T) {
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

	_, err = RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess})
	if err == nil || !strings.Contains(err.Error(), "decode boot selection") {
		t.Fatalf("RecordBootHealth(corrupt) error = %v, want decode failure", err)
	}
}

func TestRecordBootHealthIsIdempotentAfterPromotion(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:          APIVersion,
		Kind:                BootSelectionKind,
		DefaultGenerationID: "gen0",
		BootedGenerationID:  "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		BootedBootEntry:     "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-30 * time.Minute),
	})

	first, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now})
	if err != nil {
		t.Fatalf("first RecordBootHealth(success) error = %v", err)
	}
	second, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("second RecordBootHealth(success) error = %v", err)
	}
	if !first.Promoted || !second.Promoted || second.DefaultGeneration != "gen0" {
		t.Fatalf("success results = %#v / %#v", first, second)
	}
	timeout, err := RecordBootHealth(BootHealthRequest{
		Root:          root,
		GenerationID:  "gen0",
		CommandLine:   bootHealthCommandLine("gen0"),
		Result:        BootHealthTimeout,
		Now:           now.Add(2 * time.Minute),
		RequestReboot: true,
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(timeout after success) error = %v", err)
	}
	if timeout.Failed || timeout.RebootRequested {
		t.Fatalf("timeout after success = %#v, want no-op", timeout)
	}
}

func TestRecordBootHealthKeepsFallbackAfterTrialReboot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 15, 30, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
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
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-time.Minute),
	})
	writeBootHealthFallback(t, root, "gen0")
	if armed, err := ArmBootRecovery(root, "gen1"); err != nil || !armed {
		t.Fatalf("ArmBootRecovery(gen1) = %t, %v", armed, err)
	}

	trial, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen1", CommandLine: bootHealthCommandLine("gen1"), Result: BootHealthFailure, Now: now, RequestReboot: true})
	if err != nil {
		t.Fatalf("RecordBootHealth(trial failure) error = %v", err)
	}
	if !trial.RebootRequested || trial.RecoveryRequired {
		t.Fatalf("trial failure = %#v, want one reboot to gen0", trial)
	}
	if _, err := os.Stat(filepath.Join(root, "run/katl/boot-health/pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trial recovery authorization was not consumed: %v", err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	selection.BootedGenerationID = "gen0"
	selection.ActiveGenerationID = "gen0"
	selection.BootedBootEntry = "loader/entries/katl-gen0.conf"
	selection.UpdatedAt = now.Add(time.Minute)
	if err := WriteBootSelection(root, selection); err != nil {
		t.Fatal(err)
	}
	if armed, err := ArmBootRecovery(root, "gen0"); err != nil || armed {
		t.Fatalf("ArmBootRecovery(gen0) = %t, %v, want fallback boot disarmed", armed, err)
	}

	fallback, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthFailure, Now: now.Add(2 * time.Minute), RequestReboot: true})
	if err != nil {
		t.Fatalf("RecordBootHealth(fallback failure) error = %v", err)
	}
	if !fallback.KeptKnownGood || fallback.RecoveryRequired || fallback.RebootRequested || fallback.DefaultGeneration != "gen0" {
		t.Fatalf("fallback failure = %#v, want to keep gen0 without reboot", fallback)
	}
}

func TestRecordBootHealthKeepsKnownGood(t *testing.T) {
	for _, tc := range []struct {
		name, running, bootDefault, target, next string
		oneShot, queued                          bool
	}{
		{name: "ordinary", running: "gen2", bootDefault: "gen2", next: "gen2"},
		{name: "superseded fallback", running: "gen1", bootDefault: "gen2", next: "gen2"},
		{name: "future one-shot to running generation", running: "gen2", bootDefault: "gen2", target: "gen2", next: "gen2", oneShot: true, queued: true},
		{name: "future persistent to running generation", running: "gen2", bootDefault: "gen2", target: "gen2", next: "gen2", queued: true},
		{name: "future one-shot to another generation", running: "gen2", bootDefault: "gen2", target: "gen1", next: "gen1", oneShot: true, queued: true},
		{name: "future persistent to another generation", running: "gen2", bootDefault: "gen1", target: "gen1", next: "gen1", queued: true},
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
			writeBootHealthSelection(t, root, selection)
			if tc.queued {
				// Simulate a LoaderEntryOneShot that the loader has yet to consume.
				path := filepath.Join(root, "sys/firmware/efi/efivars/LoaderEntryOneShot-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte{7, 0, 0, 0}, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, before, err := ReadGeneration(root, tc.running)
			if err != nil {
				t.Fatal(err)
			}

			result, err := RecordBootHealth(BootHealthRequest{
				Root: root, CommandLine: bootHealthCommandLine(tc.running), Result: BootHealthFailure, RequestReboot: true, Now: now,
				SetBootDefault: func(_, entry string) error {
					t.Fatalf("unexpected EFI default write to %s", entry)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.KeptKnownGood || result.Failed || result.RebootRequested || result.RecoveryRequired ||
				result.DefaultGeneration != tc.bootDefault || result.NextBootGeneration != tc.next {
				t.Fatalf("boot health = %#v, want kept %s with default %s and next boot %s", result, tc.running, tc.bootDefault, tc.next)
			}
			_, after, err := ReadGeneration(root, tc.running)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("generation status changed from %#v to %#v", before, after)
			}
			got, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if got != selection {
				t.Fatalf("boot selection = %#v, want %#v", got, selection)
			}
		})
	}
}

func TestFailedKnownGoodOneShotRemainsTemporary(t *testing.T) {
	for _, next := range []string{"reboot", "health retry"} {
		t.Run(next, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthSelection(t, root, BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now.Add(-time.Minute),
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodGenerationID: "gen1", PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
				BootedGenerationID: "gen2", BootedBootEntry: "loader/entries/katl-gen2.conf", ActiveGenerationID: "gen2",
				OneShot: true, TargetBootGenerationID: "gen1", TargetBootEntry: "loader/entries/katl-gen1.conf",
			})
			gen1Cmdline := bootHealthCommandLine("gen1")
			if err := RecordBootedGeneration(root, gen1Cmdline); err != nil {
				t.Fatal(err)
			}
			result, err := RecordBootHealth(BootHealthRequest{
				Root: root, CommandLine: gen1Cmdline, Result: BootHealthFailure, RequestReboot: true, Now: now,
			})
			if err != nil || !result.KeptKnownGood || result.RebootRequested || result.NextBootGeneration != "gen2" {
				t.Fatalf("failed one-shot = %#v, %v, want kept gen1 and next boot gen2", result, err)
			}
			selection, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if selection.OneShot || selection.TargetBootGenerationID != "" || selection.TargetBootEntry != "" {
				t.Fatalf("consumed selection remains: %#v", selection)
			}

			if next == "reboot" {
				// A kernel reboot starts with a fresh /run.
				if err := os.Remove(rootedPathUnchecked(root, currentBootPath)); err != nil {
					t.Fatal(err)
				}
				if err := RecordBootedGeneration(root, bootHealthCommandLine("gen2")); err != nil {
					t.Fatalf("activate default after one-shot: %v", err)
				}
				selection, err := ReadBootSelection(root)
				if err != nil {
					t.Fatal(err)
				}
				if selection.BootedGenerationID != "gen2" || selection.ActiveGenerationID != "gen2" {
					t.Fatalf("default boot did not become active: %#v", selection)
				}
				return
			}

			for range 2 {
				result, err := RecordBootHealth(BootHealthRequest{
					Root: root, CommandLine: gen1Cmdline, Result: BootHealthSuccess, Now: now.Add(time.Minute),
					SetBootDefault: func(_, entry string) error {
						t.Fatalf("health retry attempted to set EFI default to %s", entry)
						return nil
					},
				})
				if err != nil || result.Promoted || result.DefaultGeneration != "gen2" {
					t.Fatalf("health retry = %#v, %v, want gen1 to stay temporary", result, err)
				}
				selection, err := ReadBootSelection(root)
				if err != nil {
					t.Fatal(err)
				}
				if selection.DefaultGenerationID != "gen2" || selection.DefaultBootEntry != "loader/entries/katl-gen2.conf" {
					t.Fatalf("health retry changed the boot default: %#v", selection)
				}
			}
		})
	}
}

func TestTrialFailureSelectsFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		intent       bool
		armed        bool
		missingEntry bool
	}{
		{name: "missing fallback entry", intent: true, missingEntry: true},
		{name: "legacy trial without a pending transaction ID"},
		{name: "OnFailure reboots to the recorded default", intent: true, armed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateSuperseded, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
			writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			selection := BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now.Add(-time.Minute),
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodGenerationID: "gen1", PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
				BootedGenerationID: "gen2", BootedBootEntry: "loader/entries/katl-gen2.conf",
				TrialGenerationID: "gen2", TrialBootEntry: "loader/entries/katl-gen2.conf",
				PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
			}
			if tc.intent {
				selection.PendingTransactionID = "next-boot-apply"
			}
			writeBootHealthSelection(t, root, selection)
			if tc.armed {
				writeBootHealthFallback(t, root, "gen1")
				if armed, err := ArmBootRecovery(root, "gen2"); err != nil || !armed {
					t.Fatalf("arm trial = %t, %v", armed, err)
				}
			}
			if tc.missingEntry {
				removeBootFixture(t, filepath.Join(root, "efi/loader/entries/katl-gen1.conf"))
			}

			setter := bootHealthDefaultRecorder(t, "loader/entries/katl-gen1.conf")
			if tc.missingEntry {
				setter = func(_, entry string) error {
					t.Fatalf("unexpected EFI write to %s", entry)
					return nil
				}
			}
			request := BootHealthRequest{
				Root: root, CommandLine: bootHealthCommandLine("gen2"), Result: BootHealthFailure, Now: now,
				SetBootDefault: setter,
			}
			result, err := RecordBootHealth(request)
			if tc.missingEntry {
				if err != nil || !result.Failed || !result.RecoveryRequired || result.RebootRequested ||
					!strings.Contains(result.FallbackProblem, "katl-gen1.conf") {
					t.Fatalf("boot health = %+v, %v, want recovery required with missing entry", result, err)
				}
				after, err := ReadBootSelection(root)
				if err != nil || after.DefaultGenerationID != selection.DefaultGenerationID || after.DefaultBootEntry != selection.DefaultBootEntry {
					t.Fatalf("default changed to %+v, %v", after, err)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			if !result.Failed || result.DefaultGeneration != "gen1" || result.RebootRequested || result.RecoveryRequired {
				t.Fatalf("boot health = %#v, want demotion and selected fallback", result)
			}
			_, status, err := ReadGeneration(root, "gen2")
			if err != nil {
				t.Fatal(err)
			}
			if status.BootState != BootStateFailed || status.HealthState != HealthStateUnhealthy {
				t.Fatalf("generation status = %#v, want failed/unhealthy", status)
			}

			request.RequestReboot = true
			request.SetBootDefault = func(_, entry string) error {
				t.Fatalf("failure replay attempted to set EFI default to %s", entry)
				return nil
			}
			for attempt := range 2 {
				result, err := RecordBootHealth(request)
				if err != nil || result.DefaultGeneration != "gen1" || result.RebootRequested != (tc.armed && attempt == 0) {
					t.Fatalf("failure replay %d = %+v, %v", attempt, result, err)
				}
			}
		})
	}
}

func TestTrialFailureWithUnreadableFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "status read", true: "legacy metadata read"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
			selection := BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind, UpdatedAt: now,
				DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
				TrialGenerationID: "gen1", TrialBootEntry: "loader/entries/katl-gen1.conf",
				PreviousKnownGoodGenerationID: "gen0", PreviousKnownGoodBootEntry: "loader/entries/katl-gen0.conf",
				PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending, PendingTransactionID: "next-boot-apply",
			}
			writeBootHealthSelection(t, root, selection)
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

			_, err := RecordBootHealth(BootHealthRequest{
				Root: root, CommandLine: bootHealthCommandLine("gen1"), Result: BootHealthFailure, RequestReboot: true,
				SetBootDefault: func(_, entry string) error {
					t.Fatalf("unexpected EFI write to %s", entry)
					return nil
				},
			})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "bootability indeterminate") {
				t.Fatalf("failure outcome = %v, want indeterminate with %v", err, wantErr)
			}
			if after, err := ReadBootSelection(root); err != nil || after != selection {
				t.Fatalf("selection changed to %+v, %v", after, err)
			}
			if _, status, err := ReadGeneration(root, "gen1"); err != nil || status.BootState != BootStateTrying {
				t.Fatalf("candidate status changed to %+v, %v", status, err)
			}
		})
	}
}

func TestRecordBootHealthRejectsMissingBootEvidence(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:          APIVersion,
		Kind:                BootSelectionKind,
		DefaultGenerationID: "gen0",
		DefaultBootEntry:    "loader/entries/katl-gen0.conf",
		UpdatedAt:           now.Add(-30 * time.Minute),
	})

	_, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now})
	if err == nil || !strings.Contains(err.Error(), "bootedGenerationID is required") {
		t.Fatalf("RecordBootHealth(missing boot evidence) error = %v, want bootedGenerationID failure", err)
	}
}

func TestRecordBootHealthRejectsMismatchedBootEvidence(t *testing.T) {
	tests := []struct {
		name                   string
		commandLine            string
		selection              func(BootSelectionRecord) BootSelectionRecord
		missingSpecLoaderEntry bool
		want                   string
	}{
		{
			name:        "kernel generation",
			commandLine: bootHealthCommandLine("gen1"),
			want:        "kernel command line generation gen1 does not match selected generation gen0",
		},
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
				APIVersion:          APIVersion,
				Kind:                BootSelectionKind,
				DefaultGenerationID: "gen0",
				BootedGenerationID:  "gen0",
				DefaultBootEntry:    "loader/entries/katl-gen0.conf",
				BootedBootEntry:     "loader/entries/katl-gen0.conf",
				UpdatedAt:           now.Add(-30 * time.Minute),
			}
			if tt.selection != nil {
				selection = tt.selection(selection)
			}
			writeBootHealthSelection(t, root, selection)

			_, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: tt.commandLine, Result: BootHealthSuccess, Now: now})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RecordBootHealth(%s) error = %v, want %q", tt.name, err, tt.want)
			}
		})
	}
}

func TestRecordBootHealthDoesNotMarkHealthyWhenBootDefaultFails(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:             APIVersion,
		Kind:                   BootSelectionKind,
		DefaultGenerationID:    "gen0",
		TargetBootGenerationID: "gen1",
		TrialGenerationID:      "gen1",
		BootedGenerationID:     "gen1",
		DefaultBootEntry:       "loader/entries/katl-gen0.conf",
		TargetBootEntry:        "loader/entries/katl-gen1.conf",
		TrialBootEntry:         "loader/entries/katl-gen1.conf",
		BootedBootEntry:        "loader/entries/katl-gen1.conf",
		UpdatedAt:              now.Add(-30 * time.Minute),
	})

	_, err := RecordBootHealth(BootHealthRequest{
		Root:         root,
		GenerationID: "gen1",
		CommandLine:  bootHealthCommandLine("gen1"),
		Result:       BootHealthSuccess,
		Now:          now,
		SetBootDefault: func(string, string) error {
			return os.ErrPermission
		},
	})
	if err == nil || !strings.Contains(err.Error(), "set boot default") {
		t.Fatalf("RecordBootHealth(success with boot default failure) error = %v, want boot default failure", err)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.PersistentDefaultPromotion == DefaultPromotionDone {
		t.Fatalf("selection after boot default failure = %#v, want unpromoted gen0", selection)
	}
	_, status, err := ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatalf("ReadGeneration(gen1) error = %v", err)
	}
	if status.BootState == BootStateGood || status.HealthState == HealthStateHealthy {
		t.Fatalf("gen1 status after boot default failure = %#v, want not good/healthy", status)
	}
}

func TestRecordBootHealthFailureRequiresKnownGoodRollbackTarget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 17, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-2*time.Hour))
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now.Add(-time.Hour))
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "gen0",
		TrialGenerationID:             "gen1",
		PreviousKnownGoodGenerationID: "gen0",
		BootedGenerationID:            "gen1",
		DefaultBootEntry:              "loader/entries/katl-gen0.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-gen0.conf",
		TrialBootEntry:                "loader/entries/katl-gen1.conf",
		BootedBootEntry:               "loader/entries/katl-gen1.conf",
		PendingTransactionID:          "next-boot-apply",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now.Add(-30 * time.Minute),
	})

	result, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen1", CommandLine: bootHealthCommandLine("gen1"), Result: BootHealthFailure, Now: now})
	if err != nil {
		t.Fatalf("RecordBootHealth(failure) error = %v", err)
	}
	if !result.RecoveryRequired {
		t.Fatalf("failure result = %#v, want recovery required with invalid previous target", result)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if !selection.RecoveryRequired || selection.DefaultGenerationID != "gen0" || selection.PreviousKnownGoodGenerationID != "" {
		t.Fatalf("selection after invalid rollback target = %#v", selection)
	}
}

func writeBootHealthGeneration(t *testing.T, root string, id string, previous string, commitState string, bootState string, healthState string, updatedAt time.Time) {
	t.Helper()
	record := abRecord(t, id, "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", updatedAt)
	spec := SpecFromRecord(record)
	spec.Root.RuntimeArtifactSHA256 = fmt.Sprintf("%x", sha256.Sum256(bootRootImage("boot health runtime")))
	spec.PreviousGenerationID = previous
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

func bootHealthDefaultRecorder(t *testing.T, want string) BootDefaultSetter {
	t.Helper()
	return func(root string, bootEntry string) error {
		if root == "" || bootEntry != want {
			t.Fatalf("SetBootDefault(%q, %q), want non-empty root and %q", root, bootEntry, want)
		}
		return nil
	}
}

func writeBootHealthSelection(t *testing.T, root string, selection BootSelectionRecord) {
	t.Helper()
	if err := WriteBootSelection(root, selection); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
}
