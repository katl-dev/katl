package generation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BootHealthSuccess = "success"
	BootHealthFailure = "failure"
	BootHealthTimeout = "timeout"
)

type BootHealthRequest struct {
	Root               string
	GenerationID       string
	CommandLine        string
	Result             string
	Reason             string
	Now                time.Time
	RebootRequestPath  string
	WriteRebootRequest bool
	ForceFailure       bool
	SetBootDefault     BootDefaultSetter
}

type BootHealthResult struct {
	GenerationID      string
	Result            string
	Promoted          bool
	Failed            bool
	RebootRequested   bool
	RecoveryRequired  bool
	DefaultGeneration string
	BootDefaultEntry  string
	BootDefaultSet    bool
}

type BootDefaultSetter func(root string, bootEntry string) error

func RecordBootHealth(request BootHealthRequest) (BootHealthResult, error) {
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	generationID := strings.TrimSpace(request.GenerationID)
	if generationID == "" {
		var err error
		generationID, err = SelectedGenerationFromCommandLine(request.CommandLine)
		if err != nil {
			return BootHealthResult{}, err
		}
	}
	if strings.TrimSpace(request.CommandLine) == "" {
		return BootHealthResult{}, fmt.Errorf("boot health requires kernel command line evidence")
	}
	switch strings.TrimSpace(request.Result) {
	case BootHealthSuccess:
		return promoteBootedGeneration(request, generationID, now)
	case BootHealthFailure, BootHealthTimeout:
		return failBootedGeneration(request, generationID, now)
	default:
		return BootHealthResult{}, fmt.Errorf("boot health result must be success, failure, or timeout")
	}
}

func promoteBootedGeneration(request BootHealthRequest, generationID string, now time.Time) (BootHealthResult, error) {
	root := cleanRoot(request.Root)
	selection, err := ReadBootSelection(root)
	if err != nil {
		return BootHealthResult{}, err
	}
	spec, status, err := ReadGeneration(root, generationID)
	if err != nil {
		return BootHealthResult{}, err
	}
	selection = inferBootedSelection(selection, spec, generationID, request.CommandLine)
	if err := validateBootedSelection(selection, spec, generationID, request.CommandLine); err != nil {
		return BootHealthResult{}, err
	}
	if status.CommitState != CommitStateCommitted && status.CommitState != CommitStateSuperseded {
		return BootHealthResult{}, fmt.Errorf("generation %s commitState %s cannot be promoted", generationID, status.CommitState)
	}
	previousID := strings.TrimSpace(selection.DefaultGenerationID)
	previousBootEntry := strings.TrimSpace(selection.DefaultBootEntry)
	nextDefaultBootEntry := strings.TrimSpace(selection.DefaultBootEntry)
	if strings.TrimSpace(selection.TargetBootEntry) != "" {
		nextDefaultBootEntry = strings.TrimSpace(selection.TargetBootEntry)
	} else if strings.TrimSpace(selection.TrialBootEntry) != "" {
		nextDefaultBootEntry = strings.TrimSpace(selection.TrialBootEntry)
	}
	bootDefaultSet := false
	if nextDefaultBootEntry != "" && previousBootEntry != nextDefaultBootEntry {
		if request.SetBootDefault == nil {
			return BootHealthResult{}, fmt.Errorf("boot default update required for %s but no updater is configured", nextDefaultBootEntry)
		}
		if err := request.SetBootDefault(root, nextDefaultBootEntry); err != nil {
			return BootHealthResult{}, fmt.Errorf("set boot default %s: %w", nextDefaultBootEntry, err)
		}
		bootDefaultSet = true
	}
	updatedStatus := status
	updatedStatus.CommitState = CommitStateCommitted
	updatedStatus.BootState = BootStateGood
	updatedStatus.HealthState = HealthStateHealthy
	updatedStatus.UpdatedAt = now.UTC()
	updatedStatus.StatusTransitions = append(updatedStatus.StatusTransitions, StatusTransition{
		At:          now.UTC(),
		Reason:      transitionReason(request.Reason, "katl boot health succeeded"),
		CommitState: updatedStatus.CommitState,
		BootState:   updatedStatus.BootState,
		HealthState: updatedStatus.HealthState,
	})
	if status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		if err := WriteGenerationStatus(root, spec, updatedStatus); err != nil {
			return BootHealthResult{}, err
		}
	}
	selection.DefaultGenerationID = generationID
	selection.TargetBootGenerationID = ""
	selection.TrialGenerationID = ""
	selection.BootedGenerationID = generationID
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.FailedBootGenerationID = ""
	selection.RecoveryRequired = false
	selection.DefaultBootEntry = nextDefaultBootEntry
	selection.TargetBootEntry = ""
	selection.TrialBootEntry = ""
	selection.BootCountedTrialPath = ""
	if previousID != "" && previousID != generationID {
		selection.PreviousKnownGoodGenerationID = previousID
		selection.PreviousKnownGoodBootEntry = previousBootEntry
		if err := supersedePreviousGeneration(root, previousID, generationID, now); err != nil {
			return BootHealthResult{}, err
		}
	}
	selection.UpdatedAt = now.UTC()
	if err := WriteBootSelection(root, selection); err != nil {
		return BootHealthResult{}, err
	}
	return BootHealthResult{
		GenerationID:      generationID,
		Result:            BootHealthSuccess,
		Promoted:          true,
		DefaultGeneration: selection.DefaultGenerationID,
		BootDefaultEntry:  selection.DefaultBootEntry,
		BootDefaultSet:    bootDefaultSet,
	}, nil
}

