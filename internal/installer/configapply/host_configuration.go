package configapply

import (
	"bufio"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type HostConfigurationChangePlan struct {
	Live              bool
	Sets              []string
	Paths             []string
	Effects           []generation.ConfigApplyEffect
	Message           string
	Commands          []Command
	SysctlAssignments []HostSysctlAssignment
}

type HostSysctlAssignment struct {
	Key   string
	Value string
}

func planHostConfigurationChange(current, desired manifest.HostConfiguration) HostConfigurationChangePlan {
	currentSets := current.Sets
	desiredSets := desired.Sets
	names := sortedStringUnion(hostSetNames(currentSets), hostSetNames(desiredSets))
	plan := HostConfigurationChangePlan{Live: true}
	notifications := make(map[string]string)
	udevReload := false
	systemdReload := false
	stagedReason := ""

	for _, name := range names {
		before, beforeOK := currentSets[name]
		after, afterOK := desiredSets[name]
		if beforeOK && strings.TrimSpace(before.State) == manifest.HostConfigurationAbsent {
			beforeOK = false
		}
		if afterOK && strings.TrimSpace(after.State) == manifest.HostConfigurationAbsent {
			afterOK = false
		}
		if beforeOK == afterOK && reflect.DeepEqual(before, after) {
			continue
		}
		plan.Sets = append(plan.Sets, name)
		beforeFiles := hostFilesByPath(before, beforeOK)
		afterFiles := hostFilesByPath(after, afterOK)
		changedPaths := sortedStringUnion(hostFilePaths(beforeFiles), hostFilePaths(afterFiles))
		for _, filePath := range changedPaths {
			if reflect.DeepEqual(beforeFiles[filePath], afterFiles[filePath]) {
				continue
			}
			plan.Paths = append(plan.Paths, filePath)
			switch {
			case strings.HasPrefix(filePath, "/etc/modules-load.d/"),
				strings.HasPrefix(filePath, "/etc/modprobe.d/"):
				if stagedReason == "" {
					stagedReason = "kernel module configuration is next-boot-only"
				}
				if strings.HasPrefix(filePath, "/etc/modules-load.d/") {
					if file, exists := afterFiles[filePath]; exists && file.Content != nil {
						for _, module := range parseModulesLoad(*file.Content) {
							plan.Effects = append(plan.Effects, plannedEffect("load-and-verify", "module "+module))
						}
					}
				}
			case strings.HasPrefix(filePath, "/etc/sysctl.d/") && strings.HasSuffix(filePath, ".conf"):
				assignments, ok := liveSysctlDiff(beforeFiles[filePath], afterFiles[filePath])
				if !ok {
					if file, exists := afterFiles[filePath]; exists && file.Content != nil {
						if values, concrete := parseConcreteSysctl(*file.Content); concrete {
							for key := range values {
								plan.Effects = append(plan.Effects, plannedEffect("apply-and-verify", "sysctl "+key))
							}
						}
					}
					if stagedReason == "" {
						stagedReason = "sysctl additions, removals, ambiguous assignments, and key-set changes require next boot"
					}
					continue
				}
				plan.SysctlAssignments = append(plan.SysctlAssignments, assignments...)
				for _, assignment := range assignments {
					plan.Effects = append(plan.Effects, plannedEffect("apply-and-verify", "sysctl "+assignment.Key))
				}
			case strings.HasPrefix(filePath, "/etc/udev/rules.d/") && strings.HasSuffix(filePath, ".rules"):
				udevReload = true
				if _, exists := afterFiles[filePath]; exists {
					plan.Effects = append(plan.Effects, plannedEffect("verify", "udev rules "+filePath))
					plan.Commands = append(plan.Commands, Command{
						Name:         "udev-rules-verify",
						EffectAction: "verify",
						EffectTarget: "udev rules " + filePath,
						Argv:         []string{"/usr/bin/udevadm", "verify", activeHostConfigurationPath(filePath)},
					})
				}
			default:
				selected := after
				if !afterOK {
					selected = before
				}
				if len(selected.Notify.Systemd) == 0 && stagedReason == "" {
					stagedReason = fmt.Sprintf("%s has no proven live adapter or bounded systemd notification", filePath)
				}
			}
			if strings.HasPrefix(filePath, "/etc/systemd/system/") {
				systemdReload = true
			}
		}
		selected := after
		if !afterOK {
			selected = before
		}
		for _, notification := range selected.Notify.Systemd {
			notifications[notification.Unit] = notification.Action
		}
	}

	sort.Strings(plan.Paths)
	plan.Paths = compactStrings(plan.Paths)
	seenSysctls := make(map[string]struct{}, len(plan.SysctlAssignments))
	for _, assignment := range plan.SysctlAssignments {
		if _, exists := seenSysctls[assignment.Key]; exists && stagedReason == "" {
			stagedReason = "duplicate sysctl assignments across changed files have ambiguous precedence"
		}
		seenSysctls[assignment.Key] = struct{}{}
	}
	sort.Slice(plan.SysctlAssignments, func(i, j int) bool {
		return plan.SysctlAssignments[i].Key < plan.SysctlAssignments[j].Key
	})
	if udevReload {
		plan.Effects = append(plan.Effects, plannedEffect("reload", "udev rules"))
	}
	if systemdReload {
		plan.Effects = append(plan.Effects, plannedEffect("reload", "systemd manager"))
	}
	units := make([]string, 0, len(notifications))
	for unit := range notifications {
		units = append(units, unit)
	}
	sort.Strings(units)
	for _, unit := range units {
		plan.Effects = append(plan.Effects, plannedEffect(notifications[unit], "systemd unit "+unit))
	}
	sort.SliceStable(plan.Effects, func(i, j int) bool {
		if plan.Effects[i].Target == plan.Effects[j].Target {
			return plan.Effects[i].Action < plan.Effects[j].Action
		}
		return plan.Effects[i].Target < plan.Effects[j].Target
	})
	if stagedReason != "" {
		plan.Live = false
		plan.Message = hostConfigurationMessage(stagedReason, plan.Effects)
		return plan
	}
	if udevReload {
		plan.Commands = append(plan.Commands, Command{
			Name:         "udev-rules-reload",
			EffectAction: "reload",
			EffectTarget: "udev rules",
			Argv:         []string{"/usr/bin/udevadm", "control", "--reload"},
		})
	}
	if systemdReload {
		plan.Commands = append(plan.Commands, Command{
			Name:         "systemd-daemon-reload",
			EffectAction: "reload",
			EffectTarget: "systemd manager",
			Argv:         []string{"systemctl", "daemon-reload"},
		})
	}
	for _, unit := range units {
		action := notifications[unit]
		plan.Commands = append(plan.Commands, Command{
			Name:         "systemd-notify-" + unit,
			EffectAction: action,
			EffectTarget: "systemd unit " + unit,
			Argv:         []string{"systemctl", action, unit},
		})
	}
	if len(plan.Effects) == 0 {
		plan.Live = false
		plan.Message = "changed host configuration has no live action"
	} else {
		plan.Message = hostConfigurationMessage("", plan.Effects)
		if udevReload {
			plan.Message += "; existing devices will not be retriggered"
		}
	}
	return plan
}

func plannedEffect(action, target string) generation.ConfigApplyEffect {
	return generation.ConfigApplyEffect{Action: action, Target: target, Status: generation.ConfigApplyActionPlanned}
}

func hostConfigurationMessage(prefix string, effects []generation.ConfigApplyEffect) string {
	parts := make([]string, 0, len(effects)+1)
	if strings.TrimSpace(prefix) != "" {
		parts = append(parts, prefix)
	}
	for _, effect := range effects {
		parts = append(parts, effect.Action+" "+effect.Target)
	}
	return strings.Join(parts, "; ")
}

func parseModulesLoad(content string) []string {
	var modules []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if comment := strings.IndexAny(line, "#;"); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		if fields := strings.Fields(line); len(fields) == 1 {
			modules = append(modules, fields[0])
		}
	}
	sort.Strings(modules)
	return compactStrings(modules)
}

