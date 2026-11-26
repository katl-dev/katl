package generation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
)

const (
	BootHealthSuccess = "success"
	BootHealthFailure = "failure"
	BootHealthTimeout = "timeout"
)

type BootHealthRequest struct {
	Root           string
	GenerationID   string
	CommandLine    string
	Result         string
	Reason         string
	Now            time.Time
	RequestReboot  bool
	SetBootDefault BootDefaultSetter
}

type BootHealthResult struct {
	GenerationID       string
	Result             string
	Promoted           bool
	Failed             bool
	KeptKnownGood      bool
	RebootRequested    bool
	RecoveryRequired   bool
	DefaultGeneration  string
	NextBootGeneration string
	BootDefaultEntry   string
	BootDefaultSet     bool
	FallbackProblem    string
}

type BootDefaultSetter func(root string, bootEntry string) error

// RecordBootedGeneration records the kernel's selected generation as booted
// and active before services can report boot state. Health validation still
// decides promotion.
func RecordBootedGeneration(root, commandLine string) error {
	root = cleanRoot(root)
	generationID, err := SelectedGenerationFromCommandLine(commandLine)
	if err != nil {
		return err
	}
	return withBootState(root, generationID, efiboot.Reader{}.Read, func(state bootState) error {
		if state.intent != nil {
			if err := state.intent.validateBootedGeneration(generationID); err != nil {
				return err
			}
			spec, _, err := ReadGeneration(root, generationID)
			if err != nil {
				return err
			}
			observed := BootSelectionRecord{BootedGenerationID: generationID, BootedBootEntry: spec.Boot.LoaderEntryPath}
			if err := validateBootedSelection(observed, spec, generationID, commandLine); err != nil {
				return err
			}
			return writeCurrentBoot(root, currentBoot{GenerationID: generationID})
		}
		if state.legacy == nil {
			return nil
		}
		selection := state.legacy.selection
		running := state.legacy.generations[generationID]
		if running.err != nil {
			return running.err
		}
		spec := running.spec
		if err := validatePendingBootTarget(selection, generationID); err != nil {
			return err
		}
		observed := inferBootedSelection(selection, spec, generationID, commandLine)
		if err := validateBootedSelection(observed, spec, generationID, commandLine); err != nil {
			return err
		}
		// Activation reruns when systemd-sysext.service restarts. Only its
		// first run in a boot replaces an active generation left by the
		// previous boot; later runs must keep a live promotion from this boot.
		_, recorded, err := readCurrentBoot(root)
		if err != nil {
			return err
		}
		if !recorded {
			observed.ActiveGenerationID = generationID
			observed.BootedOneShot = false
		}
		if observed != selection {
			observed.UpdatedAt = time.Now().UTC()
			if err := WriteBootSelection(root, observed); err != nil {
				return err
			}
		}
		if recorded {
			return nil
		}
		return writeCurrentBoot(root, currentBoot{GenerationID: generationID})
	})
}

// currentBootPath records that activation has run since the kernel started.
// It lives on /run, so a missing record means this boot has not been
// activated yet. A systemd soft-reboot would keep /run; KatlOS does not use it.
const currentBootPath = "/run/katl/boot-health/current"

type currentBoot struct {
	GenerationID string `json:"generationID"`
}

// CurrentBootGeneration returns the generation that activation recorded for
// this boot. It reports false when no activation run has recorded this boot.
func CurrentBootGeneration(root string) (string, bool, error) {
	boot, recorded, err := readCurrentBoot(cleanRoot(root))
	return boot.GenerationID, recorded, err
}

func readCurrentBoot(root string) (currentBoot, bool, error) {
	data, err := os.ReadFile(rootedPathUnchecked(root, currentBootPath))
	if errors.Is(err, os.ErrNotExist) {
		return currentBoot{}, false, nil
	}
	if err != nil {
		return currentBoot{}, false, fmt.Errorf("read current boot record: %w", err)
	}
	var boot currentBoot
	if err := json.Unmarshal(data, &boot); err != nil {
		return currentBoot{}, false, fmt.Errorf("decode current boot record: %w", err)
	}
	return boot, true, nil
}

