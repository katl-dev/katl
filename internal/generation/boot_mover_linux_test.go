package generation

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
)

// fakeEFI models the two systemd-boot variables. Firmware consumes the
// one-shot on every boot, including a boot chosen from the menu.
type fakeEFI struct {
	entries efiboot.Entries
	fault   string // "read", "read-back", "write" or "mismatch"
	written bool
}

var errEFIFault = errors.New("injected EFI fault")

func (f *fakeEFI) read(string) (efiboot.Entries, error) {
	if f.fault == "read" || (f.fault == "read-back" && f.written) {
		return efiboot.Entries{}, errEFIFault
	}
	return f.entries, nil
}

func (f *fakeEFI) set(entry *string) BootDefaultSetter {
	return func(_ string, path string) error {
		f.written = true
		switch f.fault {
		case "write":
			return errEFIFault
		case "mismatch":
			return nil
		}
		*entry = ""
		if path != "" {
			*entry = filepath.Base(path)
		}
		return nil
	}
}

func (f *fakeEFI) boot() string {
	entry := cmp.Or(f.entries.OneShot, f.entries.Default)
	f.entries.OneShot = ""
	return strings.TrimSuffix(strings.TrimPrefix(entry, "katl-"), ".conf")
}

var errCrash = errors.New("injected crash")

type moveWorld struct {
	t       *testing.T
	root    string
	efi     *fakeEFI
	bootID  string
	running string
	reboots int
}

