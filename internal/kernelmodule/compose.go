package kernelmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// mountedBundle contains the read-only, unpacked sysexts of one selected bundle.
// A nil contract denotes userspace-only content, not an unchecked module set.
type mountedBundle struct {
	Name     string
	Roots    []string
	Contract *Contract
}

type composeRequest struct {
	Target     Target
	BaseRoot   string
	Selections []mountedBundle
	WorkDir    string
}

type moduleFile struct {
	path    string
	source  string
	owner   string
	depends []string
}

// compose generates an index-only sysext tree for the complete selected module
// set. BaseRoot must be the unmerged target runtime, never the running /usr
// overlay. The caller owns WorkDir and packages the returned tree for retention.
// No modules are loaded and no live filesystem or boot selection is changed.
func compose(ctx context.Context, request composeRequest) (string, error) {
	if err := request.Target.Validate(); err != nil {
		return "", err
	}
	selected, err := inspectSelections(ctx, request.Target, request.Selections)
	if err != nil {
		return "", err
	}
	if len(selected) == 0 {
		return "", nil
	}
	base, builtins, err := baseModules(request.BaseRoot, request.Target.Release)
	if err != nil {
		return "", err
	}
	for _, selection := range request.Selections {
		if selection.Contract == nil {
			continue
		}
		for _, module := range selection.Contract.Modules {
			name := moduleName(module.Name)
			if builtins[name] {
				return "", fmt.Errorf("module %q cannot replace a built-in kernel provider", name)
			}
			_, exists := base[name]
			if exists != (module.Replaces != "") {
				return "", fmt.Errorf("module %q replacement declaration does not match the target base provider", name)
			}
			delete(base, name)
		}
	}
	for name, module := range selected {
		base[name] = module
	}
	for name, module := range selected {
		for _, dependency := range module.depends {
			if _, exists := base[dependency]; !exists && !builtins[dependency] {
				return "", fmt.Errorf("module %q requires unavailable module %q", name, dependency)
			}
		}
	}

	work, err := os.MkdirTemp(request.WorkDir, "module-indexes-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(work)
		}
	}()
	input, output := filepath.Join(work, "input"), filepath.Join(work, "layer")
	moduleDir := filepath.Join("usr/lib/modules", request.Target.Release)
	for _, root := range []string{input, output} {
		if err := os.MkdirAll(filepath.Join(root, moduleDir), 0o755); err != nil {
			return "", err
		}
		// kmod builds use either /lib/modules or /usr/lib/modules. Both refer
		// to the same offline tree; only /usr is included in the sysext.
		if err := os.Symlink("usr/lib", filepath.Join(root, "lib")); err != nil {
			return "", err
		}
	}
	paths := map[string]string{}
	for name, module := range base {
		if other, exists := paths[module.path]; exists {
			return "", fmt.Errorf("modules %q and %q share path %q", other, name, module.path)
		}
		paths[module.path] = name
		if err := copyModuleFile(module.source, filepath.Join(input, module.path)); err != nil {
			return "", err
		}
	}
	for _, name := range []string{"modules.order", "modules.builtin", "modules.builtin.modinfo"} {
		relative := filepath.Join(moduleDir, name)
		if err := copyModuleFile(filepath.Join(request.BaseRoot, relative), filepath.Join(input, relative)); err != nil {
			return "", fmt.Errorf("copy target kernel metadata %s: %w", name, err)
		}
	}
	// Host depmod.d policy must not affect a target generation's indexes.
	command := exec.CommandContext(ctx, "depmod", "-C", "/dev/null", "-b", input, "-o", output, request.Target.Release)
	if data, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("compose target module indexes: %w: %s", err, strings.TrimSpace(string(data)))
	}
	for _, name := range []string{"modules.dep", "modules.dep.bin", "modules.alias.bin"} {
		if info, err := os.Stat(filepath.Join(output, moduleDir, name)); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("depmod did not produce %s", name)
		}
	}
	success = true
	return output, nil
}

