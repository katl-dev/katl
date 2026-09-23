package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/releaseextensions/drbd9"
	"github.com/katl-dev/katl/internal/releaseextensions/nvidia"
)

func runCompileExtension(name string, args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("compile-"+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	sources := flags.String("sources", "", "verified source archive directory")
	destination := flags.String("destination", "", "extension root")
	output := flags.String("output", "", "build metadata directory")
	work := flags.String("work", "", "temporary build parent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *sources == "" || *destination == "" || *output == "" || *work == "" || flags.NArg() != 0 {
		return fmt.Errorf("compile-%s requires --sources, --destination, --output and --work", name)
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
	recipe, err := readKernelRecipe(cfg.RepoRoot, name)
	if err != nil {
		return err
	}
	verified := make(map[string]string, len(recipe.Sources))
	for sourceName, source := range recipe.Sources {
		path, err := verifiedKernelSource(*sources, source)
		if err != nil {
			return err
		}
		verified[sourceName] = path
	}
	interfaceLevel := os.Getenv("KATL_RUNTIME_INTERFACE")
	if interfaceLevel == "" {
		return fmt.Errorf("kernel extension requires the runtime interface")
	}
	directory, err := os.MkdirTemp(*work, name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	var contract kernelmodule.Contract
	switch name {
	case "drbd9":
		contract, err = drbd9.Compile(drbd9.Input{
			Target: target, Version: recipe.Version, Sources: verified,
			Destination: *destination, Work: directory, Stdout: stdout, Stderr: stderr,
		})
	case "nvidia":
		contract, err = nvidia.Compile(nvidia.Input{
			Target: target, Version: recipe.Version, Sources: verified,
			Destination: *destination, Work: directory, Stdout: stdout, Stderr: stderr,
		})
	default:
		return fmt.Errorf("no compiler for release extension %q", name)
	}
	if err != nil {
		return err
	}
	if err := installKernelExtensionMetadata(*destination, name, interfaceLevel); err != nil {
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
	return writeJSON(filepath.Join(*output, "katl-"+name+".build.json"), kernelBuildRecord{
		Recipe: recipe, Kernel: contract, SymversSHA256: symvers, ConfigSHA256: configDigest,
	}, cfg.RepoRoot)
}

func verifiedKernelSource(directory string, source kernelSource) (string, error) {
	name, err := kernelSourceFile(source)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if _, digest, err := fileInfo(path); err != nil || digest != source.SHA256 {
		return "", fmt.Errorf("kernel source archive failed SHA-256 verification: %s", path)
	}
	return path, nil
}

func installKernelExtensionMetadata(destination, name, interfaceLevel string) error {
	releaseDir := filepath.Join(destination, "usr/lib/extension-release.d")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return err
	}
	metadata := fmt.Sprintf("ID=katlos\nSYSEXT_LEVEL=%s\nSYSEXT_SCOPE=system\n", interfaceLevel)
	return os.WriteFile(filepath.Join(releaseDir, "extension-release.katl-"+name), []byte(metadata), 0o644)
}
