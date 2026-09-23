package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

type kernelSource struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type kernelRecipe struct {
	Version    string                  `json:"version"`
	Repository string                  `json:"repository"`
	Sources    map[string]kernelSource `json:"sources"`
}

type kernelBuildRecord struct {
	Recipe        kernelRecipe          `json:"recipe"`
	Kernel        kernelmodule.Contract `json:"kernel"`
	SymversSHA256 string                `json:"symversSHA256"`
	ConfigSHA256  string                `json:"configSHA256"`
}

func readKernelRecipe(repo, name string) (kernelRecipe, error) {
	var recipe kernelRecipe
	if !regexp.MustCompile("^[a-z][a-z0-9-]*$").MatchString(name) {
		return recipe, fmt.Errorf("invalid kernel extension name %q", name)
	}
	data, err := os.ReadFile(filepath.Join(repo, "extensions", name, "recipe.json"))
	if err != nil {
		return recipe, err
	}
	if err := json.Unmarshal(data, &recipe); err != nil {
		return recipe, err
	}
	if recipe.Version == "" {
		return recipe, fmt.Errorf("kernel extension recipe version is required")
	}
	if err := extensionrelease.ValidateRepository(recipe.Repository); err != nil {
		return recipe, err
	}
	if len(recipe.Sources) == 0 {
		return recipe, fmt.Errorf("kernel recipe sources are required")
	}
	for _, source := range recipe.Sources {
		if !strings.HasPrefix(source.URL, "https://") {
			return recipe, fmt.Errorf("kernel source requires HTTPS")
		}
		if _, err := kernelSourceFile(source); err != nil {
			return recipe, err
		}
		digest, err := hex.DecodeString(source.SHA256)
		if err != nil || len(digest) != 32 || strings.ToLower(source.SHA256) != source.SHA256 {
			return recipe, fmt.Errorf("kernel source requires a lowercase SHA-256 checksum")
		}
	}
	return recipe, nil
}