func newMoveWorld(t *testing.T) *moveWorld {
	t.Helper()
	root := t.TempDir()
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen1", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, at)
	writeBootHealthGeneration(t, root, "gen2", "gen1", CommitStateCandidate, BootStatePending, HealthStateUnknown, at)
	writeBootHealthGeneration(t, root, "gen3", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, at)
	writeBootHealthGeneration(t, root, "outside", "gen1", CommitStateCommitted, BootStateGood, HealthStateHealthy, at)
	// Every generation uses root-a, so one disk fixture serves them all.
	writeBootHealthFallback(t, root, "gen1")
	for _, id := range []string{"gen2", "gen3", "outside"} {
		spec, _, err := ReadGeneration(root, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range spec.Sysexts {
			writeBootFixture(t, rootedPathUnchecked(root, ref.Path), []byte("sysext"))
		}
		for _, ref := range spec.Confexts {
			if err := os.MkdirAll(rootedPathUnchecked(root, ref.Path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind, Revision: 4,
		DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
		BootedGenerationID: "gen1", BootedBootEntry: "loader/entries/katl-gen1.conf",
		ActiveGenerationID: "gen1", PersistentDefaultPromotion: DefaultPromotionDone, UpdatedAt: at,
	})
	if err := withBootStateLock(root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return &moveWorld{t: t, root: root, efi: &fakeEFI{entries: efiboot.Entries{Default: "katl-gen1.conf"}}, bootID: "b-1", running: "gen1"}
}

func (w *moveWorld) mover(checkpoint func(bootMoveStep) error) *bootMover {
	return w.moverWithCommandLine(bootHealthCommandLine(w.running), checkpoint)
}

func (w *moveWorld) moverWithCommandLine(commandLine string, checkpoint func(bootMoveStep) error) *bootMover {
	return &bootMover{
		root: w.root, bootID: w.bootID, commandLine: commandLine,
		now:        func() time.Time { return time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC) },
		readEFI:    w.efi.read,
		setDefault: w.efi.set(&w.efi.entries.Default),
		setOneShot: w.efi.set(&w.efi.entries.OneShot),
		checkpoint: checkpoint,
	}
}

func (w *moveWorld) reboot(bootID, running string) {
	w.efi.boot()
	w.bootID, w.running = bootID, running
}

func (w *moveWorld) selection() BootSelectionRecord {
	w.t.Helper()
	selection, err := ReadBootSelection(w.root)
	if err != nil {
		w.t.Fatal(err)
	}
	return selection
}

func (w *moveWorld) count(result bootMoveResult) {
	if result.RebootRequested {
		w.reboots++
	}
}

// moveOp is one operation of the matrix with its independently written outcomes.
type moveOp struct {
	name string
	// setup prepares the boot that issues the operation.
	setup func(*moveWorld)
	// run issues the operation, or reconciles for a migration.
	run   func(*moveWorld, *bootMover) (bootMoveResult, error)
	steps []bootMoveStep
	// issuing is the outcome after completion on the issuing boot.
	issuing moveOutcome
	// later maps a later boot's running generation to its outcome.
	later map[string]moveOutcome
}

type moveOutcome struct {
	refused   bool // an outstanding intent refuses this boot, and nothing changes
	selection selectionView
	intent    string
	efi       efiboot.Entries
	statuses  map[string]statusView
	reboots   int
}

type selectionView struct {
	Revision                  uint64
	Default, Target, Trial    string
	Failed, Spent             string
	Pending, RecoveryRequired bool
}

type statusView struct {
	Commit, Boot, Health, Unavailable string
	Events                            []string
}

func (w *moveWorld) observe() moveOutcome {
	w.t.Helper()
	selection := w.selection()
	intent, err := readBootMoveIntent(w.root)
	if err != nil {
		w.t.Fatal(err)
	}
	outcome := moveOutcome{
		selection: selectionView{
			Revision: selection.Revision, Default: selection.DefaultGenerationID,
			Target: selection.TargetBootGenerationID, Trial: selection.TrialGenerationID,
			Failed: selection.FailedBootGenerationID, Spent: selection.SpentRecoveryTrialID,
			Pending: selection.PendingHealthValidation, RecoveryRequired: selection.RecoveryRequired,
		},
		efi:      w.efi.entries,
		statuses: map[string]statusView{},
		reboots:  w.reboots,
	}
	if intent != nil {
		outcome.intent = intent.OperationID
	}
	for _, id := range []string{"gen1", "gen2", "gen3"} {
		_, status, err := ReadGeneration(w.root, id)
		if err != nil {
			w.t.Fatal(err)
		}
		view := statusView{Commit: status.CommitState, Boot: status.BootState, Health: status.HealthState, Unavailable: status.UnavailableReason}
		for _, transition := range status.StatusTransitions {
			view.Events = append(view.Events, transition.OperationID+": "+transition.Reason)
		}
		outcome.statuses[id] = view
	}
	return outcome
}

var (
	gen1Good       = statusView{Commit: "committed", Boot: "good", Health: "healthy"}
	gen2Staged     = statusView{Commit: "candidate", Boot: "pending", Health: "unknown"}
	gen3Good       = statusView{Commit: "committed", Boot: "good", Health: "healthy"}
	gen2Armed      = statusView{Commit: "candidate", Boot: "trying", Health: "unknown", Events: []string{"apply-7: trial armed"}}
	gen2FailedTrue = statusView{Commit: "candidate", Boot: "failed", Health: "unhealthy",
		Events: []string{"apply-7: trial armed", "fail-apply-7: katl-boot-health-deadline-expired"}}
	gen2Unconfirmed = statusView{Commit: "candidate", Boot: "failed", Health: "unhealthy",
		Events: []string{"apply-7: trial armed", "unconfirmed-apply-7: boot trial ended without health confirmation"}}
)

func armRequest(w *moveWorld) bootMoveRequest {
	selection := w.selection()
	selection.TrialGenerationID, selection.TrialBootEntry = "gen2", "loader/entries/katl-gen2.conf"
	selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "gen1", "loader/entries/katl-gen1.conf"
	selection.PendingTransactionID = "apply-7"
	selection.PendingHealthValidation = true
	selection.PersistentDefaultPromotion = DefaultPromotionPending
	return bootMoveRequest{
		OperationID: "apply-7", ExpectedRevision: 4, Source: "gen1", Destination: "gen2",
		Selection: selection, OneShot: "gen2", Trial: trialArm,
		StatusChanges: []bootStatusChange{{GenerationID: "gen2", Reason: "trial armed",
			CommitState: "candidate", BootState: "trying", HealthState: "unknown"}},
	}
}

// armAndBootCandidate leaves the arm authority published and bound on b-2.
func armAndBootCandidate(w *moveWorld) {
	w.t.Helper()
	if _, err := w.mover(nil).move(armRequest(w)); err != nil {
		w.t.Fatal(err)
	}
	w.reboot("b-2", "gen2")
}

// resolvedTrial is the published arm selection with its trial pointers cleared.
func resolvedTrial(w *moveWorld) BootSelectionRecord {
	selection := w.selection()
	selection.TrialGenerationID, selection.TrialBootEntry = "", ""
	selection.PendingTransactionID = ""
	selection.PendingHealthValidation = false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	return selection
}

func moveRun(request func(*moveWorld) bootMoveRequest) func(*moveWorld, *bootMover) (bootMoveResult, error) {
	return func(w *moveWorld, m *bootMover) (bootMoveResult, error) { return m.move(request(w)) }
}

func failRequest(w *moveWorld) bootMoveRequest {
	selection := resolvedTrial(w)
	selection.PersistentDefaultPromotion = ""
	selection.FailedBootGenerationID = "gen2"
	return bootMoveRequest{
		OperationID: "fail-apply-7", ExpectedRevision: 5, Source: "gen2", Destination: "gen1",
		Selection: selection, Trial: trialFail,
		StatusChanges: []bootStatusChange{{GenerationID: "gen2", Reason: "katl-boot-health-deadline-expired",
			CommitState: "candidate", BootState: "failed", HealthState: "unhealthy"}},
	}
}

func moveOps() []moveOp {
	armDone := moveOutcome{
		selection: selectionView{Revision: 5, Default: "gen1", Trial: "gen2", Pending: true},
		intent:    "apply-7",
		efi:       efiboot.Entries{Default: "katl-gen1.conf", OneShot: "katl-gen2.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2Armed, "gen3": gen3Good},
	}
	armBound := armDone
	armBound.efi = efiboot.Entries{Default: "katl-gen1.conf"}
	armUnconfirmed := moveOutcome{
		selection: selectionView{Revision: 6, Default: "gen1", Failed: "gen2", Spent: "apply-7"},
		efi:       efiboot.Entries{Default: "katl-gen1.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2Unconfirmed, "gen3": gen3Good},
	}

	promoted := moveOutcome{
		selection: selectionView{Revision: 6, Default: "gen2"},
		efi:       efiboot.Entries{Default: "katl-gen2.conf"},
		statuses: map[string]statusView{
			"gen1": {Commit: "superseded", Boot: "good", Health: "healthy", Events: []string{"promote-apply-7: superseded by gen2"}},
			"gen2": {Commit: "committed", Boot: "good", Health: "healthy", Events: []string{"apply-7: trial armed", "promote-apply-7: boot health passed"}},
			"gen3": gen3Good,
		},
	}
	failed := moveOutcome{
		selection: selectionView{Revision: 6, Default: "gen1", Failed: "gen2", Spent: "apply-7"},
		efi:       efiboot.Entries{Default: "katl-gen1.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2FailedTrue, "gen3": gen3Good},
		reboots:   1,
	}
	failedLater := failed
	failedLater.reboots = 0
	recovery := moveOutcome{
		selection: selectionView{Revision: 6, Default: "gen2", Failed: "gen2", RecoveryRequired: true},
		efi:       efiboot.Entries{Default: "katl-gen2.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2FailedTrue, "gen3": gen3Good},
	}
	persistent := moveOutcome{
		selection: selectionView{Revision: 5, Default: "gen3", Target: "gen3"},
		efi:       efiboot.Entries{Default: "katl-gen3.conf", OneShot: "katl-gen3.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2Staged, "gen3": gen3Good},
	}
	persistentLater := persistent
	persistentLater.efi.OneShot = ""
	oneShot := moveOutcome{
		selection: selectionView{Revision: 5, Default: "gen1", Target: "gen3"},
		efi:       efiboot.Entries{Default: "katl-gen1.conf", OneShot: "katl-gen3.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": gen2Staged, "gen3": gen3Good},
	}
	oneShotLater := oneShot
	oneShotLater.efi.OneShot = ""
	live := moveOutcome{
		selection: selectionView{Revision: 5, Default: "gen2"},
		efi:       efiboot.Entries{Default: "katl-gen2.conf"},
		statuses: map[string]statusView{
			"gen1": {Commit: "superseded", Boot: "good", Health: "healthy", Events: []string{"live-gen2: superseded by gen2"}},
			"gen2": {Commit: "committed", Boot: "good", Health: "healthy", Events: []string{"live-gen2: live health passed"}},
			"gen3": gen3Good,
		},
	}
	invalidated := moveOutcome{
		selection: selectionView{Revision: 5, Default: "gen1"},
		efi:       efiboot.Entries{Default: "katl-gen1.conf"},
		statuses: map[string]statusView{"gen1": gen1Good, "gen2": gen2Staged, "gen3": {Commit: "committed", Boot: "good", Health: "healthy",
			Unavailable: "OS slot has been replaced", Events: []string{"invalidate-gen3: OS slot has been replaced"}}},
	}
	legacyTrial := statusView{Commit: "candidate", Boot: "trying", Health: "unknown"}
	imported := moveOutcome{
		selection: selectionView{Revision: 1, Default: "gen1", Trial: "gen2", Pending: true},
		intent:    "legacy-apply",
		efi:       efiboot.Entries{Default: "katl-gen1.conf"},
		statuses:  map[string]statusView{"gen1": gen1Good, "gen2": legacyTrial, "gen3": gen3Good},
	}
	importedUnconfirmed := moveOutcome{
		selection: selectionView{Revision: 2, Default: "gen1", Failed: "gen2", Spent: "legacy-apply"},
		efi:       efiboot.Entries{Default: "katl-gen1.conf"},
		statuses: map[string]statusView{"gen1": gen1Good, "gen3": gen3Good, "gen2": {Commit: "candidate", Boot: "failed", Health: "unhealthy",
			Events: []string{"unconfirmed-legacy-apply: boot trial ended without health confirmation"}}},
	}
	refused := moveOutcome{refused: true}

	return []moveOp{{
		name:    "arm",
		setup:   func(*moveWorld) {},
		run:     moveRun(armRequest),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepOneShotCleared, stepDefaultSet, stepOneShotSet, stepReadBack, stepPublished},
		issuing: armDone,
		later:   map[string]moveOutcome{"gen1": armUnconfirmed, "gen2": armBound},
	}, {
		name:  "promote",
		setup: armAndBootCandidate,
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			selection := resolvedTrial(w)
			selection.DefaultGenerationID, selection.DefaultBootEntry = "gen2", "loader/entries/katl-gen2.conf"
			return bootMoveRequest{
				OperationID: "promote-apply-7", ExpectedRevision: 5, Source: "gen2", Destination: "gen2",
				Selection: selection, Trial: trialPromote,
				StatusChanges: []bootStatusChange{
					{GenerationID: "gen2", Reason: "boot health passed", CommitState: "committed", BootState: "good", HealthState: "healthy"},
					{GenerationID: "gen1", Reason: "superseded by gen2", CommitState: "superseded", BootState: "good", HealthState: "healthy"},
				},
			}
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepStatusWritten, stepOneShotCleared, stepDefaultSet, stepReadBack, stepPublished, stepResolved},
		issuing: promoted,
		later:   map[string]moveOutcome{"gen1": promoted, "gen2": promoted},
	}, {
		name:    "fail with fallback",
		setup:   armAndBootCandidate,
		run:     moveRun(failRequest),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepReadBack, stepRebootSpent, stepPublished, stepResolved},
		issuing: failed,
		later:   map[string]moveOutcome{"gen1": failedLater, "gen2": failedLater},
	}, {
		name: "fail without fallback",
		setup: func(w *moveWorld) {
			armAndBootCandidate(w)
			// The authorized trial's fallback lost its UKI after the trial began.
			if err := os.Remove(filepath.Join(w.root, "efi/EFI/Linux/katl-gen1.efi")); err != nil {
				w.t.Fatal(err)
			}
		},
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			request := failRequest(w)
			request.Destination = ""
			request.Selection.DefaultGenerationID, request.Selection.DefaultBootEntry = "gen2", "loader/entries/katl-gen2.conf"
			request.Selection.PreviousKnownGoodGenerationID, request.Selection.PreviousKnownGoodBootEntry = "", ""
			request.Selection.RecoveryRequired = true
			return request
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepOneShotCleared, stepDefaultSet, stepReadBack, stepPublished, stepResolved},
		issuing: recovery,
		// The fallback cannot activate without its UKI, so a boot of it changes nothing.
		later: map[string]moveOutcome{"gen1": refused, "gen2": recovery},
	}, {
		name:  "persistent select",
		setup: func(*moveWorld) {},
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			selection := w.selection()
			selection.DefaultGenerationID, selection.DefaultBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			selection.TargetBootGenerationID, selection.TargetBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "gen1", "loader/entries/katl-gen1.conf"
			return bootMoveRequest{OperationID: "select-gen3", ExpectedRevision: 4, Source: "gen1", Destination: "gen3", Selection: selection, OneShot: "gen3"}
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepOneShotCleared, stepDefaultSet, stepOneShotSet, stepReadBack, stepPublished, stepResolved},
		issuing: persistent,
		later:   map[string]moveOutcome{"gen1": persistentLater, "gen3": persistentLater},
	}, {
		name:  "one-shot select",
		setup: func(*moveWorld) {},
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			selection := w.selection()
			selection.TargetBootGenerationID, selection.TargetBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			selection.OneShot = true
			return bootMoveRequest{OperationID: "select-once-gen3", ExpectedRevision: 4, Source: "gen1", Destination: "gen3", Selection: selection, OneShot: "gen3"}
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepOneShotCleared, stepDefaultSet, stepOneShotSet, stepReadBack, stepPublished, stepResolved},
		issuing: oneShot,
		later:   map[string]moveOutcome{"gen1": oneShotLater, "gen3": oneShotLater},
	}, {
		name:  "live promotion",
		setup: func(*moveWorld) {},
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			selection := w.selection()
			selection.DefaultGenerationID, selection.DefaultBootEntry = "gen2", "loader/entries/katl-gen2.conf"
			selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "gen1", "loader/entries/katl-gen1.conf"
			return bootMoveRequest{
				OperationID: "live-gen2", ExpectedRevision: 4, Source: "gen1", Destination: "gen2", Selection: selection,
				StatusChanges: []bootStatusChange{
					{GenerationID: "gen2", Reason: "live health passed", CommitState: "committed", BootState: "good", HealthState: "healthy"},
					{GenerationID: "gen1", Reason: "superseded by gen2", CommitState: "superseded", BootState: "good", HealthState: "healthy"},
				},
			}
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepStatusWritten, stepOneShotCleared, stepDefaultSet, stepReadBack, stepPublished, stepResolved},
		issuing: live,
		later:   map[string]moveOutcome{"gen1": live, "gen2": live},
	}, {
		name: "invalidate slot",
		setup: func(w *moveWorld) {
			selection := w.selection()
			selection.TargetBootGenerationID, selection.TargetBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			selection.OneShot = true
			writeBootHealthSelection(w.t, w.root, selection)
			w.efi.entries.OneShot = "katl-gen3.conf"
		},
		run: moveRun(func(w *moveWorld) bootMoveRequest {
			selection := w.selection()
			selection.TargetBootGenerationID, selection.TargetBootEntry = "", ""
			selection.OneShot = false
			return bootMoveRequest{
				OperationID: "invalidate-gen3", ExpectedRevision: 4, Source: "gen3", Destination: "gen1", Selection: selection,
				StatusChanges: []bootStatusChange{{GenerationID: "gen3", Reason: "OS slot has been replaced",
					CommitState: "committed", BootState: "good", HealthState: "healthy", UnavailableReason: new("OS slot has been replaced")}},
			}
		}),
		steps:   []bootMoveStep{stepIntentWritten, stepStatusWritten, stepOneShotCleared, stepDefaultSet, stepReadBack, stepPublished, stepResolved},
		issuing: invalidated,
		later:   map[string]moveOutcome{"gen1": invalidated, "gen3": invalidated},
	}, {
		name: "migration",
		setup: func(w *moveWorld) {
			// An older runtime armed this trial, and firmware booted it.
			spec, status, err := ReadGeneration(w.root, "gen2")
			if err != nil {
				w.t.Fatal(err)
			}
			status.BootState = BootStateTrying
			if err := WriteGenerationStatus(w.root, spec, status); err != nil {
				w.t.Fatal(err)
			}
			selection := w.selection()
			selection.Revision = 0
			selection.TrialGenerationID, selection.TrialBootEntry = "gen2", "loader/entries/katl-gen2.conf"
			selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "gen1", "loader/entries/katl-gen1.conf"
			selection.PendingTransactionID = "legacy-apply"
			selection.PendingHealthValidation = true
			selection.PersistentDefaultPromotion = DefaultPromotionPending
			writeBootHealthSelection(w.t, w.root, selection)
			w.bootID, w.running = "b-2", "gen2"
		},
		run:     func(_ *moveWorld, m *bootMover) (bootMoveResult, error) { return m.reconcile() },
		steps:   []bootMoveStep{stepIntentWritten, stepReadBack, stepPublished},
		issuing: imported,
		later:   map[string]moveOutcome{"gen1": importedUnconfirmed, "gen2": imported},
	}}
}

// crashAt stops the protocol at the nth step after the operation's own
// intent is durable. Steps of an earlier reconciliation are not counted.
func crashAt(n int, seen *[]bootMoveStep) func(bootMoveStep) error {
	started := false
	return func(step bootMoveStep) error {
		started = started || step == stepIntentWritten
		if !started {
			return nil
		}
		*seen = append(*seen, step)
		if len(*seen) == n {
			return errCrash
		}
		return nil
	}
}

func TestBootMoveSteps(t *testing.T) {
	for _, op := range moveOps() {
		t.Run(op.name, func(t *testing.T) {
			w := newMoveWorld(t)
			op.setup(w)
			var seen []bootMoveStep
			result, err := op.run(w, w.mover(crashAt(0, &seen)))
			if err != nil {
				t.Fatal(err)
			}
			w.count(result)
			if !slices.Equal(seen, op.steps) {
				t.Fatalf("steps = %v, want %v", seen, op.steps)
			}
			if result.Resolved != (op.issuing.intent == "") {
				t.Fatalf("resolved = %t with intent %q", result.Resolved, op.issuing.intent)
			}
			assertMoveOutcome(t, w, op.issuing)
		})
	}
}

func TestBootMoveFaults(t *testing.T) {
	for _, op := range moveOps() {
		spent := slices.Index(op.steps, stepRebootSpent)
		for crash := 1; crash <= len(op.steps); crash++ {
			restarts := []string{"retry", "firmware", "outside", "wrong partuuid"}
			for id := range op.later {
				restarts = append(restarts, "boot "+id)
			}
			slices.Sort(restarts)
			for _, restart := range restarts {
				name := fmt.Sprintf("%s/after %d %s/%s", op.name, crash, op.steps[crash-1], restart)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					w := newMoveWorld(t)
					op.setup(w)
					var seen []bootMoveStep
					result, err := op.run(w, w.mover(crashAt(crash, &seen)))
					if !errors.Is(err, errCrash) || result.RebootRequested {
						t.Fatalf("crash at %s = %+v, %v", op.steps[crash-1], result, err)
					}

					want := op.issuing
					// A crash after the spend may leave zero reboots, never two.
					if spent >= 0 && crash > spent {
						want.reboots = 0
					}
					m := w.mover(nil)
					switch restart {
					case "retry":
					case "firmware":
						w.reboot("b-3", w.efi.boot())
						want = op.later[w.running]
						m = w.mover(nil)
					case "outside":
						w.reboot("b-3", "outside")
						m = w.mover(nil)
					case "wrong partuuid":
						w.reboot("b-3", w.efi.boot())
						m = w.moverWithCommandLine(strings.Replace(bootHealthCommandLine(w.running), "11111111", "22222222", 1), nil)
					default:
						w.reboot("b-3", strings.TrimPrefix(restart, "boot "))
						want = op.later[w.running]
						m = w.mover(nil)
					}
					if restart == "outside" || restart == "wrong partuuid" || want.refused {
						assertRefusedWithoutChange(t, w, m)
						return
					}
					if restart == "retry" {
						result, err = op.run(w, m)
					} else {
						result, err = m.reconcile()
					}
					if err != nil {
						t.Fatal(err)
					}
					w.count(result)
					assertMoveOutcome(t, w, want)
					assertMoveInvariants(t, w)

					// A repeated restart changes nothing and requests no reboot.
					before := w.observe()
					result, err = m.reconcile()
					if err != nil || result.RebootRequested || !reflect.DeepEqual(w.observe(), before) {
						t.Fatalf("second reconciliation = %+v, %v", result, err)
					}
				})
			}
		}
	}
}

