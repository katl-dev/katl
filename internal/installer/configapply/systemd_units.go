package configapply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/systemdunit"
)

// PrepareSystemdActivation runs after confext and sysext merging. Runtime
// enablement honours native [Install] metadata without writing immutable /etc.
// The target lets systemd order all starts in one transaction, including
// optional extension units and oneshots which do not remain active.
func PrepareSystemdActivation(ctx context.Context, root string, node manifest.NodeConfig, runner CommandRunner) (HostConfigurationActivationPlan, error) {
	activation := node.UnitActivation()
	var plan HostConfigurationActivationPlan
	// Early services may have started before confext merging. Reapply their
	// configuration only after all extensions are visible. Units not yet active
	// consume it on their first start; never wait here for later boot targets.
	host := HostConfigurationChangePlan{unitNotifications: map[string]string{}}
	e := Executor{Runner: runner, HostConfiguration: &host}
	config := effectiveHostConfiguration(node)
	for _, name := range sortedHostConfigurationSetNames(config.Sets) {
		set := config.Sets[name]
		if set.State == manifest.HostConfigurationAbsent {
			continue
		}
		for _, notification := range set.Notify.Systemd {
			if slices.Contains(config.MaskedUnits, notification.Unit) {
				continue
			}
			state, err := e.systemdProperty(ctx, notification.Unit, "ActiveState")
			if err != nil {
				return plan, err
			}
			if state == "active" {
				host.unitNotifications[notification.Unit] = notification.Action
			}
		}
	}
	if err := e.prepareHostSystemd(ctx); err != nil {
		return plan, err
	}
	for _, command := range host.Commands {
		plan.addCommandWithOutput(command.Name, "reconfigure", "running systemd units", command.ExpectedStdout, command.Argv...)
		plan.Commands[len(plan.Commands)-1].Timeout = 2 * time.Minute
	}
	if len(activation.Enabled) == 0 && len(activation.Required) == 0 {
		return plan, nil
	}
	// Older installed generations do not carry the target in their confext.
	// Derive an ephemeral fallback without rewriting those rollback artifacts.
	if _, err := os.Stat(filepath.Join(root, "etc/systemd/system", systemdunit.TargetName)); os.IsNotExist(err) {
		dir := filepath.Join(root, "run/systemd/system")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return plan, fmt.Errorf("prepare configured unit target: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, systemdunit.TargetName), []byte(activation.Target()), 0o644); err != nil {
			return plan, fmt.Errorf("write configured unit target: %w", err)
		}
	} else if err != nil {
		return plan, fmt.Errorf("inspect configured unit target: %w", err)
	}
	plan.addUnitEnablement(activation.Enabled)
	plan.addCommand("systemd-daemon-reload", "reload", "systemd manager", "systemctl", "daemon-reload")
	plan.addCommand("systemd-units-start", "start", "configured systemd units", "systemctl", "start", systemdunit.TargetName)
	for i := range plan.Commands {
		plan.Commands[i].Timeout = 2 * time.Minute
	}
	return plan, nil
}

func (p *HostConfigurationActivationPlan) addUnitEnablement(units []string) {
	if len(units) == 0 {
		return
	}
	p.addCommand("systemd-units-enable", "enable", "systemd units "+strings.Join(units, " "),
		append([]string{"systemctl", "--runtime", "--no-reload", "reenable"}, units...)...)
}

