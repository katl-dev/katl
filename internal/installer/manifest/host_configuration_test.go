package manifest

import (
	"strings"
	"testing"
)

func TestValidateHostConfigurationAcceptsNativeFilesAndNotifications(t *testing.T) {
	sysctl := "net.ipv4.ip_forward = 1\n"
	journal := "[Journal]\nSystemMaxUse=2G\n"
	config := HostConfiguration{Sets: map[string]HostConfigurationSet{
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
	}}
	if err := ValidateHostConfiguration(config, false); err != nil {
		t.Fatalf("ValidateHostConfiguration() error = %v", err)
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
