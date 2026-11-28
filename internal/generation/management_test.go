package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSelectionBoots(t *testing.T) {
	for _, oneShot := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistent", true: "one-shot"}[oneShot], func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
			m, efi := moverOn(t, root, "current")
			req := SelectRequest{GenerationID: "old", OneShot: oneShot}
			for range 2 {
				if err := m.Select(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			wantDefault := "old"
			if oneShot {
				wantDefault = "current"
			}
			if efi.entries.OneShot != "katl-old.conf" || efi.entries.Default != "katl-"+wantDefault+".conf" {
				t.Fatalf("firmware = %+v, want one-shot katl-old.conf and default %s", efi.entries, wantDefault)
			}
			if got := selectionOf(t, root).Revision; got != 1 {
				t.Fatalf("repeating the selection moved the revision to %d, want 1", got)
			}

			// A health service replay before reboot cannot undo the operator's choice.
			if _, err := m.ConfirmBoot(""); err != nil {
				t.Fatal(err)
			}
			queued := selectionOf(t, root)
			if queued.TargetBootGenerationID != "old" || queued.DefaultGenerationID != wantDefault {
				t.Fatalf("health replay consumed selection: %+v", queued)
			}

			running := restart(t, root, efi)
			if running != "old" {
				t.Fatalf("firmware boots %q, want the selected generation", running)
			}
			booted := newMover(root, efi, "boot-2", running)
			if _, _, err := booted.Activate(running); err != nil {
				t.Fatal(err)
			}
			// Health replay must not promote a one-shot boot, including after restart.
			for range 2 {
				if _, err := booted.ConfirmBoot(""); err != nil {
					t.Fatal(err)
				}
				selection := selectionOf(t, root)
				if selection.DefaultGenerationID != wantDefault || selection.ActiveGenerationID != "old" || selection.TargetBootGenerationID != "" {
					t.Fatalf("selection after boot: %+v", selection)
				}
			}
			if oneShot {
				running = restart(t, root, efi)
				if running != "current" {
					t.Fatalf("firmware boots %q after the one-shot, want current", running)
				}
				returned := newMover(root, efi, "boot-3", running)
				if _, _, err := returned.Activate(running); err != nil {
					t.Fatal(err)
				}
				if _, err := returned.ConfirmBoot(""); err != nil {
					t.Fatal(err)
				}
				if selection := selectionOf(t, root); selection.ActiveGenerationID != "current" || selection.OneShot {
					t.Fatalf("return boot: %+v", selection)
				}
			}
		})
	}
}

