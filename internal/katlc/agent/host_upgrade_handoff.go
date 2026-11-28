package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/imagemount"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

// The target release prepares a complete generation in a private state view
// before the source mutates the inactive root slot or arms a boot trial.
func (e *Executor) executeHostUpgradeHandoff(
	ctx context.Context, record operation.OperationRecord, resolve HostUpgradeResolver,
	prepare func(context.Context, katlosimage.Payload, generation.UpgradeHandoff, string) (preparedUpgrade, error),
) (resultErr error) {
	if record.HostUpgradeRequest == nil {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("host upgrade request is required"))
	}
	var payload katlosimage.Payload
	var prepared preparedUpgrade
	var err error
	request := *record.HostUpgradeRequest
	phase := "verify-katlos-image"
	acquired := false
	releaseAttempted := false
	release := func() error {
		releaseAttempted = true
		if err := prepared.close(); err != nil {
			return err
		}
		if acquired {
			return e.releaseHostUpgradeImage(payload, request)
		}
		return nil
	}
	defer func() {
		var cleanupErr error
		if !releaseAttempted && !imagemount.Retained(resultErr) {
			cleanupErr = release()
		}
		resultErr = errors.Join(resultErr, cleanupErr)
		if resultErr != nil {
			resultErr = e.failHostUpgrade(record, phase, resultErr)
		}
	}()
	fail := func(failedPhase string, cause error) error {
		phase = failedPhase
		return cause
	}
	if err := cleanupHostUpgradeStorage(ctx, e.Root, record.HostUpgradeRequest.ImageLocalRef, e.toolRunner(), e.clock()); err != nil {
		return fail("verify-katlos-image", fmt.Errorf("clean obsolete host upgrade workspace: %w", err))
	}
	if err := e.validateMutationBase(record.ExpectedCurrentGenerationID); err != nil {
		return fail("verify-katlos-image", err)
	}
	payload, err = resolve(ctx, request)
	if err != nil {
		return fail("verify-katlos-image", err)
	}
	acquired = true
	if payload.ImagePath == "" || payload.ImageSHA256 == "" || payload.ImageSizeBytes == 0 {
		return fail("verify-katlos-image", fmt.Errorf("target image identity is incomplete"))
	}
	previous, previousStatus, err := generation.ReadGeneration(e.Root, record.ExpectedCurrentGenerationID)
	if err != nil {
		return fail("verify-katlos-image", err)
	}
	kubernetesState, err := inspectKubernetesNodeState(e.Root, e.Store)
	if err != nil {
		return fail("verify-katlos-image", fmt.Errorf("inspect Kubernetes node state: %w", err))
	}
	if err := katlosimage.ValidateHostUpgradeSource(previous, previousStatus, kubernetesState.bootstrapped); err != nil {
		return fail("verify-katlos-image", err)
	}
	if err := katlosimage.ValidateUpgradeCompatibility(previous, payload.Index); err != nil {
		return fail("verify-katlos-image", err)
	}
	if err := kernelcmdline.ValidateRequiredCompatibility(previous.ConfiguredKernelCommandLine, payload.Boot.Compatibility.KernelCommandLine); err != nil {
		return fail("verify-katlos-image", err)
	}
	slot, err := inactiveRoot(previous.Root.Slot)
	if err != nil {
		return fail("verify-katlos-image", err)
	}
	slots, err := e.findRootSlots(ctx, previous.Root.PartitionUUID)
	if err != nil {
		return fail("verify-katlos-image", err)
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
		return fail("verify-katlos-image", err)
	}
	if _, err := generation.RenderEntry(generation.LoaderRequest{Record: bootRecord, MachineID: strings.TrimSpace(string(machineID))}); err != nil {
		return fail("verify-katlos-image", err)
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	if e.MountBootRoot != nil {
		if err := e.MountBootRoot(ctx, bootRoot); err != nil {
			return fail("verify-katlos-image", err)
		}
	}
	handoff := generation.UpgradeHandoff{
		Version: generation.UpgradeHandoffVersion, OperationID: record.OperationID,
		SourceGenerationID: previous.GenerationID, CandidateGenerationID: candidate,
		ImageSHA256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
		RootSlot: slot, RootPartitionUUID: slots.InactivePartUUID,
		UKIPath: ukiPath, LoaderEntryPath: entry, CreatedAt: createdAt,
		KubernetesBootstrapped: kubernetesState.bootstrapped,
	}
	if record.HostUpgradeRequest.ConfigYAML != "" {
		handoff.ConfigurationSHA256 = hostUpgradeConfigurationDigest(record.HostUpgradeRequest.ConfigYAML)
	}
	prepared, err = prepare(ctx, payload, handoff, record.HostUpgradeRequest.ConfigYAML)
	if err != nil {
		return fail("verify-katlos-image", fmt.Errorf("prepare target generation before reboot: %w", err))
	}
	stagingBytes, err := hostUpgradeStagingBytes(payload)
	if err != nil {
		return fail("verify-katlos-image", err)
	}
	candidateDir, err := generation.GenerationDir(prepared.root, handoff.CandidateGenerationID)
	if err != nil {
		return fail("verify-katlos-image", err)
	}
	publicationBytes, err := upgradeTreeBytes(candidateDir)
	if err != nil {
		return fail("verify-katlos-image", fmt.Errorf("measure prepared candidate: %w", err))
	}
	if publicationBytes > stagingBytes {
		stagingBytes = publicationBytes
	}
	if err := requireHostUpgradeStorage(e.AvailableStorage, hostUpgradeArtifactRoot(e.Root), stagingBytes, "root and UKI staging"); err != nil {
		return fail("verify-katlos-image", err)
	}
	if err := requireESPSpace(e.AvailableStorage, e.Root, ukiPath, uint64(payload.Boot.SizeBytes)); err != nil {
		return fail("verify-katlos-image", err)
	}
	phase = "stage-sysupdate-components"
	updatedRecord, err := e.beginHostUpgradeMutation(ctx, record.OperationID, "host-upgrade-handoff-mutation-start", slots.InactivePartUUID)
	if err != nil {
		return err
	}
	record = updatedRecord
	if err := e.prepareSysupdateSlots(ctx, slots); err != nil {
		return fail("stage-sysupdate-components", err)
	}
	if err := e.stageHostUpgrade(ctx, record, payload, slots.Inactive, slot, ukiPath); err != nil {
		return fail("stage-sysupdate-components", err)
	}
	written, err := generation.WriteEntry(bootRoot, generation.LoaderRequest{Record: bootRecord, MachineID: strings.TrimSpace(string(machineID))})
	if err != nil {
		return fail("arm-trial-boot", err)
	}
	if rel, relErr := bootRelativePath(bootRoot, written); relErr != nil || rel != entry {
		return fail("arm-trial-boot", fmt.Errorf("handoff loader entry mismatch: %w", relErr))
	}
	if err := publishPreparedUpgrade(e.Root, prepared, candidate); err != nil {
		return fail("write-candidate-generation", err)
	}
	mover, err := e.bootMover()
	if err != nil {
		return fail("arm-trial-boot", err)
	}
	armedAt, err := mover.ArmTrial(generation.TrialRequest{
		OperationID: record.OperationID, GenerationID: candidate, Now: e.clock(),
		Reason: "host upgrade handoff armed the prepared target for a bounded boot trial",
	})
	if err != nil {
		return fail("arm-trial-boot", err)
	}
	phase = "arm-trial-boot"
	if err := release(); err != nil {
		return fail("arm-trial-boot", err)
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
		current.CompleteBootTrial(armedAt)
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
		Opaque:          true,
		MaxDownloadSize: maxHostUpgradeArtifactSize, RequiredFreeBytes: hostUpgradeStorageReserve,
		AvailableBytes: e.AvailableStorage,
	}).ResolveKatlosImage(ctx, manifest.KatlosImage{
		URL: request.ImageURL, LocalRef: request.ImageLocalRef,
		SHA256: request.ImageSHA256, SizeBytes: request.ImageSizeBytes,
		Role: katlosimage.RoleUpgrade,
	})
}
