package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

func (e *Executor) planUpgradeConfig(ctx context.Context, candidate, document string, payload katlosimage.Payload) (hostExtensionPlan, []confext.NativeEtcFile, []string, error) {
	base, err := configApplyBase(e.Root, "", candidate, e.clock)
	if err != nil {
		return hostExtensionPlan{}, nil, nil, err
	}
	base.CurrentRecord.Root.RuntimeVersion = payload.Index.Version
	base.CurrentRecord.Root.RuntimeInterface = payload.Index.RuntimeInterface
	base.CurrentRecord.Root.Architecture = payload.Index.Architecture
	base.CurrentRecord.Root.Flavour = payload.Index.Flavour
	base.CurrentRecord.Root.RuntimeArtifactSHA256 = payload.Runtime.SHA256
	base.CurrentRecord.RuntimeVersion = payload.Index.Version
	base.CurrentRecord.ExtensionRelease = payload.Index.ExtensionRelease
	base.VolumeBindings = slices.Clone(base.CurrentRecord.VolumeBindings)
	base.VolumeBindingsSet = true
	fetch := e.ResolveSystemExtension
	if fetch == nil {
		fetch = func(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
			if payload.Index.ExtensionRelease != nil {
				for _, ref := range payload.Index.ExtensionRelease.Extensions {
					if ref == request.Reference {
						return payload.ResolveReleaseExtension(ctx, request)
					}
				}
			}
			return systemextensionbundle.Resolve(ctx, request)
		}
	}
	prepared, err := configapply.PrepareNodeConfigurationChange(ctx, document, base, fetch)
	if err != nil {
		return hostExtensionPlan{}, nil, nil, err
	}
	request := prepared.Request
	request.ApplyMode = generation.ApplyModeNextBoot
	result, err := configapply.PlanTrustedBundle(request)
	if errors.Is(err, configapply.ErrNoChanges) {
		return hostExtensionPlan{document: prepared.Document}, nil, nil, configapply.ErrNoChanges
	}
	if err != nil {
		return hostExtensionPlan{}, nil, nil, fmt.Errorf("combined host configuration: %w", err)
	}
	for _, domain := range result.Plan.Decision.ChangedDomains {
		switch domain {
		case configapply.DomainGenerationRetention, configapply.DomainNodeIdentity,
			configapply.DomainSSHOperatorAccess, configapply.DomainKernelCommandLine,
			configapply.DomainSystemExtensions, configapply.DomainHostConfiguration,
			configapply.DomainModulesLoad, configapply.DomainTmpfiles,
			configapply.DomainResolved, configapply.DomainAPIProxy:
		default:
			return hostExtensionPlan{}, nil, nil, fmt.Errorf("--apply-config cannot combine %s with an OS upgrade; use the configuration, Kubernetes, or storage workflow separately", domain)
		}
	}
	if _, err := confext.ValidateNativeEtcBundle("", result.Files); err != nil {
		return hostExtensionPlan{}, nil, nil, err
	}
	plan := hostExtensionPlan{
		document:  prepared.Document,
		manifest:  result.Manifest,
		desired:   result.Manifest.Node.SystemExtensions,
		materials: request.SystemExtensionPayloads,
	}
	for _, selected := range base.CurrentManifest.Node.SystemExtensions {
		for _, old := range selected.Payloads {
			plan.replace = append(plan.replace, selected.PayloadID(old.Name))
		}
	}
	plan.sysexts, plan.confexts, err = configapply.PlanSystemExtensions(candidate, base.CurrentRecord.Root, payload.Index.ExtensionRelease, plan.desired, plan.materials)
	if err != nil {
		return hostExtensionPlan{}, nil, nil, err
	}
	return plan, result.Files, result.Plan.Decision.ChangedDomains, nil
}

func (e *Executor) prepareUpgradeConfig(plan *katlosimage.HostUpgradePlan, desired manifest.Manifest, files []confext.NativeEtcFile) (string, error) {
	parent := filepath.Join(runtimeRoot(e.Root), "var/lib/katl/artifacts/host-upgrade")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(parent, "configuration-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(work)
		}
	}()
	found := false
	for i, ref := range plan.Spec.Confexts {
		if !generation.IsGeneratedConfextName(ref.Name) {
			continue
		}
		found = true
		tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
			GenerationsRoot: work,
			GenerationID:    plan.Spec.GenerationID,
			Files:           files,
			Extension: confext.ExtensionRelease{
				Name:         ref.Name,
				ID:           ref.Compatibility.ID,
				VersionID:    ref.Compatibility.VersionID,
				ConfextLevel: ref.Compatibility.ConfextLevel,
			},
		})
		if err != nil {
			return "", err
		}
		digest, err := generation.DigestDirectory(tree.ConfextDir)
		if err != nil {
			return "", err
		}
		plan.Spec.Confexts[i].SHA256 = digest
		plan.PreservedAssets = slices.DeleteFunc(plan.PreservedAssets, func(asset katlosimage.PreservedAsset) bool { return asset.TargetPath == ref.Path })
		relative, err := filepath.Rel(runtimeRoot(e.Root), tree.ConfextDir)
		if err != nil {
			return "", err
		}
		plan.PreservedAssets = append(plan.PreservedAssets, katlosimage.PreservedAsset{
			Kind:       "confext",
			Name:       ref.Name,
			SourcePath: "/" + filepath.ToSlash(relative),
			TargetPath: ref.Path,
			Directory:  true,
			SHA256:     digest,
		})
	}
	if !found {
		return "", fmt.Errorf("current generation has no generated host configuration layer")
	}
	plan.Spec.KernelCommandLine = kernelcmdline.ReplaceConfigured(plan.Spec.KernelCommandLine, plan.Spec.ConfiguredKernelCommandLine, desired.Node.Kernel.CommandLine)
	plan.Spec.ConfiguredKernelCommandLine = slices.Clone(desired.Node.Kernel.CommandLine)
	plan.Status, err = generation.NewGenerationStatus(plan.Spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, e.clock())
	if err != nil {
		return "", err
	}
	ok = true
	return work, nil
}
