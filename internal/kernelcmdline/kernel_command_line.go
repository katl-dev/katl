package kernelcmdline

import (
	"fmt"
	"slices"
	"strings"
)

// ValidateConfigured validates operator-owned kernel command-line additions.
// Each entry is one whitespace-free kernel argument. Katl-owned arguments are
// deliberately unavailable through configuration.
func ValidateConfigured(options []string) error {
	seen := make(map[string]struct{}, len(options))
	for i, option := range options {
		if option == "" {
			return fmt.Errorf("commandLine[%d] must not be empty", i)
		}
		if strings.TrimSpace(option) != option {
			return fmt.Errorf("commandLine[%d] %q must not contain leading or trailing whitespace", i, option)
		}
		if strings.ContainsAny(option, " \t\n\r") {
			return fmt.Errorf("commandLine[%d] %q must be one argument without whitespace", i, option)
		}
		if Protected(option) {
			return fmt.Errorf("commandLine[%d] %q is managed by Katl and cannot be configured", i, option)
		}
		if _, ok := seen[option]; ok {
			return fmt.Errorf("commandLine contains duplicate argument %q", option)
		}
		seen[option] = struct{}{}
	}
	return nil
}

func ValidateRequiredCompatibility(configured, required []string) error {
	requiredSet := make(map[string]struct{}, len(required))
	for _, option := range required {
		requiredSet[strings.TrimSpace(option)] = struct{}{}
	}
	for _, option := range configured {
		if _, ok := requiredSet[option]; ok {
			return fmt.Errorf("configured argument %q is required by the selected KatlOS image; remove it from kernel.commandLine", option)
		}
	}
	return nil
}

func ValidateEffective(configured, effective []string) error {
	if err := ValidateConfigured(configured); err != nil {
		return err
	}
	for _, option := range configured {
		if !slices.Contains(effective, option) {
			return fmt.Errorf("configured argument %q is missing from the effective kernel command line", option)
		}
	}
	return nil
}

// ReplaceConfigured replaces the previous operator-owned arguments while
// preserving image-required, Katl-owned, and inherited host arguments.
func ReplaceConfigured(base, previous, desired []string) []string {
	remove := make(map[string]struct{}, len(previous))
	for _, option := range previous {
		remove[option] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(desired))
	seen := make(map[string]struct{}, len(base)+len(desired))
	for _, option := range base {
		if _, configured := remove[option]; configured {
			continue
		}
		if _, ok := seen[option]; ok {
			continue
		}
		seen[option] = struct{}{}
		out = append(out, option)
	}
	for _, option := range desired {
		if _, ok := seen[option]; ok {
			continue
		}
		seen[option] = struct{}{}
		out = append(out, option)
	}
	return out
}

// MergeCurrent preserves running boot arguments that Katl does not own and
// that are not being replaced as prior operator configuration.
func MergeCurrent(base, current, replaced []string) []string {
	out := slices.Clone(base)
	seen := make(map[string]struct{}, len(base)+len(current))
	for _, option := range out {
		seen[option] = struct{}{}
	}
	skip := make(map[string]struct{}, len(replaced))
	for _, option := range replaced {
		skip[option] = struct{}{}
	}
	for _, option := range current {
		option = strings.TrimSpace(option)
		if option == "" || Protected(option) {
			continue
		}
		if _, replaced := skip[option]; replaced {
			continue
		}
		if _, ok := seen[option]; ok {
			continue
		}
		seen[option] = struct{}{}
		out = append(out, option)
	}
	return out
}

// Protected reports arguments owned by Katl's root selection, immutable-root,
// generation identity, or recovery flow.
func Protected(option string) bool {
	switch option {
	case "ro", "rw":
		return true
	}
	for _, prefix := range []string{
		"root=",
		"rootfstype=",
		"init=",
		"rdinit=",
		"usr=",
		"mount.usr=",
		"systemd.machine_id=",
		"systemd.getty_auto=",
		"systemd.gpt_auto=",
		"systemd.unit=",
		"rd.systemd.unit=",
		"systemd.volatile=",
		"systemd.verity=",
		"systemd.image_policy=",
		"katl.generation=",
		"katl.root-slot=",
	} {
		if strings.HasPrefix(option, prefix) {
			return true
		}
	}
	return false
}