func (e Executor) prepareHostSystemd(ctx context.Context) error {
	host := e.HostConfiguration
	units := slices.Concat(host.unitsToStop, host.unitsToEnable, host.unitsToDisable)
	for unit := range host.unitNotifications {
		units = append(units, unit)
	}
	slices.Sort(units)
	units = slices.Compact(units)
	var restart, reload, restoreStart, restoreReload, restoreStop, restoreEnable, clearStoppedFailures []string
	for _, unit := range units {
		state, err := e.systemdProperty(ctx, unit, "ActiveState")
		if err != nil {
			return err
		}
		if state != "active" && state != "inactive" && state != "failed" {
			return fmt.Errorf("systemd unit %s is transitioning (%s); retry once it settles", unit, state)
		}
		action := host.unitNotifications[unit]
		if action == "reload" && state != "active" && !slices.Contains(host.unitsToEnable, unit) {
			return fmt.Errorf("systemd unit %s is inactive; enable it or use try-reload-or-restart for an optional notification", unit)
		}
		if state == "failed" && (slices.Contains(host.unitsToStop, unit) || slices.Contains(host.unitsToDisable, unit)) {
			clearStoppedFailures = append(clearStoppedFailures, unit)
		}
		forceStart := action == "restart" || action == "reload-or-restart"
		if (action == "try-reload-or-restart" || action == "reload-or-restart") && state == "active" {
			canReload, err := e.systemdProperty(ctx, unit, "CanReload")
			if err != nil {
				return err
			}
			if canReload == "yes" {
				action = "reload"
			} else {
				action = "try-restart"
			}
		}
		if state == "active" {
			if action == "reload" {
				restoreReload = append(restoreReload, unit)
			} else {
				restoreStart = append(restoreStart, unit)
			}
		} else {
			restoreStop = append(restoreStop, unit)
			if state == "inactive" {
				host.resetFailures = append(host.resetFailures, unit)
			}
			if action != "" && !forceStart && !slices.Contains(host.unitsToEnable, unit) {
				host.inactiveNotifications = append(host.inactiveNotifications, unit)
			}
		}
		if slices.Contains(host.unitsToEnable, unit) || action == "restart" || (state != "active" && forceStart) || (state == "active" && action == "try-restart") {
			restart = append(restart, unit)
		} else if state == "active" && action == "reload" {
			reload = append(reload, unit)
		}
		if slices.Contains(host.unitsToEnable, unit) || slices.Contains(host.unitsToDisable, unit) {
			enablement, err := e.systemdProperty(ctx, unit, "UnitFileState")
			if err != nil {
				return err
			}
			if enablement == "enabled-runtime" {
				restoreEnable = append(restoreEnable, unit)
			}
		}
	}

	// Stop removed units before their old ExecStop files disappear. Rollback
	// restores the observed runtime state, not an assumption that all units ran.
	stop := slices.Concat(host.unitsToStop, host.unitsToDisable)
	slices.Sort(stop)
	stop = slices.Compact(stop)
	host.beforeCommands = withDefaults(unitCommands("stop", stop), e.timeout())
	host.beforeCommands = append(host.beforeCommands, withDefaults(runtimeDisableCommands(host.unitsToDisable), e.timeout())...)
	for _, unit := range clearStoppedFailures {
		host.beforeCommands = append(host.beforeCommands, Command{
			Name: "systemd-clear-removed-failure-" + unit,
			Argv: []string{"systemctl", "reset-failed", unit}, Timeout: e.timeout(),
		})
	}
	var activation HostConfigurationActivationPlan
	activation.addUnitEnablement(host.unitsToEnable)
	for _, unit := range host.unitsToEnable {
		host.Commands = append(host.Commands, Command{Name: "systemd-unit-load-" + unit, Argv: []string{"systemctl", "show", "--property=LoadState", "--value", unit}, ExpectedStdout: "loaded"})
	}
	host.Commands = append(host.Commands, activation.Commands...)
	if len(host.unitsToEnable)+len(host.unitsToDisable) > 0 {
		host.Commands = append(host.Commands, Command{Name: "systemd-enable-reload", Argv: []string{"systemctl", "daemon-reload"}})
	}
	host.Commands = append(host.Commands, unitCommands("restart", restart)...)
	host.Commands = append(host.Commands, unitCommands("reload", reload)...)

	var rollback HostConfigurationActivationPlan
	rollback.addUnitEnablement(restoreEnable)
	host.rollbackCommands = append(host.rollbackCommands, rollback.Commands...)
	if len(host.unitsToEnable)+len(host.unitsToDisable) > 0 {
		host.rollbackCommands = append(host.rollbackCommands, Command{Name: "systemd-enable-restore-reload", Argv: []string{"systemctl", "daemon-reload"}})
	}
	stops := unitCommands("stop", restoreStop)
	for i := range stops {
		stops[i].SuccessExitStatuses = []int{0, 5}
	}
	host.beforeRollbackCommands = withDefaults(stops, e.timeout())
	host.rollbackCommands = append(host.rollbackCommands, unitCommands("restart", restoreStart)...)
	host.rollbackCommands = append(host.rollbackCommands, unitCommands("reload", restoreReload)...)
	return nil
}

