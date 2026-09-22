package configbundle

import (
	"context"
	"fmt"
	"slices"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func resolveSystemExtensionBundles(ctx context.Context, source SourceConfig, planning PlanningInputs, fetch func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error)) (SourceConfig, map[string]systemextensionbundle.Resolved, error) {
	if fetch == nil {
		fetch = systemextensionbundle.Resolve
	}
	// Merge before acquisition: removed or overridden defaults must not require
	// artifacts, and each node resolves against its own runtime identity.
	bundles := make(map[string]systemextensionbundle.Resolved)
	cache := make(map[systemextensionbundle.ResolveRequest]systemextensionbundle.Resolved)
	resolve := func(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		request.LayoutURL = planning.ExtensionLayoutURLs[request.Reference]
		if bundle, ok := cache[request]; ok {
			return bundle, nil
		}
		bundle, err := fetch(ctx, request)
		if err == nil {
			cache[request] = bundle
			bundles[request.Reference] = bundle
		}
		return bundle, err
	}
	for i := range source.Spec.Nodes {
		node := &source.Spec.Nodes[i]
		if len(planning.Nodes) > 0 && !slices.Contains(planning.Nodes, node.Name) {
			node.SystemExtensions = supplied([]SourceSystemExtension{})
			continue
		}
		entries, err := mergeSourceSystemExtensions(source.Spec.Defaults.SystemExtensions, node.SystemExtensions)
		if err != nil {
			return SourceConfig{}, nil, err
		}
		values, _ := entries.Get()
		release := planning.KatlosImage.ExtensionRelease
		if perNode, ok := planning.ExtensionReleases[node.Name]; ok {
			release = &perNode
		}
		target := extensionrelease.Target{
			Architecture:     planning.KatlosImage.Architecture,
			RuntimeInterface: planning.KatlosImage.RuntimeInterface,
		}
		if release != nil {
			target = release.Target
		}
		for j := range values {
			desired, _, err := systemextensionbundle.ResolveSelection(ctx, target, release, lowerSystemExtension(values[j]), resolve)
			if err != nil {
				return SourceConfig{}, nil, fmt.Errorf("node %q system extension %q: %w", node.Name, values[j].repository(), err)
			}
			values[j].resolved = &desired
		}
		node.SystemExtensions = supplied(values)
	}
	source.Spec.Defaults.SystemExtensions = Optional[[]SourceSystemExtension]{}
	return source, bundles, nil
}
