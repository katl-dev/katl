package generation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitGenerationRecordsWriteReadKnownGood(t *testing.T) {
	root := t.TempDir()
	record := markGood(abRecord(t, "2026.06.10-001", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	spec := SpecFromRecord(record)
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, time.Date(2026, 6, 10, 8, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}

	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}

	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		t.Fatalf("GenerationDir() error = %v", err)
	}
	for _, name := range []string{"spec.json", "status.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}

	readSpec, readStatus, err := ReadGeneration(root, spec.GenerationID)
	if err != nil {
		t.Fatalf("ReadGeneration() error = %v", err)
	}
	if readSpec.GenerationID != spec.GenerationID || readStatus.SpecDigest != status.SpecDigest {
		t.Fatalf("read split = %#v/%#v, want digest %s", readSpec, readStatus, status.SpecDigest)
	}
	if !IsKnownGood(readStatus) {
		t.Fatalf("status is not known-good: %#v", readStatus)
	}

	legacyPath := filepath.Join(dir, "metadata.json")
	readRecord, err := ReadRecord(legacyPath)
	if err != nil {
		t.Fatalf("ReadRecord() split fallback error = %v", err)
	}
	if readRecord.GenerationID != spec.GenerationID || readRecord.HealthState != HealthStateHealthy {
		t.Fatalf("split fallback record = %#v", readRecord)
	}
}

func TestSplitGenerationReadsLegacyMetadata(t *testing.T) {
	dir := t.TempDir()
	record := abRecord(t, "2026.06.10-legacy", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC))
	if err := WriteRecord(filepath.Join(dir, "metadata.json"), record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}

	spec, status, err := ReadSplitRecords(dir)
	if err != nil {
		t.Fatalf("ReadSplitRecords() legacy error = %v", err)
	}
	if spec.Kind != SpecKind || status.Kind != StatusKind || status.SpecDigest == "" {
		t.Fatalf("legacy split = %#v/%#v", spec, status)
	}
	if status.CommitState != CommitStateCommitted || status.BootState != BootStatePending || status.HealthState != HealthStateUnknown {
		t.Fatalf("legacy status = %#v", status)
	}
}

