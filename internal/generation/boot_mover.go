package generation

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
)

// legacyImportBootID records that the boot which armed an imported legacy
// trial is unknown. It is never a kernel boot ID, which is a UUID.
const legacyImportBootID = "legacy-import"

// unconfirmedTrialReason records a trial that ended without a health result
// on a later boot of another recorded target.
const unconfirmedTrialReason = "boot trial ended without health confirmation"

// bootMoveStep names a durable boundary of the move protocol.
type bootMoveStep string

const (
	stepIntentWritten  bootMoveStep = "intent-written"
	stepStatusWritten  bootMoveStep = "status-written"
	stepOneShotCleared bootMoveStep = "one-shot-cleared"
	stepDefaultSet     bootMoveStep = "default-set"
	stepOneShotSet     bootMoveStep = "one-shot-set"
	stepReadBack       bootMoveStep = "read-back"
	stepRebootSpent    bootMoveStep = "reboot-spent"
	stepPublished      bootMoveStep = "published"
	stepResolved       bootMoveStep = "resolved"
)

// trialEffect is what a move does to trial authority.
type trialEffect uint8

const (
	trialNone    trialEffect = iota // grants no authority
	trialArm                        // arms a trial named by the operation ID
	trialPromote                    // resolves the bound trial without a reboot
	trialFail                       // resolves the bound trial and may spend its reboot
)

// bootMoveRequest describes one move. The owner sets the selection revision,
// spentRecoveryTrialID and updatedAt, and the time of each status change.
type bootMoveRequest struct {
	OperationID      string
	ExpectedRevision uint64
	Source           string
	Destination      string // empty when no safe destination remains
	Selection        BootSelectionRecord
	OneShot          string // generation for LoaderEntryOneShot; empty clears it
	StatusChanges    []bootStatusChange
	Trial            trialEffect
}

type bootMoveResult struct {
	// Resolved reports that no intent remains for the move, so the caller may
	// overwrite a slot or remove loader entries.
	Resolved        bool
	RebootRequested bool
}

// bootMover is the only writer of boot moves: the intent, the generation
// status it projects, the EFI variables and the published selection.
type bootMover struct {
	root        string
	bootID      string // kernel boot ID of the running boot
	commandLine string // kernel command line of the running boot
	now         func() time.Time
	readEFI     func(string) (efiboot.Entries, error)
	setDefault  BootDefaultSetter
	setOneShot  BootDefaultSetter
	// checkpoint lets tests stop at a durable boundary. A returned error
	// leaves every completed step in place, as a crash would.
	checkpoint func(bootMoveStep) error
}

// timestamp drops the monotonic reading so a published time compares equal
// to the same time decoded from disk.
func (m *bootMover) timestamp() time.Time {
	return m.now().UTC().Round(0)
}

func (m *bootMover) reached(step bootMoveStep) error {
	if m.checkpoint == nil {
		return nil
	}
	return m.checkpoint(step)
}

// reconcile completes or resolves any outstanding move for the running boot.
func (m *bootMover) reconcile() (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		var err error
		result, _, err = m.reconcileLocked(false)
		return err
	})
	return result, err
}

// move admits a request after reconciliation and carries it through the protocol.
func (m *bootMover) move(request bootMoveRequest) (bootMoveResult, error) {
	if request.Trial == trialFail {
		return bootMoveResult{}, fmt.Errorf("trial failures must use failTrial")
	}
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		reconciled, authority, err := m.reconcileLocked(false)
		if err != nil {
			return err
		}
		selection, err := ReadBootSelection(m.root)
		if err != nil {
			return err
		}
		published, err := m.published(request, selection)
		if err != nil {
			return err
		}
		if published {
			result = reconciled
			return nil
		}
		if reconciled.RebootRequested {
			result = reconciled
			return fmt.Errorf("%w: a completed move requested a reboot; reboot before starting operation %s", ErrBootMoveBlocked, request.OperationID)
		}
		if selection.Revision != request.ExpectedRevision {
			return fmt.Errorf("boot selection revision is %d, not %d: another change moved the boot target. Read the boot state again before retrying", selection.Revision, request.ExpectedRevision)
		}
		intent, err := m.admit(request, selection, authority)
		if err != nil {
			return err
		}
		result, err = m.startMove(intent)
		return err
	})
	return result, err
}

