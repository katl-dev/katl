package generation

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootid"
	"github.com/katl-dev/katl/internal/efiboot"
	"github.com/katl-dev/katl/internal/kernelcmdline"
	"github.com/katl-dev/katl/internal/persistedrecord"
	"github.com/katl-dev/katl/internal/releaseversion"
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
	trialNone     trialEffect = iota // grants no authority
	trialArm                         // arms a trial named by the operation ID
	trialPromote                     // resolves the bound trial without a reboot
	trialFail                        // resolves the bound trial and may spend its reboot
	trialWithdraw                    // replaces an authority whose candidate has not booted
)

// reconcileMode bounds what reconciliation may do for its entry point.
type reconcileMode uint8

const (
	// reconcileRestricted is activation's. Its sandbox can neither write EFI
	// variables nor open a root partition, so it defers any step that needs one.
	reconcileRestricted reconcileMode = iota
	// reconcileFull completes every step except spending a trial's reboot.
	reconcileFull
	// reconcileFailing is FailTrial's. Only the recovery and deadline units turn
	// a requested reboot into one, so only they may spend the trial's reboot.
	reconcileFailing
)

// errBootMoveDeferred marks a step that activation leaves to boot health.
var errBootMoveDeferred = errors.New("deferred")

// ErrTrialResolving reports a failed trial whose move stopped before it spent
// the trial's reboot on the boot that issued it.
var ErrTrialResolving = fmt.Errorf("%w: failed trial", ErrBootMoveBlocked)

func deferral(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBootMoveDeferred, fmt.Sprintf(format, args...))
}

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
	// Completed names an earlier operation that this call carried to resolution.
	Completed string
}

// BootMover is the only writer of boot moves: the intent, the generation
// status it projects, the EFI variables and the published selection.
type BootMover struct {
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

// BootDefaultSetter writes one systemd-boot EFI variable for a loader entry.
type BootDefaultSetter func(root string, bootEntry string) error

// BootMoverOptions describes the running boot. The zero value observes the
// running kernel and writes the firmware through bootctl.
type BootMoverOptions struct {
	BootID      string // default: the kernel boot ID
	CommandLine string // default: the kernel command line
	Now         func() time.Time
	ReadEFI     func(root string) (efiboot.Entries, error)
	SetDefault  BootDefaultSetter
	SetOneShot  BootDefaultSetter
}

// NewBootMover returns the boot-state owner for root and the running boot.
func NewBootMover(root string, options BootMoverOptions) (*BootMover, error) {
	root = cleanRoot(root)
	if options.BootID == "" {
		id, err := bootid.Read(root)
		if err != nil {
			return nil, err
		}
		options.BootID = id
	}
	if options.CommandLine == "" {
		fields, err := kernelcmdline.ReadCurrent(root)
		if err != nil {
			return nil, fmt.Errorf("read kernel command line: %w", err)
		}
		options.CommandLine = strings.Join(fields, " ")
	}
	mover := &BootMover{
		root: root, bootID: options.BootID, commandLine: options.CommandLine,
		now: options.Now, readEFI: options.ReadEFI, setDefault: options.SetDefault, setOneShot: options.SetOneShot,
	}
	if mover.now == nil {
		mover.now = time.Now
	}
	if mover.readEFI == nil {
		mover.readEFI = efiboot.Reader{}.Read
	}
	if mover.setDefault == nil {
		mover.setDefault = efiboot.SetDefault
	}
	if mover.setOneShot == nil {
		mover.setOneShot = efiboot.SetOneShot
	}
	return mover, nil
}

// timestamp drops the monotonic reading so a published time compares equal
// to the same time decoded from disk.
func (m *BootMover) timestamp() time.Time {
	return m.now().UTC().Round(0)
}

func (m *BootMover) reached(step bootMoveStep) error {
	if m.checkpoint == nil {
		return nil
	}
	return m.checkpoint(step)
}

// reconcile completes or resolves any outstanding move for the running boot.
func (m *BootMover) reconcile() (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		var err error
		result, _, err = m.reconcileLocked(reconcileFull)
		return err
	})
	return result, err
}

// move admits a request after reconciliation and carries it through the protocol.
func (m *BootMover) move(request bootMoveRequest) (bootMoveResult, error) {
	return m.moveWith(func(BootSelectionRecord, *bootMoveIntent) (*bootMoveRequest, error) {
		return &request, nil
	})
}

// moveWith reconciles, then builds the request from the selection and
// authority read under the same lock. A nil request moves nothing.
func (m *BootMover) moveWith(build func(BootSelectionRecord, *bootMoveIntent) (*bootMoveRequest, error)) (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		var err error
		result, err = m.moveWithLocked(build)
		return err
	})
	return result, err
}

func (m *BootMover) moveWithLocked(build func(BootSelectionRecord, *bootMoveIntent) (*bootMoveRequest, error)) (bootMoveResult, error) {
	reconciled, authority, err := m.reconcileLocked(reconcileFull)
	if err != nil {
		return bootMoveResult{}, err
	}
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, err
	}
	selection, err := readOwnerSelection(m.root, running)
	if err != nil {
		return bootMoveResult{}, err
	}
	request, err := build(selection, authority)
	if err != nil || request == nil {
		return reconciled, err
	}
	return m.moveLocked(*request, selection, authority, reconciled)
}