func TestSplitGenerationReadsPartialMigrationStatusFromLegacy(t *testing.T) {
	dir := t.TempDir()
	record := markGood(abRecord(t, "2026.06.10-partial", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	spec := SpecFromRecord(record)
	specData, err := MarshalCanonicalJSON(spec)
	if err != nil {
		t.Fatalf("MarshalCanonicalJSON(spec) error = %v", err)
	}
	mustWrite(t, filepath.Join(dir, "spec.json"), string(specData), 0o644)
	if err := WriteRecord(filepath.Join(dir, "metadata.json"), record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}

	readSpec, status, err := ReadSplitRecords(dir)
	if err != nil {
		t.Fatalf("ReadSplitRecords() partial migration error = %v", err)
	}
	if readSpec.GenerationID != spec.GenerationID || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		t.Fatalf("partial migration = %#v/%#v", readSpec, status)
	}
}

func TestReadRecordPrefersSplitStatusOverStaleLegacy(t *testing.T) {
	root := t.TempDir()
	record := markGood(abRecord(t, "2026.06.10-stale", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	spec := SpecFromRecord(record)
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, time.Date(2026, 6, 10, 8, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		t.Fatalf("GenerationDir() error = %v", err)
	}
	stale := record
	stale.BootState = BootStatePending
	stale.HealthState = HealthStateUnknown
	if err := WriteRecord(filepath.Join(dir, "metadata.json"), stale); err != nil {
		t.Fatalf("WriteRecord(stale) error = %v", err)
	}

	got, err := ReadRecord(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
	if got.BootState != BootStateGood || got.HealthState != HealthStateHealthy {
		t.Fatalf("ReadRecord() used stale legacy status: %#v", got)
	}
}

func TestSplitGenerationRejectsSpecMutation(t *testing.T) {
	root := t.TempDir()
	spec := SpecFromRecord(abRecord(t, "2026.06.10-immutable", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	status, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Date(2026, 6, 10, 8, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
	err = WriteGeneration(root, spec, status)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("WriteGeneration(existing) error = %v, want already exists", err)
	}

	mutated := spec
	mutated.Root.Slot = "root-b"
	mutated.Root.PartitionUUID = "66666666-7777-8888-9999-000000000000"
	mutatedStatus, err := NewGenerationStatus(mutated, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Date(2026, 6, 10, 8, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus(mutated) error = %v", err)
	}
	err = WriteGenerationStatus(root, mutated, mutatedStatus)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("WriteGenerationStatus(mutated) error = %v, want immutable", err)
	}
}

func TestWriteGenerationStatusValidatesTransitionsWithoutRewritingSpec(t *testing.T) {
	root := t.TempDir()
	spec := SpecFromRecord(abRecord(t, "2026.06.10-status", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	status, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Date(2026, 6, 10, 8, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		t.Fatalf("GenerationDir() error = %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil {
		t.Fatalf("read spec before: %v", err)
	}

	committed := status
	committed.CommitState = CommitStateCommitted
	committed.UpdatedAt = time.Date(2026, 6, 10, 8, 2, 0, 0, time.UTC)
	if err := WriteGenerationStatus(root, spec, committed); err != nil {
		t.Fatalf("WriteGenerationStatus(committed) error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil {
		t.Fatalf("read spec after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("spec changed during status write:\n%s\nwant:\n%s", after, before)
	}

	invalid := committed
	invalid.CommitState = CommitStateCandidate
	invalid.UpdatedAt = time.Date(2026, 6, 10, 8, 3, 0, 0, time.UTC)
	err = WriteGenerationStatus(root, spec, invalid)
	if err == nil || !strings.Contains(err.Error(), "invalid commitState transition") {
		t.Fatalf("WriteGenerationStatus(invalid) error = %v, want transition failure", err)
	}

	trying := committed
	trying.BootState = BootStateTrying
	trying.UpdatedAt = time.Date(2026, 6, 10, 8, 4, 0, 0, time.UTC)
	if err := WriteGenerationStatus(root, spec, trying); err != nil {
		t.Fatalf("WriteGenerationStatus(trying) error = %v", err)
	}
	good := trying
	good.BootState = BootStateGood
	good.HealthState = HealthStateHealthy
	good.UpdatedAt = time.Date(2026, 6, 10, 8, 5, 0, 0, time.UTC)
	if err := WriteGenerationStatus(root, spec, good); err != nil {
		t.Fatalf("WriteGenerationStatus(good) error = %v", err)
	}
	backToTrying := good
	backToTrying.BootState = BootStateTrying
	backToTrying.HealthState = HealthStateUnknown
	backToTrying.UpdatedAt = time.Date(2026, 6, 10, 8, 6, 0, 0, time.UTC)
	err = WriteGenerationStatus(root, spec, backToTrying)
	if err == nil || !strings.Contains(err.Error(), "invalid bootState transition") {
		t.Fatalf("WriteGenerationStatus(invalid boot) error = %v, want transition failure", err)
	}
}

func TestGenerationStatusTransitions(t *testing.T) {
	spec := SpecFromRecord(abRecord(t, "2026.06.10-transitions", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	base, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Date(2026, 6, 10, 8, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(GenerationStatus) GenerationStatus
	}{
		{name: "candidate committed", mutate: func(status GenerationStatus) GenerationStatus {
			status.CommitState = CommitStateCommitted
			return status
		}},
		{name: "candidate abandoned", mutate: func(status GenerationStatus) GenerationStatus {
			status.CommitState = CommitStateAbandoned
			return status
		}},
		{name: "pending trying", mutate: func(status GenerationStatus) GenerationStatus {
			status.BootState = BootStateTrying
			return status
		}},
		{name: "pending good", mutate: func(status GenerationStatus) GenerationStatus {
			status.BootState = BootStateGood
			status.HealthState = HealthStateHealthy
			return status
		}},
		{name: "pending failed", mutate: func(status GenerationStatus) GenerationStatus {
			status.BootState = BootStateFailed
			status.HealthState = HealthStateUnhealthy
			return status
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateStatusTransition(base, tt.mutate(base)); err != nil {
				t.Fatalf("ValidateStatusTransition() error = %v", err)
			}
		})
	}

	committed := base
	committed.CommitState = CommitStateCommitted
	superseded := committed
	superseded.CommitState = CommitStateSuperseded
	if err := ValidateStatusTransition(committed, superseded); err != nil {
		t.Fatalf("committed -> superseded error = %v", err)
	}
	trying := base
	trying.BootState = BootStateTrying
	good := trying
	good.BootState = BootStateGood
	good.HealthState = HealthStateHealthy
	if err := ValidateStatusTransition(trying, good); err != nil {
		t.Fatalf("trying -> good error = %v", err)
	}

	invalid := committed
	invalid.CommitState = CommitStateCandidate
	if err := ValidateStatusTransition(committed, invalid); err == nil {
		t.Fatal("committed -> candidate error = nil, want invalid transition")
	}
	invalidBoot := good
	invalidBoot.BootState = BootStateTrying
	if err := ValidateStatusTransition(good, invalidBoot); err == nil {
		t.Fatal("good -> trying error = nil, want invalid transition")
	}
}

func TestGenerationStatusRejectsMissingOrMismatchedSpecDigest(t *testing.T) {
	root := t.TempDir()
	spec := SpecFromRecord(abRecord(t, "2026.06.10-digest", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)))
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, time.Date(2026, 6, 10, 8, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}

	missing := status
	missing.SpecDigest = ""
	if err := ValidateGenerationStatus(spec, missing); err == nil || !strings.Contains(err.Error(), "specDigest mismatch") {
		t.Fatalf("missing digest error = %v, want mismatch", err)
	}
	mismatched := status
	mismatched.SpecDigest = "sha256:" + strings.Repeat("0", 64)
	if err := ValidateGenerationStatus(spec, mismatched); err == nil || !strings.Contains(err.Error(), "specDigest mismatch") {
		t.Fatalf("mismatched digest error = %v, want mismatch", err)
	}

	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		t.Fatalf("GenerationDir() error = %v", err)
	}
	specData, err := MarshalCanonicalJSON(spec)
	if err != nil {
		t.Fatalf("MarshalCanonicalJSON(spec) error = %v", err)
	}
	statusData, err := MarshalCanonicalJSON(mismatched)
	if err != nil {
		t.Fatalf("MarshalCanonicalJSON(status) error = %v", err)
	}
	mustWrite(t, filepath.Join(dir, "spec.json"), string(specData), 0o644)
	mustWrite(t, filepath.Join(dir, "status.json"), string(statusData), 0o644)
	if _, _, err := ReadGeneration(root, spec.GenerationID); err == nil || !strings.Contains(err.Error(), "specDigest mismatch") {
		t.Fatalf("ReadGeneration() error = %v, want digest mismatch", err)
	}
}

func TestBootSelectionReadWriteStates(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		selection BootSelectionRecord
	}{
		{
			name: "known good default",
			selection: BootSelectionRecord{
				APIVersion:                    APIVersion,
				Kind:                          BootSelectionKind,
				DefaultGenerationID:           "2026.06.10-001",
				PreviousKnownGoodGenerationID: "2026.06.10-001",
				BootedGenerationID:            "2026.06.10-001",
				DefaultBootEntry:              "loader/entries/katl-2026.06.10-001.conf",
				PersistentDefaultPromotion:    DefaultPromotionDone,
				UpdatedAt:                     now,
			},
		},
		{
			name: "candidate pending health",
			selection: BootSelectionRecord{
				APIVersion:                    APIVersion,
				Kind:                          BootSelectionKind,
				DefaultGenerationID:           "2026.06.10-001",
				TargetBootGenerationID:        "2026.06.10-002",
				TrialGenerationID:             "2026.06.10-002",
				PreviousKnownGoodGenerationID: "2026.06.10-001",
				BootedGenerationID:            "2026.06.10-002",
				TargetBootEntry:               "loader/entries/katl-2026.06.10-002.conf",
				TrialBootEntry:                "loader/entries/katl-2026.06.10-002.conf",
				PendingHealthValidation:       true,
				PersistentDefaultPromotion:    DefaultPromotionPending,
				PendingTransactionID:          "tx-1",
				UpdatedAt:                     now,
			},
		},
		{
			name: "failed boot with generation zero fallback",
			selection: BootSelectionRecord{
				APIVersion:                    APIVersion,
				Kind:                          BootSelectionKind,
				DefaultGenerationID:           "0",
				PreviousKnownGoodGenerationID: "0",
				Generation0FallbackID:         "0",
				FailedBootGenerationID:        "2026.06.10-002",
				RecoveryRequired:              true,
				UpdatedAt:                     now,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := WriteBootSelection(root, tt.selection); err != nil {
				t.Fatalf("WriteBootSelection() error = %v", err)
			}
			got, err := ReadBootSelection(root)
			if err != nil {
				t.Fatalf("ReadBootSelection() error = %v", err)
			}
			if got.DefaultGenerationID != tt.selection.DefaultGenerationID || got.PendingHealthValidation != tt.selection.PendingHealthValidation || got.FailedBootGenerationID != tt.selection.FailedBootGenerationID {
				t.Fatalf("selection = %#v, want %#v", got, tt.selection)
			}
		})
	}
}

func TestBootSelectionRejectsCorruptSelection(t *testing.T) {
	root := t.TempDir()
	path, err := BootSelectionPath(root)
	if err != nil {
		t.Fatalf("BootSelectionPath() error = %v", err)
	}
	mustWrite(t, path, "{not-json", 0o644)

	_, err = ReadBootSelection(root)
	if err == nil || !strings.Contains(err.Error(), "decode boot selection") {
		t.Fatalf("ReadBootSelection() error = %v, want corrupt refusal", err)
	}
}

func TestBootSelectionRejectsInconsistentSelection(t *testing.T) {
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		selection BootSelectionRecord
		wantErr   string
	}{
		{
			name: "pending health without pending promotion",
			selection: BootSelectionRecord{
				APIVersion:              APIVersion,
				Kind:                    BootSelectionKind,
				DefaultGenerationID:     "2026.06.10-001",
				TargetBootGenerationID:  "2026.06.10-002",
				PendingHealthValidation: true,
				UpdatedAt:               now,
			},
			wantErr: "pending persistent default promotion",
		},
		{
			name: "absolute boot entry",
			selection: BootSelectionRecord{
				APIVersion:          APIVersion,
				Kind:                BootSelectionKind,
				DefaultGenerationID: "2026.06.10-001",
				DefaultBootEntry:    "/loader/entries/katl.conf",
				UpdatedAt:           now,
			},
			wantErr: "$BOOT-relative",
		},
		{
			name: "failed boot without recovery path",
			selection: BootSelectionRecord{
				APIVersion:             APIVersion,
				Kind:                   BootSelectionKind,
				DefaultGenerationID:    "2026.06.10-001",
				FailedBootGenerationID: "2026.06.10-002",
				UpdatedAt:              now,
			},
			wantErr: "previous known-good",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBootSelection(tt.selection)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateBootSelection() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
