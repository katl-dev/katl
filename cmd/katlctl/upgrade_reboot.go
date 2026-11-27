package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
)

var upgradeRebootPollInterval = 2 * time.Second

type verifiedNodeBoot struct {
	Status     *agentapi.NodeStatus
	Generation *agentapi.Generation
	// Endpoint is where the rebooted node last answered.
	Endpoint string
}

func (boot verifiedNodeBoot) healthyOn(generationID string) bool {
	return runningGeneration(boot.Status) == generationID &&
		boot.Status.GetCurrentGenerationId() == generationID &&
		boot.Status.GetBootHealthState() == bootHealthHealthy &&
		boot.Generation.GetCommitState() == generation.CommitStateCommitted &&
		boot.Generation.GetBootState() == generation.BootStateGood &&
		boot.Generation.GetHealthState() == generation.HealthStateHealthy
}

// Boot health states reported by the node agent for its running boot.
const (
	bootHealthHealthy   = "healthy"
	bootHealthPending   = "pending"
	bootHealthFailed    = "failed"
	bootHealthUnknown   = "unknown"
	bootHealthUnhealthy = "unhealthy"
)

// runningGeneration is the generation the node booted. Active configuration
// can already name another generation while a rejected trial is running.
func runningGeneration(status *agentapi.NodeStatus) string {
	return firstNonEmpty(strings.TrimSpace(status.GetSelectedGenerationId()), strings.TrimSpace(status.GetCurrentGenerationId()))
}

