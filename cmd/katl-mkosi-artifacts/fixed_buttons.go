package main

import (
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

//go:embed fixed_buttons/*
var fixedButtonSources embed.FS

func needsFixedButtons(release string) bool {
	// These kernels expose the button driver on the platform bus but leave
	// fixed-event buttons without platform companions.
	for _, version := range []string{"6.18.57-", "6.18.58-", "6.18.59-"} {
		if strings.HasPrefix(release, version) {
			return true
		}
	}
	return false
}

func runBuildFixedButtons(args []string, stdout, stderr io.Writer) error {
	if len(args) != 3 {
		return fmt.Errorf("build-fixed-buttons requires BUILDROOT DESTDIR BUILDDIR")
	}
	lock, err := kernelrelease.Read(filepath.Join(args[0], embeddedKernelLock))
	if err != nil {
		return err
	}
	if !needsFixedButtons(lock.KernelRelease) {
		return nil
	}
	headers, err := filepath.Abs(filepath.Join(args[0], "usr/src/kernels", lock.KernelRelease))
	if err != nil {
		return err
	}
	if err := validateKernelInputs(headers, lock.KernelRelease); err != nil {
		return err
	}
	work, err := os.MkdirTemp(args[2], "fixed-buttons-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	work, err = filepath.Abs(work)
	if err != nil {
		return err
	}
	for _, name := range []string{"Makefile", "katl_fixed_buttons.c"} {
		data, err := fixedButtonSources.ReadFile("fixed_buttons/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, name), data, 0o644); err != nil {
			return err
		}
	}
	command := exec.Command("make", "-C", headers, "M="+work, "modules")
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("build fixed ACPI button module: %w", err)
	}
	module, err := os.ReadFile(filepath.Join(work, "katl_fixed_buttons.ko"))
	if err != nil {
		return err
	}
	files := map[string][]byte{
		filepath.Join("usr/lib/modules", lock.KernelRelease, "kernel/drivers/acpi/katl_fixed_buttons.ko"): module,
		"usr/lib/modules-load.d/katl-fixed-buttons.conf":                                                  []byte("katl_fixed_buttons\n"),
	}
	for path, data := range files {
		path = filepath.Join(args[1], path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