func TestBootMoveEFIFaults(t *testing.T) {
	for _, op := range moveOps() {
		// "indeterminate" fails every read once the intent is durable.
		for _, fault := range []string{"read", "indeterminate", "read-back", "write", "mismatch"} {
			t.Run(op.name+"/"+fault, func(t *testing.T) {
				t.Parallel()
				w := newMoveWorld(t)
				op.setup(w)
				before := w.observe()
				w.efi.written = false
				checkpoint := func(step bootMoveStep) error {
					if step == stepIntentWritten {
						w.efi.fault = "read"
					}
					return nil
				}
				if fault != "indeterminate" {
					w.efi.fault, checkpoint = fault, nil
				}
				result, err := op.run(w, w.mover(checkpoint))
				if fault != "read" && fault != "indeterminate" && !slices.Contains(op.steps, stepOneShotCleared) {
					// The projection already matched, so EFI was not written.
					if err != nil {
						t.Fatal(err)
					}
					w.count(result)
					assertMoveOutcome(t, w, op.issuing)
					return
				}
				if err == nil || result.RebootRequested || result.Resolved {
					t.Fatalf("move with EFI fault = %+v, %v", result, err)
				}
				after := w.observe()
				if after.selection != before.selection {
					t.Fatalf("selection published across an unverified read-back: %+v", after.selection)
				}
				if fault == "read" && after.intent != before.intent {
					t.Fatalf("intent %q written without an EFI observation", after.intent)
				}

				w.efi.fault, w.efi.written = "", false
				result, err = op.run(w, w.mover(nil))
				if err != nil {
					t.Fatal(err)
				}
				w.count(result)
				assertMoveOutcome(t, w, op.issuing)
			})
		}
	}
}

