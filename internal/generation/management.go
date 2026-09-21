package generation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type ManagedGeneration struct {
	Spec              GenerationSpec
	Status            GenerationStatus
	UnavailableReason string
	ProtectedBy       []string
}

// Inspect serializes with selection, boot health and garbage collection.
func Inspect(root string) ([]ManagedGeneration, BootSelectionRecord, error) {
	var items []ManagedGeneration
	var selection BootSelectionRecord
	err := withStateLock(rootedPathUnchecked(root, "/var/lib/katl/boot"), func() error {
		var err error
		selection, err = ReadBootSelection(root)
		if err != nil {
			return err
		}
		items, err = inspect(root, selection)
		return err
	})
	return items, selection, err
}

func inspect(root string, selection BootSelectionRecord) ([]ManagedGeneration, error) {
	entries, err := os.ReadDir(rootedPathUnchecked(root, GenerationRecordsDir))
	if err != nil {
		return nil, err
	}
	var items []ManagedGeneration
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		spec, status, err := ReadGeneration(root, entry.Name())
		if err != nil {
			return nil, err
		}
		items = append(items, ManagedGeneration{Spec: spec, Status: status})
	}
	slices.SortFunc(items, func(a, b ManagedGeneration) int {
		if c := b.Spec.CreatedAt.Compare(a.Spec.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.Spec.GenerationID, a.Spec.GenerationID)
	})
	latest := map[string]GenerationSpec{}
	for _, item := range items {
		if item.Status.CommitState != CommitStateCommitted && item.Status.CommitState != CommitStateSuperseded {
			continue
		}
		if _, ok := latest[item.Spec.Root.PartitionUUID]; !ok {
			latest[item.Spec.Root.PartitionUUID] = item.Spec
		}
	}
	rollbackSlots := map[string]bool{}
	for i := range items {
		item := &items[i]
		id := item.Spec.GenerationID
		item.UnavailableReason = item.Status.UnavailableReason
		if item.UnavailableReason == "" {
			current := latest[item.Spec.Root.PartitionUUID]
			if current.GenerationID != "" && (current.RuntimeVersion != item.Spec.RuntimeVersion || current.Boot.UKIPath != item.Spec.Boot.UKIPath || current.Root.Flavour != item.Spec.Root.Flavour || current.Root.RuntimeArtifactSHA256 != item.Spec.Root.RuntimeArtifactSHA256) {
				item.UnavailableReason = "OS slot has been replaced"
			} else if item.Spec.Boot.LoaderEntryPath == "" {
				item.UnavailableReason = "loader entry is missing"
			} else if item.Status.CommitState != CommitStateCommitted && item.Status.CommitState != CommitStateSuperseded {
				item.UnavailableReason = "generation is not committed"
			} else {
				paths := []string{filepath.Join("/efi", item.Spec.Boot.LoaderEntryPath), filepath.Join("/efi", entryPath(item.Spec.Boot.UKIPath))}
				for _, ref := range item.Spec.Sysexts {
					paths = append(paths, ref.Path)
				}
				for _, ref := range item.Spec.BundledConfexts {
					paths = append(paths, ref.Path)
				}
				for _, ref := range item.Spec.Confexts {
					paths = append(paths, ref.Path)
				}
				for _, path := range paths {
					if _, err := os.Stat(rootedPathUnchecked(root, path)); err != nil {
						item.UnavailableReason = "boot artifact unavailable: " + path
						break
					}
				}
			}
		}
		for _, p := range []struct{ id, reason string }{
			{selection.ActiveGenerationID, "active"},
			{selection.BootedGenerationID, "booted"},
			{selection.DefaultGenerationID, "default"},
			{selection.TargetBootGenerationID, "next boot"},
			{selection.TrialGenerationID, "trial"},
			{selection.PreviousKnownGoodGenerationID, "rollback"},
		} {
			if id == p.id {
				item.ProtectedBy = append(item.ProtectedBy, p.reason)
			}
		}
		if item.UnavailableReason == "" && IsKnownGood(item.Status) && !rollbackSlots[item.Spec.Root.PartitionUUID] {
			item.ProtectedBy = append(item.ProtectedBy, "slot rollback")
			rollbackSlots[item.Spec.Root.PartitionUUID] = true
		}
	}
	// A generation can own extension files still referenced by another generation.
	for i := range items {
		prefix := filepath.ToSlash(filepath.Join(GenerationRecordsDir, items[i].Spec.GenerationID)) + "/"
		for _, other := range items {
			if other.Spec.GenerationID == items[i].Spec.GenerationID {
				continue
			}
			var paths []string
			for _, ref := range other.Spec.Sysexts {
				paths = append(paths, ref.Path)
			}
			for _, ref := range other.Spec.BundledConfexts {
				paths = append(paths, ref.Path)
			}
			for _, ref := range other.Spec.Confexts {
				paths = append(paths, ref.Path)
			}
			for _, path := range paths {
				if strings.HasPrefix(path+"/", prefix) {
					items[i].ProtectedBy = append(items[i].ProtectedBy, "referenced by "+other.Spec.GenerationID)
					break
				}
			}
		}
	}
	return items, nil
}

