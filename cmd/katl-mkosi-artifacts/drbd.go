package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

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
	recipe, err := readKernelRecipe(cfg.RepoRoot, "drbd9")
	if err != nil {
		return err
	}
	for _, name := range []string{"source", "headers"} {
		if _, ok := recipe.Sources[name]; !ok {
			return fmt.Errorf("DRBD recipe requires %s source", name)
		}
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
			source: recipe.Sources["source"],
			path:   directory,
		},
		{
			source: recipe.Sources["headers"],
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
	if err := os.WriteFile(filepath.Join(directory, "drbd/.drbd_git_revision"), []byte("source-sha256: "+recipe.Sources["source"].SHA256+"\n"), 0o644); err != nil {
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
