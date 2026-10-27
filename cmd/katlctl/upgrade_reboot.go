package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

var upgradeRebootPollInterval = 2 * time.Second

type verifiedNodeBoot struct {
	Status     *agentapi.NodeStatus
	Generation *agentapi.Generation
}

func (boot verifiedNodeBoot) healthyOn(generationID string) bool {
	selected := strings.TrimSpace(boot.Status.GetSelectedGenerationId())
	return boot.Status.GetCurrentGenerationId() == generationID &&
		(selected == "" || selected == generationID) &&
		(boot.Status.GetBootHealthState() == "" || boot.Status.GetBootHealthState() == "healthy") &&
		boot.Generation.GetCommitState() == generation.CommitStateCommitted &&
		boot.Generation.GetBootState() == generation.BootStateGood &&
		boot.Generation.GetHealthState() == generation.HealthStateHealthy
}

type nodeRecovery struct {
	State  string
	Reason string
	Ready  bool
}

type nodeRecoveryRequirement struct {
	KubernetesConfigured bool
	NodeReady            bool
	ManagedEndpointReady bool
}

func requestNodeReboot(ctx context.Context, client agentapi.KatlcAgentClient, actor string, status *agentapi.NodeStatus, targetGeneration string) error {
	accepted, err := client.Reboot(ctx, &agentapi.RebootRequest{
		ApiVersion:                  generation.APIVersion,
		Kind:                        "RebootRequest",
		Actor:                       strings.TrimSpace(actor),
		ExpectedEnrollmentId:        strings.TrimSpace(status.GetEnrollmentId()),
		ExpectedInventoryNodeName:   strings.TrimSpace(status.GetInventoryNodeName()),
		ExpectedMachineId:           strings.TrimSpace(status.GetMachineId()),
		ExpectedCurrentGenerationId: strings.TrimSpace(status.GetCurrentGenerationId()),
		TargetGenerationId:          strings.TrimSpace(targetGeneration),
	})
	if err != nil {
		return err
	}
	if !accepted.GetScheduled() || accepted.GetTargetGenerationId() != strings.TrimSpace(targetGeneration) {
		return fmt.Errorf("node did not schedule reboot into generation %s", targetGeneration)
	}
	return nil
}

func waitNodeBootHealth(ctx context.Context, nodeName, endpoint, previousAgentStart, targetGeneration string, requirement nodeRecoveryRequirement, stderr io.Writer) (katlcAgentConnection, verifiedNodeBoot, error) {
	return waitNodeBootHealthWithPrefix(ctx, nodeName, endpoint, previousAgentStart, targetGeneration, requirement, "upgrade node="+nodeName, stderr)
}

