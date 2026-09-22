package generation

import (
	"testing"
	"time"
)

func TestMutationBase(t *testing.T) {
	root, now := managementFixture(t)
	if err := ValidateMutationBase(root, "current"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMutationBase(root, "stale"); err == nil {
		t.Fatal("accepted stale base")
	}

	spec := SpecFromRecord(abRecord(t, "candidate", "root-b", "22222222-2222-3333-4444-555555555555", "2", "v1.36.1", now.Add(time.Hour)))
	state, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, state); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMutationBase(root, "current"); err == nil {
		t.Fatal("accepted an unarmed candidate")
	}
	if err := Remove(root, "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMutationBase(root, "current"); err != nil {
		t.Fatalf("explicit removal did not unblock mutation: %v", err)
	}
}

func TestPendingBootBlocksMutation(t *testing.T) {
	for _, scenario := range []string{"health", "trial", "selection", "recovery"} {
		t.Run(scenario, func(t *testing.T) {
			root, _ := managementFixture(t)
			selection, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "health":
				selection.PendingHealthValidation = true
				selection.PersistentDefaultPromotion = DefaultPromotionPending
			case "trial":
				selection.TrialGenerationID = "next"
			case "selection":
				selection.TargetBootGenerationID = "next"
			case "recovery":
				selection.RecoveryRequired = true
			}
			if err := WriteBootSelection(root, selection); err != nil {
				t.Fatal(err)
			}
			if err := ValidateMutationBase(root, "current"); err == nil {
				t.Fatal("accepted mutation during pending boot")
			}
		})
	}
}