// failTrial records a health failure or deadline for the bound trial. Every
// trigger derives the same operation ID so it can finish an interrupted move.
func (m *bootMover) failTrial(reason string) (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		reconciled, authority, err := m.reconcileLocked(true)
		if err != nil {
			return err
		}
		selection, err := ReadBootSelection(m.root)
		if err != nil {
			return moveSelectionReadError(running, err)
		}
		if authority == nil && selection.FailedBootGenerationID == running {
			result = reconciled
			return nil
		}
		if !m.boundTrial(authority, running) {
			return fmt.Errorf("failure needs a trial armed for running generation %s; this boot reports health without changing the boot target. Inspect 'katlctl node status'", running)
		}

		operationID := "fail-" + *authority.Reboot.TrialID
		spec, status, err := m.readMoveGeneration(running)
		if err != nil {
			return err
		}
		failure, verificationErr := resolveFailedTrial(m.root, selection, running, running, spec.Boot.LoaderEntryPath, bootFull, false)
		if verificationErr != nil {
			return fmt.Errorf("operation %s: %w", operationID, verificationErr)
		}
		request := bootMoveRequest{
			OperationID: operationID, ExpectedRevision: selection.Revision, Source: running, Destination: failure.destination,
			Selection: failure.selection, StatusChanges: []bootStatusChange{failedTrialStatus(status, reason)}, Trial: trialFail,
		}
		intent, err := m.admit(request, selection, authority)
		if err != nil {
			return err
		}
		result, err = m.startMove(intent)
		return err
	})
	return result, err
}

// startMove durably binds the decision before projecting any of its effects.
func (m *bootMover) startMove(intent bootMoveIntent) (bootMoveResult, error) {
	if err := writeBootMoveIntent(m.root, intent); err != nil {
		return bootMoveResult{}, err
	}
	if err := m.reached(stepIntentWritten); err != nil {
		return bootMoveResult{}, err
	}
	result, _, err := m.complete(&intent, false)
	return result, err
}

// published reports whether an earlier attempt published this request: the
// next revision holds its selection and every generation it changes carries
// its operation ID. A different move with an equal selection is stale.
func (m *bootMover) published(request bootMoveRequest, selection BootSelectionRecord) (bool, error) {
	if selection.Revision != request.ExpectedRevision+1 || !sameMoveSelection(selection, request.Selection) {
		return false, nil
	}
	for _, change := range request.StatusChanges {
		_, status, err := ReadGeneration(m.root, change.GenerationID)
		if err != nil {
			return false, fmt.Errorf("operation %s: read generation %s: %w", request.OperationID, change.GenerationID, err)
		}
		if !carriesOperation(status, request.OperationID) {
			return false, nil
		}
	}
	return true, nil
}

func carriesOperation(status GenerationStatus, operationID string) bool {
	return slices.ContainsFunc(status.StatusTransitions, func(transition StatusTransition) bool {
		return transition.OperationID == operationID
	})
}

func (m *bootMover) runningGenerationID() (string, error) {
	id, err := SelectedGenerationFromCommandLine(m.commandLine)
	if err != nil {
		return "", fmt.Errorf("cannot identify the running generation: %w. Boot state is unchanged. Preserve the journal, then boot a healthy generation from the boot menu, or reinstall the node", err)
	}
	return id, nil
}

func (m *bootMover) boundTrial(authority *bootMoveIntent, running string) bool {
	return authority != nil && m.bootID != authority.BootID && running == *authority.DestinationGenerationID
}

// reconcileLocked returns the trial authority that remains after completion.
func (m *bootMover) reconcileLocked(failingTrial bool) (bootMoveResult, *bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, nil, err
	}
	intent, err := readBootMoveIntent(m.root)
	if err != nil {
		return bootMoveResult{}, nil, fmt.Errorf("boot state is unchanged for generation %s: %w", running, err)
	}
	var authority *bootMoveIntent
	var activated Record
	if intent == nil {
		// An imported legacy trial stays in memory until a move resolves it.
		if authority, err = m.importLegacyTrial(running); err != nil || authority == nil {
			return bootMoveResult{Resolved: err == nil}, nil, err
		}
		if activated, err = authority.activationRecord(m.root, running, m.commandLine); err != nil {
			return bootMoveResult{}, nil, cannotReconcileBoot(running, err)
		}
	} else {
		if activated, err = intent.activationRecord(m.root, running, m.commandLine); err != nil {
			return bootMoveResult{}, nil, cannotReconcileBoot(running, err)
		}
		var result bootMoveResult
		// A resolution checks its inputs before any EFI writes. If the arm
		// already published, its old EFI projection need not be replayed first.
		resolving := failingTrial || (m.bootID != intent.BootID && running != *intent.DestinationGenerationID)
		if result, authority, err = m.complete(intent, resolving); err != nil || authority == nil {
			return result, authority, err
		}
	}
	if m.bootID == authority.BootID || running == *authority.DestinationGenerationID {
		return bootMoveResult{}, authority, nil
	}
	unconfirmed, err := m.unconfirmedTrial(authority, running, activated.Boot.LoaderEntryPath)
	if err != nil {
		return bootMoveResult{}, authority, err
	}
	result, err := m.startMove(unconfirmed)
	return result, nil, err
}