func TestSelectionFaultLeavesIntentForNextCall(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
	m, efi := moverOn(t, root, "current")
	req := SelectRequest{GenerationID: "old"}
	efi.fault = "write"

	if err := m.Select(t.Context(), req); !errors.Is(err, errEFIFault) {
		t.Fatalf("select = %v, want the firmware fault", err)
	}

	if selection := selectionOf(t, root); selection.DefaultGenerationID != "current" || selection.TargetBootGenerationID != "" {
		t.Fatalf("a failed select published %+v", selection)
	}
	if _, err := os.Stat(rootedPathUnchecked(root, bootMovePath)); err != nil {
		t.Fatalf("a failed select left no intent for the next call: %v", err)
	}

	efi.fault = ""
	if err := m.Select(t.Context(), req); err != nil {
		t.Fatalf("select after the fault cleared: %v", err)
	}

	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "old" || selection.TargetBootGenerationID != "old" || selection.Revision != 1 {
		t.Fatalf("selection = %+v, want old as default and next boot after one revision bump", selection)
	}
	if efi.entries.Default != "katl-old.conf" || efi.entries.OneShot != "katl-old.conf" {
		t.Fatalf("firmware = %+v, want old as default and one-shot", efi.entries)
	}
	if _, err := os.Stat(rootedPathUnchecked(root, bootMovePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains after completion: %v", err)
	}
}

func TestExpiredSelectionLeavesBootTargetUnchanged(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
	m, efi := moverOn(t, root, "current")
	kept := keepsBootState(t, root, efi)
	ctx, cancel := context.WithDeadline(t.Context(), time.Time{})
	defer cancel()

	err := m.Select(ctx, SelectRequest{GenerationID: "old"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("select = %v, want expired deadline", err)
	}
	kept()
}

func TestSelectionRequiresKnownGood(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "old", "root-b", "1", now.Add(-time.Hour))
	spec, _, err := ReadGeneration(root, "old")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateFailed, HealthStateUnhealthy, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGenerationStatus(root, spec, state); err != nil {
		t.Fatal(err)
	}
	m, efi := moverOn(t, root, "current")
	kept := keepsBootState(t, root, efi)

	err = m.Select(t.Context(), SelectRequest{GenerationID: "old"})

	if err == nil || !strings.Contains(err.Error(), "has not passed boot health") {
		t.Fatalf("select = %v, want health refusal for a bootable generation", err)
	}
	kept()
}

func TestRetentionFloors(t *testing.T) {
	root, now := managementFixture(t)
	// Two versions, deliberately nonnumeric IDs. Newest and recent floors are independent.
	for _, f := range []struct {
		id, slot, version string
		age               time.Duration
	}{
		{"a-old", "root-a", "1", 90 * 24 * time.Hour},
		{"z-recent", "root-a", "1", time.Hour},
		{"b-old", "root-b", "0", 90 * 24 * time.Hour},
		{"b-newest", "root-b", "0", 80 * 24 * time.Hour},
		{"b-middle", "root-b", "0", 85 * 24 * time.Hour},
		{"a-middle", "root-a", "1", 85 * 24 * time.Hour},
	} {
		managedFixture(t, root, f.id, f.slot, f.version, now.Add(-f.age))
	}
	m, _ := moverOn(t, root, "current")
	count := 1
	removed, err := m.Prune(Retention{KeepLast: &count, MaxAge: "30d"}, now)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"a-middle", "a-old", "b-middle", "b-old"}) {
		t.Fatalf("removed %v", removed)
	}
	for _, id := range removed {
		if _, err := os.Stat(filepath.Join(root, "efi/loader/entries/katl-"+id+".conf")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale boot entry %s: %v", id, err)
		}
		if _, _, err := ReadGeneration(root, id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	if again, err := m.Prune(Retention{KeepLast: &count, MaxAge: "30d"}, now); err != nil || len(again) != 0 {
		t.Fatalf("repeat: %v %v", again, err)
	}
	// The newest healthy generation of the previous OS remains even after aging.
	if err := m.Remove("b-newest"); err == nil || !strings.Contains(err.Error(), "slot rollback") {
		t.Fatalf("rollback removal: %v", err)
	}
}

func TestPruneReplacedOS(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "obsolete", "root-b", "0", now.Add(-time.Hour))
	managedFixture(t, root, "replacement", "root-b", "1", now)
	m, _ := moverOn(t, root, "current")

	items, _, err := m.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Spec.GenerationID == "obsolete" {
			if item.UnavailableReason != "" {
				t.Fatalf("obsolete generation availability = %+v", item)
			}
		}
	}
	removed, err := m.Prune(Retention{}, now)
	if err != nil || !slices.Equal(removed, []string{"obsolete"}) {
		t.Fatalf("replaced OS cleanup = %v, %v", removed, err)
	}
	if _, _, err := ReadGeneration(root, "replacement"); err != nil {
		t.Fatalf("replacement generation was removed: %v", err)
	}
}

func TestProtectedRemoval(t *testing.T) {
	for _, role := range []string{"active", "booted", "default", "target", "trial", "rollback"} {
		t.Run(role, func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "protected", "root-a", "1", now.Add(-time.Hour))
			selection, _ := ReadBootSelection(root)
			switch role {
			case "active":
				selection.ActiveGenerationID = "protected"
			case "booted":
				selection.BootedGenerationID = "protected"
			case "default":
				selection.DefaultGenerationID = "protected"
			case "target":
				selection.TargetBootGenerationID = "protected"
			case "trial":
				selection.TrialGenerationID = "protected"
			case "rollback":
				selection.PreviousKnownGoodGenerationID = "protected"
			}
			if err := WriteBootSelection(root, selection); err != nil {
				t.Fatal(err)
			}
			m, _ := moverOn(t, root, "current")
			if err := m.Remove("protected"); err == nil {
				t.Fatal("removed protected generation")
			}
		})
	}
}

