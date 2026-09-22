package main

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
)

func TestHostUpgradeCombinedPlan(t *testing.T) {
	fake := readyHostUpgradeClient()
	fake.nodeStatus.InventoryNodeName = "cp-1"
	installKatlcDial(t, func(string) {}, fake)

	err := run(context.Background(), []string{
		"node", "upgrade", "cp-1", "--version", "2026.7.0-dev.13",
		"--config", writeClusterConfig(t), "--apply-config", "--plan",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.submitRequests) != 1 {
		t.Fatalf("requests = %d, want one complete combined preflight", len(fake.submitRequests))
	}
	for _, request := range fake.submitRequests {
		if !request.DryRun {
			t.Fatal("plan submitted a mutation")
		}
	}
	if fake.submitRequests[0].HostUpgrade.ResolveTargetOnly {
		t.Fatal("combined plan omitted configuration validation")
	}
	change, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(fake.submitRequest.HostUpgrade.ConfigYaml), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseUint(change.DesiredVersion, 10, 64); err != nil {
		t.Fatalf("configuration revision is not numeric: %q", change.DesiredVersion)
	}
	if change.ApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("combined configuration mode = %s", change.ApplyMode)
	}
	if len(fake.rebootRequests) != 0 {
		t.Fatal("plan rebooted the node")
	}
}