func cannotReconcileBoot(running string, err error) error {
	return fmt.Errorf("cannot reconcile boot for generation %s: %w. Boot state is unchanged. Preserve the journal, then boot a healthy generation from the boot menu, or reinstall the node", running, err)
}

// admit builds the intent for a request and checks it before it is durable.
func (m *bootMover) admit(request bootMoveRequest, selection BootSelectionRecord, authority *bootMoveIntent) (bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveIntent{}, err
	}
	reboot := bootMoveReboot{TrialID: new(""), Eligible: new(false), Spent: new(false)}
	switch request.Trial {
	case trialPromote, trialFail:
		if !m.boundTrial(authority, running) {
			return bootMoveIntent{}, fmt.Errorf("operation %s needs a trial armed for running generation %s; this boot reports health without changing the boot target", request.OperationID, running)
		}
		if request.Trial == trialPromote && request.Selection.DefaultGenerationID != running {
			return bootMoveIntent{}, fmt.Errorf("operation %s promotes trial %s, so it must publish candidate %s as the default, not %q",
				request.OperationID, *authority.Reboot.TrialID, running, request.Selection.DefaultGenerationID)
		}
		trialID := *authority.Reboot.TrialID
		if request.Trial == trialFail && request.Destination != "" && selection.SpentRecoveryTrialID != trialID {
			reboot = bootMoveReboot{TrialID: new(trialID), Eligible: new(true), Spent: new(false)}
		}
	default:
		if authority != nil {
			return bootMoveIntent{}, fmt.Errorf("%w: trial %s awaits its health result before operation %s can move the boot target", ErrBootMoveBlocked, *authority.Reboot.TrialID, request.OperationID)
		}
		if request.Trial == trialArm {
			if request.Selection.DefaultGenerationID != selection.DefaultGenerationID {
				return bootMoveIntent{}, fmt.Errorf("operation %s arms a trial, so it must keep default %s until the trial is promoted, not %q",
					request.OperationID, selection.DefaultGenerationID, request.Selection.DefaultGenerationID)
			}
			reboot = bootMoveReboot{TrialID: new(request.OperationID), Eligible: new(true), Spent: new(false)}
		}
	}
	if request.Trial != trialArm {
		// The reader would treat such a selection as a trial without an intent.
		trialID := legacyTrialID(request.Selection, func(id string) string {
			spec, _, err := ReadGeneration(m.root, id)
			if err != nil {
				return ""
			}
			return spec.PreviousGenerationID
		})
		if trialID != "" {
			return bootMoveIntent{}, fmt.Errorf("operation %s does not arm a trial, so its selection must not await health validation of generation %s under transaction %s",
				request.OperationID, trialID, request.Selection.PendingTransactionID)
		}
	}
	// Activation accepts the running generation as the prior one-shot, so a
	// one-shot armed for another generation would go unrecorded.
	if armed := cmp.Or(selection.TargetBootGenerationID, selection.TrialGenerationID); armed != "" &&
		running != selection.DefaultGenerationID && armed != running && armed != selection.DefaultGenerationID {
		return bootMoveIntent{}, fmt.Errorf("%w: generation %s runs while a one-shot boot of generation %s is selected. Reboot into %s or the default %s before operation %s",
			ErrBootMoveBlocked, running, armed, armed, selection.DefaultGenerationID, request.OperationID)
	}
	for _, change := range request.StatusChanges {
		_, status, err := m.readMoveGeneration(change.GenerationID)
		if err != nil {
			return bootMoveIntent{}, fmt.Errorf("operation %s: %w", request.OperationID, err)
		}
		if carriesOperation(status, request.OperationID) {
			if request.Trial == trialFail {
				return bootMoveIntent{}, fmt.Errorf("trial failure operation ID %s already appears in generation %s status. Preserve the journal and boot state from the node console for diagnosis", request.OperationID, change.GenerationID)
			}
			return bootMoveIntent{}, fmt.Errorf("operation ID %s already appears in generation %s status; give the move a new operation ID", request.OperationID, change.GenerationID)
		}
	}

	observed, err := m.observeEFI("operation "+request.OperationID, cmp.Or(request.Destination, request.Source))
	if err != nil {
		return bootMoveIntent{}, err
	}
	now := m.timestamp()
	desired := request.Selection
	desired.APIVersion, desired.Kind = APIVersion, BootSelectionKind
	desired.Revision = selection.Revision + 1
	desired.SpentRecoveryTrialID = selection.SpentRecoveryTrialID
	desired.UpdatedAt = now
	changes := slices.Clone(request.StatusChanges)
	for i := range changes {
		changes[i].At = cmp.Or(changes[i].At, now)
		if changes[i].UnavailableReason == nil {
			changes[i].UnavailableReason = new("")
		}
	}
	if changes == nil {
		changes = []bootStatusChange{}
	}
	intent, err := m.newIntent(request.OperationID, selection, desired, observed, running,
		request.Source, request.Destination, request.OneShot, changes, reboot)
	if err != nil {
		return bootMoveIntent{}, err
	}
	if request.Trial == trialArm && !intent.isTrialAuthority() {
		return bootMoveIntent{}, fmt.Errorf("operation %s arms a trial, so its selection must await health validation of destination %s", request.OperationID, request.Destination)
	}
	if request.Trial == trialFail {
		// The failure rule already verified the selected fallback. The only
		// projected status is for the failed candidate, which cannot be a fallback.
		if _, err := m.checkStatusChanges(intent); err != nil {
			return bootMoveIntent{}, err
		}
	} else if err := m.checkTargets(intent, running); err != nil {
		return bootMoveIntent{}, err
	}
	return intent, nil
}