func failBootedGeneration(request BootHealthRequest, generationID string, now time.Time) (BootHealthResult, error) {
	root := cleanRoot(request.Root)
	selection, err := ReadBootSelection(root)
	if err != nil {
		return BootHealthResult{}, err
	}
	spec, status, err := ReadGeneration(root, generationID)
	if err != nil {
		return BootHealthResult{}, err
	}
	selection = inferBootedSelection(selection, spec, generationID, request.CommandLine)
	if err := validateBootedSelection(selection, spec, generationID, request.CommandLine); err != nil {
		return BootHealthResult{}, err
	}
	if status.BootState == BootStateGood && status.HealthState == HealthStateHealthy && !request.ForceFailure {
		return BootHealthResult{
			GenerationID:      generationID,
			Result:            request.Result,
			DefaultGeneration: strings.TrimSpace(selection.DefaultGenerationID),
			BootDefaultEntry:  strings.TrimSpace(selection.DefaultBootEntry),
		}, nil
	}
	if status.BootState != BootStateFailed || status.HealthState != HealthStateUnhealthy {
		updatedStatus := status
		updatedStatus.BootState = BootStateFailed
		updatedStatus.HealthState = HealthStateUnhealthy
		updatedStatus.UpdatedAt = now.UTC()
		updatedStatus.StatusTransitions = append(updatedStatus.StatusTransitions, StatusTransition{
			At:          now.UTC(),
			Reason:      transitionReason(request.Reason, "katl boot health failed"),
			CommitState: updatedStatus.CommitState,
			BootState:   updatedStatus.BootState,
			HealthState: updatedStatus.HealthState,
		})
		if err := WriteGenerationStatus(root, spec, updatedStatus); err != nil {
			return BootHealthResult{}, err
		}
	}
	previousID := strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
	if previousID == "" && selection.DefaultGenerationID != generationID {
		previousID = strings.TrimSpace(selection.DefaultGenerationID)
	}
	if previousID != "" && !validRollbackTarget(root, previousID) {
		previousID = ""
		selection.PreviousKnownGoodGenerationID = ""
		selection.PreviousKnownGoodBootEntry = ""
	}
	selection.FailedBootGenerationID = generationID
	selection.TargetBootGenerationID = ""
	selection.TrialGenerationID = ""
	selection.TargetBootEntry = ""
	selection.TrialBootEntry = ""
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = ""
	selection.BootCountedTrialPath = ""
	if previousID != "" {
		selection.DefaultGenerationID = previousID
		selection.RecoveryRequired = false
		if strings.TrimSpace(selection.PreviousKnownGoodBootEntry) != "" {
			selection.DefaultBootEntry = selection.PreviousKnownGoodBootEntry
		}
		selection.BootedGenerationID = previousID
		selection.BootedBootEntry = selection.DefaultBootEntry
	} else {
		selection.RecoveryRequired = true
	}
	selection.UpdatedAt = now.UTC()
	if err := WriteBootSelection(root, selection); err != nil {
		return BootHealthResult{}, err
	}
	rebootRequested := false
	if request.WriteRebootRequest {
		if err := writeRebootRequest(root, request.RebootRequestPath, generationID, request.Result, now); err != nil {
			return BootHealthResult{}, err
		}
		rebootRequested = true
	}
	return BootHealthResult{
		GenerationID:      generationID,
		Result:            request.Result,
		Failed:            true,
		RebootRequested:   rebootRequested,
		RecoveryRequired:  selection.RecoveryRequired,
		DefaultGeneration: selection.DefaultGenerationID,
		BootDefaultEntry:  selection.DefaultBootEntry,
	}, nil
}

