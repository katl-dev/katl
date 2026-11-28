package generation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/persistedrecord"
)

// bootSelectionV2026_11_5 is the selection payload exactly as KatlOS 2026.11.5
// decodes it, copied from v2026.11.5:internal/generation/boot_selection.go.
type bootSelectionV2026_11_5 struct {
	OneShot                       bool      `json:"oneShot,omitempty"`
	APIVersion                    string    `json:"apiVersion"`
	Kind                          string    `json:"kind"`
	DefaultGenerationID           string    `json:"defaultGenerationID"`
	TargetBootGenerationID        string    `json:"targetBootGenerationID,omitempty"`
	TrialGenerationID             string    `json:"trialGenerationID,omitempty"`
	PreviousKnownGoodGenerationID string    `json:"previousKnownGoodGenerationID,omitempty"`
	BootedGenerationID            string    `json:"bootedGenerationID,omitempty"`
	BootedOneShot                 bool      `json:"bootedOneShot,omitempty"`
	ActiveGenerationID            string    `json:"activeGenerationID,omitempty"`
	Generation0FallbackID         string    `json:"generation0FallbackID,omitempty"`
	FailedBootGenerationID        string    `json:"failedBootGenerationID,omitempty"`
	DefaultBootEntry              string    `json:"defaultBootEntry,omitempty"`
	TargetBootEntry               string    `json:"targetBootEntry,omitempty"`
	TrialBootEntry                string    `json:"trialBootEntry,omitempty"`
	PreviousKnownGoodBootEntry    string    `json:"previousKnownGoodBootEntry,omitempty"`
	BootedBootEntry               string    `json:"bootedBootEntry,omitempty"`
	BootCountedTrialPath          string    `json:"bootCountedTrialPath,omitempty"`
	PendingTransactionID          string    `json:"pendingTransactionID,omitempty"`
	PendingHealthValidation       bool      `json:"pendingHealthValidation"`
	PersistentDefaultPromotion    string    `json:"persistentDefaultPromotion,omitempty"`
	RecoveryRequired              bool      `json:"recoveryRequired"`
	UpdatedAt                     time.Time `json:"updatedAt"`
}

// assertReadableBy11_5 decodes the published selection with the strict
// decoder of KatlOS 2026.11.5 and returns it.
func assertReadableBy11_5(t *testing.T, root string) bootSelectionV2026_11_5 {
	t.Helper()
	data, err := os.ReadFile(rootedPathUnchecked(root, bootStateDir+"/selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("2026.11.5 cannot decode the selection envelope: %v", err)
	}
	if envelope.RecordType != BootSelectionRecordType || envelope.RecordVersion != 1 {
		t.Fatalf("selection record = %s/v%d, want %s/v1", envelope.RecordType, envelope.RecordVersion, BootSelectionRecordType)
	}
	selection, err := persistedrecord.DecodePayload[bootSelectionV2026_11_5](envelope)
	if err != nil {
		t.Fatalf("2026.11.5 cannot decode the selection: %v\n%s", err, data)
	}
	return selection
}

// stampRelease records the KatlOS release a generation's runtime runs.
func stampRelease(t *testing.T, root, id, version string) {
	t.Helper()
	spec, status, err := ReadGeneration(root, id)
	if err != nil {
		t.Fatal(err)
	}
	spec.RuntimeVersion = version
	if status.SpecDigest, err = CanonicalSpecDigest(spec); err != nil {
		t.Fatal(err)
	}
	// The spec is immutable, so the fixture replaces the record.
	dir, err := GenerationDir(root, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spec.json", "status.json"} {
		removeFile(t, filepath.Join(dir, name))
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
}

// newLegacyTrialWorld is a node that KatlOS 2026.11.5 upgraded: 2026.11.5 runs
// gen1, the default with a loader entry, and armed a trial of gen2, which
// firmware booted and which runs this owner.
func newLegacyTrialWorld(t *testing.T) *moveWorld {
	t.Helper()
	w := newMoveWorld(t)
	stampRelease(t, w.root, "gen1", "2026.11.5")
	spec, status, err := ReadGeneration(w.root, "gen2")
	if err != nil {
		t.Fatal(err)
	}
	status.CommitState, status.BootState = CommitStateCommitted, BootStateTrying
	if err := WriteGenerationStatus(w.root, spec, status); err != nil {
		t.Fatal(err)
	}
	selection := w.selection()
	selection.Revision = 0
	selection.TrialGenerationID, selection.TrialBootEntry = "gen2", "loader/entries/katl-gen2.conf"
	selection.PreviousKnownGoodGenerationID, selection.PreviousKnownGoodBootEntry = "gen1", "loader/entries/katl-gen1.conf"
	selection.PendingTransactionID = "legacy-apply"
	selection.PendingHealthValidation = true
	selection.PersistentDefaultPromotion = DefaultPromotionPending
	writeBootHealthSelection(t, w.root, selection)
	w.bootID, w.running = "b-2", "gen2"
	return w
}

func assertNoIntent(t *testing.T, root string) {
	t.Helper()
	if intent, err := readBootMoveIntent(root); err != nil || intent != nil {
		t.Fatalf("intent = %+v, %v, want none", intent, err)
	}
}

func TestLegacyTrialResolves(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		w := newLegacyTrialWorld(t)

		health, err := w.mover(nil).FailTrial(trialDeadlineReason)
		if err != nil {
			t.Fatalf("FailTrial() error = %v", err)
		}

		if !health.RebootRequested {
			t.Fatalf("FailTrial() = %+v, want a reboot requested", health)
		}
		if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != "gen1" || got.FailedBootGenerationID != "gen2" || got.PendingHealthValidation {
			t.Fatalf("selection = %+v, want gen1 the default after gen2 failed", got)
		}
		if w.efi.entries.Default != "katl-gen1.conf" {
			t.Fatalf("EFI default = %q, want gen1", w.efi.entries.Default)
		}
		assertNoIntent(t, w.root)

		// A repeated failure on the same boot requests no second reboot.
		again, err := w.mover(nil).FailTrial(trialDeadlineReason)
		if err != nil || again.RebootRequested {
			t.Fatalf("repeated FailTrial() = %+v, %v, want no reboot", again, err)
		}
	})
	t.Run("success", func(t *testing.T) {
		w := newLegacyTrialWorld(t)

		if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
			t.Fatalf("ConfirmBoot() error = %v", err)
		}

		if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != "gen2" || got.PendingHealthValidation {
			t.Fatalf("selection = %+v, want gen2 promoted", got)
		}
		assertNoIntent(t, w.root)
	})
}