func (m *bootMover) observeEFI(operation, generationID string) (efiboot.Entries, error) {
	observed, err := m.readEFI(m.root)
	if err != nil {
		return efiboot.Entries{}, efiObservationError(operation, generationID, err)
	}
	return observed, nil
}

func efiObservationError(operation, generationID string, err error) error {
	return fmt.Errorf("observe EFI boot variables before %s for generation %s: %w. The boot target is unchanged. Restore access to efivarfs and retry", operation, generationID, err)
}

// newIntent records the prior targets from the published selection and the
// observed EFI variables, and validates the intent with the v1 reader's rules.
func (m *bootMover) newIntent(operationID string, selection, desired BootSelectionRecord, observed efiboot.Entries,
	running, source, destination, oneShot string, changes []bootStatusChange, reboot bootMoveReboot) (bootMoveIntent, error) {
	// The running generation is always a recorded target, so activation
	// accepts it again if the move is interrupted.
	priorOneShot := cmp.Or(selection.TargetBootGenerationID, selection.TrialGenerationID)
	if running != selection.DefaultGenerationID {
		priorOneShot = running
	}
	oneShotEntry := ""
	if oneShot != "" {
		oneShotEntry = filepath.Base(cmp.Or(desired.TargetBootEntry, desired.TrialBootEntry, desired.DefaultBootEntry))
	}
	intent := bootMoveIntent{
		OperationID:               operationID,
		BootID:                    m.bootID,
		ExpectedSelectionRevision: new(selection.Revision),
		SourceGenerationID:        source,
		DestinationGenerationID:   new(destination),
		PriorDefault:              m.observedTarget(selection.DefaultGenerationID, observed.Default),
		PriorOneShot:              m.observedTarget(priorOneShot, observed.OneShot),
		DesiredDefault:            &bootMoveTarget{new(desired.DefaultGenerationID), new(filepath.Base(desired.DefaultBootEntry))},
		DesiredOneShot:            &bootMoveTarget{new(oneShot), new(oneShotEntry)},
		DesiredSelection:          desired,
		StatusChanges:             changes,
		Reboot:                    reboot,
	}
	if err := intent.validate(); err != nil {
		return bootMoveIntent{}, fmt.Errorf("operation %s: %w", operationID, err)
	}
	if err := intent.validateBindings(m.root); err != nil {
		return bootMoveIntent{}, fmt.Errorf("operation %s: %w", operationID, err)
	}
	return intent, nil
}

// observedTarget keeps an observed entry only when it is the named
// generation's loader entry; the reader rejects any other binding.
func (m *bootMover) observedTarget(id, entry string) *bootMoveTarget {
	if id == "" {
		return &bootMoveTarget{new(""), new("")}
	}
	spec, _, err := ReadGeneration(m.root, id)
	if err != nil || entry != filepath.Base(spec.Boot.LoaderEntryPath) {
		entry = ""
	}
	return &bootMoveTarget{new(id), new(entry)}
}

