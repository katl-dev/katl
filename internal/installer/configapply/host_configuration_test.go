package configapply

import (
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

// A sysctl change applies live only when every changed key's effective value
// at next boot is known, comes from the file that set it, and the running
// kernel can snapshot, write, verify, and restore it.
func TestPlanHostConfigurationSysctlRequiresReversibleEffectiveAssignment(t *testing.T) {
	const (
		forwarding = "/etc/sysctl.d/80-forwarding.conf"
		demo       = "/etc/sysctl.d/95-demo.conf"
	)
	tests := []struct {
		name    string
		current map[string]string
		desired map[string]string
		want    []HostSysctlAssignment
		staged  string
	}{
		{
			name:    "value change",
			current: map[string]string{forwarding: "net.ipv4.ip_forward = 0\n"},
			desired: map[string]string{forwarding: "net.ipv4.ip_forward = 1\n"},
			want:    []HostSysctlAssignment{{Key: "net.ipv4.ip_forward", Value: "1"}},
		},
		{
			name:    "first assignment of an existing key",
			desired: map[string]string{demo: "vm.swappiness = 20\n"},
			want:    []HostSysctlAssignment{{Key: "vm.swappiness", Value: "20"}},
		},
		{
			name:    "key added to a managed file",
			current: map[string]string{forwarding: "net.ipv4.ip_forward = 1\n"},
			desired: map[string]string{forwarding: "net.ipv4.ip_forward = 1\nvm.swappiness = 20\n"},
			want:    []HostSysctlAssignment{{Key: "vm.swappiness", Value: "20"}},
		},
		{
			name:    "override of an earlier vendor file",
			desired: map[string]string{"/etc/sysctl.d/80-sysrq.conf": "kernel.sysrq = 1\n"},
			want:    []HostSysctlAssignment{{Key: "kernel.sysrq", Value: "1"}},
		},
		{
			name:    "replacement of a vendor file by name",
			desired: map[string]string{"/etc/sysctl.d/99-vendor.conf": "net.core.somaxconn = 1024\n"},
			want:    []HostSysctlAssignment{{Key: "net.core.somaxconn", Value: "1024"}},
		},
		{
			name:    "removal exposing a vendor value",
			current: map[string]string{forwarding: "net.ipv4.ip_forward = 0\n"},
			want:    []HostSysctlAssignment{{Key: "net.ipv4.ip_forward", Value: "1"}},
		},
		{
			name:    "addition shadowed by a later vendor file",
			desired: map[string]string{"/etc/sysctl.d/50-somaxconn.conf": "net.core.somaxconn = 1024\n"},
			staged:  "sysctl net.core.somaxconn in /etc/sysctl.d/50-somaxconn.conf is overridden by /usr/lib/sysctl.d/99-vendor.conf",
		},
		{
			name:    "conflicting managed duplicate",
			current: map[string]string{"/etc/sysctl.d/90-memory.conf": "vm.swappiness = 10\n"},
			desired: map[string]string{"/etc/sysctl.d/90-memory.conf": "vm.swappiness = 10\n", demo: "vm.swappiness = 20\n"},
			staged:  "sysctl vm.swappiness in /etc/sysctl.d/90-memory.conf is overridden by " + demo,
		},
		{
			name:    "assignment cancelled by a later exclusion",
			desired: map[string]string{"/etc/sysctl.d/10-rpf.conf": "net.ipv4.conf.all.rp_filter = 1\n"},
			staged:  "sysctl net.ipv4.conf.all.rp_filter in /etc/sysctl.d/10-rpf.conf is overridden by /usr/lib/sysctl.d/50-default.conf",
		},
		{
			name:    "assignment after an exclusion",
			desired: map[string]string{"/etc/sysctl.d/80-rpf.conf": "net.ipv4.conf.all.rp_filter = 1\n"},
			want:    []HostSysctlAssignment{{Key: "net.ipv4.conf.all.rp_filter", Value: "1"}},
		},
		{
			name:    "line without an assignment",
			desired: map[string]string{demo: "vm.swappiness 20\n"},
			staged:  "assignments Katl cannot parse",
		},
		{
			name:    "IPv6 disable",
			desired: map[string]string{demo: "net.ipv6.conf.all.disable_ipv6 = 1\n"},
			staged:  "removes IPv6 addresses",
		},
		{
			name:    "removal of the last assignment",
			current: map[string]string{demo: "vm.swappiness = 20\n"},
			staged:  "removing the last assignment of sysctl vm.swappiness",
		},
		{
			name:    "nonexistent key",
			desired: map[string]string{demo: "net.ipv4.conf.eth9.forwarding = 1\n"},
			staged:  "sysctl net.ipv4.conf.eth9.forwarding does not exist in the running kernel",
		},
		{
			name:    "glob",
			desired: map[string]string{demo: "net.ipv4.conf.*.forwarding = 1\n"},
			staged:  "sysctl globs",
		},
		{
			name:    "irreversible key",
			desired: map[string]string{demo: "kernel.modules_disabled = 1\n"},
			staged:  "sysctl kernel.modules_disabled cannot be restored",
		},
		{
			name:    "write-only key",
			desired: map[string]string{demo: "vm.drop_caches = 3\n"},
			staged:  "sysctl vm.drop_caches cannot be both read and written",
		},
		{
			name:    "one ineligible key stages the whole file",
			desired: map[string]string{demo: "vm.swappiness = 20\nnet.ipv4.conf.eth9.forwarding = 1\n"},
			staged:  "does not exist in the running kernel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := sysctlHostRoot(t)

			plan := planHostConfigurationChange(sysctlConfiguration(tt.current), sysctlConfiguration(tt.desired), root)

			if tt.staged == "" {
				if !plan.Live || !slices.Equal(plan.SysctlAssignments, tt.want) {
					t.Fatalf("plan live=%t assignments=%#v message=%q, want live %#v", plan.Live, plan.SysctlAssignments, plan.Message, tt.want)
				}
				return
			}
			if plan.Live || !strings.Contains(plan.Message, tt.staged) {
				t.Fatalf("plan live=%t message=%q, want next boot because %q", plan.Live, plan.Message, tt.staged)
			}
		})
	}
}

