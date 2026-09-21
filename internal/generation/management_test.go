package generation

import (
	"errors"
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
			firmwareDefault, firmwareNext := "loader/entries/katl-current.conf", ""
			req := SelectRequest{
				Root: root, GenerationID: "old", OneShot: oneShot, Now: now,
				SetDefault: func(_, entry string) error { firmwareDefault = entry; return nil },
				SetOneshot: func(_, entry string) error { firmwareNext = entry; return nil },
			}
			for range 2 {
				if err := Select(req); err != nil {
					t.Fatal(err)
				}
			}
			if firmwareNext != "loader/entries/katl-old.conf" {
				t.Fatal(firmwareNext)
			}
			wantDefault := "old"
			if oneShot {
				wantDefault = "current"
			}
			if firmwareDefault != "loader/entries/katl-"+wantDefault+".conf" {
				t.Fatal(firmwareDefault)
			}

			// A health service replay before reboot cannot undo the operator's choice.
			if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "current", CommandLine: bootHealthCommandLine("current"), Result: BootHealthSuccess, Now: now, SetBootDefault: req.SetDefault}); err != nil {
				t.Fatal(err)
			}
			queued, _ := ReadBootSelection(root)
			if queued.TargetBootGenerationID != "old" || queued.DefaultGenerationID != wantDefault {
				t.Fatalf("health replay consumed selection: %+v", queued)
			}
			// Health replay must not promote a one-shot boot, including after restart.
			for range 2 {
				if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "old", CommandLine: bootHealthCommandLine("old"), Result: BootHealthSuccess, Now: now, SetBootDefault: req.SetDefault}); err != nil {
					t.Fatal(err)
				}
				selection, err := ReadBootSelection(root)
				if err != nil {
					t.Fatal(err)
				}
				if selection.DefaultGenerationID != wantDefault || selection.ActiveGenerationID != "old" || selection.TargetBootGenerationID != "" {
					t.Fatalf("selection after boot: %+v", selection)
				}
			}
			if oneShot {
				if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "current", CommandLine: bootHealthCommandLine("current"), Result: BootHealthSuccess, Now: now, SetBootDefault: req.SetDefault}); err != nil {
					t.Fatal(err)
				}
				selection, _ := ReadBootSelection(root)
				if selection.ActiveGenerationID != "current" || selection.OneShot {
					t.Fatalf("return boot: %+v", selection)
				}
			}
		})
	}
}

func TestSelectionFailureRestoresDefault(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
	entry := "loader/entries/katl-current.conf"
	req := SelectRequest{Root: root, GenerationID: "old", Now: now, SetDefault: func(_, value string) error { entry = value; return nil }, SetOneshot: func(_, value string) error {
		if value != "" {
			return errors.New("EFI unavailable")
		}
		return nil
	}}
	if err := Select(req); err == nil {
		t.Fatal("expected EFI failure")
	}
	selection, _ := ReadBootSelection(root)
	if selection.DefaultGenerationID != "current" || selection.TargetBootGenerationID != "" || entry != "loader/entries/katl-current.conf" {
		t.Fatalf("selection=%+v entry=%s", selection, entry)
	}
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
	count := 1
	removed, err := Prune(root, Retention{KeepLast: &count, MaxAge: "30d"}, now)
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
	if again, err := Prune(root, Retention{KeepLast: &count, MaxAge: "30d"}, now); err != nil || len(again) != 0 {
		t.Fatalf("repeat: %v %v", again, err)
	}
	// The newest healthy generation of the previous OS remains even after aging.
	if err := Remove(root, "b-newest"); err == nil || !strings.Contains(err.Error(), "slot rollback") {
		t.Fatalf("rollback removal: %v", err)
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
			if err := Remove(root, "protected"); err == nil {
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
	if err := InvalidateSlot(root, "22222222-2222-3333-4444-555555555555", now, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, state, err := ReadGeneration(root, "obsolete")
	if err != nil || state.UnavailableReason == "" || IsKnownGood(state) {
		t.Fatalf("invalidated state: %+v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(root, "efi/loader/entries/katl-obsolete.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete boot entry survived")
	}
	items, selection, err := Inspect(root)
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
	if err := InvalidateSlot(root, "11111111-2222-3333-4444-555555555555", now, nil, nil); err == nil {
		t.Fatal("invalidated running slot")
	}
	removed, err := Prune(root, Retention{}, now)
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
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	spec.Boot.UKIPath = "/efi/EFI/katl/" + slot + ".efi"
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
		paths = append(paths, ref.Path)
	}
	for _, path := range paths {
		full := rootedPathUnchecked(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplaceStagedSlot(t *testing.T) {
	root, now := managementFixture(t)
	managedFixture(t, root, "staged", "root-b", "2", now.Add(time.Hour))
	selection, _ := ReadBootSelection(root)
	selection.TargetBootGenerationID = "staged"
	selection.TargetBootEntry = "loader/entries/katl-staged.conf"
	selection.TrialGenerationID = "staged"
	selection.TrialBootEntry = selection.TargetBootEntry
	selection.PendingHealthValidation = true
	selection.PersistentDefaultPromotion = DefaultPromotionPending
	if err := WriteBootSelection(root, selection); err != nil {
		t.Fatal(err)
	}
	firmwareDefault, firmwareNext := "loader/entries/katl-current.conf", selection.TargetBootEntry
	err := InvalidateSlot(root, "22222222-2222-3333-4444-555555555555", now,
		func(_, entry string) error { firmwareDefault = entry; return nil }, func(_, entry string) error { firmwareNext = entry; return nil })
	if err != nil {
		t.Fatal(err)
	}
	// If the replacement write subsequently fails, firmware still boots current.
	selection, _ = ReadBootSelection(root)
	if firmwareDefault != "loader/entries/katl-current.conf" || firmwareNext != "" || selection.PendingHealthValidation || selection.TargetBootGenerationID != "" {
		t.Fatalf("unsafe replacement: %+v %s %s", selection, firmwareDefault, firmwareNext)
	}
}

func TestSelectedBootFailure(t *testing.T) {
	for _, oneShot := range []bool{true, false} {
		t.Run(map[bool]string{true: "one-shot", false: "persistent"}[oneShot], func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "old", "root-a", "1", now.Add(-time.Hour))
			entry := "loader/entries/katl-current.conf"
			setter := func(_, value string) error { entry = value; return nil }
			if err := Select(SelectRequest{Root: root, GenerationID: "old", OneShot: oneShot, Now: now, SetDefault: setter, SetOneshot: func(string, string) error { return nil }}); err != nil {
				t.Fatal(err)
			}
			if _, err := RecordBootHealth(BootHealthRequest{Root: root, GenerationID: "old", CommandLine: bootHealthCommandLine("old"), Result: BootHealthFailure, Now: now, SetBootDefault: setter}); err != nil {
				t.Fatal(err)
			}
			selection, _ := ReadBootSelection(root)
			if selection.DefaultGenerationID != "current" || selection.OneShot || entry != "loader/entries/katl-current.conf" {
				t.Fatalf("failure recovery: %+v %s", selection, entry)
			}
		})
	}
}