// checkTargets verifies the status the intent projects and that every next
// target either passes the bootability check or is the running generation.
func (m *bootMover) checkTargets(intent bootMoveIntent, running string) error {
	projected, err := m.checkStatusChanges(intent)
	if err != nil {
		return err
	}
	check := bootabilityCheck{root: m.root}
	for _, id := range []string{*intent.DesiredDefault.GenerationID, *intent.DesiredOneShot.GenerationID} {
		if id == "" || id == running {
			continue
		}
		spec, status, err := m.readMoveGeneration(id)
		if err != nil {
			return fmt.Errorf("operation %s: %w", intent.OperationID, err)
		}
		if next, ok := projected[id]; ok {
			status = next
		}
		if err := check.check(spec, status, bootFull).problem(); err != nil {
			return fmt.Errorf("operation %s cannot select generation %s: %w. The boot target is unchanged. Restore the named artifact or choose a generation that 'katlctl node generations list' shows without an unavailable reason", intent.OperationID, id, err)
		}
	}
	return nil
}

func (m *bootMover) checkStatusChanges(intent bootMoveIntent) (map[string]GenerationStatus, error) {
	projected := map[string]GenerationStatus{}
	for _, change := range intent.StatusChanges {
		spec, status, err := m.readMoveGeneration(change.GenerationID)
		if err != nil {
			return nil, fmt.Errorf("operation %s: %w", intent.OperationID, err)
		}
		next := projectStatusChange(status, change, intent.OperationID)
		if err := ValidateGenerationStatus(spec, next); err != nil {
			return nil, fmt.Errorf("operation %s: invalid projected status for generation %s: %w", intent.OperationID, change.GenerationID, err)
		}
		if err := ValidateStatusTransition(status, next); err != nil {
			return nil, fmt.Errorf("operation %s: generation %s: %w", intent.OperationID, change.GenerationID, err)
		}
		projected[change.GenerationID] = next
	}
	return projected, nil
}

// importLegacyTrial returns a trial armed by a runtime without the move owner
// as an in-memory trial authority, using the reader's legacy trial
// classification. It writes nothing: a rollback target may run a reader-only
// release, which refuses any intent it cannot complete, so the authority
// becomes durable only in the intent of the move that resolves it.
func (m *bootMover) importLegacyTrial(running string) (*bootMoveIntent, error) {
	legacy, err := readLegacyBootState(m.root, running, m.readEFI)
	if err != nil {
		return nil, moveSelectionReadError(running, err)
	}
	if legacy == nil || legacy.trialGenerationID == "" {
		return nil, nil
	}
	if legacy.efiErr != nil {
		return nil, efiObservationError("importing legacy trial "+legacy.selection.PendingTransactionID, legacy.trialGenerationID, legacy.efiErr)
	}
	selection := legacy.selection
	trialID := selection.PendingTransactionID
	// The next revision only satisfies the intent's validation; nothing
	// publishes it, and a resolving move expects the legacy revision.
	desired := selection
	desired.Revision = selection.Revision + 1
	desired.UpdatedAt = m.timestamp()
	reboot := bootMoveReboot{TrialID: new(trialID), Eligible: new(true), Spent: new(selection.SpentRecoveryTrialID == trialID)}
	importer := *m
	importer.bootID = legacyImportBootID
	intent, err := importer.newIntent(trialID, selection, desired, legacy.efi, running,
		selection.DefaultGenerationID, legacy.trialGenerationID, legacy.trialGenerationID, []bootStatusChange{}, reboot)
	if err != nil {
		return nil, fmt.Errorf("import legacy trial: %w", err)
	}
	return &intent, nil
}

// unconfirmedTrial resolves an authority whose trial ended without a
// health result, on a later boot of another recorded target. It never
// reboots. The selection follows the same failure rule as a failed trial.
func (m *bootMover) unconfirmedTrial(authority *bootMoveIntent, running, runningEntry string) (bootMoveIntent, error) {
	candidate := *authority.DestinationGenerationID
	trialID := *authority.Reboot.TrialID
	selection, err := ReadBootSelection(m.root)
	if err != nil {
		return bootMoveIntent{}, fmt.Errorf("resolve trial %s: %w", trialID, moveSelectionReadError(candidate, err))
	}
	_, status, err := m.readMoveGeneration(candidate)
	if err != nil {
		return bootMoveIntent{}, err
	}
	observed, err := m.observeEFI("resolving trial "+trialID, candidate)
	if err != nil {
		return bootMoveIntent{}, err
	}

	failure, verificationErr := resolveFailedTrial(m.root, selection, candidate, running, runningEntry, bootFull, false)
	if verificationErr != nil {
		return bootMoveIntent{}, fmt.Errorf("resolve trial %s: %w", trialID, verificationErr)
	}
	desired := failure.selection
	desired.Revision = selection.Revision + 1
	desired.SpentRecoveryTrialID = trialID
	desired.UpdatedAt = m.timestamp()
	change := failedTrialStatus(status, unconfirmedTrialReason)
	change.At = desired.UpdatedAt
	// Spent from the start: the attempt already happened, so nothing reboots.
	reboot := bootMoveReboot{TrialID: new(trialID), Eligible: new(true), Spent: new(true)}
	intent, err := m.newIntent("unconfirmed-"+trialID, selection, desired, observed, running,
		candidate, failure.destination, "", []bootStatusChange{change}, reboot)
	if err != nil {
		return bootMoveIntent{}, err
	}
	if _, err := m.checkStatusChanges(intent); err != nil {
		return bootMoveIntent{}, fmt.Errorf("resolve trial %s: %w", trialID, err)
	}
	return intent, nil
}