func TestBootMoveAdmission(t *testing.T) {
	t.Run("stale revision", func(t *testing.T) {
		w := newMoveWorld(t)
		request := armRequest(w)
		request.ExpectedRevision = 3
		assertMoveRefused(t, w, request, "revision is 4, not 3")
	})
	t.Run("conflicting writer", func(t *testing.T) {
		w := newMoveWorld(t)
		var seen []bootMoveStep
		if _, err := w.mover(crashAt(1, &seen)).move(armRequest(w)); !errors.Is(err, errCrash) {
			t.Fatal(err)
		}
		// Another writer moved the selection while the intent was outstanding.
		selection := w.selection()
		selection.Revision = 9
		writeBootHealthSelection(t, w.root, selection)

		_, err := w.mover(nil).reconcile()
		if !errors.Is(err, ErrBootMoveBlocked) || !strings.Contains(err.Error(), "revision 9") {
			t.Fatalf("reconcile = %v", err)
		}
		if got := w.observe(); got.selection.Revision != 9 || got.selection.Trial != "" || got.intent != "apply-7" {
			t.Fatalf("conflicting selection replaced: %+v", got)
		}
	})
	t.Run("repeat after resolution", func(t *testing.T) {
		w := newMoveWorld(t)
		op := moveOps()[4]
		if _, err := op.run(w, w.mover(nil)); err != nil {
			t.Fatal(err)
		}
		result, err := op.run(w, w.mover(nil))
		if err != nil || result.RebootRequested {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
		assertMoveOutcome(t, w, op.issuing)
	})
	t.Run("move during trial", func(t *testing.T) {
		w := newMoveWorld(t)
		armAndBootCandidate(w)
		request := armRequest(w)
		request.OperationID, request.ExpectedRevision = "apply-8", 5
		assertMoveRefused(t, w, request, "trial apply-7 awaits its health result")
	})
	t.Run("fail on issuing boot", func(t *testing.T) {
		w := newMoveWorld(t)
		if _, err := w.mover(nil).move(armRequest(w)); err != nil {
			t.Fatal(err)
		}
		request := failRequest(w)
		assertMoveRefused(t, w, request, "needs a trial armed for running generation gen1")
	})
	t.Run("fail on first install", func(t *testing.T) {
		w := newMoveWorld(t)
		request := failRequest(w)
		request.ExpectedRevision = 4
		assertMoveRefused(t, w, request, "needs a trial armed")
	})
	t.Run("unbootable target", func(t *testing.T) {
		w := newMoveWorld(t)
		if err := os.Remove(filepath.Join(w.root, "efi/EFI/Linux/katl-gen2.efi")); err != nil {
			t.Fatal(err)
		}
		assertMoveRefused(t, w, armRequest(w), "cannot select generation gen2")
	})
}

func TestBootMoveImportsOnlyLegacyTrials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*BootSelectionRecord)
	}{
		{"first install", func(selection *BootSelectionRecord) {
			// Generation 0 awaits health without a predecessor or operation.
			selection.PendingHealthValidation = true
			selection.PersistentDefaultPromotion = DefaultPromotionPending
		}},
		{"manual select", func(selection *BootSelectionRecord) {
			selection.TargetBootGenerationID, selection.TargetBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			selection.OneShot = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			selection := w.selection()
			tc.modify(&selection)
			writeBootHealthSelection(t, w.root, selection)
			before := snapshotBootFiles(t, filepath.Join(w.root, "var"))
			result, err := w.mover(nil).reconcile()
			if err != nil || result != (bootMoveResult{}) {
				t.Fatalf("reconcile = %+v, %v", result, err)
			}
			if after := snapshotBootFiles(t, filepath.Join(w.root, "var")); !reflect.DeepEqual(before, after) {
				t.Fatal("reconciliation imported a trial without trial evidence")
			}
		})
	}
}

