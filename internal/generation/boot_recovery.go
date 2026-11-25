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
		if !selection.IsPendingBootTarget(generationID) {
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

// HasBootFallback reports whether a failed boot of generationID would leave a
// validated known-good generation to boot: the fallback of an armed trial, or
// the rollback target that recording the failure selects.
func HasBootFallback(root string, generationID string) (bool, error) {
	root = cleanRoot(root)
	generationID = strings.TrimSpace(generationID)
	var found bool
	err := withStateLock(filepath.Join(root, "var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		_, status, err := ReadGeneration(root, generationID)
		if err != nil {
			return err
		}
		boot, err := readRunningBoot(root, generationID, selection, status)
		if err != nil || boot.Armed {
			found = boot.Armed
			return err
		}
		fallbackID := rollbackCandidate(selection, generationID)
		found = fallbackID != "" && validRollbackTarget(root, fallbackID)
		return nil
	})
	return found, err
}

// ManagementNetworkFailure prefixes the boot-health failure reason recorded
// when the management network was not ready.
const ManagementNetworkFailure = "management network unavailable"

// RecoveryGuidance returns the operator's next step when selection requires
// recovery because the failed generation's boot health found the management
// network unavailable, and "" for any other state. Rebooting repeats boot
// health, which promotes the generation once the network is ready.
func RecoveryGuidance(root string, selection BootSelectionRecord) string {
	failedID := strings.TrimSpace(selection.FailedBootGenerationID)
	if !selection.RecoveryRequired || failedID == "" {
		return ""
	}
	_, status, err := ReadGeneration(root, failedID)
	if err != nil || status.BootState != BootStateFailed {
		return ""
	}
	for i := len(status.StatusTransitions) - 1; i >= 0; i-- {
		transition := status.StatusTransitions[i]
		if transition.BootState != BootStateFailed {
			continue
		}
		if !strings.HasPrefix(transition.Reason, ManagementNetworkFailure) {
			return ""
		}
		return fmt.Sprintf("generation %s failed boot health because the management network was not ready and no known-good generation exists to fall back to; reconnect the management network, then run `katlctl node reboot <node>` to repeat boot health", failedID)
	}
	return ""
}

// ClearBootRecovery removes the current boot's ephemeral reboot authorization.
func ClearBootRecovery(root string) error {
	path := rootedPathUnchecked(root, bootRecoveryPath)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear boot recovery authorization: %w", err)
	}
	return nil
}

type runningBoot struct {
	GenerationID string
	Trial        bool
	Armed        bool
	KnownGood    bool
}

func (boot runningBoot) failureDemotes() bool {
	return boot.Trial || !boot.KnownGood
}

func readRunningBoot(root, generationID string, selection BootSelectionRecord, status GenerationStatus) (runningBoot, error) {
	boot := runningBoot{
		GenerationID: generationID,
		Trial:        selection.IsPendingBootTarget(generationID),
		KnownGood:    IsKnownGood(status),
	}
	data, err := os.ReadFile(rootedPathUnchecked(root, bootRecoveryPath))
	if errors.Is(err, os.ErrNotExist) {
		return boot, nil
	}
	if err != nil {
		return runningBoot{}, fmt.Errorf("read boot recovery authorization: %w", err)
	}
	var arm bootRecoveryArm
	if err := json.Unmarshal(data, &arm); err != nil {
		return runningBoot{}, fmt.Errorf("decode boot recovery authorization: %w", err)
	}
	trialID := strings.TrimSpace(arm.TrialGeneration)
	fallbackID := strings.TrimSpace(arm.FallbackGeneration)
	if trialID == generationID && fallbackID != "" && fallbackID != trialID &&
		strings.TrimSpace(selection.PreviousKnownGoodGenerationID) == fallbackID {
		boot.Armed = validBootRecoveryTarget(root, selection, fallbackID)
	}
	boot.Trial = boot.Trial || boot.Armed
	return boot, nil
}
