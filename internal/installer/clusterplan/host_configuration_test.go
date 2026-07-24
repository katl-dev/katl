package clusterplan

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestMergeHostConfigurationReplacesAndRemovesSets(t *testing.T) {
	defaultForwarding := "net.ipv4.ip_forward = 0\n"
	nodeForwarding := "net.ipv4.ip_forward = 1\n"
	udev := `SUBSYSTEM=="usb"` + "\n"
	base := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"forwarding": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-forwarding.conf", Content: &defaultForwarding}}},
		"remove-me":  {Files: []manifest.HostConfigurationFile{{Path: "/etc/example.conf", Content: &defaultForwarding}}},
	}}
	next := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"forwarding": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/90-node-forwarding.conf", Content: &nodeForwarding}}},
		"remove-me":  {State: manifest.HostConfigurationAbsent},
		"ups-device": {Files: []manifest.HostConfigurationFile{{Path: "/etc/udev/rules.d/80-ups.rules", Content: &udev}}},
	}}
	got, err := mergeHostConfiguration(base, next)
	if err != nil {
		t.Fatalf("mergeHostConfiguration() error = %v", err)
	}
	if len(got.Sets) != 2 {
		t.Fatalf("sets = %#v", got.Sets)
	}
	if _, exists := got.Sets["remove-me"]; exists {
		t.Fatalf("absent set remained: %#v", got.Sets)
	}
	forwarding := got.Sets["forwarding"]
	if len(forwarding.Files) != 1 || forwarding.Files[0].Path != "/etc/sysctl.d/90-node-forwarding.conf" {
		t.Fatalf("forwarding replacement = %#v", forwarding)
	}
	if forwarding.State != manifest.HostConfigurationPresent || forwarding.Files[0].Mode != 0o644 {
		t.Fatalf("forwarding defaults = %#v", forwarding)
	}
}

func TestMergeHostConfigurationRejectsDuplicatePathAcrossSelectedSets(t *testing.T) {
	content := "value\n"
	_, err := mergeHostConfiguration(
		manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
			"first": {Files: []manifest.HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}}},
		}},
		manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
			"second": {Files: []manifest.HostConfigurationFile{{Path: "/etc/example.conf", Content: &content}}},
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("mergeHostConfiguration() error = %v, want duplicate ownership", err)
	}
}