func assertMoveRefused(t *testing.T, w *moveWorld, request bootMoveRequest, detail string) {
	t.Helper()
	before := snapshotBootFiles(t, filepath.Join(w.root, "var"))
	efi := w.efi.entries
	result, err := w.mover(nil).move(request)
	if err == nil || !strings.Contains(err.Error(), detail) || result != (bootMoveResult{}) {
		t.Fatalf("move = %+v, %v; want refusal containing %q", result, err, detail)
	}
	if after := snapshotBootFiles(t, filepath.Join(w.root, "var")); !reflect.DeepEqual(before, after) || w.efi.entries != efi {
		t.Fatal("refused move changed boot state")
	}
}

func assertRefusedWithoutChange(t *testing.T, w *moveWorld, m *bootMover) {
	t.Helper()
	intent, err := readBootMoveIntent(w.root)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotBootFiles(t, filepath.Join(w.root, "var"))
	efi := w.efi.entries
	result, err := m.reconcile()
	if (intent != nil) != (err != nil) || result.RebootRequested {
		t.Fatalf("reconcile on an unrecorded boot = %+v, %v with intent %v", result, err, intent != nil)
	}
	if after := snapshotBootFiles(t, filepath.Join(w.root, "var")); !reflect.DeepEqual(before, after) || w.efi.entries != efi {
		t.Fatal("reconciliation on an unrecorded boot changed boot state")
	}
}