func inspectSelections(ctx context.Context, target Target, selections []mountedBundle) (map[string]moduleFile, error) {
	modules := map[string]moduleFile{}
	for _, selection := range selections {
		wanted := map[string]Module{}
		if selection.Contract != nil {
			if err := selection.Contract.Validate(); err != nil {
				return nil, fmt.Errorf("extension %q: %w", selection.Name, err)
			}
			if selection.Contract.Target != target {
				return nil, fmt.Errorf("extension %q targets a different kernel build", selection.Name)
			}
			for _, module := range selection.Contract.Modules {
				wanted[module.Path] = module
			}
		}
		found := map[string]bool{}
		for _, root := range selection.Roots {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				relative = filepath.ToSlash(relative)
				if entry.Type()&os.ModeSymlink != 0 && slices.Contains([]string{"usr", "usr/lib", "usr/lib/modules", "lib"}, relative) {
					return fmt.Errorf("extension %q redirects module-tree ancestor %q", selection.Name, relative)
				}
				if entry.IsDir() {
					return nil
				}
				inModules := strings.HasPrefix(relative, "usr/lib/modules/") || strings.HasPrefix(relative, "lib/modules/")
				if !inModules && !isModule(relative) {
					return nil
				}
				module, exists := wanted[relative]
				if !exists || found[relative] {
					return fmt.Errorf("extension %q contains undeclared or duplicate module-tree file %q; global indexes belong to Katl", selection.Name, relative)
				}
				if !entry.Type().IsRegular() {
					return fmt.Errorf("module %q must be a regular file", relative)
				}
				dependencies, err := verifyModule(ctx, path, module, target.Release)
				if err != nil {
					return err
				}
				name := moduleName(module.Name)
				if other, exists := modules[name]; exists {
					return fmt.Errorf("module %q is provided by both %q and %q", name, other.owner, selection.Name)
				}
				modules[name] = moduleFile{
					path:    relative,
					source:  path,
					owner:   selection.Name,
					depends: dependencies,
				}
				found[relative] = true
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		for path := range wanted {
			if !found[path] {
				return nil, fmt.Errorf("extension %q is missing declared module %q", selection.Name, path)
			}
		}
	}
	return modules, nil
}

func baseModules(root, release string) (map[string]moduleFile, map[string]bool, error) {
	modules := map[string]moduleFile{}
	moduleDir := filepath.Join(root, "usr/lib/modules", release)
	err := filepath.WalkDir(moduleDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !isModule(path) {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("base module %q must be a regular file", path)
		}
		name := moduleName(path)
		if _, exists := modules[name]; exists {
			return fmt.Errorf("target base has duplicate provider for module %q", name)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		modules[name] = moduleFile{
			path:   relative,
			source: path,
			owner:  "runtime",
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(filepath.Join(moduleDir, "modules.builtin"))
	if err != nil {
		return nil, nil, err
	}
	builtins := map[string]bool{}
	for _, path := range strings.Fields(string(data)) {
		builtins[moduleName(path)] = true
	}
	return modules, builtins, nil
}

func verifyModule(ctx context.Context, path string, module Module, release string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != module.SHA256 {
		return nil, fmt.Errorf("module %q SHA-256 mismatch", module.Name)
	}
	for field, want := range map[string]string{"name": moduleName(module.Name), "vermagic": release} {
		data, err := exec.CommandContext(ctx, "modinfo", "-F", field, path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("inspect module %q: %w: %s", module.Name, err, strings.TrimSpace(string(data)))
		}
		values := strings.Fields(string(data))
		if len(values) == 0 || values[0] != want {
			return nil, fmt.Errorf("module %q %s does not match %q", module.Name, field, want)
		}
	}
	if module.Required {
		if _, err := sourceIdentity(ctx, path, moduleCommand); err != nil {
			return nil, err
		}
	}
	data, err := exec.CommandContext(ctx, "modinfo", "-F", "depends", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect dependencies of module %q: %w: %s", module.Name, err, strings.TrimSpace(string(data)))
	}
	var dependencies []string
	for _, dependency := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if dependency != "" {
			dependencies = append(dependencies, moduleName(dependency))
		}
	}
	return dependencies, nil
}

func isModule(path string) bool {
	path = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(path, ".xz"), ".zst"), ".gz")
	return strings.HasSuffix(path, ".ko")
}

func moduleName(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".xz"), ".zst"), ".gz")
	return strings.ReplaceAll(strings.TrimSuffix(name, ".ko"), "-", "_")
}

func copyModuleFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}
