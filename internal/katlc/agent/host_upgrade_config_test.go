package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func TestUpgradeConfigIsPreparedOffline(t *testing.T) {
	root := t.TempDir()
	writeConfigApplyBaseState(t, root)
	executor := &Executor{Root: root}
	payload := katlosimage.Payload{
		Index: katlosimage.Index{
			Version:          "2026.9.2",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
		},
		Runtime: katlosimage.Component{SHA256: strings.Repeat("c", 64)},
	}
	before, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}

	_, files, domains, err := executor.planUpgradeConfig(context.Background(), "candidate", configApplyLiveYAML(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(domains, configapply.DomainHostConfiguration) {
		t.Fatalf("changed domains = %v", domains)
	}
	found := false
	for _, file := range files {
		if file.Path == "/etc/sysctl.d/80-forwarding.conf" && strings.Contains(file.Content, "net.ipv4.ip_forward = 1") {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate is missing the proposed host setting")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/sysctl.d/80-forwarding.conf")); !os.IsNotExist(err) {
		t.Fatalf("preparation changed the live configuration: %v", err)
	}
	after, err := generation.ReadBootSelection(root)
	if err != nil || before != after {
		t.Fatalf("preparation changed boot selection: %+v, %v", after, err)
	}
}

func TestUpgradeConfigRejectsMembershipChanges(t *testing.T) {
	root := t.TempDir()
	writeConfigApplyBaseState(t, root)
	executor := &Executor{Root: root}
	document := strings.Replace(configApplyNoChangesYAML(), "systemRole: control-plane", "systemRole: worker", 1)
	_, _, _, err := executor.planUpgradeConfig(context.Background(), "candidate", document, katlosimage.Payload{
		Index: katlosimage.Index{
			Version:          "2026.9.2",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
		},
	})
	if err == nil {
		t.Fatal("combined upgrade accepted a role transition")
	}
}
