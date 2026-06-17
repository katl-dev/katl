package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DirectRuntimeConfig struct {
	RuntimeRoot        string
	Kernel             string
	Initrd             string
	RequireVMTestAgent bool
	Expect             string
	KernelCommandLine  []string
	VM                 VMConfig
}

type directRuntimeRecord struct {
	APIVersion         string   `json:"apiVersion"`
	Kind               string   `json:"kind"`
	RuntimeRoot        string   `json:"runtimeRoot"`
	Kernel             string   `json:"kernel"`
	Initrd             string   `json:"initrd"`
	RequireVMTestAgent bool     `json:"requireVMTestAgent"`
	KernelCommandLine  []string `json:"kernelCommandLine"`
}

func RunDirectRuntime(ctx context.Context, result Result, config DirectRuntimeConfig, runner VMRunner) Result {
	config, err := prepareDirectRuntime(result, config)
	if err != nil {
		return finishVM(result, "direct-runtime", StatusFailed, err.Error(), result.Started, runnerTime())
	}
	vm := config.VM
	vm.Phase = "direct-runtime"
	vm.Expect = first(vm.Expect, config.Expect, runtimeBootSignal)
	vm.Boot = VMBoot{
		Kernel:      config.Kernel,
		Initrd:      config.Initrd,
		Image:       config.RuntimeRoot,
		ImageFormat: DiskRaw,
		CommandLine: directRuntimeCommandLine(config),
	}
	if config.RequireVMTestAgent {
		vm.VSock.Enabled = true
		vm.Agent.RequireHealth = true
	}
	return runner.Run(ctx, result, vm)
}

func prepareDirectRuntime(result Result, config DirectRuntimeConfig) (DirectRuntimeConfig, error) {
	if strings.TrimSpace(config.RuntimeRoot) == "" {
		return config, errors.New("direct runtime root squashfs is required")
	}
	if err := requireRegularFile("direct runtime root squashfs", config.RuntimeRoot); err != nil {
		return config, err
	}
	if strings.TrimSpace(config.Kernel) == "" || strings.TrimSpace(config.Initrd) == "" {
		kernel, initrd, err := discoverDirectRuntimeBootFiles(config.RuntimeRoot)
		if err != nil {
			return config, err
		}
		if strings.TrimSpace(config.Kernel) == "" {
			config.Kernel = kernel
		}
		if strings.TrimSpace(config.Initrd) == "" {
			config.Initrd = initrd
		}
	}
	if err := requireRegularFile("direct runtime kernel", config.Kernel); err != nil {
		return config, err
	}
	if err := requireRegularFile("direct runtime initrd", config.Initrd); err != nil {
		return config, err
	}
	if err := os.MkdirAll(filepath.Dir(result.Artifacts.DirectRuntime), 0o755); err != nil {
		return config, err
	}
	record := directRuntimeRecord{
		APIVersion:         "katl.dev/v1alpha1",
		Kind:               "DirectRuntimeVMTestInput",
		RuntimeRoot:        config.RuntimeRoot,
		Kernel:             config.Kernel,
		Initrd:             config.Initrd,
		RequireVMTestAgent: config.RequireVMTestAgent,
		KernelCommandLine:  directRuntimeCommandLine(config),
	}
	if err := writeJSON(result.Artifacts.DirectRuntime, record); err != nil {
		return config, err
	}
	return config, nil
}

func discoverDirectRuntimeBootFiles(runtimeRoot string) (string, string, error) {
	dir := filepath.Dir(runtimeRoot)
	rootDir := strings.TrimSuffix(filepath.Base(runtimeRoot), ".squashfs")
	candidates := []string{
		filepath.Join(dir, rootDir),
		filepath.Join(dir, "katl-runtime-root"),
	}
	for _, candidate := range candidates {
		kernel, initrd, err := findBootFiles(candidate)
		if err == nil {
			return kernel, initrd, nil
		}
	}
	return "", "", fmt.Errorf("direct runtime kernel/initrd not found next to %s", runtimeRoot)
}

func findBootFiles(root string) (string, string, error) {
	var matches []string
	bootDir := filepath.Join(root, "boot")
	err := filepath.WalkDir(bootDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				return nil
			}
			return err
		}
		if entry.IsDir() || entry.Name() != "linux" {
			return nil
		}
		matches = append(matches, path)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	sort.Strings(matches)
	for _, kernel := range matches {
		initrd := filepath.Join(filepath.Dir(kernel), "initrd")
		if err := requireRegularFile("direct runtime kernel", kernel); err != nil {
			continue
		}
		if err := requireRegularFile("direct runtime initrd", initrd); err != nil {
			continue
		}
		return kernel, initrd, nil
	}
	return "", "", fmt.Errorf("runtime boot files not found under %s", root)
}

func directRuntimeCommandLine(config DirectRuntimeConfig) []string {
	options := []string{
		"root=/dev/vda",
		"rootfstype=squashfs",
		"ro",
		"systemd.volatile=state",
		"systemd.mask=var.mount",
		"systemd.mask=etc-kubernetes.mount",
		"systemd.mask=katl-kubeadm-ready.target",
		"systemd.mask=katlc-agent.service",
		"systemd.mask=katl-generation-activate.service",
	}
	options = append(options, runtimeConsoleOptions...)
	if config.RequireVMTestAgent {
		options = append(options, "katl.vmtest_agent=1")
	}
	if vmtestDebugOnFailure() {
		options = append(options, runtimeDebugShellOption)
	}
	options = append(options, config.KernelCommandLine...)
	return options
}

func vmtestDebugOnFailure() bool {
	return envBool("KATL_VMTEST_DEBUG_ON_FAILURE")
}

func requireRegularFile(name, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s not found: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file: %s", name, path)
	}
	return nil
}
