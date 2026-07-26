package configapply

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type HostConfigurationActivationPlan struct {
	Commands []Command
	Effects  []generation.ConfigApplyEffect
}

const (
	HostConfigurationPhasePrepare   = "prepare"
	HostConfigurationPhaseVerify    = "verify"
	HostConfigurationPhaseReconcile = "reconcile"
)

func PlanHostConfigurationActivation(config manifest.HostConfiguration, phase string) HostConfigurationActivationPlan {
	var plan HostConfigurationActivationPlan
	notifications := map[string]string{}
	sysctls := map[string]string{}
	modules := map[string]struct{}{}
	sysfs := map[string]string{}
	udevPaths := map[string]struct{}{}
	systemdReload := false

	for _, setting := range config.Sysfs {
		sysfs[setting.Name] = setting.Value
	}
	for _, name := range sortedHostConfigurationSetNames(config.Sets) {
		set := config.Sets[name]
		if strings.TrimSpace(set.State) == manifest.HostConfigurationAbsent {
			continue
		}
		for _, file := range set.Files {
			switch {
			case strings.HasPrefix(file.Path, "/etc/sysctl.d/") && strings.HasSuffix(file.Path, ".conf") && file.Content != nil:
				if values, ok := parseConcreteSysctl(*file.Content); ok {
					for key, value := range values {
						sysctls[key] = value
					}
				}
			case strings.HasPrefix(file.Path, "/etc/modules-load.d/") && file.Content != nil:
				for _, module := range parseModulesLoad(*file.Content) {
					modules[module] = struct{}{}
				}
			case strings.HasPrefix(file.Path, "/etc/udev/rules.d/") && strings.HasSuffix(file.Path, ".rules"):
				udevPaths[file.Path] = struct{}{}
			}
			if strings.HasPrefix(file.Path, "/etc/systemd/system/") {
				systemdReload = true
			}
		}
		for _, notification := range set.Notify.Systemd {
			notifications[notification.Unit] = notification.Action
		}
	}

	prepare := phase == HostConfigurationPhasePrepare || phase == HostConfigurationPhaseReconcile
	verify := phase == HostConfigurationPhaseVerify || phase == HostConfigurationPhaseReconcile
	if prepare && systemdReload {
		plan.addCommand("systemd-daemon-reload", "reload", "systemd manager", "systemctl", "daemon-reload")
	}
	if prepare && len(modules) > 0 {
		plan.addCommand("systemd-modules-load", "restart", "systemd-modules-load.service", "systemctl", "restart", "systemd-modules-load.service")
	}
	if verify {
		for _, module := range sortedKeys(modules) {
			plan.addCommand(
				"module-verify-"+module,
				"load-and-verify",
				"module "+module,
				"/usr/bin/test",
				"-d",
				filepath.Join("/sys/module", strings.ReplaceAll(module, "-", "_")),
			)
		}
	}
	if prepare && len(sysctls) > 0 {
		plan.addCommand("systemd-sysctl", "restart", "systemd-sysctl.service", "systemctl", "restart", "systemd-sysctl.service")
	}
	if verify {
		for _, key := range sortedKeys(sysctls) {
			plan.addCommandWithOutput("sysctl-verify-"+key, "apply-and-verify", "sysctl "+key, sysctls[key], "/usr/sbin/sysctl", "-n", key)
		}
	}
	if prepare && len(sysfs) > 0 {
		plan.addCommand(
			"sysfs-apply",
			"apply",
			"sysfs configuration",
			"systemd-tmpfiles",
			"--create",
			manifest.HostConfigurationSysfsTmpfilesPath,
		)
	}
	if verify {
		for _, target := range sortedKeys(sysfs) {
			plan.addCommandWithOutput(
				"sysfs-verify-"+strings.Trim(strings.ReplaceAll(target, "/", "-"), "-"),
				"apply-and-verify",
				"sysfs "+target,
				sysfs[target],
				"/usr/bin/cat",
				target,
			)
		}
	}
	if prepare {
		for _, filePath := range sortedKeys(udevPaths) {
			plan.addCommand("udev-rules-verify", "verify", "udev rules "+filePath, "/usr/bin/udevadm", "verify", filePath)
		}
		if len(udevPaths) > 0 {
			plan.addCommand("udev-rules-reload", "reload", "udev rules", "/usr/bin/udevadm", "control", "--reload")
			plan.addCommand("udev-devices-trigger", "trigger", "udev devices", "/usr/bin/udevadm", "trigger", "--type=devices", "--action=add")
			plan.addCommand("udev-events-settle", "settle", "udev events", "/usr/bin/udevadm", "settle")
		}
		for _, unit := range sortedKeys(notifications) {
			action := notifications[unit]
			plan.addCommand("systemd-notify-"+unit, action, "systemd unit "+unit, "systemctl", action, unit)
		}
	}
	return plan
}

