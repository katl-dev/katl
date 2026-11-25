package generation

import (
	"strings"
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

func TestRecoveryRefusalGuidance(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		wantReboot   bool
	}{
		{name: "management network", reason: ManagementNetworkFailure + ": networkctl list: exit status 1", wantReboot: true},
		{name: "required unit", reason: "katl-boot-health.service-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
			writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStatePending, HealthStateUnknown, now.Add(-time.Hour))
			writeBootHealthSelection(t, root, BootSelectionRecord{
				APIVersion: APIVersion, Kind: BootSelectionKind,
				DefaultGenerationID: "gen0", BootedGenerationID: "gen0",
				DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
				UpdatedAt: now.Add(-30 * time.Minute),
			})
			if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "gen0", CommandLine: bootHealthCommandLine("gen0"), Result: BootHealthFailure, Reason: tc.reason, Now: now}); err != nil {
				t.Fatal(err)
			}

			err := ValidateMutationBase(root, "gen0")
			if err == nil {
				t.Fatal("accepted mutation while recovery is required")
			}
			if got := strings.Contains(err.Error(), "katlctl node reboot <node>"); got != tc.wantReboot {
				t.Fatalf("refusal = %q, names reboot = %t, want %t", err, got, tc.wantReboot)
			}
		})
	}
}
