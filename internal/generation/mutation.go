package generation

import (
	"cmp"
	"fmt"
)

// ValidateMutationBase checks durable boot state as well as the active base.
// Callers must exclude other mutations until they publish their candidate.
func (m *BootMover) ValidateMutationBase(expected string) error {
	return m.Read(func(selection BootSelectionRecord) error {
		switch {
		case selection.TrialGenerationID != "":
			return fmt.Errorf("a boot trial of generation %s is pending; run 'katlctl node reboot <node>' to try it before applying configuration or upgrading", selection.TrialGenerationID)
		case selection.RecoveryRequired:
			return fmt.Errorf("recovery is required: generation %s failed boot health and no fallback could be used. Repair the cause, then run 'katlctl node reboot <node>'; a healthy boot clears the recovery state. Otherwise boot another generation from the boot menu, or reinstall the node", selection.DefaultGenerationID)
		case selection.PendingHealthValidation:
			return fmt.Errorf("the first boot of generation %s is not yet healthy; see 'katlctl node status <node>', fix the cause, then run 'katlctl node reboot <node>'", cmp.Or(selection.TargetBootGenerationID, selection.DefaultGenerationID))
		}
		active := cmp.Or(selection.ActiveGenerationID, selection.BootedGenerationID, selection.DefaultGenerationID)
		if expected != "" && active != expected {
			return fmt.Errorf("current generation changed from %q to %q; replan the operation", expected, active)
		}
		if selection.TargetBootGenerationID != "" && selection.TargetBootGenerationID != active {
			return fmt.Errorf("generation %q is selected for the next boot; complete or explicitly change that selection first", selection.TargetBootGenerationID)
		}
		items, err := readManagedGenerations(m.root)
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