func ExecuteHostConfigurationActivation(ctx context.Context, plan HostConfigurationActivationPlan, runner CommandRunner, observe func(generation.ConfigApplyEffect)) error {
	for index, command := range plan.Commands {
		result, err := runner.Run(ctx, command)
		if err == nil && !commandSucceeded(command, result) {
			err = commandFailure(command, result)
		}
		if err == nil && command.ExpectedStdout != "" && strings.TrimSpace(result.Stdout) != command.ExpectedStdout {
			err = fmt.Errorf("%s returned %q, want %q", command.Name, strings.TrimSpace(result.Stdout), command.ExpectedStdout)
		}
		effect := plan.Effects[index]
		effect.Status = generation.ConfigApplyActionPassed
		if err != nil {
			effect.Status = generation.ConfigApplyActionFailed
			effect.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
		}
		if observe != nil {
			observe(effect)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", command.Name, err)
		}
	}
	return nil
}

func InspectHostConfiguration(ctx context.Context, config manifest.HostConfiguration, runner CommandRunner) []generation.ConfigApplyEffect {
	plan := PlanHostConfigurationActivation(config, HostConfigurationPhaseVerify)
	var drift []generation.ConfigApplyEffect
	for index, command := range plan.Commands {
		result, err := runner.Run(ctx, command)
		if err == nil && !commandSucceeded(command, result) {
			err = commandFailure(command, result)
		}
		if err == nil && command.ExpectedStdout != "" && strings.TrimSpace(result.Stdout) != command.ExpectedStdout {
			err = fmt.Errorf("%s returned %q, want %q", command.Name, strings.TrimSpace(result.Stdout), command.ExpectedStdout)
		}
		if err == nil {
			continue
		}
		effect := plan.Effects[index]
		effect.Status = generation.ConfigApplyActionFailed
		effect.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
		drift = append(drift, effect)
	}
	return drift
}

func HostConfigurationDriftIsLive(drift []generation.ConfigApplyEffect) bool {
	if len(drift) == 0 {
		return false
	}
	for _, effect := range drift {
		if !strings.HasPrefix(effect.Target, "sysctl ") {
			return false
		}
	}
	return true
}

func PlanHostSysctlReconciliation(config manifest.HostConfiguration) HostConfigurationActivationPlan {
	verify := PlanHostConfigurationActivation(config, HostConfigurationPhaseVerify)
	plan := HostConfigurationActivationPlan{}
	var sysctlCommands []Command
	var sysctlEffects []generation.ConfigApplyEffect
	for index, effect := range verify.Effects {
		if strings.HasPrefix(effect.Target, "sysctl ") {
			sysctlCommands = append(sysctlCommands, verify.Commands[index])
			sysctlEffects = append(sysctlEffects, effect)
		}
	}
	if len(sysctlCommands) == 0 {
		return plan
	}
	plan.addCommand("systemd-sysctl", "restart", "systemd-sysctl.service", "systemctl", "restart", "systemd-sysctl.service")
	plan.Commands = append(plan.Commands, sysctlCommands...)
	plan.Effects = append(plan.Effects, sysctlEffects...)
	return plan
}

func (p *HostConfigurationActivationPlan) addCommand(name, action, target string, argv ...string) {
	p.addCommandWithOutput(name, action, target, "", argv...)
}

func (p *HostConfigurationActivationPlan) addCommandWithOutput(name, action, target, expectedStdout string, argv ...string) {
	p.Commands = append(p.Commands, Command{
		Name:           name,
		EffectAction:   action,
		EffectTarget:   target,
		Argv:           argv,
		ExpectedStdout: expectedStdout,
	})
	p.Effects = append(p.Effects, plannedEffect(action, target))
}

func sortedHostConfigurationSetNames(sets map[string]manifest.HostConfigurationSet) []string {
	names := make([]string, 0, len(sets))
	for name := range sets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
