package generation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
func (m *BootMover) Inspect() ([]ManagedGeneration, BootSelectionRecord, error) {
	var items []ManagedGeneration
	var selection BootSelectionRecord
	err := m.Read(func(published BootSelectionRecord) error {
		var err error
		selection = published
		items, err = inspect(m.root, selection, false)
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

func manualBootEligibility(result bootability, status GenerationStatus) error {
	if err := result.problem(); err != nil {
		return err
	}
	if !IsKnownGood(status) {
		return fmt.Errorf("has not passed boot health; use the staged upgrade or configuration workflow")
	}
	return nil
}

// selectedBoot builds a manual selection. Both modes arm the one-shot; a
// persistent selection also becomes the default.
func selectedBoot(root string, selection BootSelectionRecord, request SelectRequest) (*bootMoveRequest, error) {
	if err := unsettledBoot(selection, "selecting a generation"); err != nil {
		return nil, err
	}
	spec, status, err := ReadGeneration(root, request.GenerationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("generation %q does not exist or is incomplete; list generations first", request.GenerationID)
	}
	if err != nil {
		return nil, err
	}
	check := bootabilityCheck{root: root}
	if err := manualBootEligibility(check.check(spec, status, bootFull), status); err != nil {
		return nil, fmt.Errorf("generation %s %w", request.GenerationID, err)
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
	return &bootMoveRequest{
		ExpectedRevision: previous.Revision, Source: previous.DefaultGenerationID, Destination: request.GenerationID,
		Selection: selection, OneShot: request.GenerationID,
	}, nil
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

// invalidatedSlot builds the move that retires every generation in a root
// slot. When the slot holds a boot target, the running generation becomes the
// default, so a failed transfer still boots it; an armed trial whose
// candidate is in the slot is withdrawn. It returns the newly retired specs.
func invalidatedSlot(root string, selection BootSelectionRecord, authority *bootMoveIntent, running, partitionUUID string) (*bootMoveRequest, []GenerationSpec, error) {
	items, err := readManagedGenerations(root)
	if err != nil {
		return nil, nil, err
	}
	next := selection
	invalid := map[string]bool{}
	for _, item := range items {
		if !strings.EqualFold(item.Spec.Root.PartitionUUID, partitionUUID) {
			continue
		}
		id := item.Spec.GenerationID
		if id == running || id == selection.ActiveGenerationID || id == selection.BootedGenerationID {
			return nil, nil, fmt.Errorf("cannot overwrite slot containing selected generation %s; boot and select the running slot first", id)
		}
		invalid[id] = true
	}
	trial := trialNone
	if invalid[selection.DefaultGenerationID] || invalid[selection.TargetBootGenerationID] || invalid[selection.TrialGenerationID] {
		spec, state, err := ReadGeneration(root, running)
		if err != nil {
			return nil, nil, err
		}
		if !IsKnownGood(state) {
			return nil, nil, fmt.Errorf("running generation %s is not a safe boot default", running)
		}
		// The running generation becomes the only boot target, so its own boot
		// files must be present; its root is the running root.
		if err := checkBootFiles(root, spec).problem(); err != nil {
			return nil, nil, fmt.Errorf("cannot replace selected slot: running generation %s %w. Restore its boot artifacts before retrying", running, err)
		}
		next.DefaultGenerationID = running
		next.DefaultBootEntry = spec.Boot.LoaderEntryPath
		next.TargetBootGenerationID, next.TargetBootEntry = "", ""
		next.TrialGenerationID, next.TrialBootEntry = "", ""
		next.OneShot = false
		next.PendingHealthValidation = false
		next.PendingTransactionID = ""
		next.PersistentDefaultPromotion = DefaultPromotionDone
		next.BootCountedTrialPath = ""
		if authority != nil && invalid[*authority.DestinationGenerationID] {
			trial = trialWithdraw
		}
	}
	// Clear obsolete recovery pointers before invalidating their records.
	if invalid[next.PreviousKnownGoodGenerationID] {
		next.PreviousKnownGoodGenerationID, next.PreviousKnownGoodBootEntry = "", ""
	}
	if invalid[next.Generation0FallbackID] {
		next.Generation0FallbackID = ""
	}
	if invalid[next.FailedBootGenerationID] {
		next.FailedBootGenerationID = ""
	}
	var changes []bootStatusChange
	var replaced []GenerationSpec
	for _, item := range items {
		if !invalid[item.Spec.GenerationID] || item.Status.UnavailableReason == slotReplaced {
			continue
		}
		status := item.Status
		changes = append(changes, bootStatusChange{
			GenerationID: item.Spec.GenerationID, Reason: slotReplaced,
			CommitState: status.CommitState, BootState: status.BootState, HealthState: status.HealthState,
			UnavailableReason: new(slotReplaced),
		})
		replaced = append(replaced, item.Spec)
	}
	// A slot that is already invalid leaves the boot state alone.
	if len(changes) == 0 && next == selection {
		return nil, nil, nil
	}
	return &bootMoveRequest{
		ExpectedRevision: selection.Revision, Source: selection.DefaultGenerationID, Destination: next.DefaultGenerationID,
		Selection: next, OneShot: armedOneShot(next), StatusChanges: changes, Trial: trial,
	}, replaced, nil
}

// removedPointers builds the status-less move that drops the selection's
// pointers to a generation about to be removed.
func removedPointers(selection BootSelectionRecord, id string) *bootMoveRequest {
	next := selection
	if next.Generation0FallbackID == id {
		next.Generation0FallbackID = ""
	}
	if next.FailedBootGenerationID == id {
		next.FailedBootGenerationID = ""
	}
	return &bootMoveRequest{
		OperationID: "remove-" + id, ExpectedRevision: selection.Revision,
		Source: selection.DefaultGenerationID, Destination: selection.DefaultGenerationID,
		Selection: next, OneShot: armedOneShot(next),
	}
}
