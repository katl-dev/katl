package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func releaseRecipes(repo string) (map[string]kernelRecipe, error) {
	paths, err := filepath.Glob(filepath.Join(repo, "extensions", "*", "recipe.json"))
	if err != nil {
		return nil, err
	}
	recipes := map[string]kernelRecipe{}
	repositories := map[string]bool{}
	for _, path := range paths {
		name := filepath.Base(filepath.Dir(path))
		recipe, err := readKernelRecipe(repo, name)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		if repositories[recipe.Repository] {
			return nil, fmt.Errorf("duplicate release repository %s", recipe.Repository)
		}
		if _, err := os.Stat(filepath.Join(repo, "mkosi.profiles", "kernel-extension-"+name, "mkosi.conf")); err != nil {
			return nil, fmt.Errorf("recipe %s build profile: %w", name, err)
		}
		repositories[recipe.Repository] = true
		recipes[name] = recipe
	}
	if len(recipes) == 0 {
		return nil, fmt.Errorf("no release extension recipes found")
	}
	return recipes, nil
}

func recipeNames(recipes map[string]kernelRecipe) []string {
	names := make([]string, 0, len(recipes))
	for name := range recipes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func runExtensionInventory(args []string, stdout io.Writer, cfg config) error {
	if len(args) != 0 {
		return fmt.Errorf("inventory-release-extensions takes no arguments")
	}
	recipes, err := releaseRecipes(cfg.RepoRoot)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Extension []string `json:"extension"`
	}{Extension: recipeNames(recipes)})
}

func runBuildReleaseExtensions(args []string, stdout, stderr io.Writer, cfg config) error {
	recipes, err := releaseRecipes(cfg.RepoRoot)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		args = recipeNames(recipes)
	}
	for _, name := range args {
		if _, ok := recipes[name]; !ok {
			return fmt.Errorf("unknown release extension %q", name)
		}
	}
	buildDir := filepath.Dir(cfg.RuntimeRoot)
	for _, name := range args {
		if err := runBuildKernelExtension([]string{name}, stdout, stderr, cfg); err != nil {
			return err
		}
		releasePath := filepath.Join(buildDir, "katl-"+name+".release.json")
		if err := verifyReleaseImages(releasePath, cfg.RuntimeRoot, filepath.Join(buildDir, "extension-bundles"), stdout, stderr); err != nil {
			return err
		}
		release, err := readExtensionRelease(releasePath)
		if err != nil {
			return err
		}
		destination := filepath.Join(buildDir, "release-extensions", name)
		for _, ref := range release.Extensions {
			if err := payloadbundle.CopyLayout(context.Background(), filepath.Join(destination, "extension-bundles"), payloadbundle.FetchRequest{
				LayoutDir:       filepath.Join(buildDir, "extension-bundles"),
				Reference:       ref,
				ArtifactType:    systemextensionbundle.ArtifactType,
				ConfigMediaType: systemextensionbundle.ConfigMediaType,
			}); err != nil {
				return err
			}
		}
		if err := writeJSON(filepath.Join(destination, "release.json"), release, cfg.RepoRoot); err != nil {
			return err
		}
	}
	return nil
}

func readExtensionRelease(path string) (extensionrelease.Manifest, error) {
	var release extensionrelease.Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return release, err
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return release, err
	}
	return release, release.Validate()
}

func runAssembleReleaseExtensions(args []string, stdout, stderr io.Writer, cfg config) error {
	if len(args) != 0 {
		return fmt.Errorf("assemble-release-extensions takes no arguments")
	}
	recipes, err := releaseRecipes(cfg.RepoRoot)
	if err != nil {
		return err
	}
	buildDir := filepath.Dir(cfg.RuntimeRoot)
	release, err := collectReleaseExtensions(recipes, filepath.Join(buildDir, "release-extensions"), filepath.Join(buildDir, "extension-bundles"))
	if err != nil {
		return err
	}
	root, err := readAndValidateLocalMetadata("runtime root", cfg.RuntimeMetadata, cfg.RuntimeRoot)
	if err != nil {
		return err
	}
	uki, err := readAndValidateLocalMetadata("runtime UKI", cfg.RuntimeUKIMetadata, cfg.RuntimeUKI)
	if err != nil {
		return err
	}
	if err := validateKatlOSComponents(root, uki, cfg.Architecture, root.RuntimeInterface); err != nil {
		return err
	}
	if err := release.Target.ValidateRuntime(root.Version, root.Architecture, root.Flavour, root.RuntimeInterface, root.SHA256); err != nil {
		return err
	}
	if release.Target.Kernel.Release != uki.KernelVersion {
		return fmt.Errorf("extension kernel does not match runtime UKI")
	}
	// Only publish the aggregate manifest after every expected recipe and the
	// complete module combination have been verified.
	candidate := filepath.Join(buildDir, "release-extensions.candidate.json")
	if err := writeJSON(candidate, release, cfg.RepoRoot); err != nil {
		return err
	}
	defer os.Remove(candidate)
	if err := verifyReleaseImages(candidate, cfg.RuntimeRoot, filepath.Join(buildDir, "extension-bundles"), stdout, stderr); err != nil {
		return err
	}
	inventoryCandidate := filepath.Join(buildDir, "katl-runtime.extensions.candidate.json")
	if err := writeReleaseExtensionInventory(inventoryCandidate, release, filepath.Join(buildDir, "extension-bundles"), cfg.RepoRoot); err != nil {
		return err
	}
	defer os.Remove(inventoryCandidate)
	if err := os.Rename(inventoryCandidate, filepath.Join(buildDir, "katl-runtime.extensions.json")); err != nil {
		return err
	}
	return os.Rename(candidate, filepath.Join(buildDir, "release-extensions.json"))
}

