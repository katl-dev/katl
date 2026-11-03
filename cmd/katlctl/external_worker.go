package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configbundle"
)

var runAgentExternalWorkerJoin = cluster.RunAgentExternalWorkerJoin

func runExternalNodeJoin(ctx context.Context, opts nodeJoinOptions, inv inventory.Inventory, target inventory.Node, stdout, stderr io.Writer) error {
	if opts.coordinator != "" {
		return fmt.Errorf("--coordinator is not used for an external control plane; provide --bootstrap-kubeconfig")
	}
	status, current, err := readJoinNode(ctx, opts.configPath, target)
	if err != nil {
		return err
	}
	if err := validateClusterNodeLifecycle(target, status); err != nil {
		return err
	}
	if status.GetKubernetes().GetExternalEndpoint() != inv.ControlPlaneEndpoint {
		return fmt.Errorf("node %s is not provisioned for external endpoint %s; inspect its installed configuration before joining", target.Name, inv.ControlPlaneEndpoint)
	}
	result := "unchanged"
	if isClusterJoinCandidate(current.GetGenerationId()) && status.GetKubernetes().GetState() == "ready" && (current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy) {
		if err := finishNodeJoin(ctx, opts.configPath, target, stderr); err != nil {
			return err
		}
		result = "joined"
	} else if status.GetKubernetes().GetState() != "ready" || current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy {
		if !isClusterJoinCandidate(current.GetGenerationId()) && (current.GetCommitState() != generation.CommitStateCommitted || current.GetHealthState() != generation.HealthStateHealthy) {
			return fmt.Errorf("node %s must have a healthy committed generation before joining; inspect 'katlctl node status %s --config %s'", target.Name, target.Name, opts.configPath)
		}
		if opts.bootstrapKubeconfig == "" {
			return fmt.Errorf("external worker join requires --bootstrap-kubeconfig FILE containing a CA-pinned, restricted bootstrap token; do not use an administrator kubeconfig")
		}
		if next := status.GetBootTargetGenerationId(); next != "" && next != status.GetCurrentGenerationId() {
			return fmt.Errorf("node %s has configuration staged for next boot; reboot it before joining", target.Name)
		}
		bootstrap, err := os.ReadFile(opts.bootstrapKubeconfig)
		if err != nil {
			return fmt.Errorf("read bootstrap kubeconfig: %w", err)
		}
		configuration, err := loadNodeConfiguration(ctx, opts.configPath, configbundle.PlanningInputs{Nodes: []string{target.Name}})
		if err != nil {
			return err
		}
		inv = configuration.Inventory
		inv.Nodes, err = selectConfigNodes(inv.Nodes, []string{target.Name})
		if err != nil {
			return err
		}
		inv.Nodes[0].Address, inv.Nodes[0].EnrollmentID, inv.Nodes[0].MachineID = target.Address, target.EnrollmentID, target.MachineID
		ctx, err = managementContextForNode(ctx, opts.configPath, target.Name)
		if err != nil {
			return err
		}
		deps := agentBootstrapDependencies(configuration.ClusterName)
		deps.Actor = "katlctl node join"
		deps.Progress = func(progress cluster.AgentBootstrapProgress) {
			if progress.Phase != "" {
				phase := progress.Phase
				switch phase {
				case "start-external-kubelet":
					phase = "requesting-client-certificate"
				case "post-kubeadm-health":
					phase = "waiting-for-node-readiness"
				case "configure-local-api-access":
					phase = "resuming-enrollment"
				}
				_, _ = fmt.Fprintf(stderr, "node join node=%s step=%s status=running\n", target.Name, phase)
			}
		}
		_, _ = fmt.Fprintf(stderr, "node join node=%s control-plane=external status=started\nWaiting for kubelet certificate approval; inspect 'kubectl get csr' on the external cluster if this step remains pending.\n", target.Name)
		if _, err := runAgentExternalWorkerJoin(ctx, cluster.Request{Inventory: inv, ClusterName: configuration.ClusterName}, target.Name, bootstrap, deps); err != nil {
			return fmt.Errorf("join %s: %w; inspect 'katlctl node status %s --config %s' and 'kubectl get csr', then retry the same join", target.Name, err, target.Name, opts.configPath)
		}
		if err := finishNodeJoin(ctx, opts.configPath, target, stderr); err != nil {
			return err
		}
		result = "joined"
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
