package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func runPublishReleaseExtensions(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("publish-release-extensions requires MANIFEST OCI_LAYOUT")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var release extensionrelease.Manifest
	if err := json.Unmarshal(data, &release); err != nil {
		return err
	}
	if err := release.Validate(); err != nil {
		return err
	}
	names := make([]string, 0, len(release.Extensions))
	for name := range release.Extensions {
		names = append(names, name)
	}
	sort.Strings(names)
	ctx := context.Background()
	publications := make(map[string]string, len(names))
	// Validate the entire advertised selection before the first registry write.
	// Publication copies qualified bytes; it must never rebuild the bundle.
	for _, name := range names {
		_, resolved, err := systemextensionbundle.ResolveSelection(ctx, release.Target, &release, manifest.SystemExtension{Release: name}, func(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
			request.LayoutDir = args[1]
			return systemextensionbundle.Resolve(ctx, request)
		})
		if err != nil {
			return fmt.Errorf("verify release extension %s: %w", name, err)
		}
		publications[name], err = taggedReleaseReference(release.Extensions[name], release.Target, resolved.Bundle)
		if err != nil {
			return fmt.Errorf("tag release extension %s: %w", name, err)
		}
	}
	for _, name := range names {
		if _, err := payloadbundle.PublishLayout(ctx, payloadbundle.FetchRequest{
			LayoutDir:            args[1],
			Reference:            publications[name],
			ArtifactType:         systemextensionbundle.ArtifactType,
			ConfigMediaType:      systemextensionbundle.ConfigMediaType,
			UseDockerCredentials: true,
		}); err != nil {
			return fmt.Errorf("publish release extension %s: %w", name, err)
		}
		fmt.Fprintln(stdout, publications[name])
	}
	return nil
}

func taggedReleaseReference(ref string, target extensionrelease.Target, bundle systemextensionbundle.Bundle) (string, error) {
	if bundle.ArtifactVersion != target.Version {
		return "", fmt.Errorf("artifact version %q does not match release %q", bundle.ArtifactVersion, target.Version)
	}
	parsed, err := payloadbundle.ParseReference(ref)
	if err != nil {
		return "", err
	}
	tag := fmt.Sprintf("v%s-%s-%s-%s-%s", target.Version, target.Flavour, target.Architecture, bundle.Name, bundle.PayloadVersion)
	qualified := fmt.Sprintf("%s:%s@%s", parsed.Name(), tag, payloadbundle.ManifestDigest(parsed))
	if _, err := payloadbundle.ParseReference(qualified); err != nil {
		return "", fmt.Errorf("invalid release extension tag %q: %w", tag, err)
	}
	return qualified, nil
}
