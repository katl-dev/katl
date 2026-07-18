package generation

import "strings"

// MergeKernelCommandLine preserves current boot options that are not owned by
// generation selection while keeping the candidate's controlled values.
func MergeKernelCommandLine(base []string, current []string) []string {
	out := append([]string(nil), base...)
	seen := make(map[string]struct{}, len(out)+len(current))
	for _, option := range out {
		option = strings.TrimSpace(option)
		if option != "" {
			seen[option] = struct{}{}
		}
	}
	for _, option := range current {
		option = strings.TrimSpace(option)
		if option == "" || controlledKernelCommandLineOption(option) {
			continue
		}
		if _, ok := seen[option]; ok {
			continue
		}
		out = append(out, option)
		seen[option] = struct{}{}
	}
	return out
}

func controlledKernelCommandLineOption(option string) bool {
	switch {
	case option == "ro", option == "rw":
		return true
	case strings.HasPrefix(option, "root="):
		return true
	case strings.HasPrefix(option, "rootfstype="):
		return true
	case strings.HasPrefix(option, "systemd.machine_id="):
		return true
	case strings.HasPrefix(option, "systemd.getty_auto="):
		return true
	case strings.HasPrefix(option, "systemd.gpt_auto="):
		return true
	case strings.HasPrefix(option, "katl.generation="):
		return true
	case strings.HasPrefix(option, "katl.root-slot="):
		return true
	default:
		return false
	}
}
