package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// A config render can leave a partial candidate when the agent stops. Before
// serving new requests, discard only work that never reached live mutation or
// boot selection; the source generation remains the authority for a retry.
func recoverInterruptedConfigApplies(root string, store operation.Store, now time.Time) error {
	lock, err := store.AcquireMutationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	ids, err := store.OperationIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, err := store.Read(id)
		if err != nil {
			return err
		}
		if !interruptedConfigRender(record) {
			continue
		}
		candidate := record.CandidateGenerationID
		selection, err := generation.ReadBootSelection(root)
		if err != nil {
			return err
		}
		if bootSelectionReferences(selection, candidate) {
			continue
		}
		dir, err := generation.GenerationDir(root, candidate)
		if err != nil {
			return err
		}
		statusPath := filepath.Join(dir, "status.json")
		_, statusErr := os.Stat(statusPath)
		if statusErr != nil && !os.IsNotExist(statusErr) {
			return fmt.Errorf("inspect interrupted candidate %s: %w", candidate, statusErr)
		}
		if statusErr == nil {
			_, status, err := generation.ReadGeneration(root, candidate)
			if err != nil || status.CommitState != generation.CommitStateCandidate {
				// A published but unreadable record may contain state this agent
				// cannot safely discard. Keep the lock for explicit recovery.
				continue
			}
			if err := generation.Remove(root, candidate); err != nil {
				return fmt.Errorf("remove interrupted candidate %s: %w", candidate, err)
			}
		} else {
			// No boot pointer references the candidate and no external mutation
			// started. Removing a partial render also restores generation listing.
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove interrupted candidate %s: %w", candidate, err)
			}
		}
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return err
		}
		_, err = store.Update(id, "config-apply-interrupted-render-recovered", "config-apply-recovery", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.RecoveryRequired = false
			current.GenerationCommitState = operation.GenerationCommitAbandoned
			current.Fail(now, "agent stopped before configuration generation was committed", "source generation is unchanged; submit a new configuration apply")
			return current, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func interruptedConfigRender(record operation.OperationRecord) bool {
	return record.ConfigApplyRequest != nil && !record.Terminal && record.Interruption != "" &&
		(record.Phase == "accepted" || record.Phase == "render-generation") &&
		record.CandidateGenerationID != "" && record.CandidateGenerationID != record.ExpectedCurrentGenerationID &&
		!record.ExternalMutationStarted && !record.MutatingToolRan &&
		len(record.PreExecMutationMarkers) == 0 && len(record.MutationScopes) == 0 &&
		record.GenerationCommitState != operation.GenerationCommitCommitted &&
		record.ActivationState != operation.ActivationStateActiveLive && record.ActivationState != operation.ActivationStateActivating
}

func bootSelectionReferences(selection generation.BootSelectionRecord, candidate string) bool {
	return selection.DefaultGenerationID == candidate || selection.ActiveGenerationID == candidate ||
		selection.BootedGenerationID == candidate || selection.TargetBootGenerationID == candidate ||
		selection.TrialGenerationID == candidate || selection.PreviousKnownGoodGenerationID == candidate ||
		selection.Generation0FallbackID == candidate || selection.FailedBootGenerationID == candidate
}

// The operation receipt and generation status must agree that a rolled-back
// candidate is abandoned before another mutation can use the source generation.
func abandonCandidateGeneration(root, candidate, operationID, reason string, now time.Time) error {
	if candidate == "" {
		return nil
	}
	spec, status, err := generation.ReadGeneration(root, candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if status.CommitState == generation.CommitStateAbandoned {
		return nil
	}
	if status.CommitState != generation.CommitStateCandidate {
		return fmt.Errorf("generation %s cannot be abandoned from commit state %s", candidate, status.CommitState)
	}
	status.CommitState = generation.CommitStateAbandoned
	status.BootState = generation.BootStateFailed
	status.UpdatedAt = now
	status.StatusTransitions = append(status.StatusTransitions, generation.StatusTransition{
		At: now, OperationID: operationID, Reason: reason,
		CommitState: status.CommitState, BootState: status.BootState, HealthState: status.HealthState,
	})
	return generation.WriteGenerationStatus(root, spec, status)
}