type SelectRequest struct {
	Root         string
	GenerationID string
	OneShot      bool
	Now          time.Time
	SetDefault   BootDefaultSetter
	SetOneshot   BootDefaultSetter
}

func Select(request SelectRequest) error {
	return withStateLock(rootedPathUnchecked(request.Root, "/var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(request.Root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return fmt.Errorf("finish the pending boot or recover boot health before selecting a generation")
		}
		items, err := inspect(request.Root, selection)
		if err != nil {
			return err
		}
		var selected *ManagedGeneration
		for i := range items {
			if items[i].Spec.GenerationID == request.GenerationID {
				selected = &items[i]
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("generation %q does not exist; list generations first", request.GenerationID)
		}
		if selected.UnavailableReason != "" {
			return fmt.Errorf("generation %s cannot boot: %s", request.GenerationID, selected.UnavailableReason)
		}
		if !IsKnownGood(selected.Status) {
			return fmt.Errorf("generation %s has not passed boot health; use the staged upgrade or configuration workflow", request.GenerationID)
		}
		if request.SetDefault == nil || request.SetOneshot == nil {
			return fmt.Errorf("boot selection setters are required")
		}
		previous := selection
		selection.TargetBootGenerationID = request.GenerationID
		selection.TargetBootEntry = selected.Spec.Boot.LoaderEntryPath
		selection.OneShot = request.OneShot
		if !request.OneShot {
			if selection.DefaultGenerationID != request.GenerationID {
				selection.PreviousKnownGoodGenerationID = previous.DefaultGenerationID
				selection.PreviousKnownGoodBootEntry = previous.DefaultBootEntry
			}
			selection.DefaultGenerationID = request.GenerationID
			selection.DefaultBootEntry = selected.Spec.Boot.LoaderEntryPath
		}
		selection.UpdatedAt = request.Now.UTC()
		if selection.UpdatedAt.IsZero() {
			selection.UpdatedAt = time.Now().UTC()
		}
		// Publish intent before EFI: boot health must understand the selection even
		// if the machine loses power immediately after the firmware variable changes.
		if err := WriteBootSelection(request.Root, selection); err != nil {
			return err
		}
		if !request.OneShot {
			err = request.SetDefault(request.Root, selection.DefaultBootEntry)
		}
		if err == nil {
			err = request.SetOneshot(request.Root, selection.TargetBootEntry)
		}
		if err == nil {
			return nil
		}
		// Restore EFI before restoring intent; a failed compensation leaves the
		// intent visible so the operator can repeat the selection safely.
		restore := request.SetDefault(request.Root, previous.DefaultBootEntry)
		restore = errors.Join(restore, request.SetOneshot(request.Root, previous.TargetBootEntry))
		if restore == nil {
			restore = WriteBootSelection(request.Root, previous)
		}
		return errors.Join(err, restore)
	})
}

func Remove(root, id string) error {
	if _, err := cleanSegment("generation id", id); err != nil {
		return err
	}
	return withStateLock(rootedPathUnchecked(root, "/var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return fmt.Errorf("finish the pending boot or recover boot health before removing generations")
		}
		items, err := inspect(root, selection)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Spec.GenerationID != id {
				continue
			}
			if len(item.ProtectedBy) > 0 {
				return fmt.Errorf("generation %s is protected: %s", id, strings.Join(item.ProtectedBy, ", "))
			}
			if err := clearRemovedPointers(root, &selection, item.Spec.GenerationID); err != nil {
				return err
			}
			return remove(root, item.Spec)
		}
		return nil
	})
}

// Prune applies both retention floors per installed version. Callers exclude
// active operations; the boot lock excludes concurrent boot-health transitions.
func Prune(root string, policy Retention, now time.Time) ([]string, error) {
	count, age, err := policy.Limits()
	if err != nil {
		return nil, err
	}
	var removed []string
	err = withStateLock(rootedPathUnchecked(root, "/var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return nil
		}
		items, err := inspect(root, selection)
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
			if item.Status.UnavailableReason == "" && (counts[key] <= count || !item.Spec.CreatedAt.Before(now.Add(-age))) {
				continue
			}
			if err := clearRemovedPointers(root, &selection, item.Spec.GenerationID); err != nil {
				return err
			}
			if err := remove(root, item.Spec); err != nil {
				return err
			}
			removed = append(removed, item.Spec.GenerationID)
		}
		return nil
	})
	return removed, err
}

