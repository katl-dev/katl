package configapply

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestPlanHostConfigurationActivationOrdersPrepareAndVerifyEffects(t *testing.T) {
	sysctl := "net.ipv4.ip_forward = 1\n"
	modules := "dummy\n"
	rules := `SUBSYSTEM=="usb"` + "\n"
	journal := "[Journal]\nSystemMaxUse=2G\n"
	config := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"sysctl":  {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-forwarding.conf", Content: &sysctl}}},
		"modules": {Files: []manifest.HostConfigurationFile{{Path: "/etc/modules-load.d/80-lab.conf", Content: &modules}}},
		"udev":    {Files: []manifest.HostConfigurationFile{{Path: "/etc/udev/rules.d/80-ups.rules", Content: &rules}}},
		"journal": {
			Files:  []manifest.HostConfigurationFile{{Path: "/etc/systemd/journald.conf.d/80-home-lab.conf", Content: &journal}},
			Notify: manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{{Unit: "systemd-journald.service", Action: "try-reload-or-restart"}}},
		},
	}}
	prepare := PlanHostConfigurationActivation(config, HostConfigurationPhasePrepare)
	if got := commandNames(prepare.Commands); strings.Join(got, ",") != "systemd-modules-load,systemd-sysctl,udev-rules-verify,udev-rules-reload,udev-devices-trigger,udev-events-settle,systemd-notify-systemd-journald.service" {
		t.Fatalf("prepare commands = %v", got)
	}
	verify := PlanHostConfigurationActivation(config, HostConfigurationPhaseVerify)
	if got := commandNames(verify.Commands); strings.Join(got, ",") != "module-verify-dummy,sysctl-verify-net.ipv4.ip_forward" {
		t.Fatalf("verify commands = %v", got)
	}
	if verify.Commands[1].ExpectedStdout != "1" {
		t.Fatalf("sysctl expected stdout = %q", verify.Commands[1].ExpectedStdout)
	}
}

func TestInspectHostConfigurationClassifiesLiveSysctlDrift(t *testing.T) {
	sysctl := "net.ipv4.ip_forward = 1\n"
	config := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"sysctl": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-forwarding.conf", Content: &sysctl}}},
	}}
	runner := &fakeCommandRunner{results: map[string]CommandResult{
		"sysctl-verify-net.ipv4.ip_forward": {Stdout: "0\n"},
	}}
	drift := InspectHostConfiguration(context.Background(), config, runner)
	if len(drift) != 1 || drift[0].Target != "sysctl net.ipv4.ip_forward" || !HostConfigurationDriftIsLive(drift) {
		t.Fatalf("drift = %#v", drift)
	}
	reconcile := PlanHostSysctlReconciliation(config)
	if got := commandNames(reconcile.Commands); strings.Join(got, ",") != "systemd-sysctl,sysctl-verify-net.ipv4.ip_forward" {
		t.Fatalf("reconcile commands = %v", got)
	}
}

func TestPlanHostConfigurationActivationAppliesAndVerifiesSysfsWrites(t *testing.T) {
	config := manifest.HostConfiguration{Sysfs: []manifest.HostConfigurationSysfsSetting{
		{Name: "/sys/module/printk/parameters/time", Value: "N"},
	}}
	prepare := PlanHostConfigurationActivation(config, HostConfigurationPhasePrepare)
	if len(prepare.Commands) != 1 || len(prepare.Effects) != 1 ||
		prepare.Effects[0].Action != "apply" || prepare.Effects[0].Target != "sysfs configuration" {
		t.Fatalf("prepare plan = %#v", prepare)
	}
	verify := PlanHostConfigurationActivation(config, HostConfigurationPhaseVerify)
	if len(verify.Commands) != 1 || len(verify.Effects) != 1 ||
		verify.Commands[0].ExpectedStdout != "N" ||
		verify.Effects[0].Action != "apply-and-verify" ||
		verify.Effects[0].Target != "sysfs /sys/module/printk/parameters/time" {
		t.Fatalf("verify plan = %#v", verify)
	}
	if HostConfigurationDriftIsLive(verify.Effects) {
		t.Fatalf("sysfs drift must require next-boot reconciliation: %#v", verify.Effects)
	}
}

func TestExecuteHostConfigurationActivationReportsEachOutcome(t *testing.T) {
	plan := HostConfigurationActivationPlan{}
	plan.addCommand("first", "reload", "udev rules", "/usr/bin/true")
	plan.addCommand("second", "apply-and-verify", "sysctl example.value", "/usr/bin/false")
	runner := &fakeCommandRunner{results: map[string]CommandResult{"second": {ExitStatus: 1, Stderr: "rejected"}}}
	var observed []generation.ConfigApplyEffect
	err := ExecuteHostConfigurationActivation(context.Background(), plan, runner, func(effect generation.ConfigApplyEffect) {
		observed = append(observed, effect)
	})
	if err == nil || len(observed) != 2 ||
		observed[0].Status != generation.ConfigApplyActionPassed ||
		observed[1].Status != generation.ConfigApplyActionFailed {
		t.Fatalf("err/effects = %v / %#v", err, observed)
	}
}

func commandNames(commands []Command) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}
	return names
}
