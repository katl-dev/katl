package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

type drbdRecipe struct {
	Version string       `json:"version"`
	Source  kernelSource `json:"source"`
	Headers kernelSource `json:"headers"`
}

type kernelBuildRecord struct {
	Recipe        drbdRecipe            `json:"recipe"`
	Kernel        kernelmodule.Contract `json:"kernel"`
	SymversSHA256 string                `json:"symversSHA256"`
	ConfigSHA256  string                `json:"configSHA256"`
}

func readDRBDRecipe(repo string) (drbdRecipe, error) {
	var recipe drbdRecipe
	data, err := os.ReadFile(filepath.Join(repo, "extensions/drbd9/recipe.json"))
	if err != nil {
		return recipe, err
	}
	if err := json.Unmarshal(data, &recipe); err != nil {
		return recipe, err
	}
	if recipe.Version == "" {
		return recipe, fmt.Errorf("DRBD recipe version is required")
	}
	for _, source := range []kernelSource{recipe.Source, recipe.Headers} {
		if !strings.HasPrefix(source.URL, "https://") {
			return recipe, fmt.Errorf("kernel source requires HTTPS")
		}
		digest, err := hex.DecodeString(source.SHA256)
		if err != nil || len(digest) != 32 || strings.ToLower(source.SHA256) != source.SHA256 {
			return recipe, fmt.Errorf("kernel source requires a lowercase SHA-256 checksum")
		}
	}
	return recipe, nil
}

func runBuildKernelExtension(args []string, stdout, stderr io.Writer, cfg config) (result error) {
	if len(args) != 1 || args[0] != "drbd9" {
		return fmt.Errorf("build-kernel-extension requires drbd9")
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
	if uki.Version != root.Version {
		return fmt.Errorf("kernel extension requires matching runtime root and UKI versions")
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
	recipe, err := readDRBDRecipe(cfg.RepoRoot)
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
		return fmt.Errorf("extract exact runtime build base: %w", err)
	}
	for _, source := range []kernelSource{recipe.Source, recipe.Headers} {
		if err := acquireKernelSource(source, filepath.Join(sources, source.SHA256+".tar.gz")); err != nil {
			return err
		}
	}
	devel := "kernel-devel-" + target.Release
	if cfg.Flavour == "lts" {
		devel = "kernel-longterm-devel-" + target.Release
	}
	// The exact extracted runtime is mkosi's read-only base; build packages are
	// confined to its build overlay, never copied into the driver payload.
	command := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/mkosi"),
		"--profile", "kernel-extension-drbd9", "--build-package", devel,
		"--base-tree", filepath.Join(basePath, "root"),
		"--environment", "KATL_KERNEL_RELEASE="+target.Release,
		"--environment", "KATL_RUNTIME_SHA256="+target.RuntimeSHA256,
		"--environment", "KATL_RUNTIME_INTERFACE="+root.RuntimeInterface,
		"-f", "build")
	command.Stdout = stdout
	command.Stderr = stderr
	command.Dir = cfg.RepoRoot
	if err := command.Run(); err != nil {
		return fmt.Errorf("build DRBD9 for %s: %w", target.Release, err)
	}
	var record kernelBuildRecord
	data, err := os.ReadFile(filepath.Join(buildDir, "katl-drbd9.build.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Kernel.Target != target || record.Recipe != recipe {
		return fmt.Errorf("DRBD build record does not match the requested inputs")
	}
	created, err := time.Parse(time.RFC3339, root.Created)
	if err != nil {
		return err
	}
	packed, built, err := systemextensionbundle.Export(context.Background(), filepath.Join(buildDir, "extension-bundles"), systemextensionbundle.BuildRequest{
		Kernel:                     &record.Kernel,
		Name:                       "drbd9",
		ArtifactVersion:            root.Version,
		PayloadVersion:             recipe.Version,
		Architecture:               root.Architecture,
		SupportedRuntimeInterfaces: []string{root.RuntimeInterface},
		CreatedAt:                  created,
		Payloads: []systemextensionbundle.Input{{
			Path:     filepath.Join(buildDir, "katl-drbd9.raw"),
			Role:     systemextensionbundle.SysextRole,
			FileName: "katl-drbd9.raw",
		}},
		Metadata: []systemextensionbundle.Input{{
			Path:      filepath.Join(buildDir, "katl-drbd9.build.json"),
			Role:      "build-record",
			MediaType: "application/json",
			FileName:  "katl-drbd9.build.json",
		}},
	}, nil)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(buildDir, "katl-drbd9.bundle.json"), built.Bundle, cfg.RepoRoot); err != nil {
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
			"ghcr.io/katl-dev/katl/extensions/drbd9": "ghcr.io/katl-dev/katl/extensions/drbd9@" + packed.ManifestDigest,
		},
	}
	if err := release.Validate(); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(buildDir, "katl-drbd9.release.json"), release, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "kernel-extension: drbd9\npayload-version: %s\nkernel: %s\nartifact: %s\nbundle: %s\n",
		recipe.Version, target.Release, filepath.Join(buildDir, "katl-drbd9.raw"), filepath.Join(buildDir, "katl-drbd9.bundle.json"))
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
	_, copyErr := io.Copy(file, io.LimitReader(response.Body, 64<<20))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if _, digest, err := fileInfo(file.Name()); err != nil || digest != source.SHA256 {
		return fmt.Errorf("kernel source %s failed SHA-256 verification", source.URL)
	}
	return os.Rename(file.Name(), destination)
}