func inferBootedSelection(selection BootSelectionRecord, spec GenerationSpec, generationID string, commandLine string) BootSelectionRecord {
	if !selection.PendingHealthValidation {
		return selection
	}
	cmdlineGeneration, err := SelectedGenerationFromCommandLine(commandLine)
	if err != nil || cmdlineGeneration != generationID {
		return selection
	}
	trialID := strings.TrimSpace(selection.TrialGenerationID)
	targetID := strings.TrimSpace(selection.TargetBootGenerationID)
	if generationID != trialID && generationID != targetID {
		return selection
	}
	selection.BootedGenerationID = generationID
	switch {
	case generationID == trialID && strings.TrimSpace(selection.TrialBootEntry) != "":
		selection.BootedBootEntry = strings.TrimSpace(selection.TrialBootEntry)
	case generationID == targetID && strings.TrimSpace(selection.TargetBootEntry) != "":
		selection.BootedBootEntry = strings.TrimSpace(selection.TargetBootEntry)
	default:
		selection.BootedBootEntry = strings.TrimSpace(spec.Boot.LoaderEntryPath)
	}
	return selection
}

func validateBootedSelection(selection BootSelectionRecord, spec GenerationSpec, generationID string, commandLine string) error {
	if selection.PendingHealthValidation {
		targetID := strings.TrimSpace(selection.TargetBootGenerationID)
		trialID := strings.TrimSpace(selection.TrialGenerationID)
		if generationID != targetID && generationID != trialID {
			return fmt.Errorf("selected generation %s does not match pending boot target %s", generationID, firstNonEmptyBootGeneration(targetID, trialID))
		}
	}
	if strings.TrimSpace(selection.BootedGenerationID) == "" {
		return fmt.Errorf("bootedGenerationID is required for boot health")
	}
	if strings.TrimSpace(selection.BootedGenerationID) != generationID {
		return fmt.Errorf("bootedGenerationID %s does not match selected generation %s", selection.BootedGenerationID, generationID)
	}
	cmdlineGeneration, err := SelectedGenerationFromCommandLine(commandLine)
	if err != nil {
		return err
	}
	if cmdlineGeneration != generationID {
		return fmt.Errorf("kernel command line generation %s does not match selected generation %s", cmdlineGeneration, generationID)
	}
	rootUUID, err := rootPartUUIDFromCommandLine(commandLine)
	if err != nil {
		return err
	}
	if !strings.EqualFold(rootUUID, spec.Root.PartitionUUID) {
		return fmt.Errorf("kernel command line root PARTUUID %s does not match generation root PARTUUID %s", rootUUID, spec.Root.PartitionUUID)
	}
	bootedEntry := strings.TrimSpace(selection.BootedBootEntry)
	if bootedEntry == "" {
		return fmt.Errorf("bootedBootEntry is required for boot health")
	}
	if strings.TrimSpace(spec.Boot.LoaderEntryPath) == "" {
		return fmt.Errorf("generation %s loaderEntryPath is required for boot health", generationID)
	}
	if bootedEntry != spec.Boot.LoaderEntryPath {
		return fmt.Errorf("bootedBootEntry %s does not match generation loader entry %s", bootedEntry, spec.Boot.LoaderEntryPath)
	}
	return nil
}

func firstNonEmptyBootGeneration(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "<missing>"
}

func validRollbackTarget(root string, generationID string) bool {
	_, status, err := ReadGeneration(root, generationID)
	if err != nil {
		return false
	}
	return IsKnownGood(status)
}

func rootPartUUIDFromCommandLine(commandLine string) (string, error) {
	for _, field := range strings.Fields(commandLine) {
		value, ok := strings.CutPrefix(field, "root=PARTUUID=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("root PARTUUID is empty")
		}
		return value, nil
	}
	return "", fmt.Errorf("kernel command line missing root=PARTUUID")
}

func supersedePreviousGeneration(root string, previousID string, replacementID string, now time.Time) error {
	spec, status, err := ReadGeneration(root, previousID)
	if err != nil {
		return err
	}
	if status.CommitState != CommitStateCommitted {
		return nil
	}
	status.CommitState = CommitStateSuperseded
	status.UpdatedAt = now.UTC()
	status.StatusTransitions = append(status.StatusTransitions, StatusTransition{
		At:          now.UTC(),
		Reason:      "superseded by known-good generation " + replacementID,
		CommitState: status.CommitState,
		BootState:   status.BootState,
		HealthState: status.HealthState,
	})
	return WriteGenerationStatus(root, spec, status)
}

func writeRebootRequest(root string, path string, generationID string, result string, now time.Time) error {
	if strings.TrimSpace(path) == "" {
		path = "/run/katl/boot-health/reboot-requested"
	}
	path = rootedPathUnchecked(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf("generation=%s\nresult=%s\nrequestedAt=%s\n", generationID, result, now.UTC().Format(time.RFC3339Nano))
	return os.WriteFile(path, []byte(content), 0o644)
}

func transitionReason(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func cleanRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return root
}

func rootedPathUnchecked(root string, absolutePath string) string {
	root = cleanRoot(root)
	absolutePath = filepath.Clean("/" + strings.TrimPrefix(absolutePath, "/"))
	if root == "/" {
		return absolutePath
	}
	return filepath.Join(root, strings.TrimPrefix(absolutePath, "/"))
}
