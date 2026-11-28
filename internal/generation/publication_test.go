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

func TestPromoteLiveRepeat(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "old", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "new", "old", CommitStateCandidate, BootStatePending, HealthStateUnknown, now.Add(-time.Minute))
	writeBootHealthFallback(t, root, "new")
	writeBootHealthSelection(t, root, BootSelectionRecord{APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "old", DefaultBootEntry: "loader/entries/katl-old.conf", BootedGenerationID: "old", UpdatedAt: now})
	m, efi := moverOn(t, root, "old")
	request := LivePromotionRequest{GenerationID: "new", OperationID: "validated-live-apply", Now: now}

	for range 2 {
		if err := m.PromoteLive(request); err != nil {
			t.Fatal(err)
		}
	}

	if efi.entries.Default != "katl-new.conf" {
		t.Fatalf("firmware default = %q, want katl-new.conf", efi.entries.Default)
	}
	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "new" || selection.PreviousKnownGoodGenerationID != "old" || selection.BootedGenerationID != "old" || selection.Revision != 1 {
		t.Fatalf("selection = %+v, want one promotion of new over old", selection)
	}
	transitions := 0
	for _, transition := range statusOf(t, root, "new").StatusTransitions {
		if transition.OperationID == request.OperationID {
			transitions++
		}
	}
	if transitions != 1 {
		t.Fatalf("operation %s recorded %d transitions, want 1", request.OperationID, transitions)
	}
}
