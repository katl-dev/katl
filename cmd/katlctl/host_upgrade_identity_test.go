package main

import (
	"context"
	"io"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
)

func TestLocalUpgradeReusedVersion(t *testing.T) {
	for _, plan := range []bool{true, false} {
		name := "apply"
		if plan {
			name = "plan"
		}
		t.Run(name, func(t *testing.T) {
			artifact, _, _ := writeHostUpgradeArtifact(t, "2026.7.0-dev.12", "x86_64", 1024)
			fake := readyHostUpgradeClient()
			fake.generation.RuntimeVersion = "2026.7.0-dev.12"
			fake.generation.CommitState = generation.CommitStateCommitted
			fake.generation.BootState = generation.BootStateGood
			fake.generation.HealthState = generation.HealthStateHealthy
			installKatlcDial(t, nil, fake)

			args := []string{"node", "upgrade", "cp-1", "--artifact", artifact, "--config", writeClusterConfig(t)}
			if plan {
				args = append(args, "--plan")
			}
			if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}

			if len(fake.stageArtifact) == 0 || fake.submitRequest == nil {
				t.Fatal("local image was skipped because its version label matched")
			}
			if fake.submitRequest.DryRun != plan {
				t.Fatalf("dry run = %t, want %t", fake.submitRequest.DryRun, plan)
			}
			if plan && len(fake.rebootRequests) != 0 {
				t.Fatal("preview rebooted the node")
			}
			if !plan && len(fake.rebootRequests) != 1 {
				t.Fatal("explicit local replacement did not reboot once")
			}
		})
	}
}

func TestUpgradeCandidatesBelongToRequests(t *testing.T) {
	fake := readyHostUpgradeClient()
	installKatlcDial(t, nil, fake)
	config := writeClusterConfig(t)
	args := []string{"node", "upgrade", "cp-1", "--version", "2026.7.0-dev.12", "--config", config, "--plan"}

	for range 2 {
		if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	first := fake.submitRequests[0].HostUpgrade.CandidateGenerationId
	second := fake.submitRequests[1].HostUpgrade.CandidateGenerationId
	if first == second {
		t.Fatal("independent requests reused a candidate identity; retry after rollback would collide with retained state")
	}
}
