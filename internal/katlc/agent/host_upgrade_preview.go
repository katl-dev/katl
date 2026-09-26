package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

func (s *Server) previewHostUpgrade(ctx context.Context, req *agentapi.SubmitOperationRequest) (*agentapi.HostUpgradePreview, error) {
	if err := s.validateHostUpgradePlan(req.HostUpgrade); err != nil {
		return nil, err
	}
	executor, ok := s.Dispatcher.(*Executor)
	if !ok {
		return nil, fmt.Errorf("host upgrade planner is not available on this agent")
	}
	request := hostUpgradeFromProto(req.HostUpgrade)
	if req.OperationKind == operationKindHostUpgradeHandoff {
		if request.ConfigYAML != "" {
			return nil, fmt.Errorf("combined configuration is not supported by the target preparation operation; upgrade first, then apply configuration")
		}
		payload, err := executor.resolveHostUpgradeOpaque(ctx, request)
		if err != nil {
			return nil, err
		}
		defer executor.cleanupHostUpgradeMount(payload)
		previous, previousStatus, err := generation.ReadGeneration(s.Root, req.ExpectedCurrentGenerationId)
		if err != nil {
			return nil, err
		}
		if err := katlosimage.ValidateHostUpgradeSource(previous, previousStatus, false); err != nil {
			return nil, err
		}
		if payload.Index.Architecture != previous.Root.Architecture || payload.Index.RuntimeInterface != previous.Root.RuntimeInterface {
			return nil, fmt.Errorf("target boot envelope is incompatible with current runtime")
		}
		if err := kernelcmdline.ValidateRequiredCompatibility(previous.ConfiguredKernelCommandLine, payload.Boot.Compatibility.KernelCommandLine); err != nil {
			return nil, err
		}
		slot, err := inactiveRoot(previous.Root.Slot)
		if err != nil {
			return nil, err
		}
		slots, err := executor.inspectRootSlots(ctx, previous.Root.PartitionUUID)
		if err != nil {
			return nil, err
		}
		operationID := "host-upgrade-plan"
		if !req.DryRun {
			operationID = "host-upgrade-acceptance"
		}
		handoff := generation.UpgradeHandoff{
			Version: generation.UpgradeHandoffVersion, OperationID: operationID,
			SourceGenerationID: previous.GenerationID, CandidateGenerationID: request.CandidateGenerationID,
			ImageSHA256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
			RootSlot: slot, RootPartitionUUID: slots.InactivePartUUID,
			UKIPath:         generation.UKIDirectory + "/katl-" + slot + "-1.efi",
			LoaderEntryPath: "loader/entries/katl-" + request.CandidateGenerationID + ".conf",
			CreatedAt:       time.Now().UTC(),
		}
		prepared, err := executor.prepareUpgradeInNamespace(ctx, payload, handoff)
		if err != nil {
			return nil, err
		}
		defer prepared.close()
		return &agentapi.HostUpgradePreview{
			ImageSha256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
			PreviousVersion: previous.RuntimeVersion, Version: payload.Index.Version,
		}, nil
	}
	resolve := executor.ResolveHostUpgrade
	if resolve == nil {
		resolve = executor.resolveHostUpgrade
	}
	payload, err := resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	defer executor.cleanupHostUpgradeMount(payload)
	if payload.ImageSHA256 == "" || payload.ImageSizeBytes == 0 {
		return nil, fmt.Errorf("target image identity is incomplete")
	}
	if req.HostUpgrade.ResolveTargetOnly {
		return &agentapi.HostUpgradePreview{
			ImageSha256:      payload.ImageSHA256,
			ImageSizeBytes:   payload.ImageSizeBytes,
			Version:          payload.Index.Version,
			ExtensionRelease: agentapi.ExtensionReleaseFromManifest(payload.Index.ExtensionRelease),
		}, nil
	}
	prepared, err := executor.planHostUpgrade(ctx, operation.OperationRecord{
		OperationID:                 "host-upgrade-preflight",
		HostUpgradeRequest:          &request,
		ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId,
	}, payload)
	if err != nil {
		return nil, err
	}
	defer prepared.close()
	if !req.DryRun && len(prepared.extensions.document) != 0 {
		// Preserve the acquired configuration in the accepted operation. The
		// executor must not resolve a mutable tag again after preflight.
		req.HostUpgrade.ConfigYaml = string(prepared.extensions.document)
	}
	preview := &agentapi.HostUpgradePreview{
		ChangedDomains:   prepared.domains,
		ImageSha256:      payload.ImageSHA256,
		ImageSizeBytes:   payload.ImageSizeBytes,
		PreviousVersion:  prepared.previous.RuntimeVersion,
		Version:          payload.Index.Version,
		ExtensionRelease: agentapi.ExtensionReleaseFromManifest(payload.Index.ExtensionRelease),
	}
	if prepared.previous.ExtensionRelease != nil {
		preview.PreviousKernel = prepared.previous.ExtensionRelease.Target.Kernel.Release
	}
	changes := map[string]*agentapi.HostUpgradeExtensionChange{}
	for _, ref := range append(append([]generation.ExtensionRef(nil), prepared.previous.Sysexts...), prepared.previous.BundledConfexts...) {
		changes[ref.Name] = &agentapi.HostUpgradeExtensionChange{
			Name:           ref.Name,
			PreviousDigest: ref.SHA256,
		}
	}
	for _, ref := range append(append([]generation.ExtensionRef(nil), prepared.plan.Spec.Sysexts...), prepared.plan.Spec.BundledConfexts...) {
		change := changes[ref.Name]
		if change == nil {
			change = &agentapi.HostUpgradeExtensionChange{Name: ref.Name}
			changes[ref.Name] = change
		}
		change.Digest = ref.SHA256
	}
	for _, change := range changes {
		preview.Extensions = append(preview.Extensions, change)
	}
	sort.Slice(preview.Extensions, func(i, j int) bool { return preview.Extensions[i].Name < preview.Extensions[j].Name })
	return preview, nil
}
