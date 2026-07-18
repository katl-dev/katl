package generation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	marker := filepath.Join(root, "run/katl/boot-health/reboot-requested")

	result, err := RecordBootHealth(BootHealthRequest{
		Root:               root,
		GenerationID:       "gen1",
		CommandLine:        bootHealthCommandLine("gen1"),
		Result:             BootHealthTimeout,
		Reason:             "deadline",
		Now:                now,
		RebootRequestPath:  "/run/katl/boot-health/reboot-requested",
		WriteRebootRequest: true,
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
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read reboot marker: %v", err)
	}
	if !strings.Contains(string(data), "generation=gen1") || !strings.Contains(string(data), "result=timeout") {
		t.Fatalf("reboot marker = %q", data)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen0" || selection.FailedBootGenerationID != "gen1" || selection.RecoveryRequired {
		t.Fatalf("selection after timeout = %#v", selection)
	}
	if selection.BootedGenerationID != "gen0" || selection.BootedBootEntry != "loader/entries/katl-gen0.conf" {
		t.Fatalf("rollback boot evidence = %#v, want gen0 evidence", selection)
	}
	if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthSuccess, Now: now.Add(time.Minute)}); err != nil {
		t.Fatalf("RecordBootHealth(recovered gen0 success) error = %v", err)
	}
}

func TestRecordBootHealthFailureWithoutPreviousRequiresRecovery(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
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

	result, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthFailure, Now: now})
	if err != nil {
		t.Fatalf("RecordBootHealth(failure) error = %v", err)
	}
	if !result.Failed || !result.RecoveryRequired {
		t.Fatalf("failure result = %#v, want recovery required", result)
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
	marker := filepath.Join(root, "run/katl/boot-health/reboot-requested")
	timeout, err := RecordBootHealth(BootHealthRequest{
		Root:               root,
		GenerationID:       "gen0",
		CommandLine:        bootHealthCommandLine("gen0"),
		Result:             BootHealthTimeout,
		Now:                now.Add(2 * time.Minute),
		RebootRequestPath:  "/run/katl/boot-health/reboot-requested",
		WriteRebootRequest: true,
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(timeout after success) error = %v", err)
	}
	if timeout.Failed || timeout.RebootRequested {
		t.Fatalf("timeout after success = %#v, want no-op", timeout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reboot marker exists after no-op timeout: %v", err)
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
	spec.PreviousGenerationID = previous
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	status, err := NewGenerationStatus(spec, commitState, bootState, healthState, updatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus(%s) error = %v", id, err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration(%s) error = %v", id, err)
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
