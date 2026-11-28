package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func runMatchReleaseExtensions(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("match-release-extensions requires BUNDLE FLAVOUR MANIFEST OCI_LAYOUT")
	}
	bundle, flavour := args[0], args[1]
	name := publicationName("katl-runtime.extensions.json", flavour)
	expected, err := readReleaseExtensionInventory(filepath.Join(bundle, name), flavour)
	if err != nil {
		return fmt.Errorf("read bundled %s extension inventory: %w", flavour, err)
	}
	release, err := readPublicationManifest(args[2])
	if err != nil {
		return err
	}
	actual, err := collectReleaseExtensionInventory(release, args[3])
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("%s extension publication inputs differ from %s in the release bundle", flavour, name)
	}
	return nil
}

func runPublishReleaseExtensions(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("publish-release-extensions requires MANIFEST OCI_LAYOUT")
	}
	release, err := readPublicationManifest(args[0])
	if err != nil {
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
		resolved, err := resolveReleaseExtension(release, name, args[1])
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

func readPublicationManifest(path string) (extensionrelease.Manifest, error) {
	var release extensionrelease.Manifest
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &release)
	}
	if err != nil {
		return release, fmt.Errorf("read extension publication manifest: %w", err)
	}
	return release, nil
}

func taggedReleaseReference(ref string, target extensionrelease.Target, bundle systemextensionbundle.Bundle) (string, error) {
	if bundle.ArtifactVersion != target.Version {
		return "", fmt.Errorf("artifact version %q does not match release %q", bundle.ArtifactVersion, target.Version)
	}
	parsed, err := payloadbundle.ParseReference(ref)
	if err != nil {
		return "", err
	}
	tag := target.ReleaseTag(bundle.Name, bundle.PayloadVersion)
	qualified := fmt.Sprintf("%s:%s@%s", parsed.Name(), tag, payloadbundle.ManifestDigest(parsed))
	if _, err := payloadbundle.ParseReference(qualified); err != nil {
		return "", fmt.Errorf("invalid release extension tag %q: %w", tag, err)
	}
	return qualified, nil
}
