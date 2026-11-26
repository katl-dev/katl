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
		if !selection.IsPendingBootTarget(generationID) || strings.TrimSpace(selection.PreviousKnownGoodGenerationID) == "" {
			return nil
		}
		fallback := rollbackCandidate(selection, generationID)
		// Leave recovery disarmed when the fallback cannot be verified.
		// Boot health can still qualify the running generation.
		if valid, _ := validBootRecoveryTarget(root, fallback); !valid {
			return nil
		}
		data, err := json.Marshal(bootRecoveryArm{
			TrialGeneration:    generationID,
			FallbackGeneration: fallback.generationID,
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

type rollbackTarget struct {
	generationID string
	bootEntry    string
}

func rollbackCandidate(selection BootSelectionRecord, runningID string) rollbackTarget {
	defaultID := strings.TrimSpace(selection.DefaultGenerationID)
	defaultEntry := strings.TrimSpace(selection.DefaultBootEntry)
	previousID := strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
	if (selection.OneShot && defaultID != runningID) || previousID == "" {
		if defaultID != runningID {
			return rollbackTarget{generationID: defaultID, bootEntry: defaultEntry}
		}
		return rollbackTarget{}
	}
	if previousID == runningID {
		return rollbackTarget{}
	}
	previousEntry := strings.TrimSpace(selection.PreviousKnownGoodBootEntry)
	if previousEntry == "" && previousID == defaultID {
		previousEntry = defaultEntry
	}
	return rollbackTarget{generationID: previousID, bootEntry: previousEntry}
}

func validBootRecoveryTarget(root string, target rollbackTarget) (bool, error) {
	if target.generationID == "" {
		return false, nil
	}
	spec, status, err := ReadGeneration(root, target.generationID)
	if err != nil {
		return false, fmt.Errorf("read rollback target %s: %w", target.generationID, err)
	}
	if !IsKnownGood(status) {
		return false, nil
	}
	reason, err := bootTargetUnavailable(root, spec, status, target.bootEntry)
	return reason == "", err
}

// bootTargetUnavailable checks the bound entry and its required artifacts.
// Health policy is separate: a recovery retry can boot an unproven generation.
func bootTargetUnavailable(root string, spec GenerationSpec, status GenerationStatus, entry string) (string, error) {
	if status.UnavailableReason != "" {
		return status.UnavailableReason, nil
	}
	if entry == "" {
		return "loader entry is missing", nil
	}
	if entry != spec.Boot.LoaderEntryPath {
		return "loader entry does not match the generation", nil
	}
	if status.CommitState != CommitStateCommitted && status.CommitState != CommitStateSuperseded {
		return "generation is not committed", nil
	}

	paths := []string{
		filepath.Join("/efi", entry),
		filepath.Join("/efi", entryPath(spec.Boot.UKIPath)),
	}
	for _, ref := range spec.Sysexts {
		paths = append(paths, ref.Path)
	}
	for _, ref := range spec.BundledConfexts {
		paths = append(paths, ref.Path)
	}
	for _, ref := range spec.Confexts {
		paths = append(paths, ref.Path)
	}
	for _, path := range paths {
		if _, err := os.Stat(rootedPathUnchecked(root, path)); err != nil {
			reason := "boot artifact unavailable: " + path
			if errors.Is(err, os.ErrNotExist) {
				return reason, nil
			}
			return reason, fmt.Errorf("check generation %s boot artifact %s: %w", spec.GenerationID, path, err)
		}
	}
	return "", nil
}

func recoveryDefault(root string, selection BootSelectionRecord, running GenerationSpec, status GenerationStatus) (rollbackTarget, error) {
	if selection.DefaultGenerationID != "" && selection.DefaultGenerationID != running.GenerationID {
		spec, defaultStatus, err := ReadGeneration(root, selection.DefaultGenerationID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return rollbackTarget{}, err
		}
		if err == nil {
			reason, err := bootTargetUnavailable(root, spec, defaultStatus, selection.DefaultBootEntry)
			if err != nil {
				return rollbackTarget{}, err
			}
			if reason == "" {
				return rollbackTarget{generationID: spec.GenerationID, bootEntry: selection.DefaultBootEntry}, nil
			}
		}
	}

	entry := running.Boot.LoaderEntryPath
	reason, err := bootTargetUnavailable(root, running, status, entry)
	if err != nil {
		return rollbackTarget{}, err
	}
	if reason != "" {
		return rollbackTarget{}, fmt.Errorf("generation %s cannot be selected for the next boot: %s. Restore its boot artifacts from the node console", running.GenerationID, reason)
	}
	return rollbackTarget{generationID: running.GenerationID, bootEntry: entry}, nil
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
		found = boot.Fallback.generationID != ""
		return err
	})
	return found, err
}

// ManagementNetworkFailure prefixes the boot-health failure reason recorded
// when the management network was not ready.
const ManagementNetworkFailure = "management network unavailable"

// RecoveryGuidance returns the operator's next step when selection requires
// recovery because the failed generation's boot health found the management
// network unavailable, and "" for any other state.
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
		message := fmt.Sprintf("generation %s failed boot health because the management network was not ready", failedID)
		nextID, entry := selection.DefaultGenerationID, selection.DefaultBootEntry
		if selection.TargetBootGenerationID != "" {
			nextID, entry = selection.TargetBootGenerationID, selection.TargetBootEntry
		}
		spec, nextStatus, err := ReadGeneration(root, nextID)
		if err == nil {
			reason, err := bootTargetUnavailable(root, spec, nextStatus, entry)
			if err == nil && reason == "" {
				return fmt.Sprintf("%s. Reconnect the management network, then run `katlctl node reboot <node>` to run boot health on generation %s", message, nextID)
			}
		}
		return message + ". Use the node console to restore a bootable generation before rebooting"
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
	Fallback     rollbackTarget
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
	var arm bootRecoveryArm
	if err == nil {
		if err := json.Unmarshal(data, &arm); err != nil {
			return runningBoot{}, fmt.Errorf("decode boot recovery authorization: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return runningBoot{}, fmt.Errorf("read boot recovery authorization: %w", err)
	}

	fallback := rollbackCandidate(selection, generationID)
	authorized := strings.TrimSpace(arm.TrialGeneration) == generationID &&
		fallback.generationID != "" &&
		fallback.generationID == strings.TrimSpace(arm.FallbackGeneration) &&
		fallback.generationID == strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
	if !boot.failureDemotes() && !authorized {
		return boot, nil
	}
	valid, err := validBootRecoveryTarget(root, fallback)
	if err != nil {
		return runningBoot{}, err
	}
	if valid {
		boot.Fallback = fallback
		boot.Armed = authorized
		boot.Trial = boot.Trial || boot.Armed
	}
	return boot, nil
}
