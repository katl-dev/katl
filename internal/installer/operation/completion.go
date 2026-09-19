package operation

import "time"

// CompleteLiveGeneration records a validated live activation whose generation
// is also the persistent boot default. Operation history is a receipt for that
// transition; subsequent node health remains owned by the generation.
func (r *OperationRecord) CompleteLiveGeneration(now time.Time) {
	r.ActivationState = ActivationStateActiveLive
	r.BootHealthPending = false
	r.completeGeneration(now)
}

// CompleteBootTrial records staging, not boot-health success. ActivationState
// remains operation-specific: bootstrap may already be live while an OS
// upgrade has not activated its payload yet.
func (r *OperationRecord) CompleteBootTrial(now time.Time) {
	r.BootHealthPending = true
	r.completeGeneration(now)
}

func (r *OperationRecord) completeGeneration(now time.Time) {
	now = now.UTC()
	r.GenerationCommitState = GenerationCommitCommitted
	r.PhaseIndex = len(r.CompletedPhases)
	r.Terminal = true
	r.Result = ResultSucceeded
	r.CompletedAt = &now
	r.UpdatedAt = now
	r.RecoveryRequired = false
	r.FailureReason = ""
}

// Fail preserves the distinction between a refused plan and an interrupted
// mutation. Only evidence of external mutation requires node repair.
func (r *OperationRecord) Fail(now time.Time, reason, nextAction string) {
	now = now.UTC()
	r.RecoveryRequired = r.RecoveryRequired || r.ExternalMutationStarted || len(r.PreExecMutationMarkers) > 0 || r.ActivationState == ActivationStateActiveLive || r.ActivationState == ActivationStateActivating
	r.Result = "failed"
	if r.RecoveryRequired {
		r.Result = ResultFailedNeedsRepair
	}
	r.FailureReason = reason
	r.NextAction = nextAction
	r.Terminal = true
	r.CompletedAt = &now
	r.UpdatedAt = now
}
