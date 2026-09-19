package operation

import (
	"testing"
	"time"
)

func TestGenerationCompletion(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := "boot trial"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			record := OperationRecord{CompletedPhases: []string{"accepted", "activated"}, PhaseIndex: 1, ActivationState: ActivationStatePending, RecoveryRequired: true, FailureReason: "interrupted", Result: ResultFailedNeedsRepair}
			if live {
				record.CompleteLiveGeneration(now)
			} else {
				record.CompleteBootTrial(now)
			}
			if !record.Terminal || record.Result != ResultSucceeded || record.CompletedAt == nil || !record.CompletedAt.Equal(now) || record.RecoveryRequired || record.FailureReason != "" {
				t.Fatalf("completion = %+v", record)
			}
			if record.GenerationCommitState != GenerationCommitCommitted || record.BootHealthPending == live || record.PhaseIndex != 2 {
				t.Fatalf("generation completion = %+v", record)
			}
			if live && record.ActivationState != ActivationStateActiveLive {
				t.Fatal("live activation not recorded")
			}
			if !live && record.ActivationState != ActivationStatePending {
				t.Fatal("boot staging claimed runtime activation")
			}
		})
	}
}

func TestFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record OperationRecord
		repair bool
	}{
		{name: "refused plan"},
		{name: "external mutation", record: OperationRecord{ExternalMutationStarted: true}, repair: true},
		{name: "live activation", record: OperationRecord{ActivationState: ActivationStateActiveLive}, repair: true},
		{name: "interrupted activation", record: OperationRecord{ActivationState: ActivationStateActivating}, repair: true},
		{name: "existing recovery", record: OperationRecord{RecoveryRequired: true}, repair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := tc.record
			record.Fail(time.Now(), "rejected", "correct configuration and retry")
			if record.RecoveryRequired != tc.repair || (record.Result == ResultFailedNeedsRepair) != tc.repair {
				t.Fatalf("failure=%+v, want recovery=%v", record, tc.repair)
			}
			if !record.Terminal || record.CompletedAt == nil {
				t.Fatal("failure left operation active")
			}
		})
	}
}
