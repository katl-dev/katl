package generation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArmBootRecoveryOnlyForValidatedTrial(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "known-good", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "candidate", "known-good", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "known-good",
		TargetBootGenerationID:        "candidate",
		TrialGenerationID:             "candidate",
		PreviousKnownGoodGenerationID: "known-good",
		DefaultBootEntry:              "loader/entries/katl-known-good.conf",
		TrialBootEntry:                "loader/entries/katl-candidate.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-known-good.conf",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now,
	})

	armed, err := ArmBootRecovery(root, "candidate")
	if err != nil || !armed {
		t.Fatalf("ArmBootRecovery(candidate) = %t, %v", armed, err)
	}
	marker := filepath.Join(root, "run/katl/boot-health/pending")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("trial recovery authorization: %v", err)
	}

	armed, err = ArmBootRecovery(root, "known-good")
	if err != nil || armed {
		t.Fatalf("ArmBootRecovery(known-good) = %t, %v", armed, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback retained trial recovery authorization: %v", err)
	}
}

func TestArmBootRecoveryRejectsUnhealthyFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "fallback", "", CommitStateCommitted, BootStateFailed, HealthStateUnhealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "candidate", "fallback", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "fallback",
		TargetBootGenerationID:        "candidate",
		TrialGenerationID:             "candidate",
		PreviousKnownGoodGenerationID: "fallback",
		DefaultBootEntry:              "loader/entries/katl-fallback.conf",
		TrialBootEntry:                "loader/entries/katl-candidate.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-fallback.conf",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now,
	})

	armed, err := ArmBootRecovery(root, "candidate")
	if err != nil || armed {
		t.Fatalf("ArmBootRecovery(candidate) = %t, %v, want disarmed", armed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "run/katl/boot-health/pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid fallback created recovery authorization: %v", err)
	}

	spec, status, err := ReadGeneration(root, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	status.BootState = BootStateGood
	status.HealthState = HealthStateHealthy
	if err := WriteGenerationStatus(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rootedPathUnchecked(root, spec.Boot.UKIPath)); err != nil {
		t.Fatal(err)
	}
	armed, err = ArmBootRecovery(root, "candidate")
	if err != nil || armed {
		t.Fatalf("ArmBootRecovery(candidate without fallback UKI) = %t, %v, want disarmed", armed, err)
	}
}

func TestHasBootFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		defaultState string
		selection    BootSelectionRecord
		want         bool
	}{
		{
			name:      "first install",
			selection: BootSelectionRecord{DefaultGenerationID: "gen1", TargetBootGenerationID: "gen1", PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending},
		},
		{
			name:         "known-good default",
			defaultState: BootStateGood,
			selection:    BootSelectionRecord{DefaultGenerationID: "gen0", BootedGenerationID: "gen1"},
			want:         true,
		},
		{
			name:         "failed default",
			defaultState: BootStateFailed,
			selection:    BootSelectionRecord{DefaultGenerationID: "gen0", BootedGenerationID: "gen1"},
		},
		{
			name:         "known-good previous generation",
			defaultState: BootStateGood,
			selection:    BootSelectionRecord{DefaultGenerationID: "gen1", PreviousKnownGoodGenerationID: "gen0"},
			want:         true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
			if tc.defaultState != "" {
				health := HealthStateHealthy
				if tc.defaultState == BootStateFailed {
					health = HealthStateUnhealthy
				}
				writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, tc.defaultState, health, now.Add(-2*time.Hour))
			}
			writeBootHealthGeneration(t, root, "gen1", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
			selection := tc.selection
			selection.APIVersion, selection.Kind, selection.UpdatedAt = APIVersion, BootSelectionKind, now
			writeBootHealthSelection(t, root, selection)

			got, err := HasBootFallback(root, "gen1")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("HasBootFallback(gen1) = %t, want %t", got, tc.want)
			}
		})
	}
}