func writeCurrentBoot(root string, boot currentBoot) error {
	data, err := json.Marshal(boot)
	if err != nil {
		return fmt.Errorf("marshal current boot record: %w", err)
	}
	if err := writeFileAtomic(rootedPathUnchecked(root, currentBootPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write current boot record: %w", err)
	}
	return nil
}

type LivePromotionRequest struct {
	Root           string
	GenerationID   string
	OperationID    string
	Reason         string
	Now            time.Time
	SetBootDefault BootDefaultSetter
	SetBootOneshot BootDefaultSetter
}

// PromoteLiveGeneration records a candidate as known-good when the candidate's
// complete runtime payload was activated and passed its product-specific live
// health checks. It is for transitions, such as a Kubernetes-only sysext
// upgrade, whose boot payload is identical to the runtime that was just
// validated and therefore does not require a disruptive trial reboot.
func PromoteLiveGeneration(request LivePromotionRequest) error {
	return WithLegacyBootState(request.Root, func() error { return promoteLiveGeneration(request) })
}

func promoteLiveGeneration(request LivePromotionRequest) error {
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	root := cleanRoot(request.Root)
	generationID := strings.TrimSpace(request.GenerationID)
	if generationID == "" {
		return fmt.Errorf("live promotion generationID is required")
	}
	spec, status, err := ReadGeneration(root, generationID)
	if err != nil {
		return err
	}
	resuming := status.CommitState == CommitStateCommitted && IsKnownGood(status) && request.OperationID != "" && status.CommittedByOperation == request.OperationID
	if status.CommitState != CommitStateCandidate && !resuming {
		return fmt.Errorf("generation %s commitState %s cannot be promoted live", generationID, status.CommitState)
	}
	entry := strings.TrimSpace(spec.Boot.LoaderEntryPath)
	if entry == "" {
		return fmt.Errorf("generation %s loader entry is required for live promotion", generationID)
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		return err
	}
	if request.SetBootOneshot == nil {
		return fmt.Errorf("boot one-shot updater is required for live promotion")
	}
	// A committed, healthy candidate records that live validation passed. On
	// retry, replay the external boot-default write even if selection.json was
	// already published: that file cannot prove the EFI update completed.
	if resuming && selection.DefaultGenerationID == generationID {
		if selection.ActiveGenerationID != generationID || selection.PendingHealthValidation {
			return fmt.Errorf("live promotion %s conflicts with current boot selection", generationID)
		}
		if request.SetBootDefault == nil {
			return fmt.Errorf("boot default updater is required to resume live promotion")
		}
		if err := request.SetBootDefault(root, entry); err != nil {
			return err
		}
		return request.SetBootOneshot(root, entry)
	}
	if resuming && selection.DefaultGenerationID != spec.PreviousGenerationID {
		return fmt.Errorf("live promotion %s no longer follows the default generation", generationID)
	}
	previousSelection := selection
	previousID := strings.TrimSpace(selection.DefaultGenerationID)
	previousEntry := strings.TrimSpace(selection.DefaultBootEntry)
	if previousEntry != entry && request.SetBootDefault == nil {
		return fmt.Errorf("boot default update required for %s but no updater is configured", entry)
	}

	var previousSpec GenerationSpec
	var previousStatus GenerationStatus
	havePrevious := previousID != "" && previousID != generationID
	if havePrevious {
		previousSpec, previousStatus, err = ReadGeneration(root, previousID)
		if err != nil {
			return err
		}
		previousStatus.StatusTransitions = append([]StatusTransition(nil), previousStatus.StatusTransitions...)
	}
	rollbackDurable := func(cause error) error {
		var rollbackErr error
		if err := WriteBootSelection(root, previousSelection); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore boot selection: %w", err))
		}
		if havePrevious {
			if err := WriteGenerationStatus(root, previousSpec, previousStatus); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore previous generation: %w", err))
			}
		}
		compensated := status
		compensated.CommitState = CommitStateSuperseded
		compensated.UpdatedAt = now
		compensated.StatusTransitions = append(compensated.StatusTransitions, StatusTransition{
			At:          now,
			OperationID: strings.TrimSpace(request.OperationID),
			Reason:      "live promotion was compensated before changing the persistent boot default",
			CommitState: compensated.CommitState,
			BootState:   compensated.BootState,
			HealthState: compensated.HealthState,
		})
		if err := WriteGenerationStatus(root, spec, compensated); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("supersede compensated candidate generation: %w", err))
		}
		return errors.Join(cause, rollbackErr)
	}

	if !resuming {
		status.CommitState = CommitStateCommitted
		status.BootState = BootStateGood
		status.HealthState = HealthStateHealthy
		status.UpdatedAt = now
		status.CommittedAt = &now
		status.CommittedByOperation = strings.TrimSpace(request.OperationID)
		status.StatusTransitions = append(status.StatusTransitions, StatusTransition{
			At:          now,
			OperationID: strings.TrimSpace(request.OperationID),
			Reason:      transitionReason(request.Reason, "live activation passed health checks and was promoted as known-good"),
			CommitState: status.CommitState,
			BootState:   status.BootState,
			HealthState: status.HealthState,
		})
		if err := WriteGenerationStatus(root, spec, status); err != nil {
			return err
		}
	}

	if havePrevious {
		if err := supersedePreviousGeneration(root, previousID, generationID, now); err != nil {
			return rollbackDurable(err)
		}
	}
	selection.OneShot = false
	selection.DefaultGenerationID = generationID
	selection.TargetBootGenerationID = ""
	selection.TrialGenerationID = ""
	selection.ActiveGenerationID = generationID
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.FailedBootGenerationID = ""
	selection.RecoveryRequired = false
	selection.DefaultBootEntry = entry
	selection.TargetBootEntry = ""
	selection.TrialBootEntry = ""
	selection.BootCountedTrialPath = ""
	if previousID != "" && previousID != generationID {
		selection.PreviousKnownGoodGenerationID = previousID
		selection.PreviousKnownGoodBootEntry = previousEntry
	}
	selection.UpdatedAt = now
	if err := WriteBootSelection(root, selection); err != nil {
		return rollbackDurable(err)
	}
	if previousEntry != entry {
		if err := request.SetBootDefault(root, entry); err != nil {
			cause := fmt.Errorf("set boot default %s: %w", entry, err)
			if previousEntry == "" {
				return rollbackDurable(errors.Join(cause, fmt.Errorf("restore boot default: previous boot entry is unavailable")))
			}
			if restoreErr := request.SetBootDefault(root, previousEntry); restoreErr != nil {
				return rollbackDurable(errors.Join(cause, fmt.Errorf("restore boot default %s: %w", previousEntry, restoreErr)))
			}
			return rollbackDurable(cause)
		}
	}
	// A prior one-shot EFI choice can outlive its durable selection. Point the
	// next boot at the promoted generation before reporting a successful apply.
	if err := request.SetBootOneshot(root, entry); err != nil {
		cause := fmt.Errorf("set boot one-shot %s: %w", entry, err)
		restoreErr := request.SetBootOneshot(root, previousSelection.TargetBootEntry)
		if previousEntry != entry && previousEntry != "" {
			restoreErr = errors.Join(restoreErr, request.SetBootDefault(root, previousEntry))
		}
		return rollbackDurable(errors.Join(cause, restoreErr))
	}
	return nil
}

