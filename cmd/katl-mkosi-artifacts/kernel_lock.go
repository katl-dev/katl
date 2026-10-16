package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

const kernelLockFile = "katl-kernel-lock.json"
const embeddedKernelLock = "usr/lib/katl/kernel-inputs.json"

func verifyEmbeddedKernelLock(root, expected, release string) (string, error) {
	lock, err := kernelrelease.Read(filepath.Join(root, embeddedKernelLock))
	if err != nil {
		return "", fmt.Errorf("read embedded kernel lock; rebuild the runtime: %w", err)
	}
	identity, err := lock.Digest()
	if err != nil {
		return "", err
	}
	if identity != expected || lock.KernelRelease != release {
		return "", fmt.Errorf("embedded kernel inputs do not match the selected RPM lock; rebuild the runtime")
	}
	return identity, nil
}

func selectedKernelLock(cfg config) (kernelrelease.Lock, error) {
	path := cfg.KernelLock
	flavour := cfg.Flavour
	if flavour == "" {
		flavour = "standard"
	}
	if path == "" {
		release, err := exec.Command(filepath.Join(cfg.RepoRoot, "scripts/fedora-release")).Output()
		if err != nil {
			return kernelrelease.Lock{}, fmt.Errorf("resolve Fedora release for kernel inputs: %w", err)
		}
		path = filepath.Join(cfg.RepoRoot, "kernels", "fedora-"+strings.TrimSpace(string(release)), cfg.Architecture, flavour+".json")
	}
	lock, err := kernelrelease.Read(path)
	if err != nil {
		return lock, err
	}
	if lock.Flavour != flavour || lock.Architecture != cfg.Architecture {
		return lock, fmt.Errorf("kernel lock does not match the selected build flavour and architecture")
	}
	return lock, nil
}

func verifyKernelLock(cfg config, inputs kernelInputs) error {
	selected, err := selectedKernelLock(cfg)
	if err != nil {
		return err
	}
	lock, err := kernelrelease.Read(filepath.Join(filepath.Dir(cfg.RuntimeRoot), kernelLockFile))
	if err != nil {
		return fmt.Errorf("read runtime kernel lock; rebuild the runtime: %w", err)
	}
	actual, err := lock.Digest()
	if err != nil {
		return err
	}
	expected, err := selected.Digest()
	if err != nil {
		return err
	}
	if actual != expected || inputs.KernelSetSHA256 != expected || inputs.Target.Release != lock.KernelRelease {
		return fmt.Errorf("prepared kernel inputs do not match the selected RPM lock; rebuild the runtime")
	}
	return nil
}
