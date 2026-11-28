package generation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootStateFiles snapshots everything the owner may write under /var/lib/katl:
// the selection, the intent, owner markers and every generation status.
func bootStateFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	base := rootedPathUnchecked(root, "/var/lib/katl")
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !strings.HasPrefix(entry.Name(), ".") && (strings.HasSuffix(path, ".json") || strings.Contains(path, "/boot/")) {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[strings.TrimPrefix(path, base)] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertBootStateUnchanged(t *testing.T, w *moveWorld, before map[string]string, efi any) {
	t.Helper()
	after := bootStateFiles(t, w.root)
	for path, data := range before {
		if after[path] != data {
			t.Errorf("%s changed", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was written", path)
		}
	}
	if w.efi.entries != efi {
		t.Errorf("EFI entries = %#v, want %#v", w.efi.entries, efi)
	}
}

func armTrial(t *testing.T, w *moveWorld, operationID, candidate string) {
	t.Helper()
	if _, err := w.mover(nil).ArmTrial(TrialRequest{OperationID: operationID, GenerationID: candidate, Reason: "trial armed"}); err != nil {
		t.Fatalf("ArmTrial(%s) error = %v", candidate, err)
	}
}

// activate boots running under a new boot ID through the owner's activation.
func activate(t *testing.T, w *moveWorld, bootID, running string) {
	t.Helper()
	w.reboot(bootID, running)
	if _, _, err := w.mover(nil).Activate(running); err != nil {
		t.Fatalf("Activate(%s) error = %v", running, err)
	}
}

func TestFailTrialReportOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *moveWorld)
	}{
		{name: "confirmed boot"},
		{name: "first install", setup: func(t *testing.T, w *moveWorld) {
			spec, _, err := ReadGeneration(w.root, "gen1")
			if err != nil {
				t.Fatal(err)
			}
			status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStatePending, HealthStateUnknown, spec.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteGeneration(w.root, spec, status); err != nil {
				t.Fatal(err)
			}
		}},
		// The trial is armed, but the candidate has not booted.
		{name: "arming boot", setup: func(t *testing.T, w *moveWorld) { armTrial(t, w, "apply-7", "gen2") }},
		{name: "after live promotion", setup: func(t *testing.T, w *moveWorld) {
			if err := w.mover(nil).PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-8", Reason: "validated live"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			if tc.setup != nil {
				tc.setup(t, w)
			}
			before, efi := bootStateFiles(t, w.root), w.efi.entries

			result, err := w.mover(nil).FailTrial(trialDeadlineReason)
			if err != nil {
				t.Fatalf("FailTrial() error = %v", err)
			}
			if !result.ReportOnly || result.Failed || result.RebootRequested || result.RecoveryRequired {
				t.Fatalf("FailTrial() = %#v, want a report-only result", result)
			}
			assertBootStateUnchanged(t, w, before, efi)
		})
	}
}

func TestConfirmBootClearsRecoveryOnlyOnDefault(t *testing.T) {
	for _, tc := range []struct {
		running string
		cleared bool
	}{
		{running: "gen1", cleared: true},
		{running: "gen3"},
	} {
		t.Run(tc.running, func(t *testing.T) {
			w := newMoveWorld(t)
			if tc.running != "gen1" {
				// A one-shot selection is how a non-default generation boots.
				if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: tc.running, OneShot: true}); err != nil {
					t.Fatal(err)
				}
			}
			selection := w.selection()
			selection.RecoveryRequired = true
			writeBootHealthSelection(t, w.root, selection)
			activate(t, w, "b-2", tc.running)

			if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
				t.Fatalf("ConfirmBoot() error = %v", err)
			}
			if got := w.selection(); got.RecoveryRequired == tc.cleared || got.DefaultGenerationID != "gen1" {
				t.Fatalf("selection after a healthy boot of %s = %#v, want recoveryRequired=%t on default gen1", tc.running, got, !tc.cleared)
			}
		})
	}
}

func TestConfirmBootOnArmingBootChangesNothing(t *testing.T) {
	w := newMoveWorld(t)
	armTrial(t, w, "apply-7", "gen2")
	before, efi := bootStateFiles(t, w.root), w.efi.entries

	result, err := w.mover(nil).ConfirmBoot("boot health passed")
	if err != nil {
		t.Fatalf("ConfirmBoot() error = %v", err)
	}
	if result.Promoted {
		t.Fatalf("ConfirmBoot() = %#v, want no promotion before the candidate boots", result)
	}
	assertBootStateUnchanged(t, w, before, efi)
}