func remove(root string, spec GenerationSpec) error {
	if err := removeEntries(root, spec); err != nil {
		return err
	}
	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		return err
	}
	// Hide the complete record atomically before removing its artifacts. Readers
	// never interpret a partially deleted directory as a published generation.
	tombstone := filepath.Join(filepath.Dir(dir), ".removed-"+spec.GenerationID)
	if err := os.Rename(dir, tombstone); err != nil {
		return err
	}
	return os.RemoveAll(tombstone)
}

func removeEntries(root string, spec GenerationSpec) error {
	entry := spec.Boot.LoaderEntryPath
	if entry == "" {
		return nil
	}
	if err := validateBootEntryPath("loader entry", entry); err != nil {
		return err
	}
	if filepath.Dir(entry) != "loader/entries" || filepath.Ext(entry) != ".conf" {
		return fmt.Errorf("unsupported loader entry %q", entry)
	}
	dir := rootedPathUnchecked(root, "/efi/loader/entries")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(entry), ".conf")
	for _, file := range entries {
		name := file.Name()
		if name == base+".conf" || (strings.HasPrefix(name, base+"+") && strings.HasSuffix(name, ".conf")) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// InvalidateSlot removes boot entry points before the caller overwrites a root.
// Metadata stays available for diagnostics and shared artifact ownership until GC.
func InvalidateSlot(root, partitionUUID string, now time.Time, setDefault, setOneshot BootDefaultSetter) error {
	return withStateLock(rootedPathUnchecked(root, "/var/lib/katl/boot"), func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		items, err := inspect(root, selection)
		if err != nil {
			return err
		}
		invalid := map[string]bool{}
		for _, item := range items {
			if !strings.EqualFold(item.Spec.Root.PartitionUUID, partitionUUID) {
				continue
			}
			id := item.Spec.GenerationID
			if id == selection.ActiveGenerationID || id == selection.BootedGenerationID {
				return fmt.Errorf("cannot overwrite slot containing selected generation %s; boot and select the running slot first", id)
			}
			invalid[id] = true
		}
		// Before replacing a staged or default target, point firmware at the
		// healthy running generation. A failed transfer must still boot that slot.
		if invalid[selection.DefaultGenerationID] || invalid[selection.TargetBootGenerationID] || invalid[selection.TrialGenerationID] {
			active := selection.ActiveGenerationID
			if active == "" {
				active = selection.BootedGenerationID
			}
			spec, state, err := ReadGeneration(root, active)
			if err != nil {
				return err
			}
			if !IsKnownGood(state) || invalid[active] {
				return fmt.Errorf("running generation is not a safe boot default")
			}
			if setDefault == nil || setOneshot == nil {
				return fmt.Errorf("boot setters required to replace selected slot")
			}
			if err := setDefault(root, spec.Boot.LoaderEntryPath); err != nil {
				return err
			}
			if err := setOneshot(root, ""); err != nil {
				return err
			}
			selection.DefaultGenerationID = active
			selection.DefaultBootEntry = spec.Boot.LoaderEntryPath
			selection.TargetBootGenerationID = ""
			selection.TargetBootEntry = ""
			selection.TrialGenerationID = ""
			selection.TrialBootEntry = ""
			selection.OneShot = false
			selection.PendingHealthValidation = false
			selection.PendingTransactionID = ""
			selection.PersistentDefaultPromotion = DefaultPromotionDone
			selection.BootCountedTrialPath = ""
		}
		// Clear obsolete recovery pointers before invalidating their records.
		if invalid[selection.PreviousKnownGoodGenerationID] {
			selection.PreviousKnownGoodGenerationID = ""
			selection.PreviousKnownGoodBootEntry = ""
		}
		if invalid[selection.Generation0FallbackID] {
			selection.Generation0FallbackID = ""
		}
		if invalid[selection.FailedBootGenerationID] {
			selection.FailedBootGenerationID = ""
		}
		selection.UpdatedAt = now.UTC()
		if err := WriteBootSelection(root, selection); err != nil {
			return err
		}
		for _, item := range items {
			if !invalid[item.Spec.GenerationID] {
				continue
			}
			item.Status.UnavailableReason = "OS slot has been replaced"
			item.Status.UpdatedAt = now.UTC()
			if err := WriteGenerationStatus(root, item.Spec, item.Status); err != nil {
				return err
			}
			if err := removeEntries(root, item.Spec); err != nil {
				return err
			}
		}
		return nil
	})
}

func clearRemovedPointers(root string, selection *BootSelectionRecord, id string) error {
	changed := false
	if selection.Generation0FallbackID == id {
		selection.Generation0FallbackID = ""
		changed = true
	}
	if selection.FailedBootGenerationID == id {
		selection.FailedBootGenerationID = ""
		changed = true
	}
	if changed {
		return WriteBootSelection(root, *selection)
	}
	return nil
}