func TestPlanHostConfigurationOmitsShadowedSysctlEffect(t *testing.T) {
	desired := sysctlConfiguration(map[string]string{"/etc/sysctl.d/50-somaxconn.conf": "net.core.somaxconn = 1024\n"})

	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired, sysctlHostRoot(t))

	for _, effect := range plan.Effects {
		if effect.Target == "sysctl net.core.somaxconn" {
			t.Fatalf("effects = %#v, want no effect for an overridden assignment", plan.Effects)
		}
	}
	if len(plan.nextBootNotices) != 1 || !strings.Contains(plan.nextBootNotices[0], "/usr/lib/sysctl.d/99-vendor.conf") {
		t.Fatalf("next-boot notices = %q, want the overriding file named", plan.nextBootNotices)
	}
}

func TestPlanHostConfigurationReloadsUdevWithoutTrigger(t *testing.T) {
	desired := testHostConfiguration("ups-device", "/etc/udev/rules.d/80-ups.rules", `SUBSYSTEM=="usb"`+"\n")
	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired, t.TempDir())
	if !plan.Live || !strings.Contains(plan.Message, "will not be retriggered") {
		t.Fatalf("plan = %#v", plan)
	}
	if len(plan.Commands) != 2 || plan.Commands[0].Name != "udev-rules-verify" || plan.Commands[1].Name != "udev-rules-reload" {
		t.Fatalf("commands = %#v", plan.Commands)
	}
}

func TestPlanHostConfigurationStagesKernelModuleFiles(t *testing.T) {
	desired := testHostConfiguration("storage-modules", "/etc/modules-load.d/80-storage.conf", "br_netfilter\n")
	plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired, t.TempDir())
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := testHostConfiguration("example", tt.path, tt.content)
			plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired, t.TempDir())
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

func TestPlanHostConfigurationReloadsNetworking(t *testing.T) {
	for _, test := range []struct {
		path    string
		command []string
	}{
		{"20-lan.network", []string{"networkctl", "reload"}},
		{"20-lan.network.d/50-address.conf", []string{"networkctl", "reload"}},
		{"20-lan.netdev", []string{"networkctl", "reload"}},
		{"20-lan.netdev.d/50-options.conf", []string{"networkctl", "reload"}},
		{"10-uplink.link", []string{"/usr/bin/udevadm", "control", "--reload"}},
		{"10-uplink.link.d/50-options.conf", []string{"/usr/bin/udevadm", "control", "--reload"}},
	} {
		t.Run(test.path, func(t *testing.T) {
			before := testHostConfiguration("network", "/etc/systemd/network/"+test.path, "[Match]\nName=old\n")
			after := testHostConfiguration("network", "/etc/systemd/network/"+test.path, "[Match]\nName=new\n")
			for _, transition := range []struct {
				name          string
				before, after manifest.HostConfiguration
			}{
				{"add", manifest.HostConfiguration{}, after},
				{"change", before, after},
				{"remove", before, manifest.HostConfiguration{}},
			} {
				t.Run(transition.name, func(t *testing.T) {
					plan := planHostConfigurationChange(transition.before, transition.after, t.TempDir())
					if !plan.Live || len(plan.warnings) == 0 || !strings.Contains(plan.warnings[0], "different from the saved configuration") {
						t.Fatalf("plan = %#v, want live networking with convergence warning", plan)
					}
					if len(plan.Commands) != 1 || !slices.Equal(plan.Commands[0].Argv, test.command) {
						t.Fatalf("commands = %#v", plan.Commands)
					}
					if len(plan.rollbackCommands) != 1 || !slices.Equal(plan.rollbackCommands[0].Argv, test.command) {
						t.Fatalf("rollback commands = %#v", plan.rollbackCommands)
					}
				})
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
				manifest.HostConfiguration{Sysfs: tt.desired}, t.TempDir())
			if plan.Live || !strings.Contains(plan.Message, "sysfs configuration applies and verifies on next boot") {
				t.Fatalf("plan = %#v, want staged sysfs change", plan)
			}
			if len(plan.Effects) != 1 || plan.Effects[0].Action != tt.action || plan.Effects[0].Target != "sysfs "+setting.Name {
				t.Fatalf("effects = %#v", plan.Effects)
			}
		})
	}
}

