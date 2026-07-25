package configbundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func validateAuthoringSystemExtensions(source SourceConfig) error {
	validate := func(field string, extensions []manifest.SystemExtension) error {
		for i, extension := range extensions {
			if extension.OCIManifestDigest != "" ||
				extension.BundleManifestDigest != "" ||
				extension.ArtifactVersion != "" ||
				extension.PayloadVersion != "" ||
				extension.Architecture != "" ||
				len(extension.SupportedRuntimeInterfaces) != 0 ||
				len(extension.Payloads) != 0 {
				return fmt.Errorf("%s[%d] contains compiler-owned resolution fields; configure only name, state, bundle, configuration, and units", field, i)
			}
		}
		return nil
	}
	if err := validate("spec.defaults.systemExtensions", source.Spec.Defaults.SystemExtensions); err != nil {
		return err
	}
	for i, node := range source.Spec.Nodes {
		if err := validate(fmt.Sprintf("spec.nodes[%d].systemExtensions", i), node.SystemExtensions); err != nil {
			return err
		}
	}
	return nil
}

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
	resolveEntries := func(field string, extensions []manifest.SystemExtension) error {
		for i := range extensions {
			extension := &extensions[i]
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
			*extension = bundle.Desired(*extension)
		}
		return nil
	}
	if err := resolveEntries("spec.defaults.systemExtensions", source.Spec.Defaults.SystemExtensions); err != nil {
		return SourceConfig{}, nil, err
	}
	for i := range source.Spec.Nodes {
		if err := resolveEntries(fmt.Sprintf("spec.nodes[%d].systemExtensions", i), source.Spec.Nodes[i].SystemExtensions); err != nil {
			return SourceConfig{}, nil, err
		}
	}
	return source, resolved, nil
}
