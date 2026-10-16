package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/kernelrelease"
)

const kernelInputsFile = "katl-kernel-inputs.tar"

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

type kernelInputs struct {
	KernelSetSHA256 string              `json:"kernelSetSHA256"`
	Target          kernelmodule.Target `json:"target"`
	SHA256          string              `json:"sha256"`
	SizeBytes       int64               `json:"sizeBytes"`
	SymversSHA256   string              `json:"symversSHA256"`
	ConfigSHA256    string              `json:"configSHA256"`
}

func runExportKernelInputs(args []string, stdout, stderr io.Writer) error {
	if len(args) != 3 {
		return fmt.Errorf("export-kernel-inputs requires BUILDROOT OUTPUT_DIR KERNEL_LOCK")
	}
	lock, err := kernelrelease.Read(args[2])
	if err != nil {
		return err
	}
	identity, err := lock.Digest()
	if err != nil {
		return err
	}
	if err := lock.Set.VerifyDirectory(filepath.Join(filepath.Dir(args[2]), "kernel-rpms", identity)); err != nil {
		return fmt.Errorf("verify selected kernel RPMs: %w", err)
	}
	identity, err = verifyEmbeddedKernelLock(args[0], identity, lock.KernelRelease)
	if err != nil {
		return err
	}
	modules, err := os.ReadDir(filepath.Join(args[0], "usr/lib/modules"))
	if err != nil {
		return err
	}
	if len(modules) != 1 || !modules[0].IsDir() {
		return fmt.Errorf("runtime must contain one kernel when exporting build inputs")
	}
	release := modules[0].Name()
	if release != lock.KernelRelease {
		return fmt.Errorf("runtime kernel does not match the selected RPM lock")
	}
	relative := filepath.Join("usr/src/kernels", release)
	source := filepath.Join(args[0], relative)
	if err := validateKernelInputs(source, release); err != nil {
		return err
	}
	_, symvers, err := fileInfo(filepath.Join(source, "Module.symvers"))
	if err != nil {
		return err
	}
	_, config, err := fileInfo(filepath.Join(source, ".config"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(args[1], 0755); err != nil {
		return err
	}
	archive := filepath.Join(args[1], kernelInputsFile)
	// Export only the prepared kernel tree, never the runtime build overlay.
	// Normalize archive metadata so repeated exports retain the same identity.
	command := exec.Command("tar", "--create", "--file", archive, "--sort=name", "--mtime=@0", "--owner=0", "--group=0", "--numeric-owner", "--format=gnu", "--directory", args[0], relative)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("export prepared kernel inputs: %w", err)
	}
	size, digest, err := fileInfo(archive)
	if err != nil {
		return err
	}
	if err := writeChecksum(archive); err != nil {
		return err
	}
	return writeJSON(archive+".json", kernelInputs{
		KernelSetSHA256: identity,
		Target:          kernelmodule.Target{Release: release},
		SHA256:          digest,
		SizeBytes:       size,
		SymversSHA256:   symvers,
		ConfigSHA256:    config,
	}, args[1])
}

func readKernelInputs(archive string) (kernelInputs, error) {
	var inputs kernelInputs
	data, err := os.ReadFile(archive + ".json")
	if err != nil {
		return inputs, fmt.Errorf("read kernel build inputs; rebuild the runtime: %w", err)
	}
	if err := json.Unmarshal(data, &inputs); err != nil {
		return inputs, err
	}
	size, digest, err := fileInfo(archive)
	if err != nil {
		return inputs, err
	}
	if inputs.SHA256 != digest || inputs.SizeBytes != size {
		return inputs, fmt.Errorf("kernel build input archive failed SHA-256 verification")
	}
	for _, digest := range []string{inputs.ConfigSHA256, inputs.SymversSHA256, inputs.KernelSetSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
			return inputs, fmt.Errorf("invalid prepared kernel input digest")
		}
	}
	return inputs, nil
}

func runBindKernelInputs(args []string, cfg config) error {
	if len(args) != 0 {
		return fmt.Errorf("bind-kernel-inputs takes no arguments")
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
	archive := filepath.Join(filepath.Dir(cfg.RuntimeRoot), kernelInputsFile)
	inputs, err := readKernelInputs(archive)
	if err != nil {
		return err
	}
	if inputs.Target.Release != uki.KernelVersion {
		return fmt.Errorf("prepared kernel inputs do not match the runtime kernel")
	}
	if err := verifyKernelLock(cfg, inputs); err != nil {
		return err
	}
	if inputs.Target.RuntimeSHA256 != "" && inputs.Target.RuntimeSHA256 != root.SHA256 {
		return fmt.Errorf("prepared kernel inputs already belong to another runtime")
	}
	inputs.Target.RuntimeSHA256 = root.SHA256
	if err := inputs.Target.Validate(); err != nil {
		return err
	}
	return writeJSON(archive+".json", inputs, cfg.RepoRoot)
}
