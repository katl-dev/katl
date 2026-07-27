package manifest

import (
	"strings"
	"testing"
)

func TestValidateHostConfigurationAcceptsNativeFilesAndNotifications(t *testing.T) {
	sysctl := "net.ipv4.ip_forward = 1\n"
	journal := "[Journal]\nSystemMaxUse=2G\n"
	network := "[Network]\nAddress=10.254.1.1/31\n"
	config := HostConfiguration{
		Sysfs: []HostConfigurationSysfsSetting{{
			Name:  "/sys/module/printk/parameters/time",
			Value: "N",
		}},
		Sets: map[string]HostConfigurationSet{
			"forwarding": {
				Files: []HostConfigurationFile{{
					Path:    "/etc/sysctl.d/80-forwarding.conf",
					Content: &sysctl,
					Mode:    0o640,
				}},
			},
			"journal-limits": {
				Files: []HostConfigurationFile{{
					Path:    "/etc/systemd/journald.conf.d/80-home-lab.conf",
					Content: &journal,
				}},
				Notify: HostConfigurationNotifications{Systemd: []HostConfigurationSystemdNotification{{
					Unit:   "systemd-journald.service",
					Action: "try-reload-or-restart",
				}}},
			},
			"bond-address": {
				Files: []HostConfigurationFile{{
					Path:    "/etc/systemd/network/20-bond0.network.d/50-address.conf",
					Content: &network,
				}},
			},
		},
	}
	if err := ValidateHostConfiguration(config, false); err != nil {
		t.Fatalf("ValidateHostConfiguration() error = %v", err)
	}
}

func TestValidateHostConfigurationRejectsUnsafeSysfsSettings(t *testing.T) {
	tests := []struct {
		name    string
		setting HostConfigurationSysfsSetting
		want    string
	}{
		{name: "outside sysfs", setting: HostConfigurationSysfsSetting{Name: "/proc/sys/kernel/hostname", Value: "lab"}, want: "below /sys"},
		{name: "not normalized", setting: HostConfigurationSysfsSetting{Name: "/sys/module/../example", Value: "1"}, want: "normalized"},
		{name: "name glob", setting: HostConfigurationSysfsSetting{Name: "/sys/class/net/*/mtu", Value: "9000"}, want: "globs"},
		{name: "name whitespace", setting: HostConfigurationSysfsSetting{Name: "/sys/example value", Value: "1"}, want: "must not contain whitespace"},
		{name: "empty value", setting: HostConfigurationSysfsSetting{Name: "/sys/example"}, want: "non-empty single-line"},
		{name: "leading whitespace", setting: HostConfigurationSysfsSetting{Name: "/sys/example", Value: " one two"}, want: "leading or trailing whitespace"},
		{name: "newline", setting: HostConfigurationSysfsSetting{Name: "/sys/example", Value: "one\ntwo"}, want: "single-line"},
		{name: "nul", setting: HostConfigurationSysfsSetting{Name: "/sys/example", Value: "one\x00two"}, want: "single-line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := HostConfiguration{Sysfs: []HostConfigurationSysfsSetting{tt.setting}}
			err := ValidateHostConfiguration(config, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateHostConfiguration() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateHostConfigurationAcceptsSysfsValueWithoutLeakingRendererSyntax(t *testing.T) {
	config := HostConfiguration{Sysfs: []HostConfigurationSysfsSetting{{
		Name:  "/sys/example",
		Value: `one two %m [value] \ -`,
	}}}
	if err := ValidateHostConfiguration(config, false); err != nil {
		t.Fatalf("ValidateHostConfiguration() error = %v", err)
	}
}

func TestValidateHostConfigurationRejectsDuplicateSysfsNames(t *testing.T) {
	config := HostConfiguration{Sysfs: []HostConfigurationSysfsSetting{
		{Name: "/sys/module/printk/parameters/time", Value: "N"},
		{Name: "/sys/module/printk/parameters/time", Value: "Y"},
	}}
	err := ValidateHostConfiguration(config, false)
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want duplicate name rejection", err)
	}
}

func TestValidateHostConfigurationRejectsUnsafeOwnership(t *testing.T) {
	content := "value\n"
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "outside etc", path: "/var/lib/example", want: "below /etc"},
		{name: "not normalized", path: "/etc/sysctl.d/../passwd", want: "normalized"},
		{name: "katl prefix", path: "/etc/katl/example.conf", want: "KatlOS-owned prefix"},
		{name: "identity", path: "/etc/hostname", want: "owned by KatlOS"},
		{name: "kubelet drop-in", path: "/etc/systemd/system/kubelet.service.d/80-user.conf", want: "protected systemd"},
		{name: "unit enablement", path: "/etc/systemd/system/multi-user.target.wants/example.service", want: "protected systemd"},
		{name: "accounts", path: "/etc/shadow", want: "owned by KatlOS"},
		{name: "tmpfiles", path: "/etc/tmpfiles.d/80-example.conf", want: "hostConfiguration.sysfs"},
		{name: "networkd suffix", path: "/etc/systemd/network/20-bond0.conf", want: ".network, .netdev, or .link"},
		{name: "networkd drop-in suffix", path: "/etc/systemd/network/20-bond0.network.d/50-address.txt", want: "safe .conf drop-in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := HostConfiguration{Sets: map[string]HostConfigurationSet{
				"example": {Files: []HostConfigurationFile{{Path: tt.path, Content: &content}}},
			}}
			err := ValidateHostConfiguration(config, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateHostConfiguration() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateHostConfigurationRejectsUnsafeSystemdNotificationUnit(t *testing.T) {
	content := "enabled=true\n"
	config := HostConfiguration{Sets: map[string]HostConfigurationSet{
		"example": {
			Files: []HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}},
			Notify: HostConfigurationNotifications{Systemd: []HostConfigurationSystemdNotification{{
				Unit:   "--system.service",
				Action: "reload",
			}}},
		},
	}}
	if err := ValidateHostConfiguration(config, false); err == nil || !strings.Contains(err.Error(), "single systemd unit name") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want unsafe unit rejection", err)
	}
}