func TestInvalidateSlot(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "obsolete", "root-b", "0", now.Add(-time.Hour))
	selection, _ := ReadBootSelection(root)
	selection.PreviousKnownGoodGenerationID = "obsolete"
	selection.PreviousKnownGoodBootEntry = "loader/entries/katl-obsolete.conf"
	selection.Generation0FallbackID = "obsolete"
	if err := WriteBootSelection(root, selection); err != nil {
		t.Fatal(err)
	}
	m, _ := moverOn(t, root, "current")
	if err := m.InvalidateSlot("upgrade-1", "22222222-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	state := statusOf(t, root, "obsolete")
	if state.UnavailableReason == "" || IsKnownGood(state) {
		t.Fatalf("invalidated state: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(root, "efi/loader/entries/katl-obsolete.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete boot entry survived")
	}
	items, selection, err := m.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if selection.PreviousKnownGoodGenerationID != "" || selection.Generation0FallbackID != "" {
		t.Fatalf("stale pointers: %+v", selection)
	}
	for _, item := range items {
		if item.Spec.GenerationID == "obsolete" && item.UnavailableReason == "" {
			t.Fatal("obsolete generation available")
		}
	}
	if err := m.InvalidateSlot("upgrade-2", "11111111-2222-3333-4444-555555555555"); err == nil {
		t.Fatal("invalidated running slot")
	}
	removed, err := m.Prune(Retention{}, now)
	if err != nil || !slices.Equal(removed, []string{"obsolete"}) {
		t.Fatalf("invalidated GC: %v %v", removed, err)
	}
}

func TestRetentionLimits(t *testing.T) {
	zero, negative := 0, -1
	for _, policy := range []Retention{{KeepLast: &zero}, {KeepLast: &negative}, {MaxAge: "-1d"}, {MaxAge: "9999999999999d"}, {MaxAge: "tomorrow"}} {
		if _, _, err := policy.Limits(); err == nil {
			t.Fatalf("accepted %+v", policy)
		}
	}
	count, age, err := (Retention{}).Limits()
	if err != nil || count != 5 || age != 30*24*time.Hour {
		t.Fatalf("defaults: %d %s %v", count, age, err)
	}
}

func managementFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	writeBootDisk(t, root)
	managedFixture(t, root, "current", "root-a", "1", now)
	writeBootHealthSelection(t, root, BootSelectionRecord{APIVersion: APIVersion, Kind: BootSelectionKind, DefaultGenerationID: "current", ActiveGenerationID: "current", BootedGenerationID: "current", DefaultBootEntry: "loader/entries/katl-current.conf", BootedBootEntry: "loader/entries/katl-current.conf", UpdatedAt: now})
	return root, now
}