func moveSelectionReadError(id string, err error) error {
	return fmt.Errorf("cannot read boot selection for generation %s: %w. Boot state is unchanged. Restore access to intact boot state and retry", id, err)
}

func (m *bootMover) readMoveGeneration(id string) (GenerationSpec, GenerationStatus, error) {
	spec, status, err := ReadGeneration(m.root, id)
	if err != nil {
		return GenerationSpec{}, GenerationStatus{}, fmt.Errorf("cannot read generation %s: %w. Boot state is unchanged. Restore access to the generation record and retry", id, err)
	}
	return spec, status, nil
}

func failedTrialStatus(status GenerationStatus, reason string) bootStatusChange {
	return bootStatusChange{
		GenerationID: status.GenerationID, Reason: reason,
		CommitState: status.CommitState, BootState: BootStateFailed, HealthState: HealthStateUnhealthy,
		UnavailableReason: new(status.UnavailableReason),
	}
}

type failedTrialResolution struct {
	selection   BootSelectionRecord
	destination string
	rejected    []failedTrialRejection
}

type failedTrialRejection struct {
	generationID string
	verdict      bootability
}

type failedTrialVerificationError struct {
	generationID string
	cause        error
}

func (e *failedTrialVerificationError) Error() string {
	return fmt.Sprintf("could not verify generation %s: %v. The trial stays pending and the boot target is unchanged. Restore read access to the path or device in the error and retry. Rebooting retries reconciliation. If the error persists, preserve the journal, then boot a healthy generation from the boot menu, or reinstall the node", e.generationID, e.cause)
}

func (e *failedTrialVerificationError) Unwrap() error { return e.cause }

// resolveFailedTrial chooses the first usable recorded target. An unavailable
// target permits another check, while an indeterminate target keeps the trial
// pending. Without a usable target, the owner can keep only the running boot.
func resolveFailedTrial(root string, selection BootSelectionRecord, candidate, running, runningEntry string,
	level bootabilityLevel, recordedFallbackOnly bool) (failedTrialResolution, *failedTrialVerificationError) {
	fallback := strings.TrimSpace(selection.PreviousKnownGoodGenerationID)
	if (selection.OneShot || fallback == "") && selection.DefaultGenerationID != candidate {
		fallback = strings.TrimSpace(selection.DefaultGenerationID)
		selection.PreviousKnownGoodGenerationID = fallback
		selection.PreviousKnownGoodBootEntry = selection.DefaultBootEntry
	}
	recordedEntry := recoveryFallbackEntry(selection, fallback)
	targets := []string{fallback}
	if !recordedFallbackOnly {
		def := strings.TrimSpace(selection.DefaultGenerationID)
		if def != candidate && def != fallback {
			targets = append(targets, def)
		}
		if running != candidate && running != fallback && running != def {
			targets = append(targets, running)
		}
	}

	var result failedTrialResolution
	check := bootabilityCheck{root: root}
	for _, id := range targets {
		if id == "" {
			continue
		}

		var entryBinding *string
		if id == fallback {
			entryBinding = &recordedEntry
		}
		verdict := bootability{state: bootUnavailable, evidence: "fallback is the failed generation"}
		var entry string
		if id != candidate {
			entry, verdict = check.knownGoodFallback(id, entryBinding, level)
		}
		if verdict.state == bootIndeterminate {
			return result, &failedTrialVerificationError{generationID: id, cause: verdict.problem()}
		}
		if verdict.state != bootable {
			result.rejected = append(result.rejected, failedTrialRejection{generationID: id, verdict: verdict})
			continue
		}

		result.destination = id
		// The legacy writer preserves the exact values already in the record,
		// including an omitted previous-known-good entry for the default.
		if recordedFallbackOnly {
			entry = selection.DefaultBootEntry
			if strings.TrimSpace(selection.PreviousKnownGoodBootEntry) != "" {
				entry = selection.PreviousKnownGoodBootEntry
			}
		} else {
			selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = id, entry
		}
		selection.DefaultGenerationID, selection.DefaultBootEntry = id, entry
		break
	}

	selection.RecoveryRequired = result.destination == ""
	if selection.RecoveryRequired {
		selection.DefaultGenerationID, selection.DefaultBootEntry = running, runningEntry
		// Legacy clears the pointers only when it rejected a recorded fallback.
		if !recordedFallbackOnly || fallback != "" {
			selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "", ""
		}
	}
	selection.OneShot = false
	selection.BootedOneShot = false
	selection.FailedBootGenerationID = candidate
	selection.TargetBootGenerationID, selection.TargetBootEntry = "", ""
	selection.TrialGenerationID, selection.TrialBootEntry = "", ""
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = ""
	selection.BootCountedTrialPath = ""
	result.selection = selection
	return result, nil
}

