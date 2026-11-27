package generation

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
)

// legacyImportBootID records that the boot which armed an imported legacy
// trial is unknown. It is never a kernel boot ID, which is a UUID.
const legacyImportBootID = "legacy-import"

// unconfirmedTrialReason records a trial whose one attempt ended on a later
// boot of another recorded target.
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
		result, _, err = m.reconcileLocked()
		return err
	})
	return result, err
}

// move admits a request after reconciliation and carries it through the protocol.
func (m *bootMover) move(request bootMoveRequest) (bootMoveResult, error) {
	var result bootMoveResult
	err := withBootStateLock(m.root, func() error {
		reconciled, authority, err := m.reconcileLocked()
		if err != nil {
			return err
		}
		selection, err := ReadBootSelection(m.root)
		if err != nil {
			return err
		}
		if selection.Revision == request.ExpectedRevision+1 && sameMoveSelection(selection, request.Selection) {
			// The move was published and resolved by an earlier attempt.
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
		if err := writeBootMoveIntent(m.root, intent); err != nil {
			return err
		}
		if err := m.reached(stepIntentWritten); err != nil {
			return err
		}
		result, _, err = m.complete(&intent)
		return err
	})
	return result, err
}

func (m *bootMover) runningGenerationID() (string, error) {
	return SelectedGenerationFromCommandLine(m.commandLine)
}

// reconcileLocked returns the trial authority that remains after completion.
func (m *bootMover) reconcileLocked() (bootMoveResult, *bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, nil, err
	}
	intent, err := readBootMoveIntent(m.root)
	if err != nil {
		return bootMoveResult{}, nil, err
	}
	if intent == nil {
		if intent, err = m.importLegacyTrial(running); err != nil || intent == nil {
			return bootMoveResult{}, nil, err
		}
	}
	if _, err := intent.activationRecord(m.root, running, m.commandLine); err != nil {
		return bootMoveResult{}, nil, err
	}
	result, authority, err := m.complete(intent)
	if err != nil || authority == nil {
		return result, authority, err
	}
	if m.bootID == authority.BootID || running == *authority.DestinationGenerationID {
		return result, authority, nil
	}
	unconfirmed, err := m.unconfirmedTrial(authority, running)
	if err != nil {
		return bootMoveResult{}, authority, err
	}
	if err := writeBootMoveIntent(m.root, unconfirmed); err != nil {
		return bootMoveResult{}, authority, err
	}
	if err := m.reached(stepIntentWritten); err != nil {
		return bootMoveResult{}, authority, err
	}
	result, _, err = m.complete(&unconfirmed)
	return result, nil, err
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
		if authority == nil || m.bootID == authority.BootID || running != *authority.DestinationGenerationID {
			return bootMoveIntent{}, fmt.Errorf("operation %s needs a trial armed for running generation %s; this boot reports health without changing the boot target", request.OperationID, running)
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
			reboot = bootMoveReboot{TrialID: new(request.OperationID), Eligible: new(true), Spent: new(false)}
		}
	}

	observed, err := m.readEFI(m.root)
	if err != nil {
		return bootMoveIntent{}, fmt.Errorf("observe EFI boot variables before operation %s: %w", request.OperationID, err)
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
	if err := m.checkTargets(intent, running); err != nil {
		return bootMoveIntent{}, err
	}
	return intent, nil
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
	if desired.DefaultBootEntry == "" {
		*intent.DesiredDefault.Entry = ""
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
	projected := map[string]GenerationStatus{}
	for _, change := range intent.StatusChanges {
		spec, status, err := ReadGeneration(m.root, change.GenerationID)
		if err != nil {
			return fmt.Errorf("operation %s: read generation %s: %w", intent.OperationID, change.GenerationID, err)
		}
		next := projectStatusChange(status, change, intent.OperationID)
		if err := ValidateGenerationStatus(spec, next); err != nil {
			return fmt.Errorf("operation %s: %w", intent.OperationID, err)
		}
		if err := ValidateStatusTransition(status, next); err != nil {
			return fmt.Errorf("operation %s: generation %s: %w", intent.OperationID, change.GenerationID, err)
		}
		projected[change.GenerationID] = next
	}
	for _, id := range []string{*intent.DesiredDefault.GenerationID, *intent.DesiredOneShot.GenerationID} {
		if id == "" || id == running {
			continue
		}
		spec, status, err := ReadGeneration(m.root, id)
		if err != nil {
			return fmt.Errorf("operation %s: read boot target %s: %w", intent.OperationID, id, err)
		}
		if next, ok := projected[id]; ok {
			status = next
		}
		check := bootabilityCheck{root: m.root}
		if err := check.check(spec, status, bootFull).problem(); err != nil {
			return fmt.Errorf("operation %s cannot select generation %s: %w", intent.OperationID, id, err)
		}
	}
	return nil
}

// importLegacyTrial records a trial armed by a runtime without the move
// owner as a trial authority. It uses the reader's legacy trial classification.
func (m *bootMover) importLegacyTrial(running string) (*bootMoveIntent, error) {
	legacy, err := readLegacyBootState(m.root, running, m.readEFI)
	if err != nil || legacy == nil || legacy.trialGenerationID == "" {
		return nil, err
	}
	if legacy.efiErr != nil {
		return nil, fmt.Errorf("observe EFI boot variables before importing legacy trial %s: %w", legacy.selection.PendingTransactionID, legacy.efiErr)
	}
	selection := legacy.selection
	trialID := selection.PendingTransactionID
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
	if err := writeBootMoveIntent(m.root, intent); err != nil {
		return nil, err
	}
	if err := m.reached(stepIntentWritten); err != nil {
		return nil, err
	}
	return &intent, nil
}

// unconfirmedTrial resolves an authority whose one attempt ended without a
// health result, on a later boot of another recorded target. It never reboots.
func (m *bootMover) unconfirmedTrial(authority *bootMoveIntent, running string) (bootMoveIntent, error) {
	selection := authority.DesiredSelection
	candidate := *authority.DestinationGenerationID
	trialID := *authority.Reboot.TrialID
	_, status, err := ReadGeneration(m.root, candidate)
	if err != nil {
		return bootMoveIntent{}, fmt.Errorf("resolve trial %s: read generation %s: %w", trialID, candidate, err)
	}
	observed, err := m.readEFI(m.root)
	if err != nil {
		return bootMoveIntent{}, fmt.Errorf("observe EFI boot variables before resolving trial %s: %w", trialID, err)
	}

	desired := selection
	desired.Revision = selection.Revision + 1
	desired.SpentRecoveryTrialID = trialID
	desired.TrialGenerationID, desired.TrialBootEntry = "", ""
	desired.TargetBootGenerationID, desired.TargetBootEntry = "", ""
	desired.OneShot, desired.BootedOneShot = false, false
	desired.PendingTransactionID = ""
	desired.PendingHealthValidation = false
	desired.PersistentDefaultPromotion = ""
	desired.BootCountedTrialPath = ""
	desired.FailedBootGenerationID = candidate
	if desired.PreviousKnownGoodGenerationID == "" {
		desired.PreviousKnownGoodGenerationID = desired.DefaultGenerationID
		desired.PreviousKnownGoodBootEntry = desired.DefaultBootEntry
	}
	desired.UpdatedAt = m.timestamp()
	changes := []bootStatusChange{{
		GenerationID: candidate, At: desired.UpdatedAt, Reason: unconfirmedTrialReason,
		CommitState: status.CommitState, BootState: BootStateFailed, HealthState: HealthStateUnhealthy,
		UnavailableReason: new(status.UnavailableReason),
	}}
	// Spent from the start: the attempt already happened, so nothing reboots.
	reboot := bootMoveReboot{TrialID: new(trialID), Eligible: new(true), Spent: new(true)}
	return m.newIntent("unconfirmed-"+trialID, selection, desired, observed, running,
		candidate, desired.DefaultGenerationID, "", changes, reboot)
}

// complete carries a durable intent from its first unfinished step. It
// returns the intent when it remains as a trial authority.
func (m *bootMover) complete(intent *bootMoveIntent) (bootMoveResult, *bootMoveIntent, error) {
	running, err := m.runningGenerationID()
	if err != nil {
		return bootMoveResult{}, nil, err
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

	if err := m.publish(*intent); err != nil {
		return bootMoveResult{}, nil, err
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
		if slices.ContainsFunc(status.StatusTransitions, func(transition StatusTransition) bool {
			return transition.OperationID == intent.OperationID
		}) {
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
	if *intent.DesiredDefault.Entry != "" {
		if err := m.setDefault(m.root, intent.DesiredSelection.DefaultBootEntry); err != nil {
			return fmt.Errorf("operation %s: set EFI default: %w", intent.OperationID, err)
		}
		if err := m.reached(stepDefaultSet); err != nil {
			return err
		}
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

// publish compares the selection revision and publishes the desired selection.
func (m *bootMover) publish(intent bootMoveIntent) error {
	current, err := ReadBootSelection(m.root)
	if err != nil {
		return err
	}
	switch {
	case current.Revision == *intent.ExpectedSelectionRevision:
		if err := WriteBootSelection(m.root, intent.DesiredSelection); err != nil {
			return err
		}
		return m.reached(stepPublished)
	case current == intent.DesiredSelection:
		return nil
	default:
		return fmt.Errorf("%w: boot selection revision %d does not match operation %s, which expects %d. From the node console, preserve %s and %s for diagnosis",
			ErrBootMoveBlocked, current.Revision, intent.OperationID, *intent.ExpectedSelectionRevision, bootMovePath, bootStateDir+"/selection.json")
	}
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
