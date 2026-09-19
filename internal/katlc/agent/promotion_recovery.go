package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// Recovery derives eligibility from the durable health receipt written before
// promotion's external boot-default change. It never reruns workload mutation
// or infers success from a stale operation phase.
func (e *Executor) recoverLivePromotions(ctx context.Context, bootID string) error {
	ids, err := e.Store.OperationIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, err := e.Store.Read(id)
		if err != nil {
			return err
		}
		if record.Terminal || record.CandidateGenerationID == "" {
			continue
		}
		if record.ConfigApplyRequest == nil && (record.KubernetesSysextUpdate == nil || record.KubeadmUpgradeEvidence == nil) {
			continue
		}
		spec, status, err := generation.ReadGeneration(e.Root, record.CandidateGenerationID)
		if err != nil {
			continue
		}
		if status.CommitState != generation.CommitStateCommitted || !generation.IsKnownGood(status) || status.CommittedByOperation != id {
			continue
		}
		// Live health from another boot cannot establish which payload is active
		// now. Resume across a reboot only when boot health observed this candidate.
		sameBoot := len(record.Invocations) > 0 && bootID != "" && record.Invocations[len(record.Invocations)-1].BootID == bootID
		if !sameBoot {
			commandLine, err := readCurrentKernelCommandLine(e.Root)
			if err != nil {
				continue
			}
			selected, err := generation.SelectedGenerationFromCommandLine(strings.Join(commandLine, " "))
			if err != nil || selected != spec.GenerationID {
				continue
			}
			selection, err := generation.ReadBootSelection(e.Root)
			if err != nil || selection.BootedGenerationID != spec.GenerationID || selection.PendingHealthValidation {
				continue
			}
		}
		if err := e.recoverLivePromotion(ctx, record, spec); err != nil {
			_, persistErr := e.Store.Update(id, "promotion-recovery-failed", "promotion-recovery-failed", func(record operation.OperationRecord) (operation.OperationRecord, error) {
				record.RecoveryRequired = true
				record.Fail(e.clock(), err.Error(), "inspect generation and boot selection; live promotion recovery could not finish")
				return record, nil
			})
			if persistErr != nil {
				return persistErr
			}
		}
	}
	return nil
}

func (e *Executor) recoverLivePromotion(ctx context.Context, record operation.OperationRecord, spec generation.GenerationSpec) error {
	id := record.OperationID
	var apply generation.ConfigApplyStatus
	var applyPath string
	if record.ConfigApplyRequest != nil {
		var err error
		applyPath, err = generation.ConfigApplyStatusPath(e.Root, spec.GenerationID)
		if err != nil {
			return err
		}
		apply, err = generation.ReadConfigApplyStatus(applyPath)
		if err != nil {
			return err
		}
		if apply.AcceptedApplyMode != generation.ApplyModeLive || apply.Phase != generation.ConfigApplyPhaseActive {
			return fmt.Errorf("promotion %s has no completed live apply evidence", id)
		}
	}

	if err := e.promoteLiveGeneration(ctx, record, e.clock(), "resume validated live promotion"); err != nil {
		return fmt.Errorf("resume live promotion %s: %w", id, err)
	}
	if record.ConfigApplyRequest == nil {
		return e.recordHealthyKubeadmUpgrade(id, e.clock())
	}
	apply.HealthState = generation.HealthStateHealthy
	apply.UpdatedAt = e.clock()
	if err := generation.WriteConfigApplyStatus(applyPath, apply); err != nil {
		return err
	}
	return e.completeConfigApply(id, spec, apply, e.clock())
}
