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
func runComposeReleaseExtensions(args []string, stdout, stderr io.Writer, cfg config) (resultErr error) {
	if len(args) != 2 {
		return fmt.Errorf("compose-release-extensions requires INSTALL_IMAGE OUTPUT")
	}
	image, output := args[0], args[1]
	recipes, err := releaseRecipes(cfg.RepoRoot)
	if err != nil {
		return err
	}
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
	return composeReleaseExtensions(ctx, payload, recipes, output, func(operands []string) error {
		return elevateReleaseVerification(operands, stdout, stderr)
	})
}

// composeReleaseExtensions composes an extracted image's release extension
// closure into output through compose, which takes verify-release-extensions'
// operands, and writes the module inventory beside the composed images. The
// closure must hold exactly the release extensions that the recipes declare,
// so that an extension missing from the image cannot pass qualification by
// never being loaded.
func composeReleaseExtensions(ctx context.Context, payload katlosimage.Payload, recipes map[string]kernelRecipe, output string, compose func(operands []string) error) error {
	release := payload.Index.ExtensionRelease
	if release == nil || len(release.Extensions) == 0 {
		return fmt.Errorf("image advertises no release extensions")
	}
	declared := make([]string, 0, len(recipes))
	for _, recipe := range recipes {
		declared = append(declared, recipe.Repository)
	}
	slices.Sort(declared)
	if advertised := slices.Sorted(maps.Keys(release.Extensions)); !slices.Equal(advertised, declared) {
		return fmt.Errorf("image advertises release extensions %v, the release declares %v", advertised, declared)
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
		return fmt.Errorf("image release extensions ship no kernel modules")
	}

	manifestPath := filepath.Join(payload.Root, "release-extensions.json")
	data, err := json.Marshal(release)
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		return err
	}
	if err := compose([]string{manifestPath, filepath.Join(payload.Root, payload.Runtime.Path), filepath.Join(payload.Root, katlosimage.ExtensionLayoutPath), output}); err != nil {
		return err
	}
	data, err = json.MarshalIndent(contracts, "", "  ")
	if err != nil {
		return err
	}
	return writeNewFile(filepath.Join(output, releaseExtensionModulesFile), append(data, '\n'))
}
