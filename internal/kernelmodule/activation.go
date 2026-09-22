package kernelmodule

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func LoadRequired(ctx context.Context, root string, contracts []Contract) error {
	return requiredModules(ctx, root, contracts, true, moduleCommand)
}

func VerifyRequired(ctx context.Context, root string, contracts []Contract) error {
	return requiredModules(ctx, root, contracts, false, moduleCommand)
}

func moduleCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func requiredModules(ctx context.Context, root string, contracts []Contract, load bool, run func(context.Context, string, ...string) ([]byte, error)) error {
	modules := map[string]Module{}
	var target Target
	for _, contract := range contracts {
		if err := contract.Validate(); err != nil {
			return err
		}
		if target.Release != "" && target != contract.Target {
			return fmt.Errorf("selected modules target different kernel builds")
		}
		target = contract.Target
		for _, module := range contract.Modules {
			if !module.Required {
				continue
			}
			name := moduleName(module.Name)
			if previous, exists := modules[name]; exists && previous != module {
				return fmt.Errorf("required module %q has conflicting providers", name)
			}
			modules[name] = module
		}
	}
	if len(modules) == 0 {
		return nil
	}
	kernel, err := run(ctx, "uname", "-r")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(kernel)) != target.Release {
		return fmt.Errorf("required modules target kernel %q, running kernel is %q", target.Release, strings.TrimSpace(string(kernel)))
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		module := modules[name]
		path := filepath.Join(root, module.Path)
		digest, err := fileDigest(path)
		if err != nil || digest != module.SHA256 {
			return fmt.Errorf("required module %q selected file failed integrity verification: %v", name, err)
		}
		selected, err := run(ctx, "modinfo", "-F", "filename", "--", name)
		if err != nil {
			return fmt.Errorf("resolve required module %q: %w", name, err)
		}
		resolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(selected)))
		if err != nil {
			return fmt.Errorf("resolve required module %q provider: %w", name, err)
		}
		expected, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != expected {
			return fmt.Errorf("required module %q resolves to %q instead of selected provider %q", name, resolved, path)
		}
		identity, err := sourceIdentity(ctx, path, run)
		if err != nil {
			return err
		}
		loadedPath := filepath.Join(root, "sys/module", name)
		_, loadedErr := os.Stat(loadedPath)
		if loadedErr != nil && !os.IsNotExist(loadedErr) {
			return loadedErr
		}
		if load && os.IsNotExist(loadedErr) {
			// Ignore modprobe install commands: loading a recorded driver must
			// not execute extension-owned arbitrary installation hooks.
			if _, err := run(ctx, "modprobe", "--ignore-install", "--", name); err != nil {
				return fmt.Errorf("load required module %q: %w", name, err)
			}
		}
		if _, err := os.Stat(loadedPath); err != nil {
			return fmt.Errorf("required module %q is not loaded: %w", name, err)
		}
		actual, err := os.ReadFile(filepath.Join(loadedPath, "srcversion"))
		if err != nil || strings.TrimSpace(string(actual)) != identity {
			return fmt.Errorf("required module %q loaded provider does not match selected source identity %q", name, identity)
		}
	}
	return nil
}

// Required providers must be identifiable after loading, including when another
// service loads them first. A successful modprobe alone cannot establish identity.
func sourceIdentity(ctx context.Context, path string, run func(context.Context, string, ...string) ([]byte, error)) (string, error) {
	data, err := run(ctx, "modinfo", "-F", "srcversion", path)
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(data))
	if identity == "" {
		return "", fmt.Errorf("required module %q has no source identity; rebuild it with srcversion metadata so the loaded provider can be verified", path)
	}
	return identity, nil
}
