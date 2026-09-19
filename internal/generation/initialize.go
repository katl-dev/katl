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
	bootDir, err := rootedPath(root, "/var/lib/katl/boot")
	if err != nil {
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
	return withStateLock(bootDir, func() error {
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
		return WriteBootSelection(root, BootSelectionRecord{
			APIVersion: APIVersion, Kind: BootSelectionKind,
			DefaultGenerationID: spec.GenerationID, DefaultBootEntry: spec.Boot.LoaderEntryPath,
			TargetBootGenerationID: spec.GenerationID, TargetBootEntry: spec.Boot.LoaderEntryPath,
			Generation0FallbackID:   spec.GenerationID,
			PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
			UpdatedAt: spec.CreatedAt,
		})
	})
}
