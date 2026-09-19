package generation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerationPublication(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "repeat"
		if interrupted {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			record := abRecord(t, "publish", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
			spec := SpecFromRecord(record)
			status, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, record.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteGeneration(root, spec, status); err != nil {
				t.Fatal(err)
			}
			dir, err := GenerationDir(root, spec.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if interrupted {
				if err := os.Remove(filepath.Join(dir, "status.json")); err != nil {
					t.Fatal(err)
				}
				if _, _, err := ReadGeneration(root, spec.GenerationID); err == nil {
					t.Fatal("incomplete generation is readable")
				}
			}

			if err := WriteGeneration(root, spec, status); err != nil {
				t.Fatalf("resume publication: %v", err)
			}
			_, observed, err := ReadGeneration(root, spec.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if observed.CommitState != CommitStateCandidate {
				t.Fatalf("state = %s", observed.CommitState)
			}

			committed := status
			committed.CommitState = CommitStateCommitted
			committed.UpdatedAt = committed.UpdatedAt.Add(time.Second)
			if err := WriteGenerationStatus(root, spec, committed); err != nil {
				t.Fatal(err)
			}
			if err := WriteGeneration(root, spec, status); err != nil {
				t.Fatalf("repeat after progress: %v", err)
			}
			_, observed, err = ReadGeneration(root, spec.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if observed.CommitState != CommitStateCommitted {
				t.Fatal("publication reset existing progress")
			}

			changed := spec
			changed.RuntimeVersion = "different"
			changedStatus, err := NewGenerationStatus(changed, CommitStateCandidate, BootStatePending, HealthStateUnknown, record.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteGeneration(root, changed, changedStatus); err == nil {
				t.Fatal("conflicting generation replaced immutable spec")
			}
		})
	}
}

func TestResumeLivePromotion(t *testing.T) {
	for _, boundary := range []string{"health receipt", "previous superseded", "selection published"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "old", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
			writeBootHealthGeneration(t, root, "new", "old", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
			selection := BootSelectionRecord{APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "old", DefaultBootEntry: "loader/entries/katl-old.conf", BootedGenerationID: "old", UpdatedAt: now}
			writeBootHealthSelection(t, root, selection)
			spec, state, err := ReadGeneration(root, "new")
			if err != nil {
				t.Fatal(err)
			}
			state.CommitState, state.BootState, state.HealthState = CommitStateCommitted, BootStateGood, HealthStateHealthy
			state.CommittedByOperation = "validated-live-apply"
			if err := WriteGenerationStatus(root, spec, state); err != nil {
				t.Fatal(err)
			}
			if boundary != "health receipt" {
				oldSpec, oldState, err := ReadGeneration(root, "old")
				if err != nil {
					t.Fatal(err)
				}
				oldState.CommitState = CommitStateSuperseded
				if err := WriteGenerationStatus(root, oldSpec, oldState); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "selection published" {
				selection.DefaultGenerationID, selection.ActiveGenerationID = "new", "new"
				selection.DefaultBootEntry = "loader/entries/katl-new.conf"
				selection.PreviousKnownGoodGenerationID = "old"
				selection.PreviousKnownGoodBootEntry = "loader/entries/katl-old.conf"
				writeBootHealthSelection(t, root, selection)
			}

			externalDefault := "loader/entries/katl-old.conf"
			request := LivePromotionRequest{
				Root: root, GenerationID: "new", OperationID: "validated-live-apply", Now: now,
				SetBootDefault: func(_ string, entry string) error { externalDefault = entry; return nil },
			}
			if err := PromoteLiveGeneration(request); err != nil {
				t.Fatal(err)
			}
			if err := PromoteLiveGeneration(request); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			if externalDefault != "loader/entries/katl-new.conf" {
				t.Fatalf("external default = %s", externalDefault)
			}
			observed, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if observed.DefaultGenerationID != "new" || observed.PreviousKnownGoodGenerationID != "old" || observed.BootedGenerationID != "old" {
				t.Fatalf("selection = %+v", observed)
			}
			request.OperationID = "unrelated-operation"
			if err := PromoteLiveGeneration(request); err == nil {
				t.Fatal("unrelated operation reused health receipt")
			}
		})
	}
}
