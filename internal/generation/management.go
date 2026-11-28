package generation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/persistedrecord"
)

type ManagedGeneration struct {
	Spec              GenerationSpec
	Status            GenerationStatus
	UnavailableReason string
	ProtectedBy       []string
	replaced          bool
}

// Inspect lists recorded state and checks artifact presence without reading root images.
func Inspect(root string) ([]ManagedGeneration, BootSelectionRecord, error) {
	var items []ManagedGeneration
	var selection BootSelectionRecord
	err := WithLegacyBootState(root, func() error {
		var err error
		selection, err = ReadBootSelection(root)
		if err != nil {
			return err
		}
		items, err = inspect(root, selection, false)
		return err
	})
	return items, selection, err
}

func readManagedGenerations(root string) ([]ManagedGeneration, error) {
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
	return items, nil
}

func inspect(root string, selection BootSelectionRecord, verifyRoot bool) ([]ManagedGeneration, error) {
	items, err := readManagedGenerations(root)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(items, func(a, b ManagedGeneration) int {
		if c := b.Spec.CreatedAt.Compare(a.Spec.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.Spec.GenerationID, a.Spec.GenerationID)
	})
	type slotArtifact struct{ uuid, digest string }
	acceptedArtifacts := map[slotArtifact]bool{}
	for _, item := range items {
		if verifyRoot && item.Status.UnavailableReason == "" && (item.Status.CommitState == CommitStateCommitted || item.Status.CommitState == CommitStateSuperseded) {
			acceptedArtifacts[slotArtifact{strings.ToLower(item.Spec.Root.PartitionUUID), strings.ToLower(item.Spec.Root.RuntimeArtifactSHA256)}] = true
		}
	}
	check := bootabilityCheck{root: root}
	level := bootArtifacts
	if verifyRoot {
		level = bootFull
	}
	rollbackSlots := map[string]bool{}
	for i := range items {
		item := &items[i]
		id := item.Spec.GenerationID
		uuid := strings.ToLower(item.Spec.Root.PartitionUUID)
		result := check.check(item.Spec, item.Status, level)
		if err := manualBootEligibility(result, item.Status); err != nil {
			item.UnavailableReason = err.Error()
		}
		// A digest that matches another accepted generation shows this slot
		// was replaced. An unknown digest could indicate damage instead.
		item.replaced = result.state == bootUnavailable && acceptedArtifacts[slotArtifact{uuid, result.digest}]
		if result.state == bootIndeterminate {
			item.ProtectedBy = append(item.ProtectedBy, "bootability indeterminate")
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
		if item.UnavailableReason == "" && !rollbackSlots[uuid] {
			item.ProtectedBy = append(item.ProtectedBy, "slot rollback")
			rollbackSlots[uuid] = true
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

func manualBootEligibility(result bootability, status GenerationStatus) error {
	if err := result.problem(); err != nil {
		return err
	}
	if !IsKnownGood(status) {
		return fmt.Errorf("has not passed boot health; use the staged upgrade or configuration workflow")
	}
	return nil
}

func Select(ctx context.Context, request SelectRequest) error {
	return WithLegacyBootState(request.Root, func() error {
		selection, err := ReadBootSelection(request.Root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return fmt.Errorf("finish the pending boot or recover boot health before selecting a generation")
		}
		spec, status, err := ReadGeneration(request.Root, request.GenerationID)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("generation %q does not exist or is incomplete; list generations first", request.GenerationID)
		}
		if err != nil {
			return err
		}
		check := bootabilityCheck{root: request.Root}
		if err := manualBootEligibility(check.check(spec, status, bootFull), status); err != nil {
			return fmt.Errorf("generation %s %w", request.GenerationID, err)
		}
		if request.SetDefault == nil || request.SetOneshot == nil {
			return fmt.Errorf("boot selection setters are required")
		}
		previous := selection
		selection.TargetBootGenerationID = request.GenerationID
		selection.TargetBootEntry = spec.Boot.LoaderEntryPath
		selection.OneShot = request.OneShot
		if !request.OneShot {
			if selection.DefaultGenerationID != request.GenerationID {
				selection.PreviousKnownGoodGenerationID = previous.DefaultGenerationID
				selection.PreviousKnownGoodBootEntry = previous.DefaultBootEntry
			}
			selection.DefaultGenerationID = request.GenerationID
			selection.DefaultBootEntry = spec.Boot.LoaderEntryPath
		}
		selection.UpdatedAt = request.Now.UTC()
		if selection.UpdatedAt.IsZero() {
			selection.UpdatedAt = time.Now().UTC()
		}
		// Publish intent before EFI: boot health must understand the selection even
		// if the machine loses power immediately after the firmware variable changes.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	return WithLegacyBootState(root, func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return fmt.Errorf("finish the pending boot or recover boot health before removing generations")
		}
		items, err := inspect(root, selection, true)
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
	err = WithLegacyBootState(root, func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		if selection.PendingHealthValidation || selection.RecoveryRequired {
			return nil
		}
		items, err := inspect(root, selection, true)
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
			if err := persistedrecord.RemoveFile(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

const slotReplaced = "OS slot has been replaced"

// InvalidateSlot removes boot entry points before the caller overwrites a root.
// Metadata stays available for diagnostics and shared artifact ownership until GC.
func InvalidateSlot(root, partitionUUID string, now time.Time, setDefault, setOneshot BootDefaultSetter) error {
	return WithLegacyBootState(root, func() error {
		selection, err := ReadBootSelection(root)
		if err != nil {
			return err
		}
		before := selection
		items, err := readManagedGenerations(root)
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
			check := bootabilityCheck{root: root}
			if err := check.check(spec, state, bootFiles).problem(); err != nil {
				return fmt.Errorf("cannot replace selected slot: running generation %s %w. Restore its boot artifacts before retrying", active, err)
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
		// A slot that is already invalid leaves the persisted selection alone.
		if selection != before {
			selection.UpdatedAt = now.UTC()
			if err := WriteBootSelection(root, selection); err != nil {
				return err
			}
		}
		for _, item := range items {
			if !invalid[item.Spec.GenerationID] || item.Status.UnavailableReason == slotReplaced {
				continue
			}
			item.Status.UnavailableReason = slotReplaced
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
