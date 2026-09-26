package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

// The target release prepares a complete generation in a private state view
// before the source mutates the inactive root slot or arms a boot trial.
func (e *Executor) executeHostUpgradeHandoff(ctx context.Context, record operation.OperationRecord) error {
	if record.HostUpgradeRequest == nil || record.HostUpgradeRequest.ConfigYAML != "" {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("target-prepared host upgrades do not yet support --apply-config; upgrade first, then apply configuration"))
	}
	if err := generation.ValidateMutationBase(e.Root, record.ExpectedCurrentGenerationID); err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	payload, err := e.resolveHostUpgradeOpaque(ctx, *record.HostUpgradeRequest)
	if err != nil {
		e.cleanupManagedHostUpgradeArtifact(*record.HostUpgradeRequest)
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	defer e.cleanupManagedHostUpgradeArtifact(*record.HostUpgradeRequest)
	defer e.cleanupHostUpgradeMount(payload)
	if payload.ImagePath == "" || payload.ImageSHA256 == "" || payload.ImageSizeBytes == 0 {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("target image identity is incomplete"))
	}
	previous, previousStatus, err := generation.ReadGeneration(e.Root, record.ExpectedCurrentGenerationID)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	if err := katlosimage.ValidateHostUpgradeSource(previous, previousStatus, false); err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	if payload.Index.Architecture != previous.Root.Architecture || payload.Index.RuntimeInterface != previous.Root.RuntimeInterface {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("target boot envelope is incompatible with current runtime"))
	}
	if err := kernelcmdline.ValidateRequiredCompatibility(previous.ConfiguredKernelCommandLine, payload.Boot.Compatibility.KernelCommandLine); err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	slot, err := inactiveRoot(previous.Root.Slot)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	slots, err := e.inspectRootSlots(ctx, previous.Root.PartitionUUID)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	candidate := record.HostUpgradeRequest.CandidateGenerationID
	ukiPath := generation.UKIDirectory + "/katl-" + slot + "-1.efi"
	entry := "loader/entries/katl-" + candidate + ".conf"
	createdAt := e.clock()
	bootRecord := generation.Record{
		GenerationID: candidate, RuntimeVersion: payload.Index.Version,
		Root:              generation.RootSelection{Slot: slot, PartitionUUID: slots.InactivePartUUID},
		Boot:              generation.BootSelection{UKIPath: ukiPath, LoaderEntryPath: entry},
		KernelCommandLine: kernelcmdline.MergeCurrent(payload.Boot.Compatibility.KernelCommandLine, previous.KernelCommandLine, nil),
		CreatedAt:         createdAt,
	}
	machineID, err := os.ReadFile(filepath.Join(runtimeRoot(e.Root), "etc/machine-id"))
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	if _, err := generation.RenderEntry(generation.LoaderRequest{Record: bootRecord, MachineID: strings.TrimSpace(string(machineID))}); err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	if e.MountBootRoot != nil {
		if err := e.MountBootRoot(ctx, bootRoot); err != nil {
			return e.failHostUpgrade(record, "verify-katlos-image", err)
		}
	}
	handoff := generation.UpgradeHandoff{
		Version: generation.UpgradeHandoffVersion, OperationID: record.OperationID,
		SourceGenerationID: previous.GenerationID, CandidateGenerationID: candidate,
		ImageSHA256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
		RootSlot: slot, RootPartitionUUID: slots.InactivePartUUID,
		UKIPath: ukiPath, LoaderEntryPath: entry, CreatedAt: createdAt,
	}
	prepared, err := e.prepareUpgradeInNamespace(ctx, payload, handoff)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("prepare target generation before reboot: %w", err))
	}
	defer prepared.close()
	record, err = e.Store.Update(record.OperationID, "host-upgrade-handoff-mutation-start", "stage-sysupdate-components", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "stage-sysupdate-components"
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "root-slot-labels", "runtime-root", "runtime-uki")
		current.UpdatedAt = e.clock()
		return current, nil
	})
	if err != nil {
		return err
	}
	if err := generation.InvalidateSlot(e.Root, slots.InactivePartUUID, e.clock(),
		func(root, entry string) error { return e.SetBootDefault(ctx, root, entry) },
		func(root, entry string) error { return e.SetBootOneshot(ctx, root, entry) }); err != nil {
		return e.failHostUpgrade(record, "stage-sysupdate-components", err)
	}
	if err := e.prepareSysupdateSlots(ctx, slots); err != nil {
		return e.failHostUpgrade(record, "stage-sysupdate-components", err)
	}
	if err := e.stageHostUpgrade(ctx, record, payload, slots.InactiveDevice, slot, ukiPath); err != nil {
		return e.failHostUpgrade(record, "stage-sysupdate-components", err)
	}
	written, err := generation.WriteEntry(bootRoot, generation.LoaderRequest{Record: bootRecord, MachineID: strings.TrimSpace(string(machineID))})
	if err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	if rel, relErr := bootRelativePath(bootRoot, written); relErr != nil || rel != entry {
		return e.failHostUpgrade(record, "arm-trial-boot", fmt.Errorf("handoff loader entry mismatch: %w", relErr))
	}
	if err := publishPreparedUpgrade(e.Root, prepared, candidate); err != nil {
		return e.failHostUpgrade(record, "write-candidate-generation", err)
	}
	selection, err := generation.ReadBootSelection(e.Root)
	if err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	previousSelection := selection
	selection.TargetBootGenerationID = candidate
	selection.TrialGenerationID = candidate
	selection.PreviousKnownGoodGenerationID = previous.GenerationID
	selection.TargetBootEntry = entry
	selection.TrialBootEntry = entry
	selection.PreviousKnownGoodBootEntry = previous.Boot.LoaderEntryPath
	selection.PendingTransactionID = record.OperationID
	selection.PendingHealthValidation = true
	selection.PersistentDefaultPromotion = generation.DefaultPromotionPending
	selection.UpdatedAt = e.clock()
	if err := e.SetBootDefault(ctx, e.Root, previous.Boot.LoaderEntryPath); err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", fmt.Errorf("retain source boot default: %w", err))
	}
	if err := generation.WriteBootSelection(e.Root, selection); err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	if err := e.SetBootOneshot(ctx, e.Root, entry); err != nil {
		if restoreErr := generation.WriteBootSelection(e.Root, previousSelection); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	_, err = e.Store.Update(record.OperationID, "host-upgrade-handoff-staged", "arm-trial-boot", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.PreviousGenerationID = previous.GenerationID
		current.CompletedPhases = appendMissing(current.CompletedPhases, "accepted", "verify-katlos-image", "stage-sysupdate-components", "write-candidate-generation", "arm-trial-boot")
		current.Phase = "arm-trial-boot"
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "runtime-root", "runtime-uki", "boot-selection", "generation-state")
		current.ActivationState = operation.ActivationStatePending
		current.HostRollback = previous.GenerationID
		current.PostMutationRollbackAllowed = true
		current.NextAction = "reboot into the prepared target generation"
		current.CompleteBootTrial(e.clock())
		return current, nil
	})
	return err
}

func (e *Executor) resolveHostUpgradeOpaque(ctx context.Context, request operation.HostUpgrade) (katlosimage.Payload, error) {
	root := runtimeRoot(e.Root)
	return (katlosimage.Resolver{
		MediaRoot: filepath.Join(root, "var/lib/katl/artifacts"),
		WorkDir:   filepath.Join(root, "var/lib/katl/artifacts/host-upgrade"),
		Commands:  hostUpgradeCommands{run: e.toolRunner()}, Client: e.BundleClient,
		Opaque: true,
	}).ResolveKatlosImage(ctx, manifest.KatlosImage{
		URL: request.ImageURL, LocalRef: request.ImageLocalRef,
		SHA256: request.ImageSHA256, SizeBytes: request.ImageSizeBytes,
		Role: katlosimage.RoleUpgrade,
	})
}
