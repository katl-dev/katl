// Package kernelmodule defines the compatibility and ownership contract for
// modules built against an immutable KatlOS runtime.
package kernelmodule

import (
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Target binds modules to the exact runtime build that supplied their kernel
// inputs. Release is the kernel release used by modprobe, not the KatlOS version.
type Target struct {
	Release       string `json:"release" yaml:"release"`
	RuntimeSHA256 string `json:"runtimeSHA256" yaml:"runtimeSHA256"`
}

type Module struct {
	Name     string `json:"name" yaml:"name"`
	Path     string `json:"path" yaml:"path"`
	SHA256   string `json:"sha256" yaml:"sha256"`
	Replaces string `json:"replaces,omitempty" yaml:"replaces,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

type Contract struct {
	Target  Target   `json:"target" yaml:"target"`
	Modules []Module `json:"modules" yaml:"modules"`
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]*$`)

func (target Target) Validate() error {
	if target.Release == "" || target.Release == "." || target.Release == ".." || path.Base(target.Release) != target.Release || strings.ContainsAny(target.Release, "\\ \t\n\r\x00") {
		return fmt.Errorf("kernel release must be a safe directory name")
	}
	return validateDigest(target.RuntimeSHA256)
}

func (contract Contract) Validate() error {
	if err := contract.Target.Validate(); err != nil {
		return fmt.Errorf("kernel target: %w", err)
	}
	if len(contract.Modules) == 0 {
		return fmt.Errorf("kernel module inventory is required")
	}
	names, paths := map[string]bool{}, map[string]bool{}
	for _, module := range contract.Modules {
		name := strings.ReplaceAll(module.Name, "-", "_")
		if !namePattern.MatchString(module.Name) || names[name] {
			return fmt.Errorf("invalid or duplicate kernel module name %q", module.Name)
		}
		names[name] = true
		prefix := "usr/lib/modules/" + contract.Target.Release + "/"
		if !strings.HasPrefix(module.Path, prefix) || path.Clean(module.Path) != module.Path || strings.ContainsAny(module.Path, "\\\x00\n\r") || paths[module.Path] {
			return fmt.Errorf("invalid or duplicate kernel module path %q", module.Path)
		}
		file := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(module.Path, ".xz"), ".zst"), ".gz")
		if !strings.HasSuffix(file, ".ko") {
			return fmt.Errorf("kernel module path %q must name a .ko file, optionally compressed", module.Path)
		}
		if moduleName(module.Path) != name {
			return fmt.Errorf("kernel module path %q must match module name %q", module.Path, module.Name)
		}
		paths[module.Path] = true
		if module.Replaces != "" && strings.ReplaceAll(module.Replaces, "-", "_") != name {
			return fmt.Errorf("module %q may only replace the same named base module", module.Name)
		}
		if err := validateDigest(module.SHA256); err != nil {
			return fmt.Errorf("module %q: %w", module.Name, err)
		}
	}
	return nil
}

func (contract Contract) ValidateRuntime(runtimeSHA256 string) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	if contract.Target.RuntimeSHA256 != runtimeSHA256 {
		return fmt.Errorf("kernel extension requires runtime %s, target runtime is %s", contract.Target.RuntimeSHA256, runtimeSHA256)
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return fmt.Errorf("SHA-256 must contain 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid SHA-256: %w", err)
	}
	return nil
}