func liveSysctlDiff(before manifest.HostConfigurationFile, after manifest.HostConfigurationFile) ([]HostSysctlAssignment, bool) {
	if before.Content == nil || after.Content == nil {
		return nil, false
	}
	oldValues, ok := parseConcreteSysctl(*before.Content)
	if !ok {
		return nil, false
	}
	newValues, ok := parseConcreteSysctl(*after.Content)
	if !ok || len(oldValues) != len(newValues) {
		return nil, false
	}
	keys := make([]string, 0, len(newValues))
	for key := range newValues {
		if _, exists := oldValues[key]; !exists {
			return nil, false
		}
		if oldValues[key] != newValues[key] {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, true
	}
	sort.Strings(keys)
	out := make([]HostSysctlAssignment, 0, len(keys))
	for _, key := range keys {
		out = append(out, HostSysctlAssignment{Key: key, Value: newValues[key]})
	}
	return out, true
}

func parseConcreteSysctl(content string) (map[string]string, bool) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "-") {
			return nil, false
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return nil, false
			}
			key = fields[0]
			value = strings.Join(fields[1:], " ")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || !concreteSysctlKey(key) || strings.ContainsAny(value, "\x00\r\n\t ") {
			return nil, false
		}
		if _, duplicate := values[key]; duplicate {
			return nil, false
		}
		values[key] = value
	}
	return values, scanner.Err() == nil && len(values) > 0
}

func concreteSysctlKey(key string) bool {
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func activeHostConfigurationPath(filePath string) string {
	return path.Join("/run/confexts/"+generation.GeneratedConfextName, strings.TrimPrefix(filePath, "/"))
}

func hostSetNames(sets map[string]manifest.HostConfigurationSet) map[string]struct{} {
	names := make(map[string]struct{}, len(sets))
	for name := range sets {
		names[name] = struct{}{}
	}
	return names
}

func hostFilesByPath(set manifest.HostConfigurationSet, exists bool) map[string]manifest.HostConfigurationFile {
	files := make(map[string]manifest.HostConfigurationFile)
	if !exists {
		return files
	}
	for _, file := range set.Files {
		files[file.Path] = file
	}
	return files
}

func hostFilePaths(files map[string]manifest.HostConfigurationFile) map[string]struct{} {
	paths := make(map[string]struct{}, len(files))
	for filePath := range files {
		paths[filePath] = struct{}{}
	}
	return paths
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