// bootHealthFailure explains a boot that finished boot health unsuccessfully.
// The agent reports pending while boot recovery runs, so a failed boot is
// final: no automatic reboot follows it.
func bootHealthFailure(ctx context.Context, client agentapi.KatlcAgentClient, target bootWaitTarget, targetGeneration string, status *agentapi.NodeStatus) error {
	nodeName := target.nodeName
	running := runningGeneration(status)
	reason := firstNonEmpty(strings.TrimSpace(status.GetBootHealthDiagnostic()), "boot health is "+firstNonEmpty(strings.TrimSpace(status.GetBootHealthState()), "not reported"))
	message := fmt.Sprintf("node %s failed boot health on generation %s: %s", nodeName, running, reason)
	if running != targetGeneration {
		message = fmt.Sprintf("node %s booted generation %s instead of %s, and it failed boot health: %s", nodeName, running, targetGeneration, reason)
	}
	if next := strings.TrimSpace(status.GetBootTargetGenerationId()); next != "" && next != running {
		message += fmt.Sprintf(". Generation %s is recorded for next boot", next)
		list, err := client.ListGenerations(ctx, &agentapi.ListGenerationsRequest{})
		generations := list.GetGenerations()
		index := slices.IndexFunc(generations, func(gen *agentapi.Generation) bool { return gen.GetGenerationId() == next })
		if err == nil && list.GetNextBootGenerationId() == next && index >= 0 {
			if reason := generations[index].GetUnavailableReason(); reason != "" {
				message += " (" + reason + ")"
			}
		}
		message += fmt.Sprintf(". Check %s before rebooting", target.command("generations list"))
	} else {
		message += fmt.Sprintf(". Fix the failure or check %s and select an available generation with %s before rebooting", target.command("generations list"), target.command("generations select GENERATION"))
	}
	return fmt.Errorf("%s. Find the failed unit with %s and %s", message, target.command("logs"), target.command("status"))
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
	// APIProxyReady and LocalAPIEligible record the API proxy before the
	// reboot; kubelet reaches the API through it, so a node is not recovered
	// until the proxy serves again.
	APIProxyReady    bool
	LocalAPIEligible bool
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

// bootWaitTarget names where a rebooting node may answer. A node whose
// configuration moves it to a new address can answer at either endpoint, so an
// answer counts only when it reports the installation that was rebooted.
type bootWaitTarget struct {
	nodeName     string
	endpoints    []string
	machineID    string
	enrollmentID string
	// flags address the node in recovery commands, such as " --config cluster.yaml".
	flags string
}

// command renders a katlctl node subcommand that addresses the waited node.
func (target bootWaitTarget) command(subcommand string) string {
	return "'katlctl node " + subcommand + " " + target.nodeName + target.flags + "'"
}

func (target bootWaitTarget) verify(endpoint string, status *agentapi.NodeStatus) error {
	machine := strings.TrimSpace(target.machineID)
	enrollment := strings.TrimSpace(target.enrollmentID)
	if (machine != "" && status.GetMachineId() != machine) || (enrollment != "" && status.GetEnrollmentId() != enrollment) {
		return fmt.Errorf("address %s answered as a different KatlOS installation than node %s; another host may use that address, so correct the node's address in the cluster configuration before retrying", endpoint, target.nodeName)
	}
	return nil
}

// waitNodeBootHealth waits for the boot that follows previousAgentStart to
// settle boot health and, when it runs targetGeneration healthily, to recover
// Kubernetes. Only boot health the agent reports for its running boot counts:
// persisted generation health can describe an earlier boot of the same
// generation. It polls for itself, so its connections report a refused
// connection at once, and the connection it returns keeps that behaviour for the
// later calls of a node that has just been verified healthy.
func waitNodeBootHealth(ctx context.Context, target bootWaitTarget, previousAgentStart, targetGeneration string, requirement nodeRecoveryRequirement, progressPrefix string, stderr io.Writer) (katlcAgentConnection, verifiedNodeBoot, error) {
	nodeName := target.nodeName
	lastPhase := ""
	// phase reports each change of where the reboot stands, never each poll.
	phase := func(name string) {
		if name != lastPhase {
			lastPhase = name
			_, _ = fmt.Fprintf(stderr, "%s %s\n", progressPrefix, name)
		}
	}
	restarted := false
	answered := false
	lastRecovery := nodeRecovery{}
	observed := verifiedNodeBoot{}
	// probe reports done once the node answering at endpoint gives a terminal result.
	probe := func(conn katlcAgentConnection, endpoint string) (verifiedNodeBoot, bool, error) {
		status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if statusErr != nil {
			return verifiedNodeBoot{}, false, nil
		}
		answered = true
		if strings.TrimSpace(status.GetAgentStartId()) == "" || status.GetAgentStartId() == strings.TrimSpace(previousAgentStart) {
			return verifiedNodeBoot{}, false, nil
		}
		if err := target.verify(endpoint, status); err != nil {
			return verifiedNodeBoot{}, true, err
		}
		if !restarted {
			restarted = true
			restart := "agent-restarted"
			if len(target.endpoints) > 1 {
				restart += " endpoint=" + endpoint
			}
			phase(restart)
		}
		switch status.GetBootHealthState() {
		case bootHealthHealthy:
			phase("boot-health-healthy")
		case bootHealthPending, "":
			phase("boot-health-pending")
		case bootHealthUnknown:
			phase("boot-health-unknown")
		case bootHealthUnhealthy:
			progress := "boot-health-unhealthy"
			if reason := strings.TrimSpace(status.GetBootHealthDiagnostic()); reason != "" {
				progress += " (" + reason + ")"
			}
			phase(progress)
		}
		observed.Status = status
		observed.Endpoint = endpoint
		candidate, generationErr := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: targetGeneration})
		if generationErr != nil {
			return verifiedNodeBoot{}, false, nil
		}
		observed.Generation = candidate

		if status.GetBootHealthState() == bootHealthFailed {
			return verifiedNodeBoot{}, true, bootHealthFailure(ctx, conn.Client, target, targetGeneration, status)
		}
		running := runningGeneration(status)
		// Rejection is durable even while the fallback is still settling.
		if running != "" && running != targetGeneration &&
			(candidate.GetBootState() == generation.BootStateFailed || candidate.GetHealthState() == generation.HealthStateUnhealthy) {
			return verifiedNodeBoot{}, true, fmt.Errorf("node %s rejected generation %s during boot health and returned on generation %s", nodeName, targetGeneration, running)
		}
		if status.GetBootHealthState() != bootHealthHealthy {
			return verifiedNodeBoot{}, false, nil
		}
		if running == targetGeneration && !observed.healthyOn(targetGeneration) {
			return verifiedNodeBoot{}, false, nil
		}

		recovery := nodeUpgradeRecovery(status, requirement)
		if recovery != lastRecovery {
			lastRecovery = recovery
			if !recovery.Ready {
				_, _ = fmt.Fprintf(stderr, "%s waiting-for-kubernetes state=%s reason=%s\n", progressPrefix, recovery.State, recovery.Reason)
			}
		}
		if !recovery.Ready {
			return verifiedNodeBoot{}, false, nil
		}
		if running != targetGeneration {
			return verifiedNodeBoot{}, true, fmt.Errorf("node %s booted generation %s instead of %s; inspect %s, select the intended generation with %s, then reboot again", nodeName, running, targetGeneration, target.command("generations list"), target.command("generations select GENERATION"))
		}
		return verifiedNodeBoot{Status: status, Generation: candidate, Endpoint: endpoint}, true, nil
	}
	for {
		answered = false
		for _, endpoint := range target.endpoints {
			conn, err := dialKatlcAgent(transport.WithoutManagementWait(ctx), endpoint)
			if err != nil {
				continue
			}
			verified, done, err := probe(conn, endpoint)
			if done && err == nil {
				return conn, verified, nil
			}
			_ = conn.Close()
			if done {
				return katlcAgentConnection{}, observed, err
			}
		}
		// Before the node answers from a new boot, no answer at any endpoint means it is down.
		if !answered && !restarted {
			phase("node-down")
		}

		timer := time.NewTimer(upgradeRebootPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if observed.Status.GetBootHealthState() == bootHealthHealthy && lastRecovery.Reason != "" {
				return katlcAgentConnection{}, observed, nodeKubernetesRecoveryTimeoutError(nodeName, targetGeneration, lastRecovery.Reason, ctx.Err())
			}
			if observed.Status != nil {
				reason := firstNonEmpty(strings.TrimSpace(observed.Status.GetBootHealthDiagnostic()), "boot health is "+firstNonEmpty(strings.TrimSpace(observed.Status.GetBootHealthState()), "not reported"))
				return katlcAgentConnection{}, observed, fmt.Errorf("node %s did not finish boot health on generation %s: %s: %w; inspect it with %s and %s", nodeName, runningGeneration(observed.Status), reason, ctx.Err(), target.command("status"), target.command("logs"))
			}
			return katlcAgentConnection{}, observed, fmt.Errorf("node %s did not return healthy on generation %s: %w; it has not answered from a new boot, so check its console, then inspect it with %s", nodeName, targetGeneration, ctx.Err(), target.command("status"))
		case <-timer.C:
		}
	}
}