// complete carries a durable intent from its first unfinished step. It
// returns the intent when it remains as a trial authority.
func (m *bootMover) complete(intent *bootMoveIntent, resolvingTrial bool) (bootMoveResult, *bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, nil, err
	}
	// The revision is checked before any step, so a conflicting writer
	// leaves status, EFI and reboot eligibility untouched.
	current, err := ReadBootSelection(m.root)
	if err != nil {
		return bootMoveResult{}, nil, moveSelectionReadError(running, err)
	}
	published := current == intent.DesiredSelection
	if !published && current.Revision != *intent.ExpectedSelectionRevision {
		return bootMoveResult{}, nil, fmt.Errorf("%w: boot selection revision %d does not match operation %s for generation %s, which expects %d. Boot state is unchanged. From the node console, preserve %s and %s for diagnosis",
			ErrBootMoveBlocked, current.Revision, intent.OperationID, running, *intent.ExpectedSelectionRevision, bootMovePath, bootStateDir+"/selection.json")
	}
	if published && resolvingTrial && intent.isTrialAuthority() {
		return bootMoveResult{}, intent, nil
	}
	if err := m.projectStatus(*intent); err != nil {
		return bootMoveResult{}, nil, err
	}
	if err := m.syncEFI(*intent); err != nil {
		return bootMoveResult{}, nil, err
	}

	authority := intent.isTrialAuthority()
	spent := false
	if !authority && *intent.Reboot.Eligible && !*intent.Reboot.Spent {
		// The spend is durable before any reboot is requested, so a retry
		// after this point never requests a second one.
		*intent.Reboot.Spent = true
		intent.DesiredSelection.SpentRecoveryTrialID = *intent.Reboot.TrialID
		if err := writeBootMoveIntent(m.root, *intent); err != nil {
			return bootMoveResult{}, nil, err
		}
		spent = true
		if err := m.reached(stepRebootSpent); err != nil {
			return bootMoveResult{}, nil, err
		}
	}

	if !published {
		if err := WriteBootSelection(m.root, intent.DesiredSelection); err != nil {
			return bootMoveResult{}, nil, err
		}
		if err := m.reached(stepPublished); err != nil {
			return bootMoveResult{}, nil, err
		}
	}
	if authority {
		return bootMoveResult{}, intent, nil
	}
	if err := removeBootMoveIntent(m.root); err != nil {
		return bootMoveResult{}, nil, err
	}
	if err := m.reached(stepResolved); err != nil {
		return bootMoveResult{}, nil, err
	}
	reboot := spent && m.bootID == intent.BootID && running != intent.nextGenerationID()
	return bootMoveResult{Resolved: true, RebootRequested: reboot}, nil, nil
}

func (m *bootMover) projectStatus(intent bootMoveIntent) error {
	for _, change := range intent.StatusChanges {
		spec, status, err := ReadGeneration(m.root, change.GenerationID)
		if err != nil {
			return fmt.Errorf("project operation %s status: %w", intent.OperationID, err)
		}
		if carriesOperation(status, intent.OperationID) {
			// Admission refuses a reused ID, so this is this move's own change.
			continue
		}
		if err := WriteGenerationStatus(m.root, spec, projectStatusChange(status, change, intent.OperationID)); err != nil {
			return fmt.Errorf("project operation %s status: %w", intent.OperationID, err)
		}
		if err := m.reached(stepStatusWritten); err != nil {
			return err
		}
	}
	return nil
}

func projectStatusChange(status GenerationStatus, change bootStatusChange, operationID string) GenerationStatus {
	at := change.At.UTC()
	if change.CommitState == CommitStateCommitted && status.CommittedAt == nil {
		status.CommittedAt = &at
		status.CommittedByOperation = operationID
	}
	status.CommitState = change.CommitState
	status.BootState = change.BootState
	status.HealthState = change.HealthState
	status.UnavailableReason = *change.UnavailableReason
	status.UpdatedAt = at
	status.StatusTransitions = append(slices.Clone(status.StatusTransitions), StatusTransition{
		At: at, OperationID: operationID, Reason: change.Reason,
		CommitState: change.CommitState, BootState: change.BootState, HealthState: change.HealthState,
	})
	return status
}

