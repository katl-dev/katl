package generation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/persistedrecord"
)

const (
	BootHealthSuccess = "success"
	BootHealthFailure = "failure"
	BootHealthTimeout = "timeout"
)

// BootHealthResult reports a boot health decision and the boot state it left.
type BootHealthResult struct {
	GenerationID string
	Result       string
	Promoted     bool
	// Failed reports that this boot resolved a bound trial as failed.
	Failed bool
	// ReportOnly reports that no trial was bound to this boot, so the failure
	// changed no boot target.
	ReportOnly bool
	// KnownGood reports that the running generation has passed boot health.
	KnownGood          bool
	RebootRequested    bool
	RecoveryRequired   bool
	DefaultGeneration  string
	NextBootGeneration string
	BootDefaultEntry   string
	FallbackProblem    string
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
	if err := persistedrecord.WriteFileAtomic(rootedPathUnchecked(root, currentBootPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write current boot record: %w", err)
	}
	return nil
}

// observedBoot applies the booted-generation rules to the published selection
// for a boot with no trial authority. It changes only the booted fields, the
// active generation and bootedOneShot; firstActivation marks the first
// activation of this boot, which replaces the active generation a previous
// boot left. Later runs preserve this boot's live promotion.
func observedBoot(selection BootSelectionRecord, spec GenerationSpec, generationID, commandLine string, firstActivation bool) (BootSelectionRecord, error) {
	if err := validatePendingBootTarget(selection, generationID); err != nil {
		return BootSelectionRecord{}, err
	}
	observed := inferBootedSelection(selection, spec, generationID, commandLine)
	if err := validateBootedSelection(observed, spec, generationID, commandLine); err != nil {
		return BootSelectionRecord{}, err
	}
	if firstActivation {
		observed.ActiveGenerationID = generationID
		observed.BootedOneShot = false
	}
	return observed, nil
}

// LivePromotionRequest promotes a candidate whose complete runtime payload was
// activated and passed its product-specific live health checks, such as a
// Kubernetes-only sysext upgrade, without a disruptive trial reboot.
type LivePromotionRequest struct {
	GenerationID string
	OperationID  string
	Reason       string
	Now          time.Time
}

// livePromotion builds the move that makes a live-validated candidate the
// default. A repeat of a published promotion builds nothing.
func livePromotion(root string, selection BootSelectionRecord, request LivePromotionRequest) (*bootMoveRequest, error) {
	id := request.GenerationID
	if id == "" || request.OperationID == "" {
		return nil, fmt.Errorf("live promotion requires a generation and an operation ID")
	}
	spec, status, err := ReadGeneration(root, id)
	if err != nil {
		return nil, err
	}
	if carriesOperation(status, request.OperationID) {
		if selection.DefaultGenerationID == id && !selection.PendingHealthValidation {
			return nil, nil
		}
		return nil, fmt.Errorf("live promotion %s of generation %s no longer follows the boot selection; read the node status before retrying", request.OperationID, id)
	}
	if status.CommitState != CommitStateCandidate && status.CommitState != CommitStateCommitted {
		return nil, fmt.Errorf("generation %s commitState %s cannot be promoted live", id, status.CommitState)
	}
	entry := spec.Boot.LoaderEntryPath
	if entry == "" {
		return nil, fmt.Errorf("generation %s loader entry is required for live promotion", id)
	}
	changes := []bootStatusChange{{
		GenerationID: id, At: request.Now.UTC(),
		Reason:      transitionReason(request.Reason, "live activation passed health checks and was promoted as known-good"),
		CommitState: CommitStateCommitted, BootState: BootStateGood, HealthState: HealthStateHealthy,
		UnavailableReason: new(status.UnavailableReason),
	}}
	previousID, previousEntry := selection.DefaultGenerationID, selection.DefaultBootEntry
	if previousID != "" && previousID != id {
		superseded, err := supersededStatus(root, previousID, id, request.Now)
		if err != nil {
			return nil, err
		}
		changes = append(changes, superseded...)
		selection.PreviousKnownGoodGenerationID = previousID
		selection.PreviousKnownGoodBootEntry = previousEntry
	}
	selection.OneShot = false
	selection.DefaultGenerationID, selection.DefaultBootEntry = id, entry
	selection.TargetBootGenerationID, selection.TargetBootEntry = "", ""
	selection.TrialGenerationID, selection.TrialBootEntry = "", ""
	selection.ActiveGenerationID = id
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.FailedBootGenerationID = ""
	selection.RecoveryRequired = false
	selection.BootCountedTrialPath = ""
	return &bootMoveRequest{
		OperationID: request.OperationID, ExpectedRevision: selection.Revision,
		Source: previousID, Destination: id, Selection: selection, StatusChanges: changes,
	}, nil
}

// confirmedBoot builds the move that a healthy boot of generationID
// publishes, or nil when the selection already records it. Released
// reader-only runtimes interpret the selection with these same rules after a
// rollback, so they must not change.
func confirmedBoot(root string, selection BootSelectionRecord, generationID, commandLine, reason string, now time.Time) (*bootMoveRequest, error) {
	spec, status, err := ReadGeneration(root, generationID)
	if err != nil {
		return nil, err
	}
	if status.UnavailableReason != "" {
		return nil, fmt.Errorf("generation %s cannot boot: %s", generationID, status.UnavailableReason)
	}
	if err := validatePendingBootTarget(selection, generationID); err != nil {
		return nil, err
	}
	// Health retries preserve a consumed one-shot's temporary boot and any
	// manual selection queued for another generation.
	laterManualTarget := selection.TargetBootGenerationID != "" && selection.TargetBootGenerationID != generationID
	if !selection.PendingHealthValidation && generationID == selection.BootedGenerationID &&
		IsKnownGood(status) && (selection.BootedOneShot || laterManualTarget) {
		if err := validateBootedSelection(selection, spec, generationID, commandLine); err != nil {
			return nil, err
		}
		return nil, nil
	}
	publishedDefault := selection.DefaultGenerationID
	temporary := selection.OneShot && generationID != selection.DefaultGenerationID
	fallbackRecovery := isFallbackRecovery(selection, generationID)
	selection = inferBootedSelection(selection, spec, generationID, commandLine)
	if err := validateBootedSelection(selection, spec, generationID, commandLine); err != nil {
		return nil, err
	}
	if status.CommitState != CommitStateCommitted && status.CommitState != CommitStateSuperseded {
		return nil, fmt.Errorf("generation %s commitState %s cannot be promoted", generationID, status.CommitState)
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
	var changes []bootStatusChange
	if status.CommitState != CommitStateCommitted || status.BootState != BootStateGood || status.HealthState != HealthStateHealthy {
		changes = append(changes, bootStatusChange{
			GenerationID: generationID, At: now.UTC(),
			Reason:      transitionReason(reason, "katl boot health succeeded"),
			CommitState: CommitStateCommitted, BootState: BootStateGood, HealthState: HealthStateHealthy,
			UnavailableReason: new(status.UnavailableReason),
		})
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
	// Recovery ends only with a healthy boot of the published default.
	selection.RecoveryRequired = selection.RecoveryRequired && generationID != publishedDefault
	selection.DefaultBootEntry = nextDefaultBootEntry
	selection.TargetBootEntry = ""
	selection.TrialBootEntry = ""
	selection.BootCountedTrialPath = ""
	if !temporary && previousID != "" && previousID != generationID {
		superseded, err := supersededStatus(root, previousID, generationID, now)
		if err != nil {
			return nil, err
		}
		changes = append(changes, superseded...)
		if !fallbackRecovery {
			selection.PreviousKnownGoodGenerationID = previousID
			selection.PreviousKnownGoodBootEntry = previousBootEntry
		}
	}
	if fallbackRecovery {
		selection.PreviousKnownGoodGenerationID = ""
		selection.PreviousKnownGoodBootEntry = ""
	}
	return &bootMoveRequest{
		ExpectedRevision: selection.Revision, Source: generationID, Destination: selection.DefaultGenerationID,
		Selection: selection, StatusChanges: changes,
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

// supersededStatus is the status change that retires a committed previous
// default once another generation is known-good. Other states stay as they are.
func supersededStatus(root, previousID, replacementID string, now time.Time) ([]bootStatusChange, error) {
	_, status, err := ReadGeneration(root, previousID)
	if err != nil {
		return nil, err
	}
	if status.CommitState != CommitStateCommitted {
		return nil, nil
	}
	return []bootStatusChange{{
		GenerationID: previousID, At: now.UTC(),
		Reason:      "superseded by known-good generation " + replacementID,
		CommitState: CommitStateSuperseded, BootState: status.BootState, HealthState: status.HealthState,
		UnavailableReason: new(status.UnavailableReason),
	}}, nil
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