func runBuildKernelExtension(args []string, stdout, stderr io.Writer, cfg config) (result error) {
	if len(args) != 1 {
		return fmt.Errorf("build-kernel-extension requires NAME")
	}
	name := args[0]
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
	rootFlavour, err := flavour.Normalize(root.Flavour)
	buildFlavour, buildErr := flavour.Normalize(cfg.Flavour)
	if err != nil || buildErr != nil || rootFlavour != buildFlavour {
		return fmt.Errorf("kernel extension build flavor must match the runtime")
	}
	target := kernelmodule.Target{
		Release:       uki.KernelVersion,
		RuntimeSHA256: root.SHA256,
	}
	if err := target.Validate(); err != nil {
		return err
	}
	recipe, err := readKernelRecipe(cfg.RepoRoot, name)
	if err != nil {
		return err
	}
	buildDir := filepath.Dir(cfg.RuntimeRoot)
	selectedBuildDir := os.Getenv("KATL_MKOSI_BUILD_DIR")
	if selectedBuildDir == "" {
		selectedBuildDir = filepath.Join(cfg.RepoRoot, "_build/mkosi")
	}
	if absPath(cfg.RepoRoot, selectedBuildDir) != buildDir {
		return fmt.Errorf("runtime artifact must be in the selected KATL_MKOSI_BUILD_DIR")
	}
	inputArchive := filepath.Join(buildDir, kernelInputsFile)
	inputs, err := readKernelInputs(inputArchive)
	if err != nil {
		return err
	}
	if inputs.Target != target {
		return fmt.Errorf("prepared kernel inputs do not belong to the selected runtime; rebuild the runtime")
	}
	sources := filepath.Join(buildDir, "kernel-sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		return err
	}
	baseDir, err := os.MkdirTemp(buildDir, "kernel-base-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(baseDir); err == nil {
			return
		}
		// Rootless extraction can create subordinate-UID directories. Clean
		// only this private staging tree in the same namespace that created it.
		cleanup := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/mkosi"), "box", "--", "rm", "-rf", "--", filepath.Join("_build/mkosi", filepath.Base(baseDir)))
		cleanup.Dir = cfg.RepoRoot
		cleanup.Stdout = stdout
		cleanup.Stderr = stderr
		if err := cleanup.Run(); err != nil {
			result = errors.Join(result, fmt.Errorf("remove temporary runtime base %s: %w", baseDir, err))
		}
	}()
	// Preserve the verified inode if another build replaces the conventional
	// output path. Extracting it also supports rootless builders without loops.
	if err := os.Link(cfg.RuntimeRoot, filepath.Join(baseDir, "runtime.raw")); err != nil {
		return err
	}
	if err := os.Link(inputArchive, filepath.Join(baseDir, kernelInputsFile)); err != nil {
		return err
	}
	if _, digest, err := fileInfo(filepath.Join(baseDir, kernelInputsFile)); err != nil || digest != inputs.SHA256 {
		return fmt.Errorf("kernel inputs changed while preparing the extension build")
	}
	if _, digest, err := fileInfo(filepath.Join(baseDir, "runtime.raw")); err != nil || digest != target.RuntimeSHA256 {
		return fmt.Errorf("runtime changed while preparing the kernel extension build")
	}
	basePath := filepath.Join("_build/mkosi", filepath.Base(baseDir))
	extract := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/mkosi"), "box", "--",
		"unsquashfs", "-no-progress", "-d", filepath.Join(basePath, "root"), filepath.Join(basePath, "runtime.raw"))
	extract.Dir = cfg.RepoRoot
	extract.Stdout = stdout
	extract.Stderr = stderr
	if err := extract.Run(); err != nil {
		return fmt.Errorf("extract runtime build base: %w", err)
	}
	unpack := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/mkosi"), "box", "--",
		"tar", "--extract", "--file", filepath.Join(basePath, kernelInputsFile),
		"--directory", filepath.Join(basePath, "root"), "--no-same-owner", "--no-same-permissions")
	unpack.Dir = cfg.RepoRoot
	unpack.Stdout = stdout
	unpack.Stderr = stderr
	if err := unpack.Run(); err != nil {
		return fmt.Errorf("extract prepared kernel inputs: %w", err)
	}
	for _, source := range recipe.Sources {
		file, err := kernelSourceFile(source)
		if err != nil {
			return err
		}
		if err := acquireKernelSource(source, filepath.Join(sources, file)); err != nil {
			return err
		}
	}
	// The exact extracted runtime is mkosi's read-only base; build packages are
	// confined to its build overlay, never copied into the driver payload.
	command := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/mkosi"),
		"--profile", "kernel-extension-"+name,
		"--base-tree", filepath.Join(basePath, "root"),
		"--environment", "KATL_KERNEL_RELEASE="+target.Release,
		"--environment", "KATL_RUNTIME_SHA256="+target.RuntimeSHA256,
		"--environment", "KATL_RUNTIME_INTERFACE="+root.RuntimeInterface,
		"-f", "build")
	command.Stdout = stdout
	command.Stderr = stderr
	command.Dir = cfg.RepoRoot
	if err := command.Run(); err != nil {
		return fmt.Errorf("build %s for %s: %w", name, target.Release, err)
	}
	var record kernelBuildRecord
	data, err := os.ReadFile(filepath.Join(buildDir, "katl-"+name+".build.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Kernel.Target != target || !reflect.DeepEqual(record.Recipe, recipe) {
		return fmt.Errorf("kernel extension build record does not match the requested inputs")
	}
	if record.SymversSHA256 != inputs.SymversSHA256 || record.ConfigSHA256 != inputs.ConfigSHA256 {
		return fmt.Errorf("extension did not use the runtime's prepared kernel inputs")
	}
	created, err := time.Parse(time.RFC3339, root.Created)
	if err != nil {
		return err
	}
	packed, built, err := systemextensionbundle.Export(context.Background(), filepath.Join(buildDir, "extension-bundles"), systemextensionbundle.BuildRequest{
		Kernel:                     &record.Kernel,
		Name:                       name,
		ArtifactVersion:            root.Version,
		PayloadVersion:             recipe.Version,
		Architecture:               root.Architecture,
		SupportedRuntimeInterfaces: []string{root.RuntimeInterface},
		CreatedAt:                  created,
		Payloads: []systemextensionbundle.Input{{
			Path:     filepath.Join(buildDir, "katl-"+name+".raw"),
			Role:     systemextensionbundle.SysextRole,
			FileName: "katl-" + name + ".raw",
		}},
		Metadata: []systemextensionbundle.Input{{
			Path:      filepath.Join(buildDir, "katl-"+name+".build.json"),
			Role:      "build-record",
			MediaType: "application/json",
			FileName:  "katl-" + name + ".build.json",
		}},
	}, nil)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(buildDir, "katl-"+name+".bundle.json"), built.Bundle, cfg.RepoRoot); err != nil {
		return err
	}
	release := extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          root.Version,
			Architecture:     root.Architecture,
			Flavour:          rootFlavour,
			RuntimeInterface: root.RuntimeInterface,
			Kernel:           target,
		},
		Extensions: map[string]string{
			recipe.Repository: recipe.Repository + "@" + packed.ManifestDigest,
		},
	}
	if err := release.Validate(); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(buildDir, "katl-"+name+".release.json"), release, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "kernel-extension: %s\npayload-version: %s\nkernel: %s\nartifact: %s\nbundle: %s\n",
		name, recipe.Version, target.Release, filepath.Join(buildDir, "katl-"+name+".raw"), filepath.Join(buildDir, "katl-"+name+".bundle.json"))
	return nil
}

func acquireKernelSource(source kernelSource, destination string) error {
	if _, digest, err := fileInfo(destination); err == nil && digest == source.SHA256 {
		return nil
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Get(source.URL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch kernel source %s: %s", source.URL, response.Status)
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".source-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	limit := int64(64 << 20)
	if strings.HasSuffix(destination, ".run") {
		limit = 512 << 20
	}
	size, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if size > limit {
		return fmt.Errorf("kernel source %s exceeds %d bytes", source.URL, limit)
	}
	if _, digest, err := fileInfo(file.Name()); err != nil || digest != source.SHA256 {
		return fmt.Errorf("kernel source %s failed SHA-256 verification", source.URL)
	}
	return os.Rename(file.Name(), destination)
}

func kernelSourceFile(source kernelSource) (string, error) {
	parsed, err := url.Parse(source.URL)
	if err != nil {
		return "", err
	}
	suffix := ".tar.gz"
	if strings.HasSuffix(parsed.Path, ".run") {
		suffix = ".run"
	}
	return source.SHA256 + suffix, nil
}
