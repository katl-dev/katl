package generation

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/efiboot"
)

// BootTargets identifies the running, next-boot and live-selected generations
// for status reporting. Next is empty when a boot move records no destination.
type BootTargets struct {
	Current          string
	Next             string
	Live             string
	RecoveryRequired bool // Reported by the pending intent.
}

// ReadBootTargets reads the current boot marker and the pending move's desired
// target when there is an intent. Legacy callers may supply current-generation
// evidence from outside the generation store.
func ReadBootTargets(root string, legacyCurrent func(BootSelectionRecord) string) (BootTargets, error) {
	root = cleanRoot(root)
	var targets BootTargets
	err := withBootStateLock(root, func() error {
		intent, err := readBootMoveIntent(root)
		if err != nil {
			return err
		}
		if intent != nil {
			current, _, err := CurrentBootGeneration(root)
			targets = BootTargets{
				Current:          current,
				Next:             intent.nextGenerationID(),
				Live:             current,
				RecoveryRequired: intent.DesiredSelection.RecoveryRequired,
			}
			return err
		}
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		var current string
		if legacyCurrent != nil {
			current = legacyCurrent(selection)
		}
		current = cmp.Or(current, strings.TrimSpace(selection.ActiveGenerationID), strings.TrimSpace(selection.BootedGenerationID), strings.TrimSpace(selection.DefaultGenerationID))
		targets = BootTargets{
			Current: current,
			Next:    cmp.Or(selection.NextBootGenerationID(), current),
			Live:    cmp.Or(strings.TrimSpace(selection.TargetBootGenerationID), current),
		}
		return nil
	})
	return targets, err
}

type bootState struct {
	intent *bootMoveIntent
	legacy *legacyBootState
}

type bootGenerationState struct {
	spec   GenerationSpec
	status GenerationStatus
	err    error
}

type legacyBootState struct {
	selection         BootSelectionRecord
	generations       map[string]bootGenerationState
	trialGenerationID string
	// An EFI read error is indeterminate. Legacy activation uses the kernel's
	// identity and keeps working even when it cannot observe EFI.
	efi    efiboot.Entries
	efiErr error
}

// withBootState reads the intent first and keeps the lock through the caller's
// use of the snapshot. A reader that finds an intent must not infer a move
// from a possibly partially projected selection or generation status.
func withBootState(root, runningID string, readEFI func(string) (efiboot.Entries, error), action func(bootState) error) error {
	return withBootStateLock(root, func() error {
		intent, err := readBootMoveIntent(root)
		if err != nil {
			return err
		}
		if intent != nil {
			return action(bootState{intent: intent})
		}
		legacy, err := readLegacyBootState(root, runningID, readEFI)
		if err != nil {
			return err
		}
		return action(bootState{legacy: legacy})
	})
}

// readLegacyBootState imports the selected generations and raw EFI variables.
// The caller holds the boot-state lock across the entire observation.
func readLegacyBootState(root, runningID string, readEFI func(string) (efiboot.Entries, error)) (*legacyBootState, error) {
	selection, err := ReadBootSelection(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	state := &legacyBootState{
		selection:   selection,
		generations: make(map[string]bootGenerationState),
	}
	for _, id := range []string{
		runningID, selection.DefaultGenerationID, selection.TargetBootGenerationID, selection.TrialGenerationID,
		selection.PreviousKnownGoodGenerationID, selection.BootedGenerationID, selection.ActiveGenerationID,
		selection.Generation0FallbackID, selection.FailedBootGenerationID,
	} {
		if _, exists := state.generations[id]; id == "" || exists {
			continue
		}
		spec, status, err := ReadGeneration(root, id)
		if err != nil {
			err = fmt.Errorf("read boot state for generation %s: %w", id, err)
		}
		state.generations[id] = bootGenerationState{spec: spec, status: status, err: err}
	}
	// Generation 0 and manual selections can be pending without a change-triggered trial.
	trialID := cmp.Or(selection.TrialGenerationID, selection.TargetBootGenerationID)
	trial := state.generations[trialID]
	if selection.PendingHealthValidation && selection.PendingTransactionID != "" && trial.spec.PreviousGenerationID != "" {
		state.trialGenerationID = trialID
	}
	state.efi, state.efiErr = readEFI(root)
	return state, nil
}