func TestPendingTrialRefusesMoves(t *testing.T) {
	w := newMoveWorld(t)
	armTrial(t, w, "apply-7", "gen2")
	before, efi := bootStateFiles(t, w.root), w.efi.entries

	for name, move := range map[string]func(*BootMover) error{
		"select": func(m *BootMover) error {
			return m.Select(context.Background(), SelectRequest{GenerationID: "gen3"})
		},
		"live promotion": func(m *BootMover) error {
			return m.PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-8", Reason: "validated live"})
		},
		"another trial": func(m *BootMover) error {
			_, err := m.ArmTrial(TrialRequest{OperationID: "apply-8", GenerationID: "gen3", Reason: "trial armed"})
			return err
		},
	} {
		err := move(w.mover(nil))
		if err == nil || !strings.Contains(err.Error(), "a boot trial of generation gen2 is pending; run 'katlctl node reboot <node>'") {
			t.Errorf("%s while a trial is pending = %v, want the reboot guidance", name, err)
		}
	}
	assertBootStateUnchanged(t, w, before, efi)
}

func TestArmTrialRepeat(t *testing.T) {
	w := newMoveWorld(t)
	at := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	armed, err := w.mover(nil).ArmTrial(TrialRequest{OperationID: "apply-7", GenerationID: "gen2", Reason: "trial armed", Now: at})
	if err != nil || !armed.Equal(at) {
		t.Fatalf("ArmTrial() = %s, %v", armed, err)
	}
	before, efi := bootStateFiles(t, w.root), w.efi.entries

	again, err := w.mover(nil).ArmTrial(TrialRequest{OperationID: "apply-7", GenerationID: "gen2", Reason: "trial armed", Now: at.Add(time.Hour)})
	if err != nil || !again.Equal(at) {
		t.Fatalf("repeated ArmTrial() = %s, %v; want the first arm time %s", again, err, at)
	}
	assertBootStateUnchanged(t, w, before, efi)
}

func TestArmTrialFromNonDefaultNeedsOwner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker bool
	}{
		{name: "default without owner"},
		{name: "default with owner", marker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			selection := w.selection()
			selection.DefaultGenerationID, selection.DefaultBootEntry = "gen3", "loader/entries/katl-gen3.conf"
			writeBootHealthSelection(t, w.root, selection)
			w.efi.entries.Default = "katl-gen3.conf"
			if tc.marker {
				if err := recordOwner(w.root, "gen3"); err != nil {
					t.Fatal(err)
				}
			}
			before, efi := bootStateFiles(t, w.root), w.efi.entries

			_, err := w.mover(nil).ArmTrial(TrialRequest{OperationID: "apply-7", GenerationID: "gen2", Reason: "trial armed"})
			if !tc.marker {
				if err == nil || !strings.Contains(err.Error(), "select the running generation first with 'katlctl node generations select gen1 <node>'") {
					t.Fatalf("ArmTrial() = %v, want the select guidance", err)
				}
				assertBootStateUnchanged(t, w, before, efi)
				return
			}
			if err != nil {
				t.Fatalf("ArmTrial() error = %v", err)
			}
			if got := w.selection(); got.PreviousKnownGoodGenerationID != "gen3" || got.TrialGenerationID != "gen2" {
				t.Fatalf("selection = %#v, want a trial of gen2 falling back to the default gen3", got)
			}
		})
	}
}