func (m *BootMover) moveLocked(request bootMoveRequest, selection BootSelectionRecord, authority *bootMoveIntent, reconciled bootMoveResult) (bootMoveResult, error) {
	if request.Trial == trialFail {
		return bootMoveResult{}, fmt.Errorf("trial failures must use FailTrial")
	}
	published, err := m.published(request, selection)
	if err != nil {
		return bootMoveResult{}, err
	}
	if published {
		return reconciled, nil
	}
	if reconciled.RebootRequested {
		return reconciled, fmt.Errorf("%w: a completed move requested a reboot; reboot before starting operation %s", ErrBootMoveBlocked, request.OperationID)
	}
	if selection.Revision != request.ExpectedRevision {
		return bootMoveResult{}, fmt.Errorf("boot selection revision is %d, not %d: another change moved the boot target. Read the boot state again before retrying", selection.Revision, request.ExpectedRevision)
	}
	// A status-less request for the selection that already holds wants the
	// state it would publish, so it is a repeat rather than a new move.
	if request.Trial == trialNone && len(request.StatusChanges) == 0 && sameMoveSelection(selection, request.Selection) {
		return bootMoveResult{Resolved: true}, nil
	}
	intent, err := m.admit(request, selection, authority)
	if err != nil {
		return bootMoveResult{}, err
	}
	return m.startMove(intent, reconcileFull)
}

// FailTrial records a health failure or deadline. Only a trial bound to this
// boot changes boot state; any other boot reports without changing it.
func (m *BootMover) FailTrial(reason string) (BootHealthResult, error) {
	var health BootHealthResult
	err := withBootStateLock(m.root, func() error {
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		failure, err := m.failTrialLocked(running, reason)
		if err != nil {
			return err
		}
		if health, err = m.healthResult(running, BootHealthFailure); err != nil {
			return err
		}
		health.RebootRequested = failure.result.RebootRequested
		health.ReportOnly = failure.reportOnly
		health.Failed = !failure.reportOnly
		health.FallbackProblem = failure.fallbackProblem
		return nil
	})
	return health, err
}

// failTrial is FailTrial at the level of the move protocol.
func (m *BootMover) failTrial(reason string) (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		failure, err := m.failTrialLocked(running, reason)
		result = failure.result
		if err == nil && failure.reportOnly {
			err = fmt.Errorf("failure needs a trial armed for running generation %s; this boot reports health without changing the boot target. Inspect 'katlctl node status'", running)
		}
		return err
	})
	return result, err
}

type trialFailure struct {
	result bootMoveResult
	// reportOnly reports that no trial is bound to this boot.
	reportOnly      bool
	fallbackProblem string
}

// failTrialLocked derives the same operation ID from every trigger, so any
// of them can finish an interrupted move. A boot with no bound trial changes
// nothing but a manual selection it consumed.
func (m *BootMover) failTrialLocked(running, reason string) (trialFailure, error) {
	reconciled, authority, err := m.reconcileLocked(reconcileFailing)
	if err != nil {
		return trialFailure{}, err
	}
	selection, err := readOwnerSelection(m.root, running)
	if err != nil {
		return trialFailure{}, moveSelectionReadError(running, err)
	}
	if authority == nil && selection.FailedBootGenerationID == running {
		return trialFailure{result: reconciled}, nil
	}
	if !m.boundTrial(authority, running) {
		result, err := m.clearConsumedSelection(selection, authority, running)
		return trialFailure{result: result, reportOnly: true}, err
	}

	operationID := "fail-" + *authority.Reboot.TrialID
	spec, status, err := m.readMoveGeneration(running)
	if err != nil {
		return trialFailure{}, err
	}
	failure, err := resolveFailedTrial(m.root, selection, running, running, spec.Boot.LoaderEntryPath)
	if err != nil {
		return trialFailure{}, fmt.Errorf("operation %s: %w", operationID, err)
	}
	request := bootMoveRequest{
		OperationID: operationID, ExpectedRevision: selection.Revision, Source: running, Destination: failure.destination,
		Selection: failure.selection, StatusChanges: []bootStatusChange{failedTrialStatus(status, reason)}, Trial: trialFail,
	}
	intent, err := m.admit(request, selection, authority)
	if err != nil {
		return trialFailure{}, err
	}
	// Record the decision for this boot before the move starts, so a late
	// health success on the same boot cannot promote the failed candidate.
	if err := writeCurrentBoot(m.root, currentBoot{GenerationID: running, BootID: m.bootID, FailedTrial: *authority.Reboot.TrialID}); err != nil {
		return trialFailure{}, err
	}
	result, err := m.startMove(intent, reconcileFailing)
	return trialFailure{result: result, fallbackProblem: failure.problem()}, err
}

// clearConsumedSelection retires a manual selection that this failed boot
// consumed. Both manual modes write LoaderEntryOneShot, so its presence
// protects a selection for a later boot of the same generation.
func (m *BootMover) clearConsumedSelection(selection BootSelectionRecord, authority *bootMoveIntent, running string) (bootMoveResult, error) {
	if authority != nil || selection.PendingHealthValidation || selection.TargetBootGenerationID != running {
		return bootMoveResult{}, nil
	}
	efi, err := m.readEFI(m.root)
	if err != nil {
		return bootMoveResult{}, fmt.Errorf("check EFI one-shot boot selection: %w", err)
	}
	if efi.OneShot != "" {
		return bootMoveResult{}, nil
	}
	// The consumed one-shot stays as evidence about the running boot: health
	// retries and activation of the default still need it.
	consumed := selection
	consumed.BootedOneShot = selection.BootedOneShot || (selection.OneShot && running != selection.DefaultGenerationID)
	consumed.TargetBootGenerationID, consumed.TargetBootEntry = "", ""
	consumed.OneShot = false
	return m.moveLocked(bootMoveRequest{
		OperationID: "health-" + m.bootID, ExpectedRevision: selection.Revision,
		Source: running, Destination: selection.DefaultGenerationID, Selection: consumed,
		OneShot: armedOneShot(consumed),
	}, selection, nil, bootMoveResult{})
}

// armedOneShot is the generation that a selection keeps LoaderEntryOneShot on.
func armedOneShot(selection BootSelectionRecord) string {
	return cmp.Or(selection.TargetBootGenerationID, selection.TrialGenerationID)
}