func TestPlanHostConfigurationDerivesSystemdComponentNotification(t *testing.T) {
	for _, tt := range []struct {
		path, unit string
	}{
		{path: "/etc/systemd/journald.conf.d/80-home-lab.conf", unit: "systemd-journald.service"},
		{path: "/etc/systemd/timesyncd.conf.d/80-home-lab.conf", unit: "systemd-timesyncd.service"},
	} {
		t.Run(tt.unit, func(t *testing.T) {
			content := "[Service]\nSetting=value\n"
			desired := testHostConfiguration("component", tt.path, content)
			plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired, t.TempDir())
			if !plan.Live {
				t.Fatalf("plan = %#v", plan)
			}
			if len(plan.Effects) != 1 || plan.Effects[0].Action != "try-reload-or-restart" || plan.Effects[0].Target != "systemd unit "+tt.unit {
				t.Fatalf("effects = %#v", plan.Effects)
			}
		})
	}

	unsupported := testHostConfiguration("manager", "/etc/systemd/system.conf.d/80-home-lab.conf", "[Manager]\nShowStatus=yes\n")
	if plan := planHostConfigurationChange(manifest.HostConfiguration{}, unsupported, t.TempDir()); plan.Live || !strings.Contains(plan.Message, "no proven live adapter") {
		t.Fatalf("unsupported component plan = %#v", plan)
	}
}

func TestPlanHostConfigurationRegrouping(t *testing.T) {
	content := "[Journal]\nSystemMaxUse=2G\n"
	changedContent := "[Journal]\nSystemMaxUse=3G\n"
	file := manifest.HostConfigurationFile{
		Path:    "/etc/systemd/journald.conf.d/80-home-lab.conf",
		Content: &content,
		Mode:    0o644,
	}
	current := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"host": {Files: []manifest.HostConfigurationFile{file}},
	}}
	notification := manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{{
		Unit:   "systemd-journald.service",
		Action: "try-reload-or-restart",
	}}}

	for _, tt := range []struct {
		name        string
		destination manifest.HostConfigurationSet
		wantLive    bool
		wantEffect  string
		wantPath    bool
	}{
		{
			name:        "unchanged file with notification",
			destination: manifest.HostConfigurationSet{Files: []manifest.HostConfigurationFile{file}, Notify: notification},
			wantLive:    true,
			wantEffect:  "try-reload-or-restart systemd unit systemd-journald.service",
		},
		{
			name:        "unchanged file without notification",
			destination: manifest.HostConfigurationSet{Files: []manifest.HostConfigurationFile{file}},
			wantLive:    true,
		},
		{
			name: "changed file without notification",
			destination: manifest.HostConfigurationSet{Files: []manifest.HostConfigurationFile{{
				Path: file.Path, Content: &changedContent, Mode: file.Mode,
			}}},
			wantLive:   true,
			wantEffect: "try-reload-or-restart systemd unit systemd-journald.service",
			wantPath:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			desired := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
				"journald": tt.destination,
			}}
			plan := planHostConfigurationChange(current, desired, t.TempDir())
			if plan.Live != tt.wantLive {
				t.Fatalf("plan = %#v, want live = %t", plan, tt.wantLive)
			}
			if !slices.Equal(plan.Sets, []string{"host", "journald"}) {
				t.Fatalf("sets = %v", plan.Sets)
			}
			if (len(plan.Paths) != 0) != tt.wantPath {
				t.Fatalf("paths = %v, want changed path = %t", plan.Paths, tt.wantPath)
			}
			if tt.wantEffect != "" && !strings.Contains(plan.Message, tt.wantEffect) {
				t.Fatalf("message = %q, want effect %q", plan.Message, tt.wantEffect)
			}
		})
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
		},
	}}
	plan := planHostConfigurationChange(current, desired, sysctlHostRoot(t))
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

func TestUnmaskDoesNotStartUnit(t *testing.T) {
	plan := planHostConfigurationChange(manifest.HostConfiguration{MaskedUnits: []string{"bluetooth.service"}}, manifest.HostConfiguration{}, t.TempDir())
	if !plan.Live {
		t.Fatalf("unmask requires reboot: %s", plan.Message)
	}
	for _, command := range plan.Commands {
		if strings.Contains(strings.Join(command.Argv, " "), "start") {
			t.Fatalf("unmask starts a unit: %v", command.Argv)
		}
	}
	if len(plan.Commands) == 0 {
		t.Fatal("unmask must refresh systemd")
	}
}
