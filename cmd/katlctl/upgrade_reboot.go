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

type nodeRecovery struct {
	State  string
	Reason string
	Ready  bool
}

func requestNodeReboot(ctx context.Context, client agentapi.KatlcAgentClient, actor, machineID, targetGeneration string) error {
	accepted, err := client.Reboot(ctx, &agentapi.RebootRequest{
		ApiVersion:         generation.APIVersion,
		Kind:               "RebootRequest",
		Actor:              strings.TrimSpace(actor),
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

func waitNodeBootHealth(ctx context.Context, nodeName, endpoint, previousAgentStart, targetGeneration string, stderr io.Writer) (katlcAgentConnection, verifiedNodeBoot, error) {
	lastState := ""
	lastRecovery := nodeRecovery{}
	for {
		conn, err := dialKatlcAgent(ctx, endpoint)
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
							if current := status.GetCurrentGenerationId(); current != "" && current != targetGeneration {
								return katlcAgentConnection{}, verifiedNodeBoot{}, fmt.Errorf("node %s rejected generation %s during boot health and returned on generation %s", nodeName, targetGeneration, current)
							}
							return katlcAgentConnection{}, verifiedNodeBoot{}, fmt.Errorf("node %s reported generation %s unhealthy after reboot", nodeName, targetGeneration)
						}
						if status.GetCurrentGenerationId() == targetGeneration && candidate.GetCommitState() == generation.CommitStateCommitted && candidate.GetBootState() == generation.BootStateGood && candidate.GetHealthState() == generation.HealthStateHealthy {
							recovery := nodeUpgradeRecovery(status)
							if recovery != lastRecovery {
								lastRecovery = recovery
								if !recovery.Ready {
									_, _ = fmt.Fprintf(stderr, "upgrade node=%s waiting-for-kubernetes state=%s reason=%s\n", nodeName, recovery.State, recovery.Reason)
								}
							}
							if recovery.Ready {
								return conn, verifiedNodeBoot{Status: status, Generation: candidate}, nil
							}
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
			if lastRecovery.Reason != "" {
				return katlcAgentConnection{}, verifiedNodeBoot{}, nodeKubernetesRecoveryTimeoutError(nodeName, targetGeneration, lastRecovery.Reason, ctx.Err())
			}
			return katlcAgentConnection{}, verifiedNodeBoot{}, fmt.Errorf("node %s did not return healthy on generation %s: %w", nodeName, targetGeneration, ctx.Err())
		case <-timer.C:
		}
	}
}

func waitNodeKubernetesRecovery(ctx context.Context, nodeName, endpoint string, stderr io.Writer) (katlcAgentConnection, *agentapi.NodeStatus, error) {
	lastRecovery := nodeRecovery{}
	for {
		conn, err := dialKatlcAgent(ctx, endpoint)
		if err == nil {
			status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if statusErr == nil {
				recovery := nodeUpgradeRecovery(status)
				if recovery != lastRecovery {
					lastRecovery = recovery
					if !recovery.Ready {
						_, _ = fmt.Fprintf(stderr, "upgrade node=%s waiting-for-kubernetes state=%s reason=%s\n", nodeName, recovery.State, recovery.Reason)
					}
				}
				if recovery.Ready {
					return conn, status, nil
				}
			}
			_ = conn.Close()
		}
		timer := time.NewTimer(upgradeRebootPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return katlcAgentConnection{}, nil, nodeKubernetesRecoveryTimeoutError(nodeName, "", lastRecovery.Reason, ctx.Err())
		case <-timer.C:
		}
	}
}

func nodeKubernetesRecoveryTimeoutError(nodeName, generationID, reason string, err error) error {
	generationText := ""
	if generationID = strings.TrimSpace(generationID); generationID != "" {
		generationText = " on generation " + generationID
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "Kubernetes readiness was not reported"
	}
	return fmt.Errorf("node %s did not recover Kubernetes%s: %s: %w; do not schedule workloads on the node, and inspect it with 'katlctl node status'", nodeName, generationText, reason, err)
}

func nodeUpgradeRecovery(status *agentapi.NodeStatus) nodeRecovery {
	kubernetes := status.GetKubernetes()
	if kubernetes == nil {
		return nodeRecovery{
			State:  "unknown",
			Reason: "Kubernetes status is not reported by the node agent",
		}
	}
	recovery := nodeRecovery{State: strings.TrimSpace(kubernetes.GetState()), Reason: strings.TrimSpace(kubernetes.GetFailureReason())}
	if recovery.State == "" {
		recovery.State = "unknown"
	}
	if recovery.State == "not-configured" {
		recovery.Ready = true
		return recovery
	}
	if !kubernetes.GetKubeletActive() {
		if recovery.Reason == "" {
			recovery.Reason = "kubelet is not active"
		}
		return recovery
	}
	if strings.EqualFold(kubernetes.GetRole(), "control-plane") && !kubernetes.GetControlPlaneComponentsReady() {
		if recovery.Reason == "" {
			recovery.Reason = "local control-plane components are not ready"
		}
		return recovery
	}
	if !kubernetes.GetNodeReady() {
		if recovery.Reason == "" {
			recovery.Reason = "Kubernetes node is not Ready"
		}
		return recovery
	}
	if endpoint := status.GetControlPlaneEndpoint(); endpoint != nil {
		if !endpoint.GetLocalApiReady() || !endpoint.GetRouteOriginated() || !strings.EqualFold(endpoint.GetState(), "advertised") {
			recovery.State = "waiting-for-managed-endpoint"
			recovery.Reason = "managed API endpoint is " + firstNonEmpty(strings.TrimSpace(endpoint.GetState()), "not ready")
			return recovery
		}
		for _, exchange := range endpoint.GetRouteExchange() {
			if !strings.EqualFold(exchange.GetState(), "established") {
				recovery.State = "waiting-for-route-exchange"
				recovery.Reason = fmt.Sprintf("route exchange %s is %s", exchange.GetName(), firstNonEmpty(strings.TrimSpace(exchange.GetState()), "not established"))
				return recovery
			}
		}
	}
	recovery.State = "ready"
	recovery.Reason = ""
	recovery.Ready = true
	return recovery
}
