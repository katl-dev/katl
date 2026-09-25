package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
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
	if executor.activeHostUpgradeImage(payload, req.HostUpgrade.GetConfigYaml()) {
		return &agentapi.HostUpgradePreview{
			ImageSha256:    payload.ImageSHA256,
			ImageSizeBytes: payload.ImageSizeBytes,
			Version:        payload.Index.Version,
			NoChanges:      true,
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
	if err != nil || record.OperationKind != OperationKindHostUpgrade || !record.Terminal || record.Result != operation.ResultSucceeded || record.CandidateGenerationID != currentID || record.HostUpgradeRequest == nil {
		return false
	}
	// The operation that committed this generation owns its complete image identity.
	return record.HostUpgradeRequest.ImageSHA256 == payload.ImageSHA256 && record.HostUpgradeRequest.ImageSizeBytes == payload.ImageSizeBytes
}