func waitNodeBootHealthWithPrefix(ctx context.Context, nodeName, endpoint, previousAgentStart, targetGeneration string, requirement nodeRecoveryRequirement, progressPrefix string, stderr io.Writer) (katlcAgentConnection, verifiedNodeBoot, error) {
	lastState := ""
	lastRecovery := nodeRecovery{}
	lastRollback := ""
	observed := verifiedNodeBoot{}
	for {
		conn, err := dialKatlcAgent(ctx, endpoint)
		if err == nil {
			status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if statusErr == nil {
				state := fmt.Sprintf("agent=%s generation=%s running-generation=%s boot-health=%s", status.GetAgentStartId(), status.GetCurrentGenerationId(), status.GetSelectedGenerationId(), status.GetBootHealthState())
				if state != lastState {
					lastState = state
					_, _ = fmt.Fprintf(stderr, "%s waiting-for-boot-health %s\n", progressPrefix, state)
				}
				if strings.TrimSpace(status.GetAgentStartId()) != "" && status.GetAgentStartId() != strings.TrimSpace(previousAgentStart) {
					observed.Status = status
					candidate, generationErr := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: targetGeneration})
					if generationErr == nil {
						observed.Generation = candidate
						fallback := ""
						// Active configuration can already name the fallback while the
						// rejected trial is still running. Native boot health must
						// confirm the selected generation before reporting recovery.
						selected := strings.TrimSpace(status.GetSelectedGenerationId())
						if candidate.GetHealthState() == generation.HealthStateUnhealthy {
							rollback := strings.TrimSpace(status.GetBootTargetGenerationId())
							switch {
							case selected != "" && selected != targetGeneration && status.GetBootHealthState() == "healthy":
								fallback = selected
							case selected != "" && rollback != "" && rollback != targetGeneration:
								if rollback != lastRollback {
									lastRollback = rollback
									_, _ = fmt.Fprintf(stderr, "%s waiting-for-rollback generation=%s\n", progressPrefix, rollback)
								}
							case selected == "" && status.GetCurrentGenerationId() != targetGeneration:
								_ = conn.Close()
								return katlcAgentConnection{}, observed, fmt.Errorf("node %s rejected generation %s during boot health; running generation is not reported", nodeName, targetGeneration)
							case rollback != "" && rollback != targetGeneration:
								_ = conn.Close()
								return katlcAgentConnection{}, observed, fmt.Errorf("node %s rejected generation %s during boot health; rollback generation %s is selected for next boot, so reboot the node before retrying the upgrade", nodeName, targetGeneration, rollback)
							default:
								_ = conn.Close()
								return katlcAgentConnection{}, observed, fmt.Errorf("node %s reported generation %s unhealthy after reboot", nodeName, targetGeneration)
							}
						}
						if fallback != "" || observed.healthyOn(targetGeneration) {
							recovery := nodeUpgradeRecovery(status, requirement)
							if recovery != lastRecovery {
								lastRecovery = recovery
								if !recovery.Ready {
									_, _ = fmt.Fprintf(stderr, "%s waiting-for-kubernetes state=%s reason=%s\n", progressPrefix, recovery.State, recovery.Reason)
								}
							}
							if recovery.Ready {
								if fallback != "" {
									_ = conn.Close()
									return katlcAgentConnection{}, observed, fmt.Errorf("node %s rejected generation %s during boot health and returned on generation %s", nodeName, targetGeneration, fallback)
								}
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
				return katlcAgentConnection{}, observed, nodeKubernetesRecoveryTimeoutError(nodeName, targetGeneration, lastRecovery.Reason, ctx.Err())
			}
			if lastRollback != "" {
				return katlcAgentConnection{}, observed, fmt.Errorf("node %s rejected generation %s during boot health; rollback generation %s is selected but has not returned healthy, so reboot the node before retrying: %w", nodeName, targetGeneration, lastRollback, ctx.Err())
			}
			return katlcAgentConnection{}, observed, fmt.Errorf("node %s did not return healthy on generation %s: %w", nodeName, targetGeneration, ctx.Err())
		case <-timer.C:
		}
	}
}

func waitNodeKubernetesRecovery(ctx context.Context, nodeName, endpoint string, requirement nodeRecoveryRequirement, progressPrefix string, stderr io.Writer) (katlcAgentConnection, *agentapi.NodeStatus, error) {
	lastRecovery := nodeRecovery{}
	for {
		conn, err := dialKatlcAgent(ctx, endpoint)
		if err == nil {
			status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if statusErr == nil {
				recovery := nodeUpgradeRecovery(status, requirement)
				if recovery != lastRecovery {
					lastRecovery = recovery
					if !recovery.Ready {
						_, _ = fmt.Fprintf(stderr, "%s waiting-for-kubernetes state=%s reason=%s\n", progressPrefix, recovery.State, recovery.Reason)
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

func nodeRecoveryRequirementFor(status *agentapi.NodeStatus) nodeRecoveryRequirement {
	requirement := nodeRecoveryRequirement{}
	kubernetes := status.GetKubernetes()
	if kubernetes != nil && strings.TrimSpace(kubernetes.GetState()) != "not-configured" {
		requirement.KubernetesConfigured = true
		requirement.NodeReady = kubernetes.GetNodeReady()
	}
	if endpoint := status.GetControlPlaneEndpoint(); endpoint != nil {
		requirement.ManagedEndpointReady = managedEndpointReady(endpoint)
	}
	return requirement
}

func nodeUpgradeRecovery(status *agentapi.NodeStatus, requirement nodeRecoveryRequirement) nodeRecovery {
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
		if requirement.KubernetesConfigured {
			recovery.Reason = "Kubernetes was configured before the reboot but is no longer configured"
			return recovery
		}
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
	if requirement.NodeReady && !kubernetes.GetNodeReady() {
		if recovery.Reason == "" {
			recovery.Reason = "Kubernetes node is not Ready"
		}
		return recovery
	}
	if requirement.ManagedEndpointReady {
		endpoint := status.GetControlPlaneEndpoint()
		if !managedEndpointReady(endpoint) {
			recovery.State = "waiting-for-managed-endpoint"
			recovery.Reason = "managed API endpoint is " + firstNonEmpty(strings.TrimSpace(endpoint.GetState()), "not ready")
			return recovery
		}
	}
	recovery.Ready = true
	return recovery
}

func managedEndpointReady(endpoint *agentapi.ControlPlaneEndpointStatus) bool {
	return endpoint != nil && endpoint.GetLocalApiReady() && endpoint.GetLocalVipOwned() && strings.EqualFold(endpoint.GetState(), "active")
}