func runCompileDRBD(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("compile-drbd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sources := flags.String("sources", "", "verified source archive directory")
	destination := flags.String("destination", "", "extension root")
	output := flags.String("output", "", "build metadata directory")
	work := flags.String("work", "", "temporary build parent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *sources == "" || *destination == "" || *output == "" || *work == "" || flags.NArg() != 0 {
		return fmt.Errorf("compile-drbd requires --sources, --destination, --output and --work")
	}
	target := kernelmodule.Target{
		Release:       os.Getenv("KATL_KERNEL_RELEASE"),
		RuntimeSHA256: os.Getenv("KATL_RUNTIME_SHA256"),
	}
	if err := target.Validate(); err != nil {
		return err
	}
	kdir := filepath.Join("/usr/src/kernels", target.Release)
	if err := validateKernelInputs(kdir, target.Release); err != nil {
		return err
	}
	recipe, err := readDRBDRecipe(cfg.RepoRoot)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp(*work, "drbd-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	for _, archive := range []struct {
		source kernelSource
		path   string
	}{
		{
			source: recipe.Source,
			path:   directory,
		},
		{
			source: recipe.Headers,
			path:   filepath.Join(directory, "drbd/drbd-headers"),
		},
	} {
		path := filepath.Join(*sources, archive.source.SHA256+".tar.gz")
		if _, digest, err := fileInfo(path); err != nil || digest != archive.source.SHA256 {
			return fmt.Errorf("kernel source archive failed SHA-256 verification: %s", path)
		}
		if err := os.MkdirAll(archive.path, 0o755); err != nil {
			return err
		}
		// Only verified, in-tree-pinned archives reach tar. Each extraction
		// targets a fresh build directory, outside both runtime and payload.
		command := exec.Command("tar", "--extract", "--gzip", "--file", path, "--directory", archive.path, "--strip-components=1", "--no-same-owner", "--no-same-permissions")
		command.Stdout = stdout
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "drbd/.drbd_git_revision"), []byte("source-sha256: "+recipe.Source.SHA256+"\n"), 0o644); err != nil {
		return err
	}
	command := exec.Command("make", "-C", directory, fmt.Sprintf("-j%d", min(runtime.NumCPU(), 8)), "KVER="+target.Release, "KDIR="+kdir, "SPAAS=false", "WANT_DRBD_REPRODUCIBLE_BUILD=1", "SOURCE_DATE_EPOCH=315532800", "module")
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("compile DRBD: %w", err)
	}
	moduleDir, err := filepath.EvalSymlinks(filepath.Join(directory, "drbd/build-current"))
	if err != nil {
		return err
	}
	contract, err := installDRBDModules(moduleDir, *destination, target, recipe.Version)
	if err != nil {
		return err
	}
	license, err := os.ReadFile(filepath.Join(directory, "COPYING"))
	if err != nil {
		return err
	}
	licenseDir := filepath.Join(*destination, "usr/share/licenses/katl-drbd9")
	if err := os.MkdirAll(licenseDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(licenseDir, "COPYING"), license, 0o644); err != nil {
		return err
	}
	_, symvers, err := fileInfo(filepath.Join(kdir, "Module.symvers"))
	if err != nil {
		return err
	}
	_, configDigest, err := fileInfo(filepath.Join(kdir, ".config"))
	if err != nil {
		return err
	}
	releaseDir := filepath.Join(*destination, "usr/lib/extension-release.d")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return err
	}
	metadata := fmt.Sprintf("ID=katlos\nSYSEXT_LEVEL=%s\nSYSEXT_SCOPE=system\n", os.Getenv("KATL_RUNTIME_INTERFACE"))
	if err := os.WriteFile(filepath.Join(releaseDir, "extension-release.katl-drbd9"), []byte(metadata), 0o644); err != nil {
		return err
	}
	return writeJSON(filepath.Join(*output, "katl-drbd9.build.json"), kernelBuildRecord{
		Recipe:        recipe,
		Kernel:        contract,
		SymversSHA256: symvers,
		ConfigSHA256:  configDigest,
	}, cfg.RepoRoot)
}