// waitNodeKubernetesRecovery polls for itself, so the connection it returns
// reports a refused connection at once on later calls, like waitNodeBootHealth.
func waitNodeKubernetesRecovery(ctx context.Context, nodeName, endpoint string, requirement nodeRecoveryRequirement, progressPrefix string, stderr io.Writer) (katlcAgentConnection, *agentapi.NodeStatus, error) {
	lastRecovery := nodeRecovery{}
	for {
		conn, err := dialKatlcAgent(transport.WithoutManagementWait(ctx), endpoint)
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
	if proxy := status.GetApiProxy(); proxy != nil && proxy.GetState() == "ready" {
		requirement.APIProxyReady = true
		requirement.LocalAPIEligible = proxy.GetLocalApiEligible()
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
	if requirement.APIProxyReady {
		proxy := status.GetApiProxy()
		switch {
		case proxy.GetState() != "ready":
			recovery.State = "waiting-for-api-proxy"
			recovery.Reason = "Kubernetes API proxy: " + firstNonEmpty(proxy.GetFailureReason(), proxy.GetState(), "status not reported")
			return recovery
		case requirement.LocalAPIEligible && !proxy.GetLocalApiEligible():
			recovery.State = "waiting-for-api-proxy"
			recovery.Reason = "Kubernetes API proxy does not reach the local API server"
			for _, backend := range proxy.GetBackends() {
				if backend.GetLocal() {
					recovery.Reason += fmt.Sprintf(" at %s: %s", backend.GetAddress(), firstNonEmpty(backend.GetReason(), "not eligible"))
				}
			}
			return recovery
		}
	}
	recovery.Ready = true
	return recovery
}

func managedEndpointReady(endpoint *agentapi.ControlPlaneEndpointStatus) bool {
	return endpoint != nil && endpoint.GetLocalApiReady() && endpoint.GetLocalVipOwned() && strings.EqualFold(endpoint.GetState(), "active")
}
