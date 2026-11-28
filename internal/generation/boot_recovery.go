package generation

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// CheckTrialFallback verifies the known-good generation that a staged trial can
// return to. Callers own when the trial is eligible to be armed.
func CheckTrialFallback(root string, selection BootSelectionRecord) error {
	id := selection.PreviousKnownGoodGenerationID
	if id == "" || selection.IsPendingBootTarget(id) {
		return fmt.Errorf("cannot arm trial without a distinct known-good fallback. Boot a known-good generation before retrying")
	}
	check := bootabilityCheck{root: root}
	entry := recoveryFallbackEntry(selection, id)
	if _, verdict := check.knownGoodFallback(id, &entry, bootFull); verdict.problem() != nil {
		return fmt.Errorf("cannot arm trial: fallback generation %s %w. Check 'katlctl node generations list' and restore the fallback or boot a known-good generation before retrying", id, verdict.problem())
	}
	return nil
}

func recoveryFallbackEntry(selection BootSelectionRecord, generationID string) string {
	entry := strings.TrimSpace(selection.PreviousKnownGoodBootEntry)
	if entry == "" && strings.TrimSpace(selection.DefaultGenerationID) == generationID {
		entry = strings.TrimSpace(selection.DefaultBootEntry)
	}
	return entry
}

// knownGoodFallback checks a candidate's own loader entry unless the selection
// recorded a fallback entry for it, in which case the binding must match.
func (check *bootabilityCheck) knownGoodFallback(generationID string, recordedEntry *string, level bootabilityLevel) (string, bootability) {
	spec, status, err := ReadGeneration(check.root, generationID)
	if errors.Is(err, os.ErrNotExist) {
		return "", bootability{state: bootUnavailable, evidence: "fallback generation record is missing"}
	}
	if err != nil {
		return "", bootability{state: bootIndeterminate, err: fmt.Errorf("read fallback generation %s health: %w", generationID, err)}
	}
	entry := spec.Boot.LoaderEntryPath
	if !IsKnownGood(status) {
		return entry, bootability{state: bootUnavailable, evidence: "fallback generation is not known-good"}
	}
	if recordedEntry != nil && (*recordedEntry == "" || *recordedEntry != strings.TrimSpace(entry)) {
		return entry, bootability{state: bootUnavailable, evidence: "fallback loader entry does not match the generation"}
	}
	return entry, check.check(spec, status, level)
}