func assertMoveOutcome(t *testing.T, w *moveWorld, want moveOutcome) {
	t.Helper()
	got := w.observe()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outcome:\n got %+v\nwant %+v", got, want)
	}
	for _, id := range []string{"gen2", "gen3"} {
		// The owner never removes a loader entry; callers do after resolution.
		if _, err := os.Stat(filepath.Join(w.root, "efi/loader/entries/katl-"+id+".conf")); err != nil {
			t.Fatalf("loader entry for %s: %v", id, err)
		}
	}
}

// assertMoveInvariants checks the state after a successful reconciliation
// through the reader, the EFI store and the bootability check.
func assertMoveInvariants(t *testing.T, w *moveWorld) {
	t.Helper()
	selection := w.selection()
	intent, err := readBootMoveIntent(w.root)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := filepath.Base(selection.DefaultBootEntry)
	if intent != nil {
		wantDefault = *intent.DesiredDefault.Entry
		for _, target := range []*bootMoveTarget{intent.PriorDefault, intent.PriorOneShot, intent.DesiredDefault, intent.DesiredOneShot} {
			if id := *target.GenerationID; id != "" {
				if _, err := intent.activationRecord(w.root, id, bootHealthCommandLine(id)); err != nil {
					t.Fatalf("activation rejects recorded target %s: %v", id, err)
				}
			}
		}
	}
	if w.efi.entries.Default != wantDefault {
		t.Fatalf("EFI default = %q, want projection %q", w.efi.entries.Default, wantDefault)
	}
	next := strings.TrimSuffix(strings.TrimPrefix(cmp.Or(w.efi.entries.OneShot, w.efi.entries.Default), "katl-"), ".conf")
	spec, status, err := ReadGeneration(w.root, next)
	if err != nil {
		t.Fatal(err)
	}
	if next != w.running {
		if err := CheckBootability(w.root, spec, status); err != nil {
			t.Fatalf("firmware target %s is neither bootable nor running: %v", next, err)
		}
	}
	if w.reboots > 1 {
		t.Fatalf("%d automatic reboots for one trial", w.reboots)
	}
}
