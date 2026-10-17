package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/imagemount"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

func (s *Server) previewHostUpgrade(ctx context.Context, req *agentapi.SubmitOperationRequest) (preview *agentapi.HostUpgradePreview, resultErr error) {
	if err := s.validateHostUpgradePlan(req.HostUpgrade); err != nil {
		return nil, err
	}
	executor, ok := s.Dispatcher.(*Executor)
	if !ok {
		return nil, fmt.Errorf("host upgrade planner is not available on this agent")
	}
	request := hostUpgradeFromProto(req.HostUpgrade)
	if err := cleanupHostUpgradeStorage(ctx, s.Root, request.ImageLocalRef, executor.toolRunner(), s.clock()); err != nil {
		return nil, fmt.Errorf("clean obsolete host upgrade workspace: %w", err)
	}
	if req.OperationKind == operationKindHostUpgradeHandoff {
		payload, err := executor.resolveHostUpgradeOpaque(ctx, request)
		if err != nil {
			return nil, err
		}
		var prepared preparedUpgrade
		defer func() {
			if imagemount.Retained(resultErr) {
				preview = nil
				return
			}
			if err := prepared.close(); err != nil {
				preview = nil
				resultErr = errors.Join(resultErr, err)
				return
			}
			if err := payload.Close(); err != nil {
				preview = nil
				resultErr = errors.Join(resultErr, err)
			}
		}()
		previous, previousStatus, err := generation.ReadGeneration(s.Root, req.ExpectedCurrentGenerationId)
		if err != nil {
			return nil, err
		}
		kubernetesState, err := inspectKubernetesNodeState(s.Root, s.Store)
		if err != nil {
			return nil, fmt.Errorf("inspect Kubernetes node state: %w", err)
		}
		if err := katlosimage.ValidateHostUpgradeSource(previous, previousStatus, kubernetesState.bootstrapped); err != nil {
			return nil, err
		}
		if err := katlosimage.ValidateUpgradeCompatibility(previous, payload.Index); err != nil {
			return nil, err
		}
		if err := kernelcmdline.ValidateRequiredCompatibility(previous.ConfiguredKernelCommandLine, payload.Boot.Compatibility.KernelCommandLine); err != nil {
			return nil, err
		}
		if executor.activeHostUpgradeImage(payload, request.ConfigYAML) {
			return &agentapi.HostUpgradePreview{
				ImageSha256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
				PreviousVersion: previous.RuntimeVersion, Version: payload.Index.Version,
				NoChanges: true,
			}, nil
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
			UKIPath:                generation.UKIDirectory + "/katl-" + slot + "-1.efi",
			LoaderEntryPath:        "loader/entries/katl-" + request.CandidateGenerationID + ".conf",
			CreatedAt:              time.Now().UTC(),
			KubernetesBootstrapped: kubernetesState.bootstrapped,
		}
		if request.ConfigYAML != "" {
			handoff.ConfigurationSHA256 = hostUpgradeConfigurationDigest(request.ConfigYAML)
		}
		prepared, err = executor.prepareUpgradeInNamespace(ctx, payload, handoff, request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		return &agentapi.HostUpgradePreview{
			ImageSha256: payload.ImageSHA256, ImageSizeBytes: payload.ImageSizeBytes,
			PreviousVersion: previous.RuntimeVersion, Version: payload.Index.Version,
			ChangedDomains: prepared.result.ChangedDomains,
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
	var prepared preparedHostUpgrade
	defer func() {
		if imagemount.Retained(resultErr) {
			preview = nil
			return
		}
		if err := prepared.close(); err != nil {
			preview = nil
			resultErr = errors.Join(resultErr, err)
			return
		}
		if err := payload.Close(); err != nil {
			preview = nil
			resultErr = errors.Join(resultErr, err)
		}
	}()
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
	if executor.activeHostUpgradeImage(payload, req.HostUpgrade.GetConfigYaml()) {
		return &agentapi.HostUpgradePreview{
			ImageSha256:    payload.ImageSHA256,
			ImageSizeBytes: payload.ImageSizeBytes,
			Version:        payload.Index.Version,
			NoChanges:      true,
		}, nil
	}
	prepared, err = executor.planHostUpgrade(ctx, operation.OperationRecord{
		OperationID:                 "host-upgrade-preflight",
		HostUpgradeRequest:          &request,
		ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId,
	}, payload)
	if err != nil {
		return nil, err
	}
	if !req.DryRun && len(prepared.extensions.document) != 0 {
		// Preserve the acquired configuration in the accepted operation. The
		// executor must not resolve a mutable tag again after preflight.
		req.HostUpgrade.ConfigYaml = string(prepared.extensions.document)
	}
	preview = &agentapi.HostUpgradePreview{
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

func (e *Executor) activeHostUpgradeImage(payload katlosimage.Payload, configYAML string) bool {
	if configYAML != "" {
		return false
	}
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return false
	}
	current, state, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil || !generation.IsKnownGood(state) || state.CommittedByOperation == "" || current.RuntimeVersion != payload.Index.Version {
		return false
	}
	record, err := e.Store.Read(state.CommittedByOperation)
	if err != nil || (record.OperationKind != OperationKindHostUpgrade && record.OperationKind != operationKindHostUpgradeHandoff) || !record.Terminal || record.Result != operation.ResultSucceeded || record.CandidateGenerationID != currentID || record.HostUpgradeRequest == nil {
		return false
	}
	// The operation that committed this generation owns its complete image identity.
	return record.HostUpgradeRequest.ImageSHA256 == payload.ImageSHA256 && record.HostUpgradeRequest.ImageSizeBytes == payload.ImageSizeBytes
}