// writeReleaseGeneration writes a known-good generation running the given
// KatlOS release on the world's root slot.
func writeReleaseGeneration(t *testing.T, root, id, version string) {
	t.Helper()
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	record := abRecord(t, id, "root-a", "11111111-2222-3333-4444-555555555555", version, "v1.36.1", at)
	spec := SpecFromRecord(record)
	spec.Root.RuntimeArtifactSHA256 = fmt.Sprintf("%x", sha256.Sum256(bootRootImage("boot health runtime")))
	spec.PreviousGenerationID = "gen1"
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	setExtensionDigests(t, &spec, "sysext")
	status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{spec.Boot.UKIPath, filepath.Join("/efi", spec.Boot.LoaderEntryPath)} {
		writeBootFixture(t, rootedPathUnchecked(root, path), []byte(id))
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

func TestBootMoveReaderGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		refused bool
	}{
		{version: "2026.11.5", refused: true},
		{version: "2026.11.6-rc.1"},
		{version: "2026.11.6"},
		{version: "0.0.0-dev"},
		{version: "0000.0.0-dev.1"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			for name, move := range map[string]func(*moveWorld) error{
				// The release is the destination of a one-shot selection.
				"one-shot destination": func(w *moveWorld) error {
					return w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen4", OneShot: true})
				},
				// The release is the prior default a failed move falls back to.
				"prior default": func(w *moveWorld) error {
					selection := w.selection()
					selection.DefaultGenerationID, selection.DefaultBootEntry = "gen4", "loader/entries/katl-gen4.conf"
					writeBootHealthSelection(t, w.root, selection)
					w.efi.entries.Default = "katl-gen4.conf"
					activate(t, w, "b-2", "gen1")
					return w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen1"})
				},
			} {
				t.Run(name, func(t *testing.T) {
					w := newMoveWorld(t)
					writeReleaseGeneration(t, w.root, "gen4", tc.version)

					err := move(w)
					if !tc.refused {
						if err != nil {
							t.Fatalf("move error = %v", err)
						}
						return
					}
					if err == nil || !strings.Contains(err.Error(), "generation gen4 runs KatlOS 2026.11.5, which cannot read boot-move records") {
						t.Fatalf("move = %v, want the reader refusal", err)
					}
					if intent, err := readBootMoveIntent(w.root); err != nil || intent != nil {
						t.Fatalf("refused move left intent %#v, %v", intent, err)
					}
				})
			}
		})
	}

	// The running generation completes its own moves, whatever its release.
	t.Run("running generation", func(t *testing.T) {
		w := newMoveWorld(t)
		writeReleaseGeneration(t, w.root, "gen4", "2026.11.5")
		selection := w.selection()
		selection.DefaultGenerationID, selection.DefaultBootEntry = "gen4", "loader/entries/katl-gen4.conf"
		selection.BootedGenerationID, selection.BootedBootEntry = "gen4", "loader/entries/katl-gen4.conf"
		selection.ActiveGenerationID = "gen4"
		writeBootHealthSelection(t, w.root, selection)
		w.efi.entries.Default = "katl-gen4.conf"
		activate(t, w, "b-2", "gen4")
		// The old release is the prior default, but it runs this move.
		if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen1"}); err != nil {
			t.Fatalf("Select() from the running old release error = %v", err)
		}
	})
}

func TestObservationWritesOnlyBootedFields(t *testing.T) {
	w := newMoveWorld(t)
	if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "gen3", OneShot: true}); err != nil {
		t.Fatal(err)
	}
	before := w.selection()
	statuses := bootStateFiles(t, w.root)
	w.reboot("b-2", "gen3")
	efi := w.efi.entries

	if _, _, err := w.mover(nil).Activate("gen3"); err != nil {
		t.Fatal(err)
	}

	want := before
	want.Revision++
	want.BootedGenerationID, want.BootedBootEntry, want.BootedOneShot = "gen3", "loader/entries/katl-gen3.conf", false
	got := w.selection()
	want.ActiveGenerationID, want.UpdatedAt = got.ActiveGenerationID, got.UpdatedAt
	if got != want {
		t.Fatalf("selection after observation = %#v, want %#v", got, want)
	}
	if w.efi.entries != efi {
		t.Fatalf("observation wrote EFI entries %#v", w.efi.entries)
	}
	for path, data := range bootStateFiles(t, w.root) {
		if strings.Contains(path, "/generations/") && statuses[path] != data {
			t.Errorf("observation changed %s", path)
		}
	}
}

