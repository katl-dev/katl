package agent

import (
	"context"
	"path/filepath"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func (e *Executor) prepareHostModuleIndexes(ctx context.Context, payload katlosimage.Payload, plan *katlosimage.HostUpgradePlan, extensions hostExtensionPlan) (configapply.PreparedModuleIndexes, error) {
	sources := map[string]string{}
	for _, asset := range plan.PreservedAssets {
		sources[asset.TargetPath] = filepath.Join(runtimeRoot(e.Root), asset.SourcePath)
	}
	for _, asset := range plan.BundledAssets {
		sources[asset.TargetPath] = asset.SourcePath
	}
	prepared, err := configapply.PrepareModuleIndexes(ctx, configapply.ModuleIndexRequest{
		Root:             runtimeRoot(e.Root),
		GenerationID:     plan.Spec.GenerationID,
		Runtime:          plan.Spec.Root,
		Release:          payload.Index.ExtensionRelease,
		RuntimeImagePath: payload.ComponentPath(payload.Runtime),
		Extensions:       extensions.manifest.Node.SystemExtensions,
		Sysexts:          plan.Spec.Sysexts,
		Sources:          sources,
		Materials:        extensions.materials,
		WorkDir:          filepath.Join(runtimeRoot(e.Root), "var/lib/katl/artifacts/host-upgrade"),
	})
	if err != nil || prepared.Path == "" {
		return prepared, err
	}
	plan.Spec.Sysexts = append(plan.Spec.Sysexts, prepared.Ref)
	plan.BundledAssets = append(plan.BundledAssets, katlosimage.BundledAsset{
		Kind:       "module-indexes",
		Name:       prepared.Ref.Name,
		SourcePath: prepared.Path,
		TargetPath: prepared.Ref.Path,
		SHA256:     prepared.Ref.SHA256,
	})
	plan.Status, err = generation.NewGenerationStatus(plan.Spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, e.clock())
	if err != nil {
		_ = prepared.Close()
		return configapply.PreparedModuleIndexes{}, err
	}
	return prepared, nil
}
