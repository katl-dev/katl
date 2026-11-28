package generation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
	"golang.org/x/sys/unix"
)

func TestReadBootMoveIntent(t *testing.T) {
	root := t.TempDir()
	writeIntentFixture(t, root, intentFixture(t))
	got, err := readBootMoveFile(filepath.Join(root, bootMovePath))
	if err != nil {
		t.Fatal(err)
	}

	want := &bootMoveIntent{
		OperationID:               "recover-gen1",
		BootID:                    "6baea692-085e-49ac-9bd6-c948a8da8406",
		ExpectedSelectionRevision: new(uint64(7)),
		SourceGenerationID:        "gen1",
		DestinationGenerationID:   new("gen2"),
		PriorDefault:              &bootMoveTarget{new("gen0"), new("katl-gen0.conf")},
		PriorOneShot:              &bootMoveTarget{new("gen1"), new("")},
		DesiredDefault:            &bootMoveTarget{new("gen2"), new("katl-gen2.conf")},
		DesiredOneShot:            &bootMoveTarget{new("gen3"), new("katl-gen3.conf")},
		DesiredSelection: BootSelectionRecord{
			APIVersion: "katl.dev/v1alpha1", Kind: "BootSelection",
			Revision: 8, SpentRecoveryTrialID: "trial-gen1",
			DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
			TargetBootGenerationID: "gen3", TargetBootEntry: "loader/entries/katl-gen3.conf",
			OneShot: true, UpdatedAt: time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC),
		},
		StatusChanges: []bootStatusChange{{
			GenerationID: "gen1", At: time.Date(2026, 11, 26, 14, 59, 0, 0, time.UTC),
			Reason: "trial failed", CommitState: "committed", BootState: "failed",
			HealthState: "unhealthy", UnavailableReason: new(""),
		}},
		Reboot: bootMoveReboot{TrialID: new("trial-gen1"), Eligible: new(true), Spent: new(true)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded intent = %#v, want %#v", got, want)
	}
}

func TestBootStateRefusesInvalidIntent(t *testing.T) {
	fixture := intentFixture(t)
	for _, tc := range []struct{ name, data, detail string }{
		{"missing operation", strings.Replace(fixture, `"operationID": "recover-gen1",`, "", 1), "operationID"},
		{"missing expected revision", strings.Replace(fixture, `"expectedSelectionRevision": 7,`, "", 1), "expectedSelectionRevision"},
		{"missing one-shot", strings.Replace(fixture, `"priorOneShot": {"generationID": "gen1", "entry": ""},`, "", 1), "priorOneShot"},
		{"contradictory default", strings.Replace(fixture, `"desiredDefault": {"generationID": "gen2"`, `"desiredDefault": {"generationID": "outside"`, 1), "desiredDefault disagrees"},
		{"contradictory prior entry", strings.Replace(fixture, `"priorDefault": {"generationID": "gen0", "entry": "katl-gen0.conf"}`, `"priorDefault": {"generationID": "gen0", "entry": "katl-outside.conf"}`, 1), "does not match its generation loader entry"},
		{"missing spent", strings.Replace(fixture, `, "spent": true`, "", 1), "spent"},
		{"unsupported", strings.Replace(fixture, `"recordVersion": 1`, `"recordVersion": 2`, 1), "unsupported boot move intent katl.boot.move-intent/v2"},
		{"corrupt", "{", "decode envelope"},
		{"directory", "", "not a regular file"},
		{"unreadable", "", "too many levels of symbolic links"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeBootMoveLegacyFixture(t, root)
			writeBootHealthGeneration(t, root, "outside", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, time.Now().UTC())
			switch tc.name {
			case "directory":
				// A directory cannot be read as an intent even by a privileged test runner.
				if err := os.Mkdir(filepath.Join(root, bootMovePath), 0o755); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				// A broken link must not turn a present path into a missing intent.
				if err := os.Symlink("missing.json", filepath.Join(root, bootMovePath)); err != nil {
					t.Fatal(err)
				}
			default:
				writeIntentFixture(t, root, tc.data)
			}
			assertIntentError := func(err error) {
				t.Helper()
				if err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), tc.detail) ||
					!strings.Contains(err.Error(), bootMovePath) || !strings.Contains(err.Error(), "node console") {
					t.Fatalf("expected actionable %q error, got %v", tc.detail, err)
				}
			}
			booted := "gen1"
			if tc.name == "contradictory default" {
				booted = "outside"
				writeBootDisk(t, root)
			}
			m, _ := moverOn(t, root, booted)
			_, _, err := m.Activate(booted)
			assertIntentError(err)
			assertIntentError(m.Select(t.Context(), SelectRequest{GenerationID: "gen0"}))
		})
	}
}

