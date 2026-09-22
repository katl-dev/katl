package katlosimage

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

const ExtensionLayoutPath = "components/extensions"

// ResolveReleaseExtension uses the image's immutable local closure. Missing or
// corrupt release artifacts are image defects, not a reason to contact a registry.
func (p Payload) ResolveReleaseExtension(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
	if p.Index.ExtensionRelease == nil {
		return systemextensionbundle.Resolved{}, fmt.Errorf("image has no extension release mapping")
	}
	found := false
	for _, ref := range p.Index.ExtensionRelease.Extensions {
		found = found || ref == request.Reference
	}
	if !found {
		return systemextensionbundle.Resolved{}, fmt.Errorf("extension %q is not advertised by this image", request.Reference)
	}
	request.LayoutDir = filepath.Join(p.Root, ExtensionLayoutPath)
	return systemextensionbundle.Resolve(ctx, request)
}

func (p Payload) validateExtensionClosure(ctx context.Context) error {
	release := p.Index.ExtensionRelease
	if release == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(release.Extensions)) {
		_, _, err := systemextensionbundle.ResolveSelection(ctx, release.Target, release, manifest.SystemExtension{
			Release: name,
		}, p.ResolveReleaseExtension)
		if err != nil {
			return fmt.Errorf("image extension %q: %w", name, err)
		}
	}
	return nil
}
