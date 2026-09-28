package generation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const bootRecoveryPath = "/run/katl/boot-health/pending"

type bootRecoveryArm struct {
	TrialGeneration    string `json:"trialGeneration"`
	FallbackGeneration string `json:"fallbackGeneration"`
}

// ArmBootRecovery creates the ephemeral authorization for one automatic
// recovery reboot. Normal and fallback boots clear the authorization instead.
func ArmBootRecovery(root string, generationID string) (bool, error) {
	root = cleanRoot(root)
	generationID = strings.TrimSpace(generationID)
	if generationID == "" {
		return false, fmt.Errorf("generationID is required to arm boot recovery")
	}

	var armed bool
	err := withStateLock(filepath.Join(root, "var/lib/katl/boot"), func() error {
		if err := ClearBootRecovery(root); err != nil {
			return err
		}
		selection, err := ReadBootSelection(root)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		trialID := strings.TrimSpace(selection.TrialGenerationID)
		targetID := strings.TrimSpace(selection.TargetBootGenerationID)
		if !selection.PendingHealthValidation || (generationID != trialID && generationID != targetID) {
			return nil
		}
		fallbackID := strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
		if fallbackID == "" || fallbackID == generationID || !validBootRecoveryTarget(root, selection, fallbackID) {
			return nil
		}
		data, err := json.Marshal(bootRecoveryArm{
			TrialGeneration:    generationID,
			FallbackGeneration: fallbackID,
		})
		if err != nil {
			return fmt.Errorf("marshal boot recovery authorization: %w", err)
		}
		path := rootedPathUnchecked(root, bootRecoveryPath)
		if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
			return fmt.Errorf("arm boot recovery: %w", err)
		}
		armed = true
		return nil
	})
	return armed, err
}

func validBootRecoveryTarget(root string, selection BootSelectionRecord, generationID string) bool {
	spec, status, err := ReadGeneration(root, generationID)
	if err != nil || !IsKnownGood(status) {
		return false
	}
	entry := strings.TrimSpace(selection.PreviousKnownGoodBootEntry)
	if entry == "" && strings.TrimSpace(selection.DefaultGenerationID) == generationID {
		entry = strings.TrimSpace(selection.DefaultBootEntry)
	}
	if entry == "" || entry != strings.TrimSpace(spec.Boot.LoaderEntryPath) {
		return false
	}
	for _, path := range []string{
		filepath.Join("/efi", entry),
		filepath.Join("/efi", entryPath(spec.Boot.UKIPath)),
	} {
		if _, err := os.Stat(rootedPathUnchecked(root, path)); err != nil {
			return false
		}
	}
	return true
}

// ClearBootRecovery removes the current boot's ephemeral reboot authorization.
func ClearBootRecovery(root string) error {
	path := rootedPathUnchecked(root, bootRecoveryPath)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear boot recovery authorization: %w", err)
	}
	return nil
}

func bootRecoveryArmed(root string, generationID string) (bool, error) {
	root = cleanRoot(root)
	data, err := os.ReadFile(rootedPathUnchecked(root, bootRecoveryPath))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read boot recovery authorization: %w", err)
	}
	var arm bootRecoveryArm
	if err := json.Unmarshal(data, &arm); err != nil {
		return false, fmt.Errorf("decode boot recovery authorization: %w", err)
	}
	trialID := strings.TrimSpace(arm.TrialGeneration)
	fallbackID := strings.TrimSpace(arm.FallbackGeneration)
	if trialID != strings.TrimSpace(generationID) || fallbackID == "" || fallbackID == trialID {
		return false, nil
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(selection.PreviousKnownGoodGenerationID) != fallbackID {
		return false, nil
	}
	return validBootRecoveryTarget(root, selection, fallbackID), nil
}
