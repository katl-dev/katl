package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
)

type nodeJoinOptions struct {
	configPath, node, coordinator, output string
	timeout                               time.Duration
}

func newNodeJoinCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := nodeJoinOptions{timeout: 15 * time.Minute, output: "text"}
	cmd := &cobra.Command{
		Use: "join NODE", Short: "Join an installed node to an existing Kubernetes cluster",
		Long: `Join one installed KatlOS node using the complete ClusterConfig and a ready
control plane. The node's configured role determines whether it joins as a
worker or control plane. Install the node with this config before joining it.

Repeating a completed join is a no-op. An interrupted join resumes its generation
health checks, rebooting only if the joined generation still needs a trial boot.
Use 'katlctl cluster bootstrap' to create the first control plane, and
'katlctl cluster apply' for configuration changes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := selectHostNode(&opts.node, args); err != nil {
				return err
			}
			return runNodeJoin(ctx, opts, stdout, stderr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.configPath, "config", "", "complete ClusterConfig YAML or Katl config bundle")
	f.StringVar(&opts.node, "node", "", "node name (alternative to NODE)")
	f.StringVar(&opts.coordinator, "coordinator", "", "ready control plane to coordinate the join (default: automatic)")
	f.DurationVar(&opts.timeout, "timeout", opts.timeout, "time to wait for the join and node health")
	f.StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func runNodeJoin(ctx context.Context, opts nodeJoinOptions, stdout, stderr io.Writer) error {
	if strings.TrimSpace(opts.node) == "" {
		return fmt.Errorf("NODE is required; use 'katlctl node join NODE --config cluster.yaml'")
	}
	if strings.TrimSpace(opts.configPath) == "" {
		return fmt.Errorf("--config is required")
	}
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	if err := refreshConfiguredManagement(ctx, opts.configPath, "", "", stderr, opts.node); err != nil {
		return err
	}
	inv, err := kubeadmConfigInventory(kubeadmControlPlaneConfigOptions{configPath: opts.configPath})
	if err != nil {
		return err
	}
	selected, err := selectConfigNodes(inv.Nodes, []string{opts.node})
	if err != nil {
		return err
	}
	target := selected[0]
	if opts.coordinator == target.Name {
		return fmt.Errorf("node %s cannot coordinate its own join; choose an existing control plane", target.Name)
	}
	status, current, err := readJoinNode(ctx, opts.configPath, target)
	if err != nil {
		return err
	}
	if err := validateClusterNodeLifecycle(target, status); err != nil {
		return err
	}
	result := "unchanged"
	switch {
	case isClusterJoinCandidate(current.GetGenerationId()) && (current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy):
		if err := finishNodeJoin(ctx, opts.configPath, target, stderr); err != nil {
			return err
		}
		result = "joined"
	case status.GetKubernetes().GetState() == "not-configured":
		if current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy {
			return fmt.Errorf("node %s must have a healthy committed generation before joining; inspect 'katlctl node status %s --config %s'", target.Name, target.Name, opts.configPath)
		}
		if next := status.GetBootTargetGenerationId(); next != "" && next != status.GetCurrentGenerationId() {
			return fmt.Errorf("node %s has configuration staged for next boot; reboot it before joining", target.Name)
		}
		coordinator, err := joinCoordinator(ctx, opts, inv, stderr)
		if err != nil {
			return err
		}
		topology, err := resolveClusterConfigTopology(opts.configPath)
		if err != nil {
			return err
		}
		deps := agentBootstrapDependencies(topology.ClusterName)
		deps.Actor = "katlctl node join"
		deps.Progress = func(progress cluster.AgentBootstrapProgress) {
			if progress.Node == target.Name && progress.Phase != "" {
				_, _ = fmt.Fprintf(stderr, "node join node=%s step=%s status=running\n", target.Name, progress.Phase)
			}
		}
		_, _ = fmt.Fprintf(stderr, "node join node=%s coordinator=%s status=started\n", target.Name, coordinator.Name)
		// Only the target and coordinator participate in joining; unrelated nodes
		// need not be reachable and must not receive lifecycle operations.
		inv.Nodes = []inventory.Node{coordinator, target}
		if _, err := runAgentNodeJoin(ctx, cluster.Request{Inventory: inv, InitNode: coordinator.Name}, target.Name, deps); err != nil {
			return fmt.Errorf("join %s: %w; inspect 'katlctl node status %s --config %s' and retry node join", target.Name, err, target.Name, opts.configPath)
		}
		if err := finishNodeJoin(ctx, opts.configPath, target, stderr); err != nil {
			return err
		}
		result = "joined"
	default:
		recovery := nodeUpgradeRecovery(status, nodeRecoveryRequirement{KubernetesConfigured: true})
		if current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy || !recovery.Ready {
			return fmt.Errorf("node %s is already configured but is not healthy (%s); inspect 'katlctl node status %s --config %s' before retrying", target.Name, firstNonEmpty(recovery.Reason, recovery.State), target.Name, opts.configPath)
		}
	}
	if opts.output == "json" {
		return json.NewEncoder(stdout).Encode(struct {
			Node   string `json:"node"`
			Result string `json:"result"`
		}{target.Name, result})
	}
	if result == "unchanged" {
		_, err = fmt.Fprintf(stdout, "%s is already joined\n", target.Name)
	} else {
		_, err = fmt.Fprintf(stdout, "%s joined Kubernetes\n", target.Name)
	}
	return err
}

func readJoinNode(ctx context.Context, configPath string, node inventory.Node) (*agentapi.NodeStatus, *agentapi.Generation, error) {
	ctx, err := managementContextForNode(ctx, configPath, node.Name)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialKatlcAgent(ctx, cluster.AgentEndpoint(node.Address, "9443"))
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", node.Name, err)
	}
	defer conn.Close()
	status, current, err := readHostState(ctx, conn.Client, node.Name)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyPlannedStatus(managementTarget{nodeName: node.Name, endpoint: cluster.AgentEndpoint(node.Address, "9443"), enrollmentID: node.EnrollmentID, machineID: node.MachineID}, status); err != nil {
		return nil, nil, err
	}
	return status, current, nil
}

func joinCoordinator(ctx context.Context, opts nodeJoinOptions, inv inventory.Inventory, stderr io.Writer) (inventory.Node, error) {
	var failures []string
	for _, node := range inv.Nodes {
		if node.Name == opts.node || node.SystemRole != inventory.RoleControlPlane || opts.coordinator != "" && node.Name != opts.coordinator {
			continue
		}
		if err := refreshConfiguredManagement(ctx, opts.configPath, "", "", stderr, node.Name); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		refreshed, err := kubeadmConfigInventory(kubeadmControlPlaneConfigOptions{configPath: opts.configPath})
		if err != nil {
			return inventory.Node{}, err
		}
		index := slices.IndexFunc(refreshed.Nodes, func(candidate inventory.Node) bool { return candidate.Name == node.Name })
		if index < 0 {
			return inventory.Node{}, fmt.Errorf("coordinator %s disappeared from --config; restore the complete ClusterConfig and retry", node.Name)
		}
		node = refreshed.Nodes[index]
		status, current, err := readJoinNode(ctx, opts.configPath, node)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		if err := validateClusterNodeLifecycle(node, status); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		recovery := nodeUpgradeRecovery(status, nodeRecoveryRequirement{KubernetesConfigured: true})
		if recovery.Ready && current.GetCommitState() == generation.CommitStateCommitted && current.GetHealthState() == generation.HealthStateHealthy {
			return node, nil
		}
		failures = append(failures, node.Name+" is not a healthy ready control plane")
	}
	return inventory.Node{}, fmt.Errorf("no ready control plane can coordinate the join; use --coordinator to select a healthy existing control plane, or 'katlctl cluster bootstrap' for a new cluster: %s", strings.Join(failures, "; "))
}

func finishNodeJoin(ctx context.Context, configPath string, node inventory.Node, progress io.Writer) error {
	status, current, err := readJoinNode(ctx, configPath, node)
	if err != nil {
		return err
	}
	ctx, err = managementContextForNode(ctx, configPath, node.Name)
	if err != nil {
		return err
	}
	endpoint := cluster.AgentEndpoint(node.Address, "9443")
	requirement := nodeRecoveryRequirement{KubernetesConfigured: true}
	if current.GetCommitState() == generation.CommitStateCommitted && current.GetHealthState() == generation.HealthStateHealthy {
		conn, _, err := waitNodeKubernetesRecovery(ctx, node.Name, endpoint, requirement, "node join node="+node.Name, progress)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if !isClusterJoinCandidate(current.GetGenerationId()) || current.GetHealthState() == generation.HealthStateUnhealthy {
		return fmt.Errorf("node %s did not reach a healthy joined generation; inspect 'katlctl node status %s --config %s' before retrying", node.Name, node.Name, configPath)
	}
	previousStart := status.GetAgentStartId()
	if previousStart == "" {
		return fmt.Errorf("node %s did not report its current start identity", node.Name)
	}
	conn, err := dialKatlcAgent(ctx, endpoint)
	if err != nil {
		return err
	}
	// An uncommitted joined generation must finish its trial boot for promotion.
	err = requestNodeReboot(ctx, conn.Client, "katlctl node join", status, current.GetGenerationId())
	_ = conn.Close()
	if err != nil {
		return fmt.Errorf("reboot joined node %s: %w", node.Name, err)
	}
	_, _ = fmt.Fprintf(progress, "node join node=%s step=reboot status=scheduled\n", node.Name)
	verified, _, err := waitNodeBootHealthWithPrefix(ctx, node.Name, endpoint, previousStart, current.GetGenerationId(), requirement, "node join node="+node.Name, progress)
	if err != nil {
		return err
	}
	_ = verified.Close()
	return nil
}

func isClusterJoinCandidate(id string) bool {
	return strings.HasPrefix(id, "bootstrap-join-") && strings.HasSuffix(id, "-candidate")
}
