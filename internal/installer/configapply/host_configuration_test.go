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

func TestPlanHostConfigurationStagesBootOwnedOverlays(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    string
		target  string
	}{
		{
			name:    "containerd overlay",
			path:    "/etc/containerd/conf.d/80-debug.toml",
			content: "[debug]\n  level = \"warn\"\n",
			want:    "load on next boot",
			target:  "containerd configuration /etc/containerd/conf.d/80-debug.toml",
		},
		{
			name:    "networkd drop-in",
			path:    "/etc/systemd/network/20-bond0.network.d/50-address.conf",
			content: "[Network]\nAddress=10.254.1.1/31\n",
			want:    "applies on next boot",
			target:  "networkd configuration /etc/systemd/network/20-bond0.network.d/50-address.conf",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := testHostConfiguration("example", tt.path, tt.content)
			plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
			if plan.Live || !strings.Contains(plan.Message, tt.want) {
				t.Fatalf("plan = %#v, want staged change", plan)
			}
			found := false
			for _, effect := range plan.Effects {
				found = found || effect.Target == tt.target
			}
			if !found {
				t.Fatalf("effects = %#v, want target %q", plan.Effects, tt.target)
			}
		})
	}
}

func TestPlanHostConfigurationStagesSysfsSettings(t *testing.T) {
	setting := manifest.HostConfigurationSysfsSetting{Name: "/sys/module/printk/parameters/time", Value: "N"}
	for _, tt := range []struct {
		name    string
		current []manifest.HostConfigurationSysfsSetting
		desired []manifest.HostConfigurationSysfsSetting
		action  string
	}{
		{name: "change", current: []manifest.HostConfigurationSysfsSetting{setting}, desired: []manifest.HostConfigurationSysfsSetting{{Name: setting.Name, Value: "Y"}}, action: "apply-and-verify"},
		{name: "remove", current: []manifest.HostConfigurationSysfsSetting{setting}, action: "stop-managing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := planHostConfigurationChange(
				manifest.HostConfiguration{Sysfs: tt.current},
				manifest.HostConfiguration{Sysfs: tt.desired},
			)
			if plan.Live || !strings.Contains(plan.Message, "sysfs configuration applies and verifies on next boot") {
				t.Fatalf("plan = %#v, want staged sysfs change", plan)
			}
			if len(plan.Effects) != 1 || plan.Effects[0].Action != tt.action || plan.Effects[0].Target != "sysfs "+setting.Name {
				t.Fatalf("effects = %#v", plan.Effects)
			}
		})
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

func TestPlanHostConfigurationExposesEveryBoundedEffect(t *testing.T) {
	sysctlBefore := "net.ipv4.ip_forward = 0\n"
	sysctlAfter := "net.ipv4.ip_forward = 1\n"
	rules := `SUBSYSTEM=="usb"` + "\n"
	journal := "[Journal]\nSystemMaxUse=2G\n"
	current := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"sysctl": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-forwarding.conf", Content: &sysctlBefore}}},
	}}
	desired := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"sysctl": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-forwarding.conf", Content: &sysctlAfter}}},
		"udev":   {Files: []manifest.HostConfigurationFile{{Path: "/etc/udev/rules.d/80-ups.rules", Content: &rules}}},
		"journal": {
			Files: []manifest.HostConfigurationFile{{Path: "/etc/systemd/journald.conf.d/80-home-lab.conf", Content: &journal}},
			Notify: manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{{
				Unit: "systemd-journald.service", Action: "try-reload-or-restart",
			}}},
		},
	}}
	plan := planHostConfigurationChange(current, desired)
	if !plan.Live {
		t.Fatalf("plan = %#v", plan)
	}
	got := map[string]bool{}
	for _, effect := range plan.Effects {
		got[effect.Action+" "+effect.Target] = true
	}
	for _, want := range []string{
		"apply-and-verify sysctl net.ipv4.ip_forward",
		"verify udev rules /etc/udev/rules.d/80-ups.rules",
		"reload udev rules",
		"try-reload-or-restart systemd unit systemd-journald.service",
	} {
		if !got[want] || !strings.Contains(plan.Message, want) {
			t.Fatalf("effects/message = %#v / %q, missing %q", plan.Effects, plan.Message, want)
		}
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
