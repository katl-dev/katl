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
		if fallbackID == "" || fallbackID == generationID {
			return nil
		}
		if err := CheckTrialFallback(root, selection); err != nil {
			return err
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

// CheckTrialFallback verifies the known-good generation that a staged trial can
// return to. Callers own when the trial is eligible to be armed.
func CheckTrialFallback(root string, selection BootSelectionRecord) error {
	id := selection.PreviousKnownGoodGenerationID
	if id == "" || selection.IsPendingBootTarget(id) {
		return fmt.Errorf("cannot arm trial without a distinct known-good fallback. Boot a known-good generation before retrying")
	}
	if err := checkRecoveryFallback(root, selection, id, true).problem(); err != nil {
		return fmt.Errorf("cannot arm trial: fallback generation %s %w. Check 'katlctl node generations list' and restore the fallback or boot a known-good generation before retrying", id, err)
	}
	return nil
}

func checkRecoveryFallback(root string, selection BootSelectionRecord, generationID string, verifyRoot bool) bootability {
	spec, status, err := ReadGeneration(root, generationID)
	if errors.Is(err, os.ErrNotExist) {
		return bootability{state: bootUnavailable, evidence: "fallback generation record is missing"}
	}
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("read fallback generation %s health: %w", generationID, err)}
	}
	if !IsKnownGood(status) {
		return bootability{state: bootUnavailable, evidence: "fallback generation is not known-good"}
	}
	entry := strings.TrimSpace(selection.PreviousKnownGoodBootEntry)
	if entry == "" && strings.TrimSpace(selection.DefaultGenerationID) == generationID {
		entry = strings.TrimSpace(selection.DefaultBootEntry)
	}
	if entry == "" || entry != strings.TrimSpace(spec.Boot.LoaderEntryPath) {
		return bootability{state: bootUnavailable, evidence: "fallback loader entry does not match the generation"}
	}
	if verifyRoot {
		check := bootabilityCheck{root: root}
		return check.check(spec, status)
	}
	return checkBootFiles(root, spec)
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
		fallback := checkRecoveryFallback(root, selection, fallbackID, false)
		if fallback.state == bootIndeterminate {
			return runningBoot{}, fallback.problem()
		}
		boot.Armed = fallback.state == bootable
	}
	boot.Trial = boot.Trial || boot.Armed
	return boot, nil
}

func recordedFallback(selection BootSelectionRecord, generationID string) bool {
	return !selection.PendingHealthValidation && !selection.RecoveryRequired && !selection.OneShot &&
		selection.TargetBootGenerationID == "" && selection.TrialGenerationID == "" &&
		selection.FailedBootGenerationID == generationID &&
		selection.DefaultGenerationID != "" && selection.DefaultGenerationID == selection.PreviousKnownGoodGenerationID
}
