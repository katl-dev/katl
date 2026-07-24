package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestPlanHostConfigurationSysctlRequiresReversibleConcreteDiff(t *testing.T) {
	current := testHostConfiguration("forwarding", "/etc/sysctl.d/80-forwarding.conf", "net.ipv4.ip_forward = 0\n")
	desired := testHostConfiguration("forwarding", "/etc/sysctl.d/80-forwarding.conf", "net.ipv4.ip_forward = 1\n")
	plan := planHostConfigurationChange(current, desired)
	if !plan.Live || len(plan.SysctlAssignments) != 1 {
		t.Fatalf("plan = %#v, want one live sysctl assignment", plan)
	}
	if plan.SysctlAssignments[0] != (HostSysctlAssignment{Key: "net.ipv4.ip_forward", Value: "1"}) {
		t.Fatalf("assignments = %#v", plan.SysctlAssignments)
	}

	addition := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
	if addition.Live || !strings.Contains(addition.Message, "additions") {
		t.Fatalf("addition plan = %#v, want next boot", addition)
	}

	ambiguous := testHostConfiguration("forwarding", "/etc/sysctl.d/80-forwarding.conf", "net.ipv4.conf.*.forwarding = 1\n")
	ambiguousPlan := planHostConfigurationChange(current, ambiguous)
	if ambiguousPlan.Live {
		t.Fatalf("ambiguous plan = %#v, want next boot", ambiguousPlan)
	}
}

func TestPlanHostConfigurationReloadsUdevWithoutTrigger(t *testing.T) {
	desired := testHostConfiguration("ups-device", "/etc/udev/rules.d/80-ups.rules", `SUBSYSTEM=="usb"`+"\n")
	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
	if !plan.Live || !strings.Contains(plan.Message, "will not be retriggered") {
		t.Fatalf("plan = %#v", plan)
	}
	if len(plan.Commands) != 2 || plan.Commands[0].Name != "udev-rules-verify" || plan.Commands[1].Name != "udev-rules-reload" {
		t.Fatalf("commands = %#v", plan.Commands)
	}
}

func TestPlanHostConfigurationStagesKernelModuleFiles(t *testing.T) {
	desired := testHostConfiguration("storage-modules", "/etc/modules-load.d/80-storage.conf", "br_netfilter\n")
	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
	if plan.Live || !strings.Contains(plan.Message, "next-boot-only") {
		t.Fatalf("plan = %#v, want staged module configuration", plan)
	}
}

func TestPlanHostConfigurationUsesBoundedSystemdNotification(t *testing.T) {
	content := "[Journal]\nSystemMaxUse=2G\n"
	desired := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"journal-limits": {
			Files: []manifest.HostConfigurationFile{{Path: "/etc/systemd/journald.conf.d/80-home-lab.conf", Content: &content}},
			Notify: manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{{
				Unit:   "systemd-journald.service",
				Action: "try-reload-or-restart",
			}}},
		},
	}}
	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
	if !plan.Live {
		t.Fatalf("plan = %#v", plan)
	}
	if len(plan.Commands) != 1 {
		t.Fatalf("commands = %#v", plan.Commands)
	}
	if got := strings.Join(plan.Commands[0].Argv, " "); got != "systemctl try-reload-or-restart systemd-journald.service" {
		t.Fatalf("notification argv = %q", got)
	}
}

func testHostConfiguration(setName, filePath, content string) manifest.HostConfiguration {
	return manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		setName: {
			State: manifest.HostConfigurationPresent,
			Files: []manifest.HostConfigurationFile{{
				Path:    filePath,
				Content: &content,
				Mode:    0o644,
			}},
		},
	}}
}
