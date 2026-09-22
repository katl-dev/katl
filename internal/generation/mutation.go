package generation

import "fmt"

// ValidateMutationBase checks durable boot state as well as the active base.
// Callers must exclude other mutations until they publish their candidate.
func ValidateMutationBase(root, expected string) error {
	return withStateLock(rootedPathUnchecked(root, "/var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		if selection.RecoveryRequired || selection.PendingHealthValidation || selection.TrialGenerationID != "" {
			return fmt.Errorf("a boot trial or recovery is pending; complete it before applying configuration or upgrading")
		}
		active := selection.ActiveGenerationID
		if active == "" {
			active = selection.BootedGenerationID
		}
		if active == "" {
			active = selection.DefaultGenerationID
		}
		if expected != "" && active != expected {
			return fmt.Errorf("current generation changed from %q to %q; replan the operation", expected, active)
		}
		if selection.TargetBootGenerationID != "" && selection.TargetBootGenerationID != active {
			return fmt.Errorf("generation %q is selected for the next boot; complete or explicitly change that selection first", selection.TargetBootGenerationID)
		}
		items, err := inspect(root, selection)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Status.CommitState == CommitStateCandidate {
				return fmt.Errorf("candidate generation %q is pending; complete it or explicitly remove it before applying configuration or upgrading", item.Spec.GenerationID)
			}
		}
		return nil
	})
}