func TestLegacyReaderRemedies(t *testing.T) {
	t.Run("pending trial", func(t *testing.T) {
		w := newLegacyTrialWorld(t)
		m := w.mover(nil)

		if err := m.Read(func(BootSelectionRecord) error { return nil }); err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if _, _, err := m.Inspect(); err != nil {
			t.Fatalf("Inspect() error = %v", err)
		}
	})
	t.Run("promoted upgrade", func(t *testing.T) {
		w := newLegacyTrialWorld(t)
		if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
			t.Fatal(err)
		}
		m := w.mover(nil)

		if err := m.Select(context.Background(), SelectRequest{GenerationID: "gen1"}); err != nil {
			t.Fatalf("Select(gen1) error = %v", err)
		}
		if err := m.Remove("gen3"); err != nil {
			t.Fatalf("Remove(gen3) error = %v", err)
		}

		if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != "gen1" || got.TargetBootGenerationID != "gen1" {
			t.Fatalf("selection = %+v, want gen1 selected", got)
		}
		if _, _, err := ReadGeneration(w.root, "gen3"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("gen3 record after Remove: %v", err)
		}
	})
}

// Observation is the publish that activation makes without an intent or a
// trial, so it carries no field 2026.11.5 rejects.
func TestObservationWritesLegacyForm(t *testing.T) {
	w := newMoveWorld(t)
	stampRelease(t, w.root, "outside", "2026.11.5")
	if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen3", OneShot: true}); err != nil {
		t.Fatal(err)
	}
	w.reboot("b-2", "gen3")

	activate(t, w, "b-2", "gen3")

	data, err := os.ReadFile(rootedPathUnchecked(w.root, bootStateDir+"/selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "revision") {
		t.Fatalf("selection carries a revision while 2026.11.5 is bootable:\n%s", data)
	}
	if got := assertReadableBy11_5(t, w.root); got.BootedGenerationID != "gen3" {
		t.Fatalf("selection = %+v, want gen3 observed", got)
	}
}

func TestLegacyReaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		setup   func(t *testing.T, root string)
		running string
		want    bool
	}{
		{name: "entry present", version: "2026.11.5", running: "gen1", want: true},
		{name: "entry absent", version: "2026.11.5", running: "gen1", setup: func(t *testing.T, root string) {
			removeFile(t, rootedPathUnchecked(root, "/efi/loader/entries/katl-gen3.conf"))
		}},
		{name: "boot-counted entry", version: "2026.11.5", running: "gen1", want: true, setup: func(t *testing.T, root string) {
			entries := rootedPathUnchecked(root, loaderEntriesDir)
			if err := os.Rename(filepath.Join(entries, "katl-gen3.conf"), filepath.Join(entries, "katl-gen3+3.conf")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "running excluded", version: "2026.11.5", running: "gen3"},
		{name: "unreadable record", version: "2026.11.6", running: "gen1", want: true, setup: func(t *testing.T, root string) {
			writeBootFixture(t, rootedPathUnchecked(root, "/var/lib/katl/generations/outside/status.json"), []byte("{"))
		}},
		{name: "missing entries directory", version: "2026.11.5", running: "gen1", want: true, setup: func(t *testing.T, root string) {
			if err := os.RemoveAll(rootedPathUnchecked(root, loaderEntriesDir)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "development stamp", version: "2026.11.5-dev.0", running: "gen1"},
		{name: "release candidate", version: "2026.11.5-rc.1", running: "gen1", want: true},
		{name: "reader release", version: "2026.11.6", running: "gen1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			stampRelease(t, w.root, "gen3", tc.version)
			if tc.setup != nil {
				tc.setup(t, w.root)
			}

			if got := legacyReaders(w.root, tc.running); got != tc.want {
				t.Fatalf("legacyReaders() = %t, want %t", got, tc.want)
			}
		})
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// Every interruption of a move in the v1 form completes on retry, with at
// most one reboot and a selection 2026.11.5 decodes.
func TestLegacyFormMoveFaults(t *testing.T) {
	for _, op := range []struct {
		name  string
		setup func(*testing.T) *moveWorld
		run   func(*moveWorld, *BootMover) (bootMoveResult, error)
		steps []bootMoveStep
		want  string // the default after the move
	}{{
		name:  "fail trial",
		setup: newLegacyTrialWorld,
		run:   failMove,
		steps: []bootMoveStep{stepIntentWritten, stepStatusWritten, stepReadBack, stepRebootSpent, stepPublished, stepResolved},
		want:  "gen1",
	}, {
		name: "select",
		setup: func(t *testing.T) *moveWorld {
			w := newMoveWorld(t)
			stampRelease(t, w.root, "gen3", "2026.11.5")
			writeBootHealthSelection(t, w.root, lowerSelection(w.selection()))
			return w
		},
		run: func(_ *moveWorld, m *BootMover) (bootMoveResult, error) {
			return bootMoveResult{}, m.Select(context.Background(), SelectRequest{GenerationID: "gen3"})
		},
		steps: []bootMoveStep{stepIntentWritten, stepOneShotCleared, stepDefaultSet, stepOneShotSet, stepReadBack, stepPublished, stepResolved},
		want:  "gen3",
	}} {
		for crash := 1; crash <= len(op.steps); crash++ {
			t.Run(fmt.Sprintf("%s/after %d %s", op.name, crash, op.steps[crash-1]), func(t *testing.T) {
				w := op.setup(t)
				var seen []bootMoveStep
				result, err := op.run(w, w.mover(crashAt(crash, &seen)))
				if !errors.Is(err, errCrash) || !slices.Equal(seen, op.steps[:crash]) {
					t.Fatalf("crash at %s = %+v, %v after steps %v", op.steps[crash-1], result, err, seen)
				}
				w.count(result)
				assertReadableBy11_5(t, w.root)

				result, err = op.run(w, w.mover(nil))
				if err != nil {
					t.Fatalf("retry error = %v", err)
				}
				w.count(result)

				if w.reboots > 1 {
					t.Fatalf("reboots = %d, want at most one", w.reboots)
				}
				if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != op.want {
					t.Fatalf("selection = %+v, want default %s", got, op.want)
				}
				assertNoIntent(t, w.root)
			})
		}
	}
}

// crashFailedTrial leaves a legacy trial's failure interrupted after it
// projected the candidate's status.
func crashFailedTrial(t *testing.T) *moveWorld {
	t.Helper()
	w := newLegacyTrialWorld(t)
	var seen []bootMoveStep
	if _, err := w.mover(crashAt(2, &seen)).failTrial(trialDeadlineReason); !errors.Is(err, errCrash) || seen[1] != stepStatusWritten {
		t.Fatalf("failTrial() = %v after %v, want a crash after the status write", err, seen)
	}
	return w
}

// rewriteAsLegacy publishes the selection as a release without boot-move
// records would after booting gen1: without a revision and with new content.
func rewriteAsLegacy(t *testing.T, w *moveWorld) {
	t.Helper()
	selection := lowerSelection(w.selection())
	selection.BootedGenerationID, selection.BootedBootEntry = "gen1", "loader/entries/katl-gen1.conf"
	selection.UpdatedAt = selection.UpdatedAt.Add(time.Hour)
	writeBootHealthSelection(t, w.root, selection)
}

func TestSupersededIntent(t *testing.T) {
	t.Run("replaced selection", func(t *testing.T) {
		w := crashFailedTrial(t)
		rewriteAsLegacy(t, w)
		w.reboot("b-3", "gen3")

		if err := w.mover(nil).Read(func(BootSelectionRecord) error { return nil }); err != nil {
			t.Fatalf("Read() error = %v", err)
		}

		assertNoIntent(t, w.root)
	})
	t.Run("identical selection", func(t *testing.T) {
		w := crashFailedTrial(t)
		w.reboot("b-3", "gen2")

		if _, err := w.mover(nil).reconcile(); err != nil {
			t.Fatalf("reconcile() error = %v", err)
		}

		assertNoIntent(t, w.root)
		if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != "gen1" || got.FailedBootGenerationID != "gen2" {
			t.Fatalf("selection = %+v, want the failure completed", got)
		}
	})
}

// A release without boot-move records resolved the trial after the failure
// projected the candidate's status, then the candidate boots again.
func TestSupersededResolvedTrial(t *testing.T) {
	w := crashFailedTrial(t)
	selection := lowerSelection(w.selection())
	selection.BootedGenerationID, selection.BootedBootEntry = "gen1", "loader/entries/katl-gen1.conf"
	selection.TrialGenerationID, selection.TrialBootEntry = "", ""
	selection.PendingTransactionID, selection.PendingHealthValidation = "", false
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.FailedBootGenerationID = "gen2"
	selection.UpdatedAt = selection.UpdatedAt.Add(time.Hour)
	writeBootHealthSelection(t, w.root, selection)
	w.reboot("b-3", "gen2")

	health, err := w.mover(nil).FailTrial(trialDeadlineReason)
	if err != nil {
		t.Fatalf("FailTrial() error = %v", err)
	}
	if health.RebootRequested {
		t.Fatalf("FailTrial() = %+v, want no reboot for a resolved trial", health)
	}
	assertNoIntent(t, w.root)
	if got := assertReadableBy11_5(t, w.root); got.DefaultGenerationID != "gen1" || got.PendingHealthValidation {
		t.Fatalf("selection = %+v", got)
	}
}

// The v1 form lasts while the 2026.11.5 entry exists: InvalidateSlot's own
// publish is still v1, and the publish after its entry is gone carries a
// revision again.
func TestLegacyFormEnds(t *testing.T) {
	w := newMoveWorld(t)
	const slotB = "22222222-2222-3333-4444-555555555555"
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	spec := SpecFromRecord(abRecord(t, "gen4", "root-b", slotB, "2026.11.5", "v1.36.1", at))
	spec.Boot.LoaderEntryPath = "loader/entries/katl-gen4.conf"
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(w.root, spec, status); err != nil {
		t.Fatal(err)
	}
	writeBootFixture(t, rootedPathUnchecked(w.root, "/efi/"+spec.Boot.LoaderEntryPath), []byte("gen4"))
	m := w.mover(nil)

	if err := m.InvalidateSlot("slot-b", slotB); err != nil {
		t.Fatalf("InvalidateSlot() error = %v", err)
	}
	if got := w.selection(); got.Revision != 0 {
		t.Fatalf("InvalidateSlot published revision %d while the 2026.11.5 entry existed", got.Revision)
	}
	if _, err := os.Stat(rootedPathUnchecked(w.root, "/efi/"+spec.Boot.LoaderEntryPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gen4 loader entry after InvalidateSlot: %v", err)
	}

	if err := m.Select(context.Background(), SelectRequest{GenerationID: "gen3"}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got := w.selection(); got.Revision != 1 {
		t.Fatalf("Select() published revision %d after the 2026.11.5 entry went, want 1", got.Revision)
	}
}

// A recorded generation that reads boot-move records keeps the revision.
func TestReaderFormKeepsRevision(t *testing.T) {
	w := newMoveWorld(t)
	stampRelease(t, w.root, "gen3", "2026.11.6")

	if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen3"}); err != nil {
		t.Fatal(err)
	}

	if got := w.selection(); got.Revision != 5 {
		t.Fatalf("revision = %d, want 5", got.Revision)
	}
}
