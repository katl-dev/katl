package generation

import (
	"strings"
	"testing"
	"time"
)

func TestMutationBase(t *testing.T) {
	root, now := managementFixture(t)
	m, _ := moverOn(t, root, "current")
	if err := m.ValidateMutationBase("current"); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateMutationBase("stale"); err == nil {
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
	if err := m.ValidateMutationBase("current"); err == nil {
		t.Fatal("accepted an unarmed candidate")
	}
	if err := m.Remove("candidate"); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateMutationBase("current"); err != nil {
		t.Fatalf("explicit removal did not unblock mutation: %v", err)
	}
}

func TestPendingBootBlocksMutation(t *testing.T) {
	// Each refusal names the action for its own state. Trials are refused by
	// the trial authority; see TestMutationBaseDuringTrial.
	refusals := map[string]string{
		"recovery":  "recovery is required: generation current failed boot health",
		"health":    "the first boot of generation current is not yet healthy; see 'katlctl node status <node>'",
		"selection": `generation "next" is selected for the next boot`,
	}
	for scenario, want := range refusals {
		t.Run(scenario, func(t *testing.T) {
			root, _ := managementFixture(t)
			selection := selectionOf(t, root)
			switch scenario {
			case "health":
				selection.PendingHealthValidation = true
				selection.PersistentDefaultPromotion = DefaultPromotionPending
			case "selection":
				selection.TargetBootGenerationID = "next"
			case "recovery":
				selection.RecoveryRequired = true
			}
			if err := WriteBootSelection(root, selection); err != nil {
				t.Fatal(err)
			}
			m, _ := moverOn(t, root, "current")

			err := m.ValidateMutationBase("current")

			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ValidateMutationBase = %v, want %q", err, want)
			}
			for other, text := range refusals {
				if other != scenario && strings.Contains(err.Error(), text) {
					t.Fatalf("ValidateMutationBase = %v, also carries the %s refusal", err, other)
				}
			}
		})
	}
}