// A failed trial with nothing usable makes the candidate the default and
// requires recovery; repairing the cause and booting it again clears that.
func TestRecoveryRequiredClearsOnRepairedBoot(t *testing.T) {
	w := newMoveWorld(t)
	armTrial(t, w, "apply-7", "gen2")
	activate(t, w, "b-2", "gen2")
	// The fallback's UKI is gone, so no fallback is usable.
	if err := os.Remove(rootedPathUnchecked(w.root, "/efi/EFI/Linux/katl-gen1.efi")); err != nil {
		t.Fatal(err)
	}
	result, err := w.mover(nil).FailTrial(trialDeadlineReason)
	if err != nil {
		t.Fatalf("FailTrial() error = %v", err)
	}
	if !result.RecoveryRequired || result.RebootRequested || result.DefaultGeneration != "gen2" {
		t.Fatalf("FailTrial() = %#v, want recovery required on default gen2 without a reboot", result)
	}
	if w.efi.entries.Default != "katl-gen2.conf" {
		t.Fatalf("EFI default = %q, want the running candidate", w.efi.entries.Default)
	}

	activate(t, w, "b-3", "gen2")
	if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
		t.Fatalf("ConfirmBoot() after repair error = %v", err)
	}
	if got := w.selection(); got.RecoveryRequired || got.DefaultGenerationID != "gen2" {
		t.Fatalf("selection after the repaired boot = %#v, want gen2 default without recovery", got)
	}
}

// Activation that meets an intent needing EFI writes completes nothing,
// reports the deferral and activates the accepted generation; boot health
// then completes the move.
func TestActivateDefersEFIWrites(t *testing.T) {
	for name, interrupt := range map[string]func(*BootMover) error{
		"select": func(m *BootMover) error {
			return m.Select(context.Background(), SelectRequest{GenerationID: "gen3"})
		},
		// A live promotion also changes status, which activation must not project.
		"live promotion": func(m *BootMover) error {
			return m.PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-8", Reason: "validated live"})
		},
	} {
		t.Run(name, func(t *testing.T) { testActivateDefersEFIWrites(t, interrupt) })
	}
}

func testActivateDefersEFIWrites(t *testing.T, interrupt func(*BootMover) error) {
	w := newMoveWorld(t)
	if err := interrupt(w.mover(crashAt(1, new([]bootMoveStep)))); err == nil {
		t.Fatal("move with a crash after the intent succeeded")
	}
	intent, err := readBootMoveIntent(w.root)
	if err != nil || intent == nil {
		t.Fatalf("interrupted select left intent %#v, %v", intent, err)
	}
	w.reboot("b-2", "gen1")
	before, efi := bootStateFiles(t, w.root), w.efi.entries
	m := w.mover(nil)
	m.setDefault = func(string, string) error { t.Fatal("activation wrote the EFI default"); return nil }
	m.setOneShot = func(string, string) error { t.Fatal("activation wrote the EFI one-shot"); return nil }

	plan, outcome, err := m.Activate("gen1")
	if err != nil || plan.GenerationID != "gen1" {
		t.Fatalf("Activate() = %#v, %v", plan, err)
	}
	if !outcome.Deferred() || outcome.TrialBound {
		t.Fatalf("Activate() outcome = %#v, want a deferral without a trial", outcome)
	}
	after := bootStateFiles(t, w.root)
	for path, data := range before {
		if after[path] != data {
			t.Errorf("activation changed %s", path)
		}
	}
	if w.efi.entries != efi {
		t.Fatalf("activation changed EFI entries to %#v", w.efi.entries)
	}

	if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
		t.Fatalf("ConfirmBoot() error = %v", err)
	}
	if intent, err := readBootMoveIntent(w.root); err != nil || intent != nil {
		t.Fatalf("ConfirmBoot left intent %#v, %v", intent, err)
	}
	// Boot health completes the move; the EFI default then matches the
	// published default, whichever generation that settled on.
	if got := w.selection(); got.Revision < 5 || filepath.Base(got.DefaultBootEntry) != w.efi.entries.Default {
		t.Fatalf("selection %#v, EFI %#v; want the completed move projected", got, w.efi.entries)
	}
}

