package configbundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func resolveSystemExtensionBundles(
	ctx context.Context,
	source SourceConfig,
	image manifest.KatlosImage,
	resolver func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error),
) (SourceConfig, map[string]systemextensionbundle.Resolved, error) {
	if resolver == nil {
		resolver = systemextensionbundle.Resolve
	}
	resolved := make(map[string]systemextensionbundle.Resolved)
	resolveEntries := func(field string, extensions *Optional[[]SourceSystemExtension]) error {
		values, ok := extensions.Get()
		if !ok {
			return nil
		}
		values = cloneSourceSystemExtensions(values)
		for i := range values {
			extension := &values[i]
			state := strings.TrimSpace(extension.State)
			if state == manifest.SystemExtensionAbsent {
				continue
			}
			ref := strings.TrimSpace(extension.Bundle)
			bundle, ok := resolved[ref]
			if !ok {
				var err error
				bundle, err = resolver(ctx, systemextensionbundle.ResolveRequest{
					Reference:        ref,
					Architecture:     strings.TrimSpace(image.Architecture),
					RuntimeInterface: strings.TrimSpace(image.RuntimeInterface),
				})
				if err != nil {
					return fmt.Errorf("%s[%d] %q: %w", field, i, extension.Name, err)
				}
				resolved[ref] = bundle
			}
			desired := bundle.Desired(lowerSystemExtension(*extension))
			extension.resolved = &desired
		}
		*extensions = supplied(values)
		return nil
	}
	if err := resolveEntries("spec.defaults.systemExtensions", &source.Spec.Defaults.SystemExtensions); err != nil {
		return SourceConfig{}, nil, err
	}
	for i := range source.Spec.Nodes {
		if err := resolveEntries(sourceNodePath(source.Spec.Nodes[i], i)+".systemExtensions", &source.Spec.Nodes[i].SystemExtensions); err != nil {
			return SourceConfig{}, nil, err
		}
	}
	return source, resolved, nil
}