func RecordBootHealth(request BootHealthRequest) (BootHealthResult, error) {
	var result BootHealthResult
	err := WithLegacyBootState(request.Root, func() error {
		var err error
		result, err = recordBootHealth(request)
		return err
	})
	return result, err
}

func recordBootHealth(request BootHealthRequest) (BootHealthResult, error) {
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
	if status.UnavailableReason != "" {
		return BootHealthResult{}, fmt.Errorf("generation %s cannot boot: %s", generationID, status.UnavailableReason)
	}
	if err := validatePendingBootTarget(selection, generationID); err != nil {
		return BootHealthResult{}, err
	}
	// Health retries preserve a consumed one-shot's temporary boot and any
	// manual selection queued for another generation.
	laterManualTarget := selection.TargetBootGenerationID != "" && selection.TargetBootGenerationID != generationID
	if !selection.PendingHealthValidation && generationID == selection.BootedGenerationID &&
		IsKnownGood(status) && (selection.BootedOneShot || laterManualTarget) {
		if err := validateBootedSelection(selection, spec, generationID, request.CommandLine); err != nil {
			return BootHealthResult{}, err
		}
		return BootHealthResult{GenerationID: generationID, Result: BootHealthSuccess, DefaultGeneration: selection.DefaultGenerationID, BootDefaultEntry: selection.DefaultBootEntry}, nil
	}
	temporary := selection.OneShot && generationID != selection.DefaultGenerationID
	fallbackRecovery := isFallbackRecovery(selection, generationID)
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
	} else if fallbackRecovery {
		nextDefaultBootEntry = strings.TrimSpace(spec.Boot.LoaderEntryPath)
	}
	if temporary {
		nextDefaultBootEntry = previousBootEntry
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
	if !temporary {
		selection.DefaultGenerationID = generationID
		selection.OneShot = false
	}
	selection.TargetBootGenerationID = ""
	selection.TrialGenerationID = ""
	selection.BootedGenerationID = generationID
	selection.ActiveGenerationID = generationID
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.FailedBootGenerationID = ""
	selection.RecoveryRequired = false
	selection.DefaultBootEntry = nextDefaultBootEntry
	selection.TargetBootEntry = ""
	selection.TrialBootEntry = ""
	selection.BootCountedTrialPath = ""
	if !temporary && previousID != "" && previousID != generationID {
		if err := supersedePreviousGeneration(root, previousID, generationID, now); err != nil {
			return BootHealthResult{}, err
		}
		if !fallbackRecovery {
			selection.PreviousKnownGoodGenerationID = previousID
			selection.PreviousKnownGoodBootEntry = previousBootEntry
		}
	}
	if fallbackRecovery {
		selection.PreviousKnownGoodGenerationID = ""
		selection.PreviousKnownGoodBootEntry = ""
	}
	selection.UpdatedAt = now.UTC()
	if err := WriteBootSelection(root, selection); err != nil {
		return BootHealthResult{}, err
	}
	return BootHealthResult{
		GenerationID:      generationID,
		Result:            BootHealthSuccess,
		Promoted:          !temporary,
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
	boot, err := readRunningBoot(root, generationID, selection, status)
	if err != nil {
		return BootHealthResult{}, err
	}
	if recordedFallback(selection, generationID) {
		// OnFailure can consume the reboot authorization after health already
		// selected the fallback. It must not select a target a second time.
		reboot := request.RequestReboot && boot.Armed
		if reboot {
			if err := ClearBootRecovery(root); err != nil {
				return BootHealthResult{}, err
			}
		}
		return BootHealthResult{
			GenerationID:      generationID,
			Result:            request.Result,
			Failed:            true,
			RebootRequested:   reboot,
			DefaultGeneration: selection.DefaultGenerationID,
			BootDefaultEntry:  selection.DefaultBootEntry,
		}, nil
	}
	if !boot.failureDemotes() {
		if !selection.PendingHealthValidation && strings.TrimSpace(selection.TargetBootGenerationID) == boot.GenerationID {
			// Both manual selection modes write LoaderEntryOneShot. Its presence
			// protects a selection for a later boot of this same generation.
			_, err := os.Stat(rootedPathUnchecked(root, loaderEntryOneShotPath))
			if errors.Is(err, os.ErrNotExist) {
				// Retain the consumed one-shot as evidence about the running boot.
				// Health retries and activation of the default still need it.
				selection.BootedOneShot = selection.BootedOneShot || (selection.OneShot && generationID != selection.DefaultGenerationID)
				selection.TargetBootGenerationID = ""
				selection.TargetBootEntry = ""
				selection.OneShot = false
				selection.UpdatedAt = now.UTC()
				if err := WriteBootSelection(root, selection); err != nil {
					return BootHealthResult{}, err
				}
			} else if err != nil {
				return BootHealthResult{}, fmt.Errorf("check EFI one-shot boot selection: %w", err)
			}
		}
		return BootHealthResult{
			GenerationID:       boot.GenerationID,
			Result:             request.Result,
			KeptKnownGood:      true,
			DefaultGeneration:  strings.TrimSpace(selection.DefaultGenerationID),
			NextBootGeneration: selection.NextBootGenerationID(),
			BootDefaultEntry:   strings.TrimSpace(selection.DefaultBootEntry),
		}, nil
	}
	if err := validatePendingBootTarget(selection, generationID); err != nil {
		return BootHealthResult{}, err
	}
	originalDefaultEntry := selection.DefaultBootEntry
	previousID := strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
	if (selection.OneShot || previousID == "") && selection.DefaultGenerationID != generationID {
		previousID = strings.TrimSpace(selection.DefaultGenerationID)
		selection.PreviousKnownGoodGenerationID = previousID
		selection.PreviousKnownGoodBootEntry = originalDefaultEntry
	}
	var fallbackProblem string
	if previousID != "" {
		fallback := bootability{state: bootUnavailable, evidence: "fallback is the failed generation"}
		if previousID != generationID {
			fallback = checkRecoveryFallback(root, selection, previousID, bootFiles)
		}
		if fallback.state == bootIndeterminate {
			return BootHealthResult{}, fmt.Errorf("check fallback before changing the boot target: %w. Preserve boot diagnostics and recover from the console", fallback.problem())
		}
		if fallback.state == bootUnavailable {
			fallbackProblem = fmt.Sprintf("generation %s %v. The boot default was left unchanged. Preserve boot diagnostics and recover from the console", previousID, fallback.problem())
			previousID = ""
			selection.PreviousKnownGoodGenerationID = ""
			selection.PreviousKnownGoodBootEntry = ""
		}
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
	selection.OneShot = false
	selection.BootedOneShot = false
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
	} else {
		selection.RecoveryRequired = true
	}
	selection.UpdatedAt = now.UTC()
	if !selection.RecoveryRequired && selection.DefaultBootEntry != originalDefaultEntry {
		if request.SetBootDefault == nil {
			return BootHealthResult{}, fmt.Errorf("boot default updater required for recovery")
		}
		if err := request.SetBootDefault(root, selection.DefaultBootEntry); err != nil {
			return BootHealthResult{}, err
		}
	}
	// Consume the ephemeral authorization before handing a clean exit to
	// systemd. A service restart cannot request a second automatic reboot.
	rebootRequested := request.RequestReboot && boot.Armed && !selection.RecoveryRequired
	if rebootRequested {
		if err := ClearBootRecovery(root); err != nil {
			return BootHealthResult{}, err
		}
	}
	if err := WriteBootSelection(root, selection); err != nil {
		return BootHealthResult{}, err
	}
	return BootHealthResult{
		GenerationID:      generationID,
		Result:            request.Result,
		Failed:            true,
		RebootRequested:   rebootRequested,
		RecoveryRequired:  selection.RecoveryRequired,
		DefaultGeneration: selection.DefaultGenerationID,
		BootDefaultEntry:  selection.DefaultBootEntry,
		FallbackProblem:   fallbackProblem,
	}, nil
}

func inferBootedSelection(selection BootSelectionRecord, spec GenerationSpec, generationID string, commandLine string) BootSelectionRecord {
	cmdlineGeneration, err := SelectedGenerationFromCommandLine(commandLine)
	if err != nil || cmdlineGeneration != generationID {
		return selection
	}
	entry := strings.TrimSpace(spec.Boot.LoaderEntryPath)
	if selection.PendingHealthValidation {
		if !selection.IsPendingBootTarget(generationID) {
			return selection
		}
		switch {
		case generationID == strings.TrimSpace(selection.TrialGenerationID) && strings.TrimSpace(selection.TrialBootEntry) != "":
			entry = strings.TrimSpace(selection.TrialBootEntry)
		case generationID == strings.TrimSpace(selection.TargetBootGenerationID) && strings.TrimSpace(selection.TargetBootEntry) != "":
			entry = strings.TrimSpace(selection.TargetBootEntry)
		}
	} else {
		failedID := strings.TrimSpace(selection.FailedBootGenerationID)
		bootedID := strings.TrimSpace(selection.BootedGenerationID)
		atDefault := generationID == strings.TrimSpace(selection.DefaultGenerationID)
		switch {
		case generationID == selection.TargetBootGenerationID:
		case isManualKnownGoodFallback(selection, generationID):
		case atDefault && (selection.OneShot || selection.BootedOneShot):
		case atDefault && generationID == strings.TrimSpace(selection.ActiveGenerationID) && generationID != bootedID:
		case atDefault && failedID != "" && bootedID == failedID:
		default:
			return selection
		}
	}
	if generationID != strings.TrimSpace(selection.BootedGenerationID) {
		selection.BootedOneShot = false
	}
	selection.BootedGenerationID = generationID
	selection.BootedBootEntry = entry
	return selection
}

func isManualKnownGoodFallback(selection BootSelectionRecord, generationID string) bool {
	return !selection.PendingHealthValidation &&
		generationID != "" &&
		generationID != strings.TrimSpace(selection.DefaultGenerationID) &&
		generationID == strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
}

func isFallbackRecovery(selection BootSelectionRecord, generationID string) bool {
	if isManualKnownGoodFallback(selection, generationID) {
		return true
	}
	failedID := strings.TrimSpace(selection.FailedBootGenerationID)
	if !selection.PendingHealthValidation && failedID != "" && generationID != failedID &&
		generationID == strings.TrimSpace(selection.DefaultGenerationID) &&
		generationID == strings.TrimSpace(selection.PreviousKnownGoodGenerationID) {
		return true
	}
	return !selection.PendingHealthValidation &&
		generationID != "" &&
		generationID != strings.TrimSpace(selection.DefaultGenerationID) &&
		generationID == strings.TrimSpace(selection.BootedGenerationID) &&
		generationID == strings.TrimSpace(selection.FailedBootGenerationID)
}

func validatePendingBootTarget(selection BootSelectionRecord, generationID string) error {
	if selection.PendingHealthValidation && !selection.IsPendingBootTarget(generationID) {
		targetID := strings.TrimSpace(selection.TargetBootGenerationID)
		trialID := strings.TrimSpace(selection.TrialGenerationID)
		return fmt.Errorf("selected generation %s does not match pending boot target %s", generationID, firstNonEmptyBootGeneration(targetID, trialID))
	}
	return nil
}

func validateBootedSelection(selection BootSelectionRecord, spec GenerationSpec, generationID string, commandLine string) error {
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
	rootUUID, err := SelectedRootPartUUIDFromCommandLine(commandLine)
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

func SelectedRootPartUUIDFromCommandLine(commandLine string) (string, error) {
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