func managedFixture(t *testing.T, root, id, slot, version string, created time.Time) {
	t.Helper()
	uuid := "11111111-2222-3333-4444-555555555555"
	if slot == "root-b" {
		uuid = "22222222-2222-3333-4444-555555555555"
	}
	record := abRecord(t, id, slot, uuid, version, "v1.36.1", created)
	spec := SpecFromRecord(record)
	image := bootRootImage(version)
	spec.Root.RuntimeArtifactSHA256 = fmt.Sprintf("%x", sha256.Sum256(image))
	writeBootFixture(t, bootDevicePath(root, slot), append(image, []byte("old partition tail")...))
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	spec.Boot.UKIPath = "/efi/EFI/katl/" + slot + ".efi"
	setExtensionDigests(t, &spec, "artifact")
	state, err := NewGenerationStatus(spec, CommitStateCommitted, BootStateGood, HealthStateHealthy, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, state); err != nil {
		t.Fatal(err)
	}
	paths := []string{spec.Boot.UKIPath, "/efi/" + spec.Boot.LoaderEntryPath}
	for _, ref := range spec.Sysexts {
		paths = append(paths, ref.Path)
	}
	for _, ref := range spec.Confexts {
		if err := os.MkdirAll(rootedPathUnchecked(root, ref.Path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		writeBootFixture(t, rootedPathUnchecked(root, path), []byte("artifact"))
	}
}

func TestReplaceStagedSlot(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "staged", "root-b", "2", now.Add(time.Hour))
	spec, _, err := ReadGeneration(root, "staged")
	if err != nil {
		t.Fatal(err)
	}
	// A staged candidate has not been committed.
	dir, err := GenerationDir(root, "staged")
	if err != nil {
		t.Fatal(err)
	}
	removeBootFixture(t, filepath.Join(dir, "status.json"))
	candidate, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, candidate); err != nil {
		t.Fatal(err)
	}
	m, efi := moverOn(t, root, "current")
	if _, err := m.ArmTrial(TrialRequest{OperationID: "arm-staged", GenerationID: "staged", Reason: "test", Now: now}); err != nil {
		t.Fatal(err)
	}
	const partitionUUID = "22222222-2222-3333-4444-555555555555"
	if efi.entries.OneShot != "katl-staged.conf" {
		t.Fatalf("firmware one-shot = %q, want the armed candidate", efi.entries.OneShot)
	}

	if err := m.InvalidateSlot("upgrade-1", partitionUUID); err != nil {
		t.Fatal(err)
	}

	selection := selectionOf(t, root)
	if selection.DefaultGenerationID != "current" || selection.TrialGenerationID != "" || selection.TargetBootGenerationID != "" ||
		selection.PendingHealthValidation || selection.OneShot || selection.FailedBootGenerationID != "" || selection.RecoveryRequired {
		t.Fatalf("unsafe replacement: %+v", selection)
	}
	if efi.entries.Default != "katl-current.conf" || efi.entries.OneShot != "" {
		t.Fatalf("firmware = %+v, want the running generation and no one-shot", efi.entries)
	}
	staged := statusOf(t, root, "staged")
	if staged.UnavailableReason == "" || staged.BootState == BootStateFailed || staged.HealthState == HealthStateUnhealthy {
		t.Fatalf("withdrawn candidate status = %+v, want unavailable and not failed", staged)
	}
	if _, err := os.Stat(filepath.Join(root, "efi", spec.Boot.LoaderEntryPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged loader entry remains: %v", err)
	}
	if _, err := os.Stat(rootedPathUnchecked(root, bootMovePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("withdrawal left an intent: %v", err)
	}
}

func TestFailedBootConsumesManualSelection(t *testing.T) {
	for _, oneShot := range []bool{true, false} {
		t.Run(map[bool]string{true: "one-shot", false: "persistent"}[oneShot], func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
			m, efi := moverOn(t, root, "current")
			if err := m.Select(t.Context(), SelectRequest{GenerationID: "old", OneShot: oneShot}); err != nil {
				t.Fatal(err)
			}
			wantDefault := "old"
			if oneShot {
				wantDefault = "current"
			}
			running := restart(t, root, efi)
			if running != "old" {
				t.Fatalf("firmware boots %q, want the selected generation", running)
			}
			efi.written = false

			result, err := newMover(root, efi, "boot-2", running).FailTrial("health failed")
			if err != nil {
				t.Fatal(err)
			}

			if !result.ReportOnly || result.Failed || result.RebootRequested || result.RecoveryRequired || result.DefaultGeneration != wantDefault {
				t.Fatalf("boot health = %#v, want known-good and default %s without reboot", result, wantDefault)
			}
			if status := statusOf(t, root, "old"); !IsKnownGood(status) {
				t.Fatalf("selected generation lost its known-good status: %#v", status)
			}
			selection := selectionOf(t, root)
			if selection.DefaultGenerationID != wantDefault || selection.DefaultBootEntry != "loader/entries/"+efi.entries.Default ||
				selection.TargetBootGenerationID != "" || selection.TargetBootEntry != "" || selection.OneShot {
				t.Fatalf("boot selection = %+v, want default %s and no target", selection, wantDefault)
			}
			if efi.written {
				t.Fatal("a report-only failure wrote firmware variables")
			}
		})
	}
}

func TestInvalidateSlotLeavesSelectionUnchanged(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "obsolete", "root-b", "0", now.Add(-time.Hour))
	path, err := BootSelectionPath(root)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := moverOn(t, root, "current")
	if err := m.InvalidateSlot("upgrade-1", "22222222-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	invalidated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, partitionUUID string
	}{
		{"already invalid slot", "22222222-2222-3333-4444-555555555555"},
		{"slot without generations", "99999999-2222-3333-4444-555555555555"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := m.InvalidateSlot("upgrade-2", test.partitionUUID); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, invalidated) {
				t.Fatalf("selection.json changed:\n%s\nwant:\n%s", after, invalidated)
			}
		})
	}
}

func TestValidateBootSelectionRejectsPaddedGenerationID(t *testing.T) {
	root, _ := managementFixture(t)
	selection := selectionOf(t, root)
	selection.PreviousKnownGoodGenerationID = " gen1"

	if err := ValidateBootSelection(selection); err == nil {
		t.Fatal("ValidateBootSelection accepted a generation ID with leading space")
	}
	data, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	path, err := BootSelectionPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBootSelection(root); err == nil {
		t.Fatal("ReadBootSelection accepted a generation ID with leading space")
	}
}