// While recovery is required, booting a known-good generation from the boot
// menu is accepted, and its healthy boot makes it the default.
func TestRecoveryBootFromMenu(t *testing.T) {
	w := newMoveWorld(t)
	armTrial(t, w, "apply-7", "gen2")
	activate(t, w, "b-2", "gen2")
	if err := os.Remove(rootedPathUnchecked(w.root, "/efi/EFI/Linux/katl-gen1.efi")); err != nil {
		t.Fatal(err)
	}
	if result, err := w.mover(nil).FailTrial(trialDeadlineReason); err != nil || !result.RecoveryRequired {
		t.Fatalf("FailTrial() = %#v, %v; want recovery required", result, err)
	}

	activate(t, w, "b-3", "gen3")
	if _, err := w.mover(nil).ConfirmBoot("boot health passed"); err != nil {
		t.Fatalf("ConfirmBoot() of a menu boot error = %v", err)
	}
	got := w.selection()
	if got.RecoveryRequired || got.DefaultGenerationID != "gen3" || got.BootedGenerationID != "gen3" {
		t.Fatalf("selection after a menu boot of gen3 = %#v, want gen3 default without recovery", got)
	}
	if w.efi.entries.Default != "katl-gen3.conf" {
		t.Fatalf("EFI default = %q, want gen3", w.efi.entries.Default)
	}
}

// A health success that lands after the deadline failed the trial on the same
// boot reports and changes nothing; the failure stands.
func TestConfirmBootAfterSameBootFailure(t *testing.T) {
	w := newMoveWorld(t)
	armTrial(t, w, "apply-7", "gen2")
	activate(t, w, "b-2", "gen2")
	if result, err := w.mover(nil).FailTrial(trialDeadlineReason); err != nil || !result.RebootRequested {
		t.Fatalf("FailTrial() = %#v, %v; want the trial's reboot", result, err)
	}
	// A repeat activation on the same boot must not forget the failure.
	if _, _, err := w.mover(nil).Activate("gen2"); err != nil {
		t.Fatalf("repeat Activate() error = %v", err)
	}
	before, efi := bootStateFiles(t, w.root), w.efi.entries

	result, err := w.mover(nil).ConfirmBoot("boot health passed")
	if err != nil {
		t.Fatalf("ConfirmBoot() error = %v", err)
	}
	if result.Promoted || !result.ReportOnly {
		t.Fatalf("ConfirmBoot() = %#v, want a report-only result", result)
	}
	assertBootStateUnchanged(t, w, before, efi)
}

// The config-apply smoke sequence: a live apply promotes a generation on the
// running runtime, then a next-boot apply arms a trial without a reboot in
// between. The live-promoted default is the trial's fallback.
func TestArmTrialAfterLivePromotion(t *testing.T) {
	w := newMoveWorld(t)
	if err := w.mover(nil).PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-6", Reason: "validated live"}); err != nil {
		t.Fatal(err)
	}

	armTrial(t, w, "apply-7", "gen2")

	if got := w.selection(); got.DefaultGenerationID != "gen3" || got.PreviousKnownGoodGenerationID != "gen3" || got.TrialGenerationID != "gen2" {
		t.Fatalf("selection = %#v, want a trial of gen2 falling back to the live-promoted gen3", got)
	}
}

// A second live promotion in one boot runs while the default is no longer the
// running generation. Each published default carries its own loader entry,
// and the previous known-good stays the generation the node runs, so booting
// it from the menu is a known-good fallback.
func TestSecondLivePromotion(t *testing.T) {
	w := newMoveWorld(t)
	assertPublished := func(step, defaultID, previousID string) {
		t.Helper()
		got := w.selection()
		if got.DefaultGenerationID != defaultID || got.DefaultBootEntry != "loader/entries/katl-"+defaultID+".conf" || w.efi.entries.Default != "katl-"+defaultID+".conf" {
			t.Fatalf("%s: selection = %+v, EFI = %+v, want default %s with its own entry", step, got, w.efi.entries, defaultID)
		}
		wantEntry := ""
		if previousID != "" {
			wantEntry = "loader/entries/katl-" + previousID + ".conf"
		}
		if got.PreviousKnownGoodGenerationID != previousID || got.PreviousKnownGoodBootEntry != wantEntry {
			t.Fatalf("%s: previous known-good = %q %q, want %q %q", step, got.PreviousKnownGoodGenerationID, got.PreviousKnownGoodBootEntry, previousID, wantEntry)
		}
	}
	assertPublished("running", "gen1", "")

	if err := w.mover(nil).PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-6", Reason: "validated live"}); err != nil {
		t.Fatalf("first PromoteLive() error = %v", err)
	}
	assertPublished("first promotion", "gen3", "gen1")

	if err := w.mover(nil).PromoteLive(LivePromotionRequest{GenerationID: "gen2", OperationID: "apply-7", Reason: "validated live"}); err != nil {
		t.Fatalf("second PromoteLive() error = %v", err)
	}
	assertPublished("second promotion", "gen2", "gen1")

	// The boot menu boots gen1 although EFI names gen2.
	activate(t, w, "b-2", "gen1")
	result, err := w.mover(nil).ConfirmBoot("boot health passed")
	if err != nil {
		t.Fatalf("ConfirmBoot() error = %v", err)
	}
	if !result.Promoted {
		t.Fatalf("ConfirmBoot() = %#v, want gen1 promoted", result)
	}
	assertPublished("menu boot of gen1", "gen1", "")
	if got := w.selection(); got.BootedGenerationID != "gen1" || got.BootedBootEntry != "loader/entries/katl-gen1.conf" || got.RecoveryRequired {
		t.Fatalf("selection = %+v, want gen1 booted with its own entry and no recovery", got)
	}
}