func collectReleaseExtensions(recipes map[string]kernelRecipe, directory, layout string) (extensionrelease.Manifest, error) {
	combined := extensionrelease.Manifest{Extensions: map[string]string{}}
	for _, name := range recipeNames(recipes) {
		memberDir := filepath.Join(directory, name)
		member, err := readExtensionRelease(filepath.Join(memberDir, "release.json"))
		if err != nil {
			return combined, fmt.Errorf("release extension %s: %w", name, err)
		}
		if combined.Target.Version == "" {
			combined.Target = member.Target
		}
		if combined.Target != member.Target {
			return combined, fmt.Errorf("release extension %s targets a different runtime", name)
		}
		repository := recipes[name].Repository
		ref, ok := member.Extensions[repository]
		if !ok || len(member.Extensions) != 1 {
			return combined, fmt.Errorf("release extension %s must supply exactly its recipe repository", name)
		}
		resolved, err := resolveReleaseExtension(member, repository, filepath.Join(memberDir, "extension-bundles"))
		if err != nil {
			return combined, err
		}
		if resolved.Bundle.Name != name || resolved.Bundle.PayloadVersion != recipes[name].Version {
			return combined, fmt.Errorf("release extension %s does not match its recipe", name)
		}
		if err := payloadbundle.CopyLayout(context.Background(), layout, payloadbundle.FetchRequest{
			LayoutDir:       filepath.Join(memberDir, "extension-bundles"),
			Reference:       ref,
			ArtifactType:    systemextensionbundle.ArtifactType,
			ConfigMediaType: systemextensionbundle.ConfigMediaType,
		}); err != nil {
			return combined, err
		}
		combined.Extensions[repository] = ref
	}
	return combined, combined.Validate()
}

func resolveReleaseExtension(release extensionrelease.Manifest, repository, layout string) (systemextensionbundle.Resolved, error) {
	_, resolved, err := systemextensionbundle.ResolveSelection(context.Background(), release.Target, &release, manifest.SystemExtension{Release: repository}, func(ctx context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		request.LayoutDir = layout
		return systemextensionbundle.Resolve(ctx, request)
	})
	return resolved, err
}

func verifyReleaseImages(release, runtime, layout string, stdout, stderr io.Writer) error {
	args := []string{"verify-release-extensions", release, runtime, layout}
	if os.Geteuid() == 0 {
		return runVerifyReleaseExtensions(args[1:])
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// Elevate only read-only image inspection and module-index composition, not
	// recipe execution. Local and CI builds use this same verification boundary.
	command := exec.Command("sudo", append([]string{"--", executable}, args...)...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func runVerifyReleaseExtensions(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("verify-release-extensions requires MANIFEST RUNTIME OCI_LAYOUT")
	}
	release, err := readExtensionRelease(args[0])
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "katl-release-composition-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	request := kernelmodule.ImageRequest{
		Target: release.Target.Kernel,
		Runtime: kernelmodule.Image{
			Path:   args[1],
			SHA256: release.Target.Kernel.RuntimeSHA256,
		},
		RuntimeInterface: release.Target.RuntimeInterface,
		RuntimeVersion:   release.Target.Version,
		Architecture:     release.Target.Architecture,
	}
	for repository := range release.Extensions {
		resolved, err := resolveReleaseExtension(release, repository, args[2])
		if err != nil {
			return err
		}
		bundle := kernelmodule.BundleImages{
			Name:     repository,
			Contract: resolved.Bundle.Kernel,
		}
		for _, payload := range resolved.Payloads {
			if payload.Descriptor.Role != systemextensionbundle.SysextRole {
				continue
			}
			path := filepath.Join(work, resolved.Bundle.Name+"-"+payload.Descriptor.FileName)
			if err := os.WriteFile(path, payload.Data, 0600); err != nil {
				return err
			}
			bundle.Images = append(bundle.Images, kernelmodule.Image{
				Name:   payload.Descriptor.FileName,
				Path:   path,
				SHA256: strings.TrimPrefix(payload.Descriptor.Digest, "sha256:"),
			})
		}
		request.Bundles = append(request.Bundles, bundle)
	}
	prepared, err := kernelmodule.PrepareImages(context.Background(), request)
	if err != nil {
		return err
	}
	return prepared.Close()
}
