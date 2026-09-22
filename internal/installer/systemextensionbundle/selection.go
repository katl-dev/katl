package systemextensionbundle

import (
	"context"
	"fmt"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
)

// ResolveSelection acquires an artifact using the same selection and target
// rules used to reuse retained generation assets.
func ResolveSelection(ctx context.Context, target extensionrelease.Target, release *extensionrelease.Manifest, source manifest.SystemExtension, fetch func(context.Context, ResolveRequest) (Resolved, error)) (manifest.SystemExtension, Resolved, error) {
	ref, err := selectionReference(target, release, source)
	if err != nil {
		return manifest.SystemExtension{}, Resolved{}, err
	}
	if source.State == manifest.SystemExtensionAbsent {
		return source, Resolved{}, nil
	}
	if fetch == nil {
		fetch = Resolve
	}
	resolved, err := fetch(ctx, ResolveRequest{
		Reference:        ref,
		Architecture:     target.Architecture,
		RuntimeInterface: target.RuntimeInterface,
		RuntimeSHA256:    target.Kernel.RuntimeSHA256,
	})
	if err != nil {
		return manifest.SystemExtension{}, Resolved{}, err
	}
	desired, err := resolvedSelection(target, ref, resolved.Desired(source))
	return desired, resolved, err
}

// ReuseSelection selects metadata from an already verified generation only when
// its artifact matches the target's exact pin. It does not reconstruct OCI
// objects; the caller must verify the retained native payloads before use.
func ReuseSelection(target extensionrelease.Target, release *extensionrelease.Manifest, source manifest.SystemExtension, retained []manifest.SystemExtension) (manifest.SystemExtension, bool, error) {
	ref, err := selectionReference(target, release, source)
	if err != nil {
		return manifest.SystemExtension{}, false, err
	}
	if source.State == manifest.SystemExtensionAbsent {
		return source, true, nil
	}
	parsed, err := payloadbundle.ParseReference(ref)
	if err != nil {
		return manifest.SystemExtension{}, false, err
	}
	pin := payloadbundle.ManifestDigest(parsed)
	if pin == "" {
		return manifest.SystemExtension{}, false, nil
	}
	for _, previous := range retained {
		if previous.Repository() != source.Repository() || previous.OCIManifestDigest != pin || previous.State == manifest.SystemExtensionAbsent {
			continue
		}
		previous.Release = source.Release
		previous.Bundle = source.Bundle
		previous.Configuration = source.Configuration
		previous.Units = source.Units
		previous.State = manifest.SystemExtensionPresent
		desired, err := resolvedSelection(target, ref, previous)
		return desired, err == nil, err
	}
	return manifest.SystemExtension{}, false, nil
}

func selectionReference(target extensionrelease.Target, release *extensionrelease.Manifest, source manifest.SystemExtension) (string, error) {
	if err := manifest.ValidateSystemExtensions([]manifest.SystemExtension{source}, true); err != nil {
		return "", err
	}
	if source.State == manifest.SystemExtensionAbsent {
		return "", nil
	}
	if source.Release == "" {
		return source.Bundle, nil
	}
	if release == nil {
		return "", fmt.Errorf("target KatlOS release has no extension manifest; upgrade to a release providing %q", source.Release)
	}
	return release.Resolve(target, source.Release)
}

func resolvedSelection(target extensionrelease.Target, ref string, desired manifest.SystemExtension) (manifest.SystemExtension, error) {
	if target.Architecture != "" && desired.Architecture != target.Architecture {
		return manifest.SystemExtension{}, fmt.Errorf("extension %q architecture does not match target", desired.Repository())
	}
	if target.RuntimeInterface != "" && !contains(desired.SupportedRuntimeInterfaces, target.RuntimeInterface) {
		return manifest.SystemExtension{}, fmt.Errorf("extension %q runtime interface does not match target", desired.Repository())
	}
	if desired.Kernel != nil {
		if err := desired.Kernel.ValidateRuntime(target.Kernel.RuntimeSHA256); err != nil {
			return manifest.SystemExtension{}, err
		}
		if target.Kernel.Release != desired.Kernel.Target.Release {
			return manifest.SystemExtension{}, fmt.Errorf("extension %q kernel release does not match target", desired.Repository())
		}
	}
	parsed, err := payloadbundle.ParseReference(ref)
	if err != nil {
		return manifest.SystemExtension{}, err
	}
	if pin := payloadbundle.ManifestDigest(parsed); pin != "" && pin != desired.OCIManifestDigest {
		return manifest.SystemExtension{}, fmt.Errorf("extension %q resolved OCI digest differs from its pin", desired.Repository())
	}
	desired.ResolvedBundle = parsed.Name() + "@" + desired.OCIManifestDigest
	desired.ReleaseTarget = nil
	if desired.Release != "" {
		desired.ReleaseTarget = &target
	}
	if err := manifest.ValidateSystemExtensions([]manifest.SystemExtension{desired}, false); err != nil {
		return manifest.SystemExtension{}, err
	}
	return desired, nil
}