// startMove durably binds the decision before projecting any of its effects.
func (m *BootMover) startMove(intent bootMoveIntent, mode reconcileMode) (bootMoveResult, error) {
	if err := m.recordDefaultOwner(*intent.DesiredDefault.GenerationID); err != nil {
		return bootMoveResult{}, err
	}
	if err := writeBootMoveIntent(m.root, intent); err != nil {
		return bootMoveResult{}, err
	}
	if err := m.reached(stepIntentWritten); err != nil {
		return bootMoveResult{}, err
	}
	result, _, err := m.complete(&intent, false, mode)
	return result, err
}

// published reports whether an earlier attempt published this request: the
// next revision holds its selection and every generation it changes carries
// its operation ID. A different move with an equal selection is stale.
func (m *BootMover) published(request bootMoveRequest, selection BootSelectionRecord) (bool, error) {
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

func (m *BootMover) runningGenerationID() (string, error) {
	id, err := SelectedGenerationFromCommandLine(m.commandLine)
	if err != nil {
		return "", fmt.Errorf("cannot identify the running generation: %w. Boot state is unchanged. Preserve the journal, then boot a healthy generation from the boot menu, or reinstall the node", err)
	}
	return id, nil
}

func (m *BootMover) boundTrial(authority *bootMoveIntent, running string) bool {
	return authority != nil && m.bootID != authority.BootID && running == *authority.DestinationGenerationID
}

// reconcileLocked returns the trial authority that remains after completion.
func (m *BootMover) reconcileLocked(mode reconcileMode) (bootMoveResult, *bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, nil, err
	}
	intent, err := readBootMoveIntent(m.root)
	if err != nil {
		return bootMoveResult{}, nil, fmt.Errorf("boot state is unchanged for generation %s: %w", running, err)
	}
	if intent != nil {
		if intent, err = m.retireSuperseded(intent, running); err != nil {
			return bootMoveResult{}, nil, err
		}
	}
	var authority *bootMoveIntent
	var activated Record
	var result bootMoveResult
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
		// A resolution checks its inputs before any EFI writes. If the arm
		// already published, its old EFI projection need not be replayed first.
		resolving := mode == reconcileFailing || (m.bootID != intent.BootID && running != *intent.DestinationGenerationID)
		if result, authority, err = m.complete(intent, resolving, mode); err != nil || authority == nil {
			return result, authority, err
		}
	}
	if m.bootID == authority.BootID || running == *authority.DestinationGenerationID {
		return result, authority, nil
	}
	if mode == reconcileRestricted {
		return result, authority, deferral("trial %s ended without a health result; resolving it needs a full bootability check, which katl-boot-health.service or katl-boot-recovery.service runs", *authority.Reboot.TrialID)
	}
	unconfirmed, err := m.unconfirmedTrial(authority, running, activated.Boot.LoaderEntryPath)
	if err != nil {
		return bootMoveResult{}, authority, err
	}
	result, err = m.startMove(unconfirmed, mode)
	result.Completed = cmp.Or(result.Completed, unconfirmed.OperationID)
	return result, nil, err
}

func cannotReconcileBoot(running string, err error) error {
	return fmt.Errorf("cannot reconcile boot for generation %s: %w. Boot state is unchanged. Preserve the journal, then boot a healthy generation from the boot menu, or reinstall the node", running, err)
}