func TestActivationWithBootMoveIntent(t *testing.T) {
	for _, id := range []string{"gen0", "gen1", "gen2", "gen3", "outside"} {
		t.Run(id, func(t *testing.T) {
			root := t.TempDir()
			selection := writeBootMoveLegacyFixture(t, root)
			writeBootHealthGeneration(t, root, "outside", "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, selection.UpdatedAt)
			writeBootDisk(t, root)
			for _, generation := range []string{"gen0", "gen1", "gen2", "gen3"} {
				writeBootHealthFallback(t, root, generation)
			}
			fixture := intentFixture(t)
			if id == "gen0" {
				// The fallback boot can also follow a one-shot consumed after the snapshot.
				fixture = strings.Replace(fixture, `"priorOneShot": {"generationID": "gen1", "entry": ""}`,
					`"priorOneShot": {"generationID": "gen1", "entry": "katl-gen1.conf"}`, 1)
			}
			writeIntentFixture(t, root, fixture)
			m, efi := moverOn(t, root, id)
			efi.entries = efiboot.Entries{Default: "firmware.conf"}
			before := snapshotBootFiles(t, filepath.Join(root, "var"))

			_, outcome, err := m.Activate(id)

			if id == "outside" {
				if err == nil || !strings.Contains(err.Error(), "outside the recorded boot targets") {
					t.Fatalf("activation of %s = %v", id, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// The intent expects a selection revision this root never had, so
			// it cannot complete; activation reports that and accepts the boot.
			if id != "outside" && !errors.Is(outcome.ReconcileErr, ErrBootMoveBlocked) {
				t.Fatalf("reconcile error = %v, want the blocked move", outcome.ReconcileErr)
			}
			got, recorded, err := CurrentBootGeneration(root)
			if err != nil || recorded != (id != "outside") || (recorded && got != id) {
				t.Fatalf("current boot = %q, %t, %v", got, recorded, err)
			}
			if efi.written || efi.entries.OneShot != "" {
				t.Fatalf("activation wrote firmware variables: %+v", efi.entries)
			}
			// Activation records which generations have run the owner and
			// nothing else.
			after := snapshotBootFiles(t, filepath.Join(root, "var"))
			for path := range after {
				if strings.HasPrefix(path, filepath.Join(root, bootOwnersDir)+"/") {
					delete(after, path)
				}
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("activation under an intent changed durable boot state")
			}
		})
	}
}

func TestBootMoveActivatesWithInvalidatedPredecessor(t *testing.T) {
	root := t.TempDir()
	writeBootDisk(t, root)
	fixture := strings.Replace(intentFixture(t), `"unavailableReason": ""`, `"unavailableReason": "OS slot has been replaced"`, 1)
	writeIntentFixture(t, root, fixture)

	previous := activationRecord(t, root, "gen1", "shared Kubernetes sysext")
	target := activationRecord(t, root, "gen2", "shared Kubernetes sysext")
	target.PreviousGenerationID = previous.GenerationID
	for _, id := range []string{"gen0", "gen3"} {
		writeBootHealthGeneration(t, root, id, "", CommitStateCommitted, BootStateGood, HealthStateHealthy, target.CreatedAt)
	}
	for _, record := range []Record{previous, target} {
		record.Boot.LoaderEntryPath = "loader/entries/katl-" + record.GenerationID + ".conf"
		writeActivationGeneration(t, root, record)
		mustWrite(t, rootedPathUnchecked(root, record.Boot.UKIPath), "UKI", 0o644)
		mustWrite(t, rootedPathUnchecked(root, "/efi/"+record.Boot.LoaderEntryPath), "loader entry", 0o644)
	}
	spec, status, err := ReadGeneration(root, previous.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	status.UnavailableReason = "OS slot has been replaced"
	status.BootState = BootStateFailed
	status.HealthState = HealthStateUnhealthy
	status.UpdatedAt = time.Date(2026, 11, 26, 14, 59, 0, 0, time.UTC)
	status.StatusTransitions = append(status.StatusTransitions, StatusTransition{
		At: status.UpdatedAt, OperationID: "recover-gen1", Reason: "trial failed",
		CommitState: status.CommitState, BootState: status.BootState, HealthState: status.HealthState,
	})
	if err := WriteGenerationStatus(root, spec, status); err != nil {
		t.Fatal(err)
	}
	m, _ := moverOn(t, root, target.GenerationID)

	if _, _, err := m.Activate(target.GenerationID); err != nil {
		t.Fatal(err)
	}

	assertSymlink(t, filepath.Join(root, "run/extensions/kubernetes.raw"), target.Sysexts[0].Path)
}

func TestIncompletableIntentRefusesMovesAndReads(t *testing.T) {
	root := t.TempDir()
	writeBootMoveLegacyFixture(t, root)
	writeIntentFixture(t, root, intentFixture(t))
	m, efi := moverOn(t, root, "gen1")
	kept := keepsBootState(t, root, efi)

	for name, action := range map[string]func() error{
		"select":   func() error { return m.Select(t.Context(), SelectRequest{GenerationID: "gen0"}) },
		"arm":      func() error { _, err := m.ArmTrial(TrialRequest{OperationID: "arm", GenerationID: "gen2"}); return err },
		"promote":  func() error { return m.PromoteLive(LivePromotionRequest{GenerationID: "gen2", OperationID: "live"}) },
		"confirm":  func() error { _, err := m.ConfirmBoot(""); return err },
		"invalid":  func() error { return m.InvalidateSlot("upgrade", "22222222-2222-3333-4444-555555555555") },
		"remove":   func() error { return m.Remove("gen3") },
		"read":     func() error { return m.Read(func(BootSelectionRecord) error { return nil }) },
		"validate": func() error { return m.ValidateMutationBase("gen0") },
	} {
		err := action()
		if !errors.Is(err, ErrBootMoveBlocked) || !strings.Contains(err.Error(), "node console") {
			t.Fatalf("%s with an incompletable intent = %v, want a blocked move with console guidance", name, err)
		}
	}
	kept()
}

func TestLegacyBootStateImport(t *testing.T) {
	root := t.TempDir()
	selection := writeBootMoveLegacyFixture(t, root)
	writeObservedBootDefault(t, root)
	before := snapshotBootFiles(t, root)
	reader := efiboot.Reader{Statfs: func(_ int, stat *unix.Statfs_t) error {
		stat.Type = unix.EFIVARFS_MAGIC
		return nil
	}}

	for range 2 {
		legacy, err := readLegacyBootState(root, "gen0", reader.Read)
		if err != nil {
			t.Fatal(err)
		}
		if legacy == nil {
			t.Fatal("expected legacy state")
		}
		if legacy.selection != selection || legacy.selection.Revision != 0 ||
			legacy.selection.PendingTransactionID != "legacy-op" || legacy.trialGenerationID != "gen1" {
			t.Fatalf("legacy selection = %#v, trial = %q", legacy.selection, legacy.trialGenerationID)
		}
		if legacy.generations["gen1"].status.BootState != BootStateTrying {
			t.Fatalf("legacy candidate status was lost: %#v", legacy.generations["gen1"])
		}
		if legacy.efiErr != nil || legacy.efi.Default != "firmware.conf" || legacy.efi.OneShot != "" {
			t.Fatalf("legacy EFI = %#v, %v", legacy.efi, legacy.efiErr)
		}
	}
	if after := snapshotBootFiles(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("legacy import changed boot-state files or their mtimes")
	}
}

func TestLegacyBootStateDistinguishesTrials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*BootSelectionRecord)
	}{
		{"first install", func(selection *BootSelectionRecord) {
			selection.TrialGenerationID = "gen0"
			selection.PendingTransactionID = ""
		}},
		{"manual selection", func(selection *BootSelectionRecord) {
			selection.TargetBootGenerationID = "gen1"
			selection.TrialGenerationID = ""
			selection.PendingHealthValidation = false
			selection.PersistentDefaultPromotion = ""
			selection.OneShot = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			selection := writeBootMoveLegacyFixture(t, root)
			tc.change(&selection)
			writeBootHealthSelection(t, root, selection)

			legacy, err := readLegacyBootState(root, "gen0", efiboot.Reader{}.Read)

			if err != nil || legacy == nil || legacy.trialGenerationID != "" {
				t.Fatalf("%s unexpectedly imported as a trial: %+v, %v", tc.name, legacy, err)
			}
		})
	}
}

func TestReadWaitsForBootStateLock(t *testing.T) {
	root := t.TempDir()
	selection := writeBootMoveLegacyFixture(t, root)
	selection.PendingHealthValidation, selection.PendingTransactionID = false, ""
	selection.TrialGenerationID, selection.TrialBootEntry = "", ""
	selection.PersistentDefaultPromotion = DefaultPromotionDone
	selection.BootedGenerationID, selection.BootedBootEntry = "gen0", "loader/entries/katl-gen0.conf"
	writeBootHealthSelection(t, root, selection)
	m, _ := moverOn(t, root, "gen0")
	start := make(chan struct{})
	done := make(chan error, 1)
	observed := make(chan BootSelectionRecord, 1)
	err := withBootStateLock(root, func() error {
		// Model a writer that has published its selection before the lock is released.
		selection.TargetBootGenerationID, selection.TargetBootEntry = "gen3", "loader/entries/katl-gen3.conf"
		selection.OneShot = true
		if err := WriteBootSelection(root, selection); err != nil {
			return err
		}
		go func() {
			close(start)
			done <- m.Read(func(published BootSelectionRecord) error {
				observed <- published
				return nil
			})
		}()
		<-start
		select {
		case err := <-done:
			t.Fatalf("read returned under the writer's lock, error = %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not return after the writer released the lock")
	}
	if got := <-observed; got.TargetBootGenerationID != "gen3" {
		t.Fatalf("read observed %+v, want the writer's selection", got)
	}
}

func writeBootMoveLegacyFixture(t *testing.T, root string) BootSelectionRecord {
	t.Helper()
	now := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "gen0", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now)
	writeBootHealthGeneration(t, root, "gen1", "gen0", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
	for _, id := range []string{"gen2", "gen3"} {
		writeBootHealthGeneration(t, root, id, "gen0", CommitStateCommitted, BootStateGood, HealthStateHealthy, now)
	}
	selection := BootSelectionRecord{
		APIVersion: APIVersion, Kind: BootSelectionKind,
		DefaultGenerationID: "gen0", DefaultBootEntry: "loader/entries/katl-gen0.conf",
		TrialGenerationID: "gen1", TrialBootEntry: "loader/entries/katl-gen1.conf",
		BootedGenerationID: "gen1", BootedBootEntry: "loader/entries/katl-gen1.conf",
		PreviousKnownGoodGenerationID: "gen0", PendingTransactionID: "legacy-op",
		PendingHealthValidation: true, PersistentDefaultPromotion: DefaultPromotionPending, UpdatedAt: now,
	}
	writeBootHealthSelection(t, root, selection)
	if err := withBootStateLock(root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return selection
}

func intentFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../installer/testdata/persisted/v1/katl.boot.move-intent.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeIntentFixture(t *testing.T, root, data string) {
	t.Helper()
	path := filepath.Join(root, bootMovePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeObservedBootDefault(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "sys/firmware/efi/efivars")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte{7, 0, 0, 0, 'f', 0, 'i', 0, 'r', 0, 'm', 0, 'w', 0, 'a', 0, 'r', 0, 'e', 0, '.', 0, 'c', 0, 'o', 0, 'n', 0, 'f', 0, 0, 0}
	if err := os.WriteFile(filepath.Join(dir, "LoaderEntryDefault-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

type bootFileSnapshot struct {
	data    string
	modTime time.Time
}

func snapshotBootFiles(t *testing.T, root string) map[string]bootFileSnapshot {
	t.Helper()
	files := make(map[string]bootFileSnapshot)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = bootFileSnapshot{string(data), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
