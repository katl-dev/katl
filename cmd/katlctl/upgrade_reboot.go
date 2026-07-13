package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

var upgradeRebootPollInterval = 2 * time.Second

type verifiedNodeBoot struct {
	Status     *agentapi.NodeStatus
	Generation *agentapi.Generation
}

func requestNodeReboot(ctx context.Context, client agentapi.KatlcAgentClient, machineID, targetGeneration string) error {
	accepted, err := client.Reboot(ctx, &agentapi.RebootRequest{
		ApiVersion:         generation.APIVersion,
		Kind:               "RebootRequest",
		Actor:              "katlctl upgrade rollout",
		ExpectedMachineId:  strings.TrimSpace(machineID),
		TargetGenerationId: strings.TrimSpace(targetGeneration),
	})
	if err != nil {
		return err
	}
	if !accepted.GetScheduled() || accepted.GetTargetGenerationId() != strings.TrimSpace(targetGeneration) {
		return fmt.Errorf("node did not schedule reboot into generation %s", targetGeneration)
	}
	return nil
}

func waitNodeBootHealth(ctx context.Context, nodeName, endpoint, token, previousAgentStart, targetGeneration string, stderr io.Writer) (katlcAgentConnection, verifiedNodeBoot, error) {
	lastState := ""
	for {
		conn, err := dialKatlcAgent(ctx, endpoint, token)
		if err == nil {
			status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if statusErr == nil {
				state := fmt.Sprintf("agent=%s generation=%s", status.GetAgentStartId(), status.GetCurrentGenerationId())
				if state != lastState {
					lastState = state
					_, _ = fmt.Fprintf(stderr, "upgrade node=%s waiting-for-boot-health %s\n", nodeName, state)
				}
				if strings.TrimSpace(status.GetAgentStartId()) != "" && status.GetAgentStartId() != strings.TrimSpace(previousAgentStart) {
					candidate, generationErr := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: targetGeneration})
					if generationErr == nil {
						if candidate.GetHealthState() == generation.HealthStateUnhealthy {
							_ = conn.Close()
							return katlcAgentConnection{}, verifiedNodeBoot{}, fmt.Errorf("node %s rejected generation %s during boot health and rolled back", nodeName, targetGeneration)
						}
						if status.GetCurrentGenerationId() == targetGeneration && candidate.GetCommitState() == generation.CommitStateCommitted && candidate.GetBootState() == generation.BootStateGood && candidate.GetHealthState() == generation.HealthStateHealthy {
							return conn, verifiedNodeBoot{Status: status, Generation: candidate}, nil
						}
					}
				}
			}
			_ = conn.Close()
		}

		timer := time.NewTimer(upgradeRebootPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return katlcAgentConnection{}, verifiedNodeBoot{}, fmt.Errorf("node %s did not return healthy on generation %s: %w", nodeName, targetGeneration, ctx.Err())
		case <-timer.C:
		}
	}
}
