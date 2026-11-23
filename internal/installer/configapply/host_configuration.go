package configapply

import (
	"bufio"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/networkdconfig"
	"github.com/katl-dev/katl/internal/systemdunit"
)

type HostConfigurationChangePlan struct {
	Live     bool
	Sets     []string
	Paths    []string
	Effects  []generation.ConfigApplyEffect
	Message  string
	warnings []string
	// nextBootNotices explain staged changes that the next boot will not
	// apply as written.
	nextBootNotices        []string
	Commands               []Command
	rollbackCommands       []Command
	unitsToStop            []string
	unitsToRemove          []string
	unitsToEnable          []string
	unitsToDisable         []string
	unitNotifications      map[string]string
	beforeCommands         []Command
	beforeRollbackCommands []Command
	enablementStarted      bool
	inactiveNotifications  []string
	resetFailures          []string
	SysctlAssignments      []HostSysctlAssignment
	netdevs                netdevPlan
}

type HostSysctlAssignment struct {
	Key   string
	Value string
}

func sortedStringUnion[A any](left, right map[string]A) []string {
	union := maps.Clone(left)
	for key, value := range right {
		union[key] = value
	}
	keys := slices.Collect(maps.Keys(union))
	slices.Sort(keys)
	return keys
}

// planHostConfigurationChange classifies a host configuration change. Sysctl
// eligibility also depends on the sysctl.d files and kernel parameters below
// root, the running host's root directory.
func planHostConfigurationChange(current, desired manifest.HostConfiguration, root string) HostConfigurationChangePlan {
	currentSets := current.Sets
	desiredSets := desired.Sets
	names := sortedStringUnion(hostSetNames(currentSets), hostSetNames(desiredSets))
	plan := HostConfigurationChangePlan{Live: true}
	notifications := make(map[string]string)
	plan.unitsToEnable = unitDifference(desired.EnabledUnits, current.EnabledUnits)
	plan.unitsToDisable = unitDifference(current.EnabledUnits, desired.EnabledUnits)
	for _, unit := range slices.Concat(plan.unitsToEnable, plan.unitsToDisable) {
		plan.Paths = append(plan.Paths, "systemd:"+unit)
	}
	udevReload := false
	networkdReload := false
	linkReload := false
	systemdReload := false
	stagedReason := ""
	for _, unit := range desired.MaskedUnits {
		if !slices.Contains(current.MaskedUnits, unit) {
			plan.unitsToStop = append(plan.unitsToStop, unit)
			plan.Paths = append(plan.Paths, "/etc/systemd/system/"+unit)
			systemdReload = true
		}
	}
	for _, unit := range current.MaskedUnits {
		if !slices.Contains(desired.MaskedUnits, unit) {
			plan.Paths = append(plan.Paths, "/etc/systemd/system/"+unit)
			systemdReload = true
		}
	}
	var masks HostConfigurationActivationPlan
	masks.addUnitMasks(plan.unitsToStop, true, true)
	plan.Effects = append(plan.Effects, masks.Effects...)

	currentSysfs := hostSysfsByName(current.Sysfs)
	desiredSysfs := hostSysfsByName(desired.Sysfs)
	for _, name := range sortedStringUnion(hostSysfsNames(currentSysfs), hostSysfsNames(desiredSysfs)) {
		before, beforeExists := currentSysfs[name]
		after, afterExists := desiredSysfs[name]
		if beforeExists == afterExists && before == after {
			continue
		}
		plan.Paths = append(plan.Paths, name)
		if stagedReason == "" {
			stagedReason = "sysfs configuration applies and verifies on next boot"
		}
		if afterExists {
			plan.Effects = append(plan.Effects, plannedEffect("apply-and-verify", "sysfs "+name))
		} else {
			plan.Effects = append(plan.Effects, plannedEffect("stop-managing", "sysfs "+name))
		}
	}

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
		selected := after
		if !afterOK {
			selected = before
		}
		for _, notification := range selected.Notify.Systemd {
			notifications[notification.Unit] = notification.Action
		}
	}

	currentFiles := hostFilesByPath(currentSets)
	desiredFiles := hostFilesByPath(desiredSets)
	changedPaths := sortedStringUnion(hostFilePaths(currentFiles), hostFilePaths(desiredFiles))
	runtimeFilesChanged := false
	changedSysctls := make(map[string]struct{})
	for _, filePath := range changedPaths {
		before, beforeExists := currentFiles[filePath]
		after, afterExists := desiredFiles[filePath]
		if beforeExists == afterExists && reflect.DeepEqual(before.File, after.File) {
			continue
		}
		runtimeFilesChanged = true
		plan.Paths = append(plan.Paths, filePath)
		unit := changedUnit(filePath)
		component := changedSystemdComponent(filePath)
		switch {
		case filePath == "/etc/systemd/system/"+systemdunit.TargetName:
			systemdReload = true
		case unit != "" && beforeExists && !afterExists && filePath == "/etc/systemd/system/"+unit &&
			!slices.Contains(desired.EnabledUnits, unit) && !slices.Contains(plan.unitsToDisable, unit):
			// A restart after daemon-reload would target a unit systemd can no
			// longer load, so stop it while its definition still exists.
			plan.unitsToStop = append(plan.unitsToStop, unit)
			plan.unitsToRemove = append(plan.unitsToRemove, unit)
			plan.Effects = append(plan.Effects, plannedEffect("remove", "systemd unit "+unit))
		case unit != "":
			deriveSystemdNotification(notifications, filePath)
			if slices.Contains(desired.EnabledUnits, unit) && !slices.Contains(plan.unitsToEnable, unit) {
				plan.unitsToEnable = append(plan.unitsToEnable, unit)
			}
		case component != "":
			deriveSystemdNotification(notifications, filePath)
		case networkdconfig.IsPath(filePath):
			if isLinkConfigurationPath(filePath) {
				udevReload = true
				linkReload = true
			} else {
				networkdReload = true
			}
			if afterExists {
				plan.Effects = append(plan.Effects, plannedEffect("load", "networkd configuration "+filePath))
			} else {
				plan.Effects = append(plan.Effects, plannedEffect("stop-managing", "networkd configuration "+filePath))
			}
		case strings.HasPrefix(filePath, "/etc/modules-load.d/"),
			strings.HasPrefix(filePath, "/etc/modprobe.d/"):
			if stagedReason == "" {
				stagedReason = "kernel module configuration is next-boot-only"
			}
			if strings.HasPrefix(filePath, "/etc/modules-load.d/") {
				if afterExists && after.File.Content != nil {
					for _, module := range parseModulesLoad(*after.File.Content) {
						plan.Effects = append(plan.Effects, plannedEffect("load-and-verify", "module "+module))
					}
				}
			}
		case isSysctlPath(filePath):
			beforeValues, beforeConcrete := sysctlFileValues(before.File, beforeExists)
			afterValues, afterConcrete := sysctlFileValues(after.File, afterExists)
			if !beforeConcrete || !afterConcrete {
				for _, key := range sortedKeys(afterValues) {
					plan.Effects = append(plan.Effects, plannedEffect("apply-and-verify", "sysctl "+key))
				}
				if stagedReason == "" {
					stagedReason = "sysctl globs, exclusions, and assignments Katl cannot parse require next boot"
				}
				continue
			}
			for _, key := range sortedStringUnion(beforeValues, afterValues) {
				previous, hadPrevious := beforeValues[key]
				next, hasNext := afterValues[key]
				if hadPrevious != hasNext || previous != next {
					changedSysctls[key] = struct{}{}
				}
			}
		case strings.HasPrefix(filePath, "/etc/containerd/conf.d/") && strings.HasSuffix(filePath, ".toml"):
			if stagedReason == "" {
				stagedReason = "containerd configuration overlays load on next boot"
			}
			if afterExists {
				plan.Effects = append(plan.Effects, plannedEffect("load", "containerd configuration "+filePath))
			}
		case strings.HasPrefix(filePath, "/etc/udev/rules.d/") && strings.HasSuffix(filePath, ".rules"):
			udevReload = true
			if afterExists {
				plan.Effects = append(plan.Effects, plannedEffect("verify", "udev rules "+filePath))
				plan.Commands = append(plan.Commands, Command{
					Name:         "udev-rules-verify",
					EffectAction: "verify",
					EffectTarget: "udev rules " + filePath,
					Argv:         []string{"/usr/bin/udevadm", "verify", activeHostConfigurationPath(filePath)},
				})
			}
		default:
			selected := after.Notify
			if !afterExists {
				selected = before.Notify
			}
			if len(selected.Systemd) == 0 && stagedReason == "" {
				stagedReason = fmt.Sprintf("%s has no proven live adapter or bounded systemd notification", filePath)
			}
		}
		if strings.HasPrefix(filePath, "/etc/systemd/system/") {
			systemdReload = true
		}
	}
	plan.netdevs = planNetdevs(currentFiles, desiredFiles, desired.PreservedNetdevs)
	for _, name := range plan.netdevs.deletes {
		plan.Effects = append(plan.Effects, plannedEffect("delete", "netdev "+name))
	}
	for _, name := range plan.netdevs.changed {
		plan.Effects = append(plan.Effects, plannedEffect("recreate", "netdev "+name))
	}
	if len(plan.netdevs.changed) > 0 {
		reason := fmt.Sprintf("netdev %s settings apply only when networkd creates the device, so they take effect after the next boot; apply with --mode auto or --mode next-boot", strings.Join(plan.netdevs.changed, ", "))
		if stagedReason == "" {
			stagedReason = reason
		} else {
			stagedReason += "; " + reason
		}
	}
	for unit := range notifications {
		if slices.Contains(desired.MaskedUnits, unit) || slices.Contains(plan.unitsToDisable, unit) || slices.Contains(plan.unitsToRemove, unit) {
			delete(notifications, unit)
		}
	}
	plan.unitNotifications = notifications

	sort.Strings(plan.Paths)
	plan.Paths = compactStrings(plan.Paths)
	if len(changedSysctls) > 0 {
		var reasons, notices []string
		plan.SysctlAssignments, plan.Effects, reasons, notices = planSysctlAssignments(root, currentFiles, desiredFiles, changedSysctls, plan.Effects)
		plan.nextBootNotices = append(plan.nextBootNotices, notices...)
		if len(reasons) > 0 && stagedReason == "" {
			stagedReason = strings.Join(reasons, "; ")
		}
	}
	if udevReload {
		plan.Effects = append(plan.Effects, plannedEffect("reload", "udev rules"))
	}
	if networkdReload {
		plan.Effects = append(plan.Effects, plannedEffect("reload", "network configuration"))
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
	for _, unit := range plan.unitsToEnable {
		plan.Effects = append(plan.Effects, plannedEffect("enable-and-start", "systemd unit "+unit))
	}
	for _, unit := range plan.unitsToDisable {
		plan.Effects = append(plan.Effects, plannedEffect("disable-and-stop", "systemd unit "+unit))
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
	if networkdReload {
		// Let networkd reconfigure matching links without deleting devices it retains.
		plan.Commands = append(plan.Commands, Command{
			Name: "networkd-reload", EffectAction: "reload", EffectTarget: "network configuration",
			Argv: []string{"networkctl", "reload"},
		})
	}
	if networkdReload || linkReload {
		plan.warnings = append(plan.warnings, "Live network changes may interrupt management access and may leave running interfaces different from the saved configuration; inspect the network and recreate affected interfaces if needed.")
	}
	if linkReload {
		plan.warnings = append(plan.warnings, "Changed .link settings are loaded for future device events; existing interfaces are not retriggered. Recreate the affected interface or reboot when needed to apply them.")
	}
	// Rollback reloads the restored generation, then enforces its masks. The
	// candidate's rule verification is not replayed: an added rules file does
	// not exist in the restored generation.
	plan.rollbackCommands = slices.DeleteFunc(slices.Clone(plan.Commands), func(command Command) bool {
		return command.Name == "udev-rules-verify"
	})
	if systemdReload && len(current.MaskedUnits) > 0 {
		var previous HostConfigurationActivationPlan
		previous.addUnitMasks(current.MaskedUnits, true, true)
		plan.rollbackCommands = append(plan.rollbackCommands, previous.Commands...)
	}
	plan.Commands = append(plan.Commands, masks.Commands...)
	if len(plan.Effects) == 0 && runtimeFilesChanged {
		plan.Live = false
		plan.Message = "changed host configuration has no live action"
	} else if len(plan.Effects) == 0 {
		plan.Message = "host configuration metadata changed without changing runtime state"
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

func isLinkConfigurationPath(filePath string) bool {
	return networkdconfig.IsPath(filePath) && (strings.HasSuffix(filePath, ".link") || strings.Contains(filePath, ".link.d/"))
}

func hostSysfsByName(settings []manifest.HostConfigurationSysfsSetting) map[string]string {
	out := make(map[string]string, len(settings))
	for _, setting := range settings {
		out[setting.Name] = setting.Value
	}
	return out
}

func hostSysfsNames(settings map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(settings))
	for name := range settings {
		out[name] = struct{}{}
	}
	return out
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

// planSysctlAssignments selects the live writes for changed sysctl keys. A key
// applies live only when its effective desired assignment comes from a value
// that no later file overrides with another value and the running kernel can
// snapshot, write, verify, and restore it. Otherwise it returns why the change
// must wait for the next boot, and notices for managed assignments that the
// next boot would not apply either.
func planSysctlAssignments(root string, currentFiles, desiredFiles map[string]ownedHostFile, keys map[string]struct{}, effects []generation.ConfigApplyEffect) (assignments []HostSysctlAssignment, _ []generation.ConfigApplyEffect, reasons, notices []string) {
	owned := make(map[string]struct{})
	for filePath := range currentFiles {
		owned[filePath] = struct{}{}
	}
	managed := make(map[string]string)
	for filePath, file := range desiredFiles {
		owned[filePath] = struct{}{}
		if !isSysctlPath(filePath) {
			continue
		}
		if file.File.Content == nil {
			// Without inline content, the active file is current: a changed
			// one has already staged the request.
			delete(owned, filePath)
			continue
		}
		managed[filePath] = *file.File.Content
	}
	files, err := effectiveSysctlFiles(root, managed, owned)
	if err != nil {
		for _, key := range sortedKeys(keys) {
			effects = append(effects, plannedEffect("apply-and-verify", "sysctl "+key))
		}
		return nil, effects, []string{err.Error()}, nil
	}

	for _, key := range sortedKeys(keys) {
		winner, assigned := effectiveSysctl(files, key)
		if !assigned || winner.Excluded && shadowedSysctl(managed, key, winner) == "" {
			reasons = append(reasons, fmt.Sprintf("removing the last assignment of sysctl %s leaves it to the kernel default at next boot", key))
			continue
		}
		// An overridden managed value never reaches the kernel, so it is
		// neither applied nor reported as an effect.
		if shadowed := shadowedSysctl(managed, key, winner); shadowed != "" {
			override := fmt.Sprintf("sysctl %s in %s is overridden by %s", key, shadowed, winner.Path)
			reasons = append(reasons, override)
			notices = append(notices, override+"; the next boot keeps the overriding setting. Rename or remove one of the files.")
			continue
		}
		effects = append(effects, plannedEffect("apply-and-verify", "sysctl "+key))
		if restriction := sysctlRuntimeRestriction(root, key); restriction != "" {
			reasons = append(reasons, restriction)
			continue
		}
		if winner.Value == "" || strings.ContainsAny(winner.Value, "\x00\r\n\t ") {
			reasons = append(reasons, fmt.Sprintf("sysctl %s value from %s cannot be verified at runtime", key, winner.Path))
			continue
		}
		assignments = append(assignments, HostSysctlAssignment{Key: key, Value: winner.Value})
	}
	return assignments, effects, reasons, notices
}

// shadowedSysctl returns a managed file whose assignment of key differs from
// the effective entry, or is cancelled by an exclusion.
func shadowedSysctl(managed map[string]string, key string, winner sysctlAssignment) string {
	for _, filePath := range sortedKeys(managed) {
		for _, line := range sysctlAssignmentLines(managed[filePath]) {
			if line.key == key && !line.excluded && (winner.Excluded || line.value != winner.Value) {
				return filePath
			}
		}
	}
	return ""
}

// isSysctlPath reports whether systemd-sysctl reads a host file; it does not
// read subdirectories of sysctl.d.
func isSysctlPath(filePath string) bool {
	return path.Dir(filePath) == "/etc/sysctl.d" && strings.HasSuffix(filePath, ".conf")
}

// sysctlFileValues returns a managed file's concrete assignments; an absent
// file has none. It reports false when Katl cannot resolve every assignment
// to a concrete key and value.
func sysctlFileValues(file manifest.HostConfigurationFile, exists bool) (map[string]string, bool) {
	if !exists {
		return map[string]string{}, true
	}
	if file.Content == nil {
		return nil, false
	}
	return parseConcreteSysctl(*file.Content)
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
		// systemd-sysctl ignores a line without "=".
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, false
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
	return values, scanner.Err() == nil
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

type ownedHostFile struct {
	File   manifest.HostConfigurationFile
	Notify manifest.HostConfigurationNotifications
}

func hostFilesByPath(sets map[string]manifest.HostConfigurationSet) map[string]ownedHostFile {
	files := make(map[string]ownedHostFile)
	for _, set := range sets {
		if strings.TrimSpace(set.State) == manifest.HostConfigurationAbsent {
			continue
		}
		for _, file := range set.Files {
			files[file.Path] = ownedHostFile{File: file, Notify: set.Notify}
		}
	}
	return files
}

func hostFilePaths(files map[string]ownedHostFile) map[string]struct{} {
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
