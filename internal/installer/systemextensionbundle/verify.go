package systemextensionbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"oras.land/oras-go/v2/content/oci"
)

// VerifyLocal checks a release-owned bundle without retaining its payloads in memory.
func VerifyLocal(ctx context.Context, layoutDir, ref, repository string, target extensionrelease.Target) error {
	parsed, err := payloadbundle.ParseReference(ref)
	if err != nil {
		return err
	}
	pin := payloadbundle.ManifestDigest(parsed)
	if pin == "" {
		return fmt.Errorf("local OCI artifact requires a manifest digest pin")
	}
	root, err := os.OpenRoot(layoutDir)
	if err != nil {
		return fmt.Errorf("open local OCI layout: %w", err)
	}
	defer root.Close()
	store, err := oci.NewFromFS(ctx, root.FS())
	if err != nil {
		return fmt.Errorf("read local OCI layout: %w", err)
	}
	fetched, err := payloadbundle.FetchTargetManifest(ctx, store, pin, parsed, ArtifactType, ConfigMediaType)
	if err != nil {
		return err
	}
	var bundle Bundle
	if err := json.Unmarshal(fetched.Config, &bundle); err != nil {
		return fmt.Errorf("%w: decode custom manifest: %v", ErrInvalidBundle, err)
	}
	request := ResolveRequest{Reference: ref, Architecture: target.Architecture, RuntimeInterface: target.RuntimeInterface, RuntimeSHA256: target.Kernel.RuntimeSHA256}
	if err := validateBundle(bundle, fetched.Manifest, request); err != nil {
		return err
	}
	resolved := Resolved{OCIManifestDigest: fetched.ManifestDigest, BundleManifestDigest: digestBytes(fetched.Config), Bundle: bundle}
	for _, descriptor := range bundle.Payloads {
		resolved.Payloads = append(resolved.Payloads, Payload{Descriptor: descriptor})
	}
	if _, err := resolvedSelection(target, ref, resolved.Desired(manifest.SystemExtension{Release: repository})); err != nil {
		return err
	}
	// The manifest and selection checks above bind every advertised descriptor.
	// Stream each blob so large unused extensions do not exhaust agent memory.
	for _, layer := range fetched.Manifest.Layers {
		if _, err := payloadbundle.CopyContent(ctx, store, layer, io.Discard); err != nil {
			return fmt.Errorf("verify OCI layer %s: %w", layer.Digest, err)
		}
	}
	return nil
}