func validateKernelInputs(directory, release string) error {
	actual, err := os.ReadFile(filepath.Join(directory, "include/config/kernel.release"))
	if err != nil || strings.TrimSpace(string(actual)) != release {
		return fmt.Errorf("prepared kernel inputs do not match %s", release)
	}
	for _, name := range []string{"Module.symvers", ".config", "include/generated/autoconf.h"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("prepared kernel inputs require nonempty %s", name)
		}
	}
	return nil
}

func installDRBDModules(source, destination string, target kernelmodule.Target, version string) (kernelmodule.Contract, error) {
	contract := kernelmodule.Contract{Target: target}
	found := map[string]bool{}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".ko") {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("built module must be a regular file: %s", path)
		}
		name := strings.TrimSuffix(entry.Name(), ".ko")
		if name != "drbd" && !strings.HasPrefix(name, "drbd_transport_") {
			return fmt.Errorf("unexpected DRBD build module %q", name)
		}
		if found[name] {
			return fmt.Errorf("duplicate built module %q", name)
		}
		found[name] = true
		for field, expected := range map[string]string{"name": strings.ReplaceAll(name, "-", "_"), "vermagic": target.Release} {
			value, err := exec.Command("modinfo", "-F", field, path).Output()
			parts := strings.Fields(string(value))
			if err != nil || len(parts) == 0 || parts[0] != expected {
				return fmt.Errorf("built module %s has unexpected %s: %s", name, field, value)
			}
		}
		if name == "drbd" {
			value, err := exec.Command("modinfo", "-F", "version", path).Output()
			if err != nil || strings.TrimSpace(string(value)) != version {
				return fmt.Errorf("built DRBD version does not match recipe %s", version)
			}
		}
		relative := filepath.Join("usr/lib/modules", target.Release, "extra/katl", entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		output := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return err
		}
		_, digest, err := fileInfo(output)
		if err != nil {
			return err
		}
		module := kernelmodule.Module{
			Name:     name,
			Path:     relative,
			SHA256:   digest,
			Required: name == "drbd" || name == "drbd_transport_tcp",
		}
		if name == "drbd" {
			module.Replaces = "drbd"
		}
		contract.Modules = append(contract.Modules, module)
		return nil
	})
	if err != nil {
		return contract, err
	}
	if !found["drbd"] || !found["drbd_transport_tcp"] {
		return contract, fmt.Errorf("DRBD build must supply drbd and drbd_transport_tcp")
	}
	return contract, contract.Validate()
}
