package generation

import (
	"errors"
	"fmt"
	"os"
)

// Initialize publishes the first generation after the offline installer has
// prepared its payload and node inputs. Selection is published last; retry
// preserves both first-boot health and subsequent generation transitions.
func Initialize(root string, spec GenerationSpec) error {
	if _, err := BootSelectionPath(root); err != nil {
		return err
	}
	if spec.PreviousGenerationID != "" {
		return fmt.Errorf("initial generation must not have a predecessor")
	}
	if spec.Boot.LoaderEntryPath == "" {
		return fmt.Errorf("initial generation loader entry is required")
	}
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStatePending, HealthStateUnknown, spec.CreatedAt)
	if err != nil {
		return err
	}
	return withBootStateLock(root, func() error {
		intent, err := readBootMoveIntent(root)
		if err != nil {
			return err
		}
		if intent != nil {
			return fmt.Errorf("%w (operation %s): the target root already has boot moves", ErrBootMoveBlocked, intent.OperationID)
		}
		selection, err := ReadBootSelection(root)
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if exists && selection.Generation0FallbackID != spec.GenerationID {
			return fmt.Errorf("boot selection already belongs to initial generation %s", selection.Generation0FallbackID)
		}
		if err := WriteGeneration(root, spec, status); err != nil {
			return err
		}
		if exists {
			return nil
		}
		return publishInitialSelection(root, spec)
	})
}
