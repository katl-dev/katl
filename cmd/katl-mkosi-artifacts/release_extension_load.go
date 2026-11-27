package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

// releaseExtensionModulesFile names the inventory, written beside the composed
// images, of the modules each release extension ships.
const releaseExtensionModulesFile = "modules.json"

// runComposeReleaseExtensions composes the release extensions carried by a
// KatlOS install image for that image's own runtime, so that a node installed
// from the image can merge them and load their modules. It reads only the
// image's bytes and composes them with the release build's verification.
func runComposeReleaseExtensions(args []string, stdout, stderr io.Writer) (resultErr error) {
	if len(args) != 2 {
		return fmt.Errorf("compose-release-extensions requires INSTALL_IMAGE OUTPUT")
	}
	image, output := args[0], args[1]
	metadata, err := katlosimage.ReadArtifactMetadata(image+".json", katlosimage.RoleInstall)
	if err != nil {
		return err
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		return err
	}
	// The extracted image is large, so it stays beside the output rather than
	// in a temporary directory that may be memory-backed.
	root := filepath.Join(output, ".image")
	defer func() {
		resultErr = errors.Join(resultErr, os.RemoveAll(root))
	}()
	if text, err := exec.Command("unsquashfs", "-no-progress", "-d", root, image).CombinedOutput(); err != nil {
		return fmt.Errorf("extract %s: %w: %s", image, err, text)
	}
	ctx := context.Background()
	payload, err := katlosimage.ResolveDirectory(ctx, root, manifest.KatlosImage{
		Role:             metadata.ImageRole,
		Version:          metadata.Version,
		Architecture:     metadata.Architecture,
		RuntimeInterface: metadata.RuntimeInterface,
	})
	if err != nil {
		return err
	}
	release := payload.Index.ExtensionRelease
	if release == nil || len(release.Extensions) == 0 {
		return fmt.Errorf("%s advertises no release extensions", image)
	}

	contracts := map[string]kernelmodule.Contract{}
	for _, repository := range slices.Sorted(maps.Keys(release.Extensions)) {
		resolved, err := payload.ResolveReleaseExtension(ctx, systemextensionbundle.ResolveRequest{
			Reference:        release.Extensions[repository],
			Architecture:     payload.Index.Architecture,
			RuntimeInterface: payload.Index.RuntimeInterface,
			RuntimeSHA256:    payload.Runtime.SHA256,
		})
		if err != nil {
			return fmt.Errorf("resolve release extension %s: %w", repository, err)
		}
		if resolved.Bundle.Kernel != nil {
			contracts[repository] = *resolved.Bundle.Kernel
		}
	}
	if len(contracts) == 0 {
		return fmt.Errorf("%s release extensions ship no kernel modules", image)
	}

	manifestPath := filepath.Join(root, "release-extensions.json")
	data, err := json.Marshal(release)
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		return err
	}
	if err := elevateReleaseVerification([]string{manifestPath, filepath.Join(root, payload.Runtime.Path), filepath.Join(root, katlosimage.ExtensionLayoutPath), output}, stdout, stderr); err != nil {
		return err
	}
	data, err = json.MarshalIndent(contracts, "", "  ")
	if err != nil {
		return err
	}
	return writeNewFile(filepath.Join(output, releaseExtensionModulesFile), append(data, '\n'))
}