// admit builds the intent for a request and checks it before it is durable.
func (m *BootMover) admit(request bootMoveRequest, selection BootSelectionRecord, authority *bootMoveIntent) (bootMoveIntent, error) {
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
	case trialWithdraw:
		// Once the candidate has booted under the authority, only its health
		// result may resolve the trial.
		if authority == nil || m.boundTrial(authority, running) {
			return bootMoveIntent{}, fmt.Errorf("operation %s withdraws only a trial whose candidate has not booted", request.OperationID)
		}
	default:
		if authority != nil {
			return bootMoveIntent{}, m.pendingTrialError(authority, "operation "+request.OperationID)
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

func (m *BootMover) observeEFI(operation, generationID string) (efiboot.Entries, error) {
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
// It never refuses for the releases that might read the selection next:
// publishSelection writes a form they decode.
func (m *BootMover) newIntent(operationID string, selection, desired BootSelectionRecord, observed efiboot.Entries,
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
func (m *BootMover) observedTarget(id, entry string) *bootMoveTarget {
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
func (m *BootMover) checkTargets(intent bootMoveIntent, running string) error {
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

func (m *BootMover) checkStatusChanges(intent bootMoveIntent) (map[string]GenerationStatus, error) {
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
// classification. It writes nothing and never refuses: a rollback target may
// run a reader-only release, which refuses any intent it cannot complete, so
// the authority becomes durable only in the intent of the move that resolves it.
func (m *BootMover) importLegacyTrial(running string) (*bootMoveIntent, error) {
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
	selection, err := readOwnerSelection(m.root, running)
	if err != nil {
		return nil, moveSelectionReadError(running, err)
	}
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
func (m *BootMover) unconfirmedTrial(authority *bootMoveIntent, running, runningEntry string) (bootMoveIntent, error) {
	candidate := *authority.DestinationGenerationID
	trialID := *authority.Reboot.TrialID
	selection, err := readOwnerSelection(m.root, running)
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

	failure, err := resolveFailedTrial(m.root, selection, candidate, running, runningEntry)
	if err != nil {
		return bootMoveIntent{}, fmt.Errorf("resolve trial %s: %w", trialID, err)
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

func (m *BootMover) readMoveGeneration(id string) (GenerationSpec, GenerationStatus, error) {
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
// The selection records the running boot, which a later activation observes.
func resolveFailedTrial(root string, selection BootSelectionRecord, candidate, running, runningEntry string) (failedTrialResolution, error) {
	fallback := selection.PreviousKnownGoodGenerationID
	if (selection.OneShot || fallback == "") && selection.DefaultGenerationID != candidate {
		fallback = selection.DefaultGenerationID
		selection.PreviousKnownGoodGenerationID = fallback
		selection.PreviousKnownGoodBootEntry = selection.DefaultBootEntry
	}
	recordedEntry := recoveryFallbackEntry(selection, fallback)
	targets := []string{fallback}
	def := selection.DefaultGenerationID
	if def != candidate && def != fallback {
		targets = append(targets, def)
	}
	if running != candidate && running != fallback && running != def {
		targets = append(targets, running)
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
			entry, verdict = check.knownGoodFallback(id, entryBinding, bootFull)
		}
		if verdict.state == bootIndeterminate {
			return result, &failedTrialVerificationError{generationID: id, cause: verdict.problem()}
		}
		if verdict.state != bootable {
			result.rejected = append(result.rejected, failedTrialRejection{generationID: id, verdict: verdict})
			continue
		}

		result.destination = id
		selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = id, entry
		selection.DefaultGenerationID, selection.DefaultBootEntry = id, entry
		break
	}

	selection.RecoveryRequired = result.destination == ""
	if selection.RecoveryRequired {
		selection.DefaultGenerationID, selection.DefaultBootEntry = running, runningEntry
		selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "", ""
	}
	selection.BootedGenerationID, selection.BootedBootEntry = running, runningEntry
	selection.ActiveGenerationID = running
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
func (m *BootMover) complete(intent *bootMoveIntent, resolvingTrial bool, mode reconcileMode) (bootMoveResult, *bootMoveIntent, error) {
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
	published, awaiting := intent.progress(current)
	if !published && !awaiting {
		return bootMoveResult{}, nil, fmt.Errorf("%w: boot selection revision %d does not match operation %s for generation %s, which expects %d. Boot state is unchanged. From the node console, preserve %s and %s for diagnosis",
			ErrBootMoveBlocked, current.Revision, intent.OperationID, running, *intent.ExpectedSelectionRevision, bootMovePath, bootStateDir+"/selection.json")
	}
	if published && resolvingTrial && intent.isTrialAuthority() {
		return bootMoveResult{}, intent, nil
	}
	authority := intent.isTrialAuthority()
	if spend := !authority && *intent.Reboot.Eligible && !*intent.Reboot.Spent; spend && mode != reconcileFailing {
		// On the issuing boot the reboot is still owed, and only the recovery
		// and deadline units reboot. A later boot has already left the trial.
		if mode == reconcileRestricted {
			return bootMoveResult{}, nil, deferral("the failed trial of generation %s is resolved by katl-boot-health.service or katl-boot-recovery.service", intent.SourceGenerationID)
		}
		if m.bootID == intent.BootID {
			return bootMoveResult{}, nil, fmt.Errorf("%w: a failed trial of generation %s is being resolved; katl-boot-recovery.service finishes it, or run 'katlctl node reboot <node>'", ErrTrialResolving, intent.SourceGenerationID)
		}
	}
	// One observation decides both the deferral and the EFI writes, so a
	// deferred move leaves every file and a later failed read cannot defer
	// after status was projected.
	observed, readErr := m.readEFI(m.root)
	synced := readErr == nil && intent.efiMatches(observed, m.bootID == intent.BootID)
	if mode == reconcileRestricted && !synced {
		return bootMoveResult{}, nil, deferral("operation %s needs EFI boot variable writes, which katl-boot-health.service or katl-boot-recovery.service makes", intent.OperationID)
	}
	if err := m.projectStatus(*intent); err != nil {
		return bootMoveResult{}, nil, err
	}
	if err := m.syncEFI(*intent, synced); err != nil {
		return bootMoveResult{}, nil, err
	}

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
		if err := publishSelection(m.root, running, intent.DesiredSelection); err != nil {
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
	return bootMoveResult{Resolved: true, RebootRequested: reboot, Completed: intent.OperationID}, nil, nil
}

func (m *BootMover) projectStatus(intent bootMoveIntent) error {
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

// syncEFI writes the intent's EFI targets in a fixed order, unless the
// caller's observation already matched them, and verifies the writes by
// reading both variables back. An unreadable variable never counts as a
// match. Restricted callers defer before reaching it.
func (m *BootMover) syncEFI(intent bootMoveIntent, synced bool) error {
	issuing := m.bootID == intent.BootID
	if synced {
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
	if err := persistedrecord.WriteFileAtomic(rootedPathUnchecked(root, bootMovePath), data, 0o600); err != nil {
		return fmt.Errorf("write boot move intent: %w", err)
	}
	return nil
}

// removeBootMoveIntent resolves the intent. The directory sync makes the
// removal durable before the caller may overwrite a slot or reboot.
func removeBootMoveIntent(root string) error {
	if err := persistedrecord.RemoveFile(rootedPathUnchecked(root, bootMovePath)); err != nil {
		return fmt.Errorf("resolve boot move intent: %w", err)
	}
	return nil
}

// pendingTrialError refuses a move while a trial holds the boot target. On the
// candidate's own boot a reboot would end the trial, so the guidance differs.
func (m *BootMover) pendingTrialError(authority *bootMoveIntent, action string) error {
	candidate := *authority.DestinationGenerationID
	if running, err := m.runningGenerationID(); err == nil && m.boundTrial(authority, running) {
		return fmt.Errorf("%w: generation %s is in its boot trial and boot health has not decided it; wait for 'katlctl node status <node>' to report the result, then retry %s", ErrBootMoveBlocked, candidate, action)
	}
	return fmt.Errorf("%w: a boot trial of generation %s is pending; run 'katlctl node reboot <node>' to try it, then retry %s once boot health has decided it", ErrBootMoveBlocked, candidate, action)
}

// FirstBootMoveReader is the first KatlOS release that reads boot-move records.
var FirstBootMoveReader = releaseversion.Version{Year: 2026, Month: 11, Patch: 6}

// ReadsBootMoves reports whether a runtime can read boot-move records. A final
// release or an alpha, beta or rc candidate is judged by its base against
// FirstBootMoveReader; any -dev.N build or version outside the release grammar
// counts as a reader. A development stamp does not identify the code a build
// contains: depending on the build path it comes from the calendar month or
// from the release tags reachable on the built branch.
func ReadsBootMoves(runtimeVersion string) bool {
	version, err := releaseversion.Parse(runtimeVersion)
	if err != nil || version.Stage == releaseversion.Dev {
		return true
	}
	return version.Base().Compare(FirstBootMoveReader) >= 0
}

// legacyReaders reports whether a generation other than the running one is
// bootable, by a loader entry the boot menu offers, and runs a release that
// cannot decode boot-move fields in the selection. Every doubt counts towards
// such a reader: an unreadable record is one, and without a readable entries
// directory every record counts as bootable. A directory without a record is
// not a generation yet.
func legacyReaders(root, running string) bool {
	records, err := os.ReadDir(rootedPathUnchecked(root, GenerationRecordsDir))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	entries, entriesErr := os.ReadDir(rootedPathUnchecked(root, loaderEntriesDir))
	for _, record := range records {
		id := record.Name()
		if !record.IsDir() || strings.HasPrefix(id, ".") || id == running {
			continue
		}
		spec, _, err := ReadGeneration(root, id)
		if err != nil {
			if recordless(root, id) {
				continue
			}
			return true
		}
		if ReadsBootMoves(spec.RuntimeVersion) {
			continue
		}
		if entriesErr != nil || slices.ContainsFunc(entries, func(entry os.DirEntry) bool {
			return isLoaderEntryFile(spec.Boot.LoaderEntryPath, entry.Name())
		}) {
			return true
		}
	}
	return false
}

// recordless reports whether a generation directory holds neither spec.json
// nor metadata.json, as a candidate does while it is staged and after an
// interrupted apply. A directory without a record has no loader entry, so no
// release boots it. The prepared host-upgrade handoff writes its candidate's
// entry before the directory exists, then renames the directory in as a
// complete tree (#1084). The installer
// writes its target's entry first, but publishes no selection until that
// record exists.
func recordless(root, id string) bool {
	dir := rootedPathUnchecked(root, filepath.Join(GenerationRecordsDir, id))
	for _, name := range []string{"spec.json", "metadata.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// lowerSelection is the v1 form that every released runtime decodes:
// omitempty drops both fields.
func lowerSelection(s BootSelectionRecord) BootSelectionRecord {
	s.Revision, s.SpentRecoveryTrialID = 0, ""
	return s
}

// selectionToken derives a revision from a selection's v1 content, for the
// form that carries none. Bit 63 marks a token and bit 62 stays clear, so the
// next revision of a token is still a token.
func selectionToken(s BootSelectionRecord) uint64 {
	data, err := MarshalCanonicalJSON(lowerSelection(s))
	if err != nil {
		// A BootSelectionRecord always marshals.
		panic(fmt.Sprintf("marshal boot selection: %v", err))
	}
	sum := sha256.Sum256(data)
	return binary.BigEndian.Uint64(sum[:8])&^(1<<62) | 1<<63
}

func isSelectionToken(revision uint64) bool {
	return revision&(1<<63) != 0
}

// readOwnerSelection is the owner's view of the published selection. While
// legacyReaders holds, the selection is in its v1 form, so its revision is
// the content token and it records no spent trial.
func readOwnerSelection(root, running string) (BootSelectionRecord, error) {
	selection, err := ReadBootSelection(root)
	if err != nil || !legacyReaders(root, running) {
		return selection, err
	}
	lowered := lowerSelection(selection)
	lowered.Revision = selectionToken(lowered)
	return lowered, nil
}

// publishSelection writes the selection in the form every generation that
// could boot next decodes. A token revision was derived from the v1 form, so
// a selection built on one stays in that form even when legacyReaders no
// longer holds: written as a counter, the token would block every later move.
func publishSelection(root, running string, s BootSelectionRecord) error {
	if isSelectionToken(s.Revision) || legacyReaders(root, running) {
		s = lowerSelection(s)
	}
	return WriteBootSelection(root, s)
}

// progress reports whether the published selection already holds the
// intent's desired selection, or still holds the selection it expects. The
// intent's own form decides the test: a token-form intent expects content.
func (intent bootMoveIntent) progress(published BootSelectionRecord) (done, awaiting bool) {
	desired := intent.DesiredSelection
	done = published == desired || published == lowerSelection(desired)
	expected := *intent.ExpectedSelectionRevision
	if isSelectionToken(expected) {
		return done, selectionToken(published) == expected
	}
	return done, published.Revision == expected
}

// retireSuperseded removes a token-form intent whose expected selection a
// runtime without the owner replaced: that runtime booted and decided the
// boot, so the move no longer applies. It returns the intent that remains.
//
// A superseded intent is reached only after that runtime resolved the trial
// or left it alone. KatlOS 2026.11.5 cannot rewrite a selection whose trial
// is pending for another generation: its activation and its failure path both
// refuse a boot outside the pending target, so it changes the content only
// once no trial of the candidate is pending.
func (m *BootMover) retireSuperseded(intent *bootMoveIntent, running string) (*bootMoveIntent, error) {
	if !isSelectionToken(*intent.ExpectedSelectionRevision) {
		return intent, nil
	}
	published, err := ReadBootSelection(m.root)
	if err != nil {
		return nil, moveSelectionReadError(running, err)
	}
	if done, awaiting := intent.progress(published); done || awaiting {
		return intent, nil
	}
	if err := removeBootMoveIntent(m.root); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "retired boot move %s: an earlier release replaced the selection it expected\n", intent.OperationID)
	return nil, nil
}

// problem describes the first recorded target the failure rule rejected.
func (resolution failedTrialResolution) problem() string {
	if len(resolution.rejected) == 0 {
		return ""
	}
	rejected := resolution.rejected[0]
	return fmt.Sprintf("generation %s was not used as the fallback: %v. Check 'katlctl node generations list' and restore its boot artifacts", rejected.generationID, rejected.verdict.problem())
}

func (m *BootMover) healthResult(running, result string) (BootHealthResult, error) {
	selection, err := readOwnerSelection(m.root, running)
	if err != nil {
		return BootHealthResult{}, moveSelectionReadError(running, err)
	}
	_, status, err := m.readMoveGeneration(running)
	if err != nil {
		return BootHealthResult{}, err
	}
	return BootHealthResult{
		GenerationID: running, Result: result, KnownGood: IsKnownGood(status),
		RecoveryRequired:  selection.RecoveryRequired,
		DefaultGeneration: selection.DefaultGenerationID, NextBootGeneration: selection.NextBootGenerationID(),
		BootDefaultEntry: selection.DefaultBootEntry,
	}, nil
}

// ActivationOutcome reports what activation did to boot state.
type ActivationOutcome struct {
	// TrialBound reports that this boot runs the candidate of an armed trial.
	TrialBound bool
	// Reconciled names the operation that activation completed.
	Reconciled string
	// ReconcileErr is a step activation deferred, or another reconcile
	// error. Neither fails activation of an accepted generation.
	ReconcileErr error
}

// Deferred reports whether the reconcile error is a step left to boot health.
func (outcome ActivationOutcome) Deferred() bool {
	return errors.Is(outcome.ReconcileErr, errBootMoveDeferred)
}

// Activate applies the kernel-selected generation. It reconciles within the
// activation sandbox, accepts the running generation against any outstanding
// move, and records the booted generation when no move is outstanding.
func (m *BootMover) Activate(id string) (ActivationPlan, ActivationOutcome, error) {
	var plan ActivationPlan
	var outcome ActivationOutcome
	err := withBootStateLock(m.root, func() error {
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		if id != running {
			return fmt.Errorf("selected generation %s does not match kernel generation %s", id, running)
		}
		boot, activated, err := readCurrentBoot(m.root)
		if err != nil {
			return err
		}
		result, authority, reconcileErr := m.reconcileLocked(reconcileRestricted)
		outcome.Reconciled, outcome.ReconcileErr = result.Completed, reconcileErr

		intent, err := readBootMoveIntent(m.root)
		if err != nil {
			return err
		}
		var record Record
		switch {
		case intent != nil:
			authority = nil
			if intent.isTrialAuthority() {
				authority = intent
			}
			record, err = intent.activationRecord(m.root, running, m.commandLine)
		case authority != nil:
			record, err = authority.activationRecord(m.root, running, m.commandLine)
		default:
			record, err = m.observe(running, !activated)
		}
		if err != nil {
			return err
		}
		outcome.TrialBound = m.boundTrial(authority, running)

		if plan, err = ApplyActivation(m.root, record); err != nil {
			return err
		}
		// A repeat activation keeps the failure this boot already decided.
		if boot.BootID != m.bootID || boot.GenerationID != running {
			boot = currentBoot{GenerationID: running}
		}
		return writeCurrentBoot(m.root, boot)
	})
	return plan, outcome, err
}

// observe records the booted generation in the published selection. It
// publishes one file and touches neither EFI nor generation status.
func (m *BootMover) observe(running string, firstActivation bool) (Record, error) {
	record, err := plainActivationRecord(m.root, running)
	if err != nil {
		return Record{}, err
	}
	selection, err := readOwnerSelection(m.root, running)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return Record{}, err
	}
	spec, status, err := ReadGeneration(m.root, running)
	if err != nil {
		return Record{}, err
	}
	observed, err := observedBoot(selection, spec, running, m.commandLine, IsKnownGood(status), firstActivation)
	if err != nil {
		return Record{}, fmt.Errorf("record booted generation: %w", err)
	}
	if observed == selection {
		return record, nil
	}
	observed.Revision = selection.Revision + 1
	observed.UpdatedAt = m.timestamp()
	if err := publishSelection(m.root, running, observed); err != nil {
		return Record{}, fmt.Errorf("record booted generation: %w", err)
	}
	return record, nil
}

// ActivateGeneration applies an explicitly named generation without kernel
// evidence, for tests and target preparation. It writes no boot state.
func ActivateGeneration(root, id string) (ActivationPlan, error) {
	root = cleanRoot(root)
	var plan ActivationPlan
	err := withBootStateLock(root, func() error {
		intent, err := readBootMoveIntent(root)
		if err != nil {
			return err
		}
		if intent != nil {
			return fmt.Errorf("%w (operation %s): activate the generation the kernel selected", ErrBootMoveBlocked, intent.OperationID)
		}
		record, err := plainActivationRecord(root, id)
		if err != nil {
			return err
		}
		plan, err = ApplyActivation(root, record)
		return err
	})
	return plan, err
}

// bootOwnersDir records each generation whose runtime runs this owner, so a
// trial can tell whether its fallback completes the records it leaves. Only
// startMove writes a marker, for the default a move publishes; every path that
// makes a generation the default, including live promotion, moves through it.
const bootOwnersDir = bootStateDir + "/owners"

// recordDefaultOwner marks a move's desired default when its runtime runs
// this owner: it is the running generation, or it boots the same runtime
// artifact, as a live-promoted generation does. The marker only adds a true
// fact, so it may precede the move it belongs to.
func (m *BootMover) recordDefaultOwner(id string) error {
	if id == "" {
		return nil
	}
	running, err := m.runningGenerationID()
	if err != nil {
		return err
	}
	if id != running {
		spec, _, err := ReadGeneration(m.root, id)
		if err != nil {
			return err
		}
		runningSpec, _, err := ReadGeneration(m.root, running)
		if err != nil {
			return err
		}
		artifact := spec.Root.RuntimeArtifactSHA256
		if artifact == "" || artifact != runningSpec.Root.RuntimeArtifactSHA256 {
			return nil
		}
	}
	return recordOwner(m.root, id)
}

func recordOwner(root, id string) error {
	path := rootedPathUnchecked(root, filepath.Join(bootOwnersDir, id))
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := persistedrecord.WriteFileAtomic(path, nil, 0o644); err != nil {
		return fmt.Errorf("record boot-state owner for generation %s: %w", id, err)
	}
	return nil
}

func hasOwner(root, id string) (bool, error) {
	_, err := os.Stat(rootedPathUnchecked(root, filepath.Join(bootOwnersDir, id)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ConfirmBoot records that this boot passed health. It promotes a bound
// trial, records the boot otherwise, and leaves an armed trial waiting on the
// boot that armed it.
func (m *BootMover) ConfirmBoot(reason string) (BootHealthResult, error) {
	var health BootHealthResult
	err := withBootStateLock(m.root, func() error {
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		if boot, _, err := readCurrentBoot(m.root); err != nil {
			return err
		} else if boot.FailedTrial != "" && boot.BootID == m.bootID && boot.GenerationID == running {
			// This boot already failed its trial; the decision stands.
			health, err = m.healthResult(running, BootHealthSuccess)
			health.ReportOnly = true
			return err
		}
		reconciled, authority, err := m.reconcileLocked(reconcileFull)
		if err != nil {
			return err
		}
		selection, err := readOwnerSelection(m.root, running)
		if err != nil {
			return moveSelectionReadError(running, err)
		}
		promoted := false
		if authority == nil || m.boundTrial(authority, running) {
			request, err := confirmedBoot(m.root, selection, running, m.commandLine, reason, m.timestamp())
			if err != nil {
				return err
			}
			if request != nil {
				request.OperationID, request.Trial = "health-"+m.bootID, trialNone
				if authority != nil {
					request.OperationID, request.Trial = "promote-"+*authority.Reboot.TrialID, trialPromote
				}
				request.OneShot = armedOneShot(request.Selection)
				if _, err := m.moveLocked(*request, selection, authority, reconciled); err != nil {
					return err
				}
				promoted = request.Selection.DefaultGenerationID == running
			}
		}
		if health, err = m.healthResult(running, BootHealthSuccess); err != nil {
			return err
		}
		health.Promoted = promoted
		return nil
	})
	return health, err
}

// TrialRequest arms a boot trial of a committed candidate. The caller has
// written the candidate's loader entry.
type TrialRequest struct {
	OperationID  string
	GenerationID string
	Reason       string
	Now          time.Time
}

// ArmTrial arms the one trial that config apply, host upgrade and a prepared
// handoff start. The published default is the fallback. It returns when the
// candidate was committed to the trial.
func (m *BootMover) ArmTrial(request TrialRequest) (time.Time, error) {
	candidate := request.GenerationID
	at := request.Now.UTC().Round(0)
	if at.IsZero() {
		at = m.timestamp()
	}
	_, err := m.moveWith(func(selection BootSelectionRecord, authority *bootMoveIntent) (*bootMoveRequest, error) {
		spec, status, err := m.readMoveGeneration(candidate)
		if err != nil {
			return nil, err
		}
		if authority != nil && *authority.Reboot.TrialID == request.OperationID {
			// A repeat of a published arm.
			at = armedAt(status, request.OperationID, at)
			return nil, nil
		}
		if authority != nil {
			return nil, m.pendingTrialError(authority, "operation "+request.OperationID)
		}
		running, err := m.runningGenerationID()
		if err != nil {
			return nil, err
		}
		if running != selection.DefaultGenerationID {
			// A failed trial returns to the published default, which must
			// complete the records this owner leaves.
			owner, err := hasOwner(m.root, selection.DefaultGenerationID)
			if err != nil {
				return nil, err
			}
			if !owner {
				return nil, fmt.Errorf("generation %s runs while generation %s is the boot default, and generation %s has not run a KatlOS release that can return from a failed trial; select the running generation first with 'katlctl node generations select %s <node>', then retry",
					running, selection.DefaultGenerationID, selection.DefaultGenerationID, running)
			}
		}
		entry := spec.Boot.LoaderEntryPath
		trial := selection
		trial.OneShot = false
		trial.TargetBootGenerationID, trial.TargetBootEntry = candidate, entry
		trial.TrialGenerationID, trial.TrialBootEntry = candidate, entry
		trial.PreviousKnownGoodGenerationID = selection.DefaultGenerationID
		trial.PreviousKnownGoodBootEntry = selection.DefaultBootEntry
		if trial.Generation0FallbackID == "" {
			trial.Generation0FallbackID = selection.DefaultGenerationID
		}
		trial.PendingTransactionID = request.OperationID
		trial.PendingHealthValidation = true
		trial.PersistentDefaultPromotion = DefaultPromotionPending
		if err := CheckTrialFallback(m.root, trial); err != nil {
			return nil, err
		}
		return &bootMoveRequest{
			OperationID: request.OperationID, ExpectedRevision: selection.Revision,
			Source: running, Destination: candidate, Selection: trial, OneShot: candidate,
			StatusChanges: []bootStatusChange{{
				GenerationID: candidate, At: at, Reason: request.Reason,
				CommitState: CommitStateCommitted, BootState: BootStateTrying, HealthState: HealthStateUnknown,
				UnavailableReason: new(status.UnavailableReason),
			}},
			Trial: trialArm,
		}, nil
	})
	return at, err
}

func armedAt(status GenerationStatus, operationID string, fallback time.Time) time.Time {
	for _, transition := range status.StatusTransitions {
		if transition.OperationID == operationID {
			return transition.At
		}
	}
	return fallback
}

// PromoteLive makes a live-validated candidate the default without a reboot.
// A retry with the same operation ID repeats the published promotion.
func (m *BootMover) PromoteLive(request LivePromotionRequest) error {
	running, err := m.runningGenerationID()
	if err != nil {
		return err
	}
	_, err = m.moveWith(func(selection BootSelectionRecord, _ *bootMoveIntent) (*bootMoveRequest, error) {
		return livePromotion(m.root, selection, running, request)
	})
	return err
}

// SelectRequest selects a known-good generation for the next boot, or as the
// default unless OneShot is set.
type SelectRequest struct {
	GenerationID string
	OneShot      bool
}

// Select records a manual boot selection. A cancelled context leaves either
// nothing or a complete move.
func (m *BootMover) Select(ctx context.Context, request SelectRequest) error {
	_, err := m.moveWith(func(selection BootSelectionRecord, authority *bootMoveIntent) (*bootMoveRequest, error) {
		if authority != nil {
			return nil, m.pendingTrialError(authority, "the generation selection")
		}
		next, err := selectedBoot(m.root, selection, request)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next.OperationID = fmt.Sprintf("select-%d", selection.Revision+1)
		return next, nil
	})
	return err
}

// InvalidateSlot removes boot entry points before the caller overwrites the
// root slot. Generation records stay for diagnostics and shared artifact
// ownership until garbage collection. Loader entries go only after the move
// resolves, so an interrupted move never leaves a target without its entry.
func (m *BootMover) InvalidateSlot(operationID, partitionUUID string) error {
	var replaced []GenerationSpec
	_, err := m.moveThen(func(selection BootSelectionRecord, authority *bootMoveIntent) (*bootMoveRequest, error) {
		running, err := m.runningGenerationID()
		if err != nil {
			return nil, err
		}
		request, specs, err := invalidatedSlot(m.root, selection, authority, running, partitionUUID)
		if err != nil || request == nil {
			return nil, err
		}
		replaced = specs
		request.OperationID = "invalidate-" + operationID
		return request, nil
	}, func(bootMoveResult) error {
		for _, spec := range replaced {
			if err := removeEntries(m.root, spec); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// Remove deletes a generation that nothing protects.
func (m *BootMover) Remove(id string) error {
	if _, err := cleanSegment("generation id", id); err != nil {
		return err
	}
	var target *GenerationSpec
	_, err := m.moveThen(func(selection BootSelectionRecord, authority *bootMoveIntent) (*bootMoveRequest, error) {
		if authority != nil {
			return nil, m.pendingTrialError(authority, "removing generations")
		}
		if err := unsettledBoot(selection, "removing generations"); err != nil {
			return nil, err
		}
		items, err := inspect(m.root, selection, true)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.Spec.GenerationID != id {
				continue
			}
			if len(item.ProtectedBy) > 0 {
				return nil, fmt.Errorf("generation %s is protected: %s", id, strings.Join(item.ProtectedBy, ", "))
			}
			target = &item.Spec
			return removedPointers(selection, id), nil
		}
		return nil, nil
	}, func(bootMoveResult) error {
		if target == nil {
			return nil
		}
		return remove(m.root, *target)
	})
	return err
}

// Prune applies both retention floors per installed version. Callers exclude
// active operations; the boot lock excludes concurrent boot moves.
func (m *BootMover) Prune(policy Retention, now time.Time) ([]string, error) {
	count, age, err := policy.Limits()
	if err != nil {
		return nil, err
	}
	var removed []string
	err = withBootStateLock(m.root, func() error {
		reconciled, authority, err := m.reconcileLocked(reconcileFull)
		if err != nil {
			return err
		}
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		selection, err := readOwnerSelection(m.root, running)
		if err != nil {
			return err
		}
		if authority != nil || selection.PendingHealthValidation || selection.RecoveryRequired {
			return nil
		}
		items, err := inspect(m.root, selection, true)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for _, item := range items {
			key := item.Spec.RuntimeVersion + "/" + item.Spec.Root.Flavour
			counts[key]++
			if len(item.ProtectedBy) > 0 {
				continue
			}
			if item.Status.UnavailableReason == "" && !item.replaced &&
				(counts[key] <= count || !item.Spec.CreatedAt.Before(now.Add(-age))) {
				continue
			}
			if _, err := m.moveLocked(*removedPointers(selection, item.Spec.GenerationID), selection, nil, reconciled); err != nil {
				return err
			}
			if err := remove(m.root, item.Spec); err != nil {
				return err
			}
			// The removal can end the v1 form, so the next move reads the
			// selection as it now stands.
			if selection, err = readOwnerSelection(m.root, running); err != nil {
				return err
			}
			removed = append(removed, item.Spec.GenerationID)
		}
		return nil
	})
	return removed, err
}

// Read reconciles, then reads the published selection under the lock. An
// outstanding move that cannot complete is an error, so a caller never
// interprets a partly projected selection.
func (m *BootMover) Read(read func(BootSelectionRecord) error) error {
	return m.readWithAuthority(func(selection BootSelectionRecord, _ *bootMoveIntent) error {
		return read(selection)
	})
}

// readWithAuthority is Read that also passes the trial authority, which alone
// decides whether a trial is pending.
func (m *BootMover) readWithAuthority(read func(BootSelectionRecord, *bootMoveIntent) error) error {
	return withBootStateLock(m.root, func() error {
		_, authority, err := m.reconcileLocked(reconcileFull)
		if err != nil {
			return err
		}
		running, err := m.runningGenerationID()
		if err != nil {
			return err
		}
		selection, err := readOwnerSelection(m.root, running)
		if err != nil {
			return err
		}
		return read(selection, authority)
	})
}

// moveThen is moveWith followed, under the same lock, by an action that may
// run only once the move resolved.
func (m *BootMover) moveThen(build func(BootSelectionRecord, *bootMoveIntent) (*bootMoveRequest, error), after func(bootMoveResult) error) (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		var err error
		if result, err = m.moveWithLocked(build); err != nil {
			return err
		}
		if !result.Resolved {
			return fmt.Errorf("%w: the boot move did not resolve", ErrBootMoveBlocked)
		}
		return after(result)
	})
	return result, err
}

// publishInitialSelection is the owner's first publication, on the target
// root of an installation: revision 0 with generation 0 pending health.
func publishInitialSelection(root string, spec GenerationSpec) error {
	return publishSelection(root, spec.GenerationID, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: spec.GenerationID, DefaultBootEntry: spec.Boot.LoaderEntryPath,
		TargetBootGenerationID: spec.GenerationID, TargetBootEntry: spec.Boot.LoaderEntryPath,
		Generation0FallbackID:   spec.GenerationID,
		PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending,
		UpdatedAt: spec.CreatedAt,
	})
}