// syncEFI writes the projection in a fixed order and verifies it by reading
// both variables back. An unreadable variable never counts as a match.
func (m *bootMover) syncEFI(intent bootMoveIntent) error {
	issuing := m.bootID == intent.BootID
	if observed, err := m.readEFI(m.root); err == nil && intent.efiMatches(observed, issuing) {
		return m.reached(stepReadBack)
	}
	if m.setDefault == nil || m.setOneShot == nil {
		return fmt.Errorf("operation %s needs EFI boot variable writers", intent.OperationID)
	}
	if err := m.setOneShot(m.root, ""); err != nil {
		return fmt.Errorf("operation %s: clear EFI one-shot: %w", intent.OperationID, err)
	}
	if err := m.reached(stepOneShotCleared); err != nil {
		return err
	}
	if err := m.setDefault(m.root, intent.DesiredSelection.DefaultBootEntry); err != nil {
		return fmt.Errorf("operation %s: set EFI default: %w", intent.OperationID, err)
	}
	if err := m.reached(stepDefaultSet); err != nil {
		return err
	}
	if issuing && *intent.DesiredOneShot.Entry != "" {
		selection := intent.DesiredSelection
		path := cmp.Or(selection.TargetBootEntry, selection.TrialBootEntry, selection.DefaultBootEntry)
		if err := m.setOneShot(m.root, path); err != nil {
			return fmt.Errorf("operation %s: set EFI one-shot: %w", intent.OperationID, err)
		}
		if err := m.reached(stepOneShotSet); err != nil {
			return err
		}
	}
	observed, err := m.readEFI(m.root)
	if err != nil {
		return fmt.Errorf("operation %s: EFI read-back is indeterminate: %w. Restore access to efivarfs and retry", intent.OperationID, err)
	}
	if !intent.efiMatches(observed, issuing) {
		return fmt.Errorf("operation %s: EFI read-back found default %q and one-shot %q, not the recorded targets. Check the firmware's EFI variable support and retry", intent.OperationID, observed.Default, observed.OneShot)
	}
	return m.reached(stepReadBack)
}

// efiMatches compares observed variables with the projection for this boot.
// After the issuing boot ends, the desired one-shot counts as delivered.
func (intent bootMoveIntent) efiMatches(observed efiboot.Entries, issuing bool) bool {
	if entry := *intent.DesiredDefault.Entry; entry != "" && observed.Default != entry {
		return false
	}
	oneShot := *intent.DesiredOneShot.Entry
	if issuing {
		return observed.OneShot == oneShot
	}
	return observed.OneShot == "" || observed.OneShot == oneShot
}

// isTrialAuthority reports whether the intent arms recovery and so remains
// after publication until a move resolves it.
func (intent bootMoveIntent) isTrialAuthority() bool {
	selection := intent.DesiredSelection
	destination := *intent.DestinationGenerationID
	return *intent.Reboot.Eligible && !*intent.Reboot.Spent && selection.PendingHealthValidation &&
		destination != "" && destination == cmp.Or(selection.TrialGenerationID, selection.TargetBootGenerationID)
}

// sameMoveSelection compares the fields a request controls.
func sameMoveSelection(published, requested BootSelectionRecord) bool {
	published.Revision, published.SpentRecoveryTrialID, published.UpdatedAt = 0, "", time.Time{}
	requested.Revision, requested.SpentRecoveryTrialID, requested.UpdatedAt = 0, "", time.Time{}
	requested.APIVersion, requested.Kind = APIVersion, BootSelectionKind
	return published == requested
}

func writeBootMoveIntent(root string, intent bootMoveIntent) error {
	if err := intent.validate(); err != nil {
		return fmt.Errorf("write boot move intent: %w", err)
	}
	data, err := marshalRecordEnvelope(bootMoveRecordType, intent)
	if err != nil {
		return fmt.Errorf("marshal boot move intent: %w", err)
	}
	if err := writeFileAtomic(rootedPathUnchecked(root, bootMovePath), data, 0o600); err != nil {
		return fmt.Errorf("write boot move intent: %w", err)
	}
	return nil
}

// removeBootMoveIntent resolves the intent. The directory sync makes the
// removal durable before the caller may overwrite a slot or reboot.
func removeBootMoveIntent(root string) error {
	path := rootedPathUnchecked(root, bootMovePath)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve boot move intent: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("resolve boot move intent: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("resolve boot move intent: %w", err)
	}
	return nil
}
