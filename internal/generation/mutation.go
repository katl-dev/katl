package generation

import (
	"cmp"
	"fmt"
)

// RecoveryGuidance tells the operator how to leave required recovery.
const RecoveryGuidance = "Repair the cause, then run 'katlctl node reboot <node>'; a healthy boot clears the recovery state. " +
	"Otherwise boot a known-good generation from the boot menu; its healthy boot makes it the default and clears the recovery state. " +
	"If none boots, reinstall the node"

// ValidateMutationBase checks durable boot state as well as the active base.
// Callers must exclude other mutations until they publish their candidate.
func (m *BootMover) ValidateMutationBase(expected string) error {
	return m.readWithAuthority(func(selection BootSelectionRecord, authority *bootMoveIntent) error {
		if authority != nil {
			return m.pendingTrialError(authority, "the configuration apply or upgrade")
		}
		if err := unsettledBoot(selection, "applying configuration or upgrading"); err != nil {
			return err
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

// unsettledBoot refuses action while boot health has not settled outside a
// trial: required recovery, or a first boot not yet healthy. A pending trial
// is the trial authority's to report, so callers check it first. Each refusal
// names the command that settles it.
func unsettledBoot(selection BootSelectionRecord, action string) error {
	switch {
	case selection.RecoveryRequired:
		return fmt.Errorf("recovery is required: generation %s failed boot health and no fallback could be used. %s", selection.DefaultGenerationID, RecoveryGuidance)
	case selection.PendingHealthValidation:
		return fmt.Errorf("the first boot of generation %s is not yet healthy; see 'katlctl node status <node>', fix the cause, then run 'katlctl node reboot <node>' before %s", cmp.Or(selection.TargetBootGenerationID, selection.DefaultGenerationID), action)
	}
	return nil
}