func (e Executor) systemdProperty(ctx context.Context, unit, property string) (string, error) {
	name := "systemd-state-" + unit
	if property != "ActiveState" {
		name += "-" + property
	}
	command := Command{Name: name, Argv: []string{"systemctl", "show", "--property=" + property, "--value", unit}, Timeout: e.timeout()}
	result, err := e.Runner.Run(ctx, command)
	if err == nil && !commandSucceeded(command, result) {
		err = commandFailure(command, result)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func unitCommands(action string, units []string) []Command {
	if len(units) == 0 {
		return nil
	}
	if len(units) > 1 && (action == "restart" || action == "stop") {
		// systemd 259 submits multi-argument systemctl jobs separately. Each
		// transient barrier instead builds one dependency transaction, and is
		// collected after completion without leaving propagation dependencies.
		barrier := func(name, dependency string) Command {
			// systemd-run may exit successfully on a dependency failure without
			// executing the service. Require evidence that the barrier itself ran.
			return Command{Name: name, ExpectedStdout: "katl-systemd-transaction-complete", Argv: []string{
				"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
				"--property=Type=oneshot", "--property=DefaultDependencies=no",
				"--property=After=" + strings.Join(units, " "),
				"--property=" + dependency + "=" + strings.Join(units, " "), "/usr/bin/echo", "katl-systemd-transaction-complete",
			}}
		}
		if action == "stop" {
			return []Command{barrier("systemd-units-stop", "Conflicts")}
		}
		return []Command{barrier("systemd-units-stop-for-restart", "Conflicts"), barrier("systemd-units-restart", "Requires")}
	}
	return []Command{{Name: "systemd-units-" + action, Argv: append([]string{"systemctl", action}, units...)}}
}

func runtimeDisableCommands(units []string) []Command {
	if len(units) == 0 {
		return nil
	}
	return []Command{{Name: "systemd-units-disable", Argv: append([]string{"systemctl", "--runtime", "--no-reload", "disable"}, units...)}}
}

func changedUnit(filePath string) string {
	const prefix = "/etc/systemd/system/"
	if !strings.HasPrefix(filePath, prefix) {
		return ""
	}
	rel := strings.TrimPrefix(filePath, prefix)
	unit, rest, nested := strings.Cut(rel, "/")
	if nested {
		if !strings.HasSuffix(unit, ".d") || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".conf") {
			return ""
		}
		unit = strings.TrimSuffix(unit, ".d")
	}
	// Type-wide drop-ins affect an open-ended set of units and need an explicit
	// notification; a concrete unit or instance has a bounded live action.
	if !strings.Contains(unit, ".") || strings.Contains(unit, "@.") {
		return ""
	}
	return unit
}

func unitDifference(left, right []string) []string {
	var units []string
	for _, unit := range left {
		if !slices.Contains(right, unit) {
			units = append(units, unit)
		}
	}
	slices.Sort(units)
	return units
}

func effectiveHostConfiguration(node manifest.NodeConfig) manifest.HostConfiguration {
	config := manifest.NormalizeHostConfiguration(node.HostConfiguration)
	if config.Sets == nil {
		config.Sets = make(map[string]manifest.HostConfigurationSet)
	}
	activation := node.UnitActivation()
	content := activation.Target()
	config.Sets["systemd activation"] = manifest.HostConfigurationSet{Files: []manifest.HostConfigurationFile{{
		Path: "/etc/systemd/system/" + systemdunit.TargetName, Content: &content,
	}}}
	config.EnabledUnits = slices.Concat(activation.Enabled, activation.Required)
	slices.Sort(config.EnabledUnits)
	config.EnabledUnits = slices.Compact(config.EnabledUnits)
	for _, extension := range node.SystemExtensions {
		if extension.State == manifest.SystemExtensionAbsent {
			continue
		}
		set := manifest.HostConfigurationSet{Files: slices.Clone(extension.Configuration.Files)}
		for _, unit := range extension.Units {
			for _, dropIn := range unit.DropIns {
				set.Files = append(set.Files, manifest.HostConfigurationFile{
					Path:    "/etc/systemd/system/" + unit.Name + ".d/" + dropIn.Name,
					Content: dropIn.Content, Source: dropIn.Source,
				})
			}
			if (unit.Enable || unit.RequiredForBootHealth) && !slices.Contains(config.MaskedUnits, unit.Name) {
				set.Notify.Systemd = append(set.Notify.Systemd, manifest.HostConfigurationSystemdNotification{Unit: unit.Name, Action: "try-reload-or-restart"})
			}
		}
		config.Sets["systemExtensions["+extension.Name+"]"] = set
	}
	return config
}

func extensionPayloadsEqual(before, after []manifest.SystemExtension) bool {
	stripConfiguration := func(extensions []manifest.SystemExtension) []manifest.SystemExtension {
		out := slices.Clone(extensions)
		for i := range out {
			out[i].Configuration = manifest.SystemExtensionConfiguration{}
			out[i].Units = nil
		}
		return out
	}
	return reflect.DeepEqual(stripConfiguration(before), stripConfiguration(after))
}