func TestValidateHostConfigurationRejectsAmbiguousSets(t *testing.T) {
	content := "value\n"
	config := HostConfiguration{Sets: map[string]HostConfigurationSet{
		"first": {
			Files:  []HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}},
			Notify: HostConfigurationNotifications{Systemd: []HostConfigurationSystemdNotification{{Unit: "example.service", Action: "reload"}}},
		},
		"second": {
			Files:  []HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}},
			Notify: HostConfigurationNotifications{Systemd: []HostConfigurationSystemdNotification{{Unit: "example.service", Action: "try-restart"}}},
		},
	}}
	err := ValidateHostConfiguration(config, false)
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want duplicate ownership", err)
	}

	config.Sets["second"] = HostConfigurationSet{
		Files:  []HostConfigurationFile{{Path: "/etc/second.conf", Content: &content}},
		Notify: HostConfigurationNotifications{Systemd: []HostConfigurationSystemdNotification{{Unit: "example.service", Action: "try-restart"}}},
	}
	err = ValidateHostConfiguration(config, false)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want notification conflict", err)
	}
}

func TestValidateHostConfigurationSourceIsAuthoringOnly(t *testing.T) {
	config := HostConfiguration{Sets: map[string]HostConfigurationSet{
		"example": {Files: []HostConfigurationFile{{Path: "/etc/example.conf", Source: "files/example.conf"}}},
	}}
	if err := ValidateHostConfiguration(config, true); err != nil {
		t.Fatalf("ValidateHostConfiguration(allowSource) error = %v", err)
	}
	err := ValidateHostConfiguration(config, false)
	if err == nil || !strings.Contains(err.Error(), "resolved before installation") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want unresolved source rejection", err)
	}
}

func TestValidateHostConfigurationAbsentSetCannotCarryPayload(t *testing.T) {
	content := "value\n"
	config := HostConfiguration{Sets: map[string]HostConfigurationSet{
		"example": {
			State: HostConfigurationAbsent,
			Files: []HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}},
		},
	}}
	err := ValidateHostConfiguration(config, false)
	if err == nil || !strings.Contains(err.Error(), "must not declare") {
		t.Fatalf("ValidateHostConfiguration() error = %v, want absent payload rejection", err)
	}
}