// A live promotion records the running generation as the previous known-good
// only when it is known-good, so a later trial always has a fallback that
// recovery accepts.
func TestLivePromotionFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		boot         func(*testing.T, *moveWorld)
		wantFallback string
	}{
		{
			// The menu boots gen2, which has not passed boot health. Only the
			// kernel names it; activation would refuse the unselected boot.
			name:         "menu boot of an unproven generation",
			boot:         func(t *testing.T, w *moveWorld) { w.reboot("b-2", "gen2") },
			wantFallback: "gen1",
		},
		{
			name: "one-shot boot of a known-good generation",
			boot: func(t *testing.T, w *moveWorld) {
				if err := w.mover(nil).Select(context.Background(), SelectRequest{GenerationID: "outside", OneShot: true}); err != nil {
					t.Fatalf("Select(outside, one-shot) error = %v", err)
				}
				activate(t, w, "b-2", "outside")
			},
			wantFallback: "outside",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			tc.boot(t, w)
			if got := w.selection(); got.DefaultGenerationID != "gen1" {
				t.Fatalf("selection before promotion = %+v, want default gen1", got)
			}

			if err := w.mover(nil).PromoteLive(LivePromotionRequest{GenerationID: "gen3", OperationID: "apply-6", Reason: "validated live"}); err != nil {
				t.Fatalf("PromoteLive(gen3) error = %v", err)
			}

			got := w.selection()
			wantEntry := "loader/entries/katl-" + tc.wantFallback + ".conf"
			if got.DefaultGenerationID != "gen3" || got.PreviousKnownGoodGenerationID != tc.wantFallback || got.PreviousKnownGoodBootEntry != wantEntry {
				t.Fatalf("selection = %+v, want default gen3 falling back to %s at %s", got, tc.wantFallback, wantEntry)
			}
			if _, err := w.mover(nil).ArmTrial(TrialRequest{OperationID: "apply-7", GenerationID: "gen2", Reason: "trial armed"}); err != nil {
				t.Fatalf("ArmTrial(gen2) after promotion error = %v", err)
			}
		})
	}
}

// The trial authority decides whether a trial blocks a mutation, and the
// candidate's own boot gets no reboot advice: a reboot would end its trial.
func TestMutationBaseDuringTrial(t *testing.T) {
	for _, tc := range []struct {
		name, want, refused string
		boot                func(*testing.T, *moveWorld)
	}{
		{name: "armed", want: "a boot trial of generation gen2 is pending; run 'katlctl node reboot <node>' to try it"},
		{
			name: "candidate boot", want: "generation gen2 is in its boot trial and boot health has not decided it; wait for 'katlctl node status <node>'",
			refused: "katlctl node reboot",
			boot:    func(t *testing.T, w *moveWorld) { activate(t, w, "b-2", "gen2") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			armTrial(t, w, "apply-7", "gen2")
			expected := "gen1"
			if tc.boot != nil {
				tc.boot(t, w)
				expected = "gen2"
			}

			err := w.mover(nil).ValidateMutationBase(expected)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateMutationBase() = %v, want %q", err, tc.want)
			}
			if tc.refused != "" && strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("ValidateMutationBase() = %v, must not advise %q", err, tc.refused)
			}
		})
	}
}
