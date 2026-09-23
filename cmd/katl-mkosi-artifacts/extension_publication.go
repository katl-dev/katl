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
	// Validate the entire advertised selection before the first registry write.
	// Publication copies qualified bytes; it must never rebuild the bundle.
	for _, name := range names {
		_, _, err := systemextensionbundle.ResolveSelection(ctx, release.Target, &release, manifest.SystemExtension{Release: name}, func(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
			request.LayoutDir = args[1]
			return systemextensionbundle.Resolve(ctx, request)
		})
		if err != nil {
			return fmt.Errorf("verify release extension %s: %w", name, err)
		}
	}
	for _, name := range names {
		ref := release.Extensions[name]
		if _, err := payloadbundle.PublishLayout(ctx, payloadbundle.FetchRequest{
			LayoutDir:            args[1],
			Reference:            ref,
			ArtifactType:         systemextensionbundle.ArtifactType,
			ConfigMediaType:      systemextensionbundle.ConfigMediaType,
			UseDockerCredentials: true,
		}); err != nil {
			return fmt.Errorf("publish release extension %s: %w", name, err)
		}
		fmt.Fprintln(stdout, ref)
	}
	return nil
}
