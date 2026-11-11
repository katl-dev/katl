package cluster

import (
	"context"
	"fmt"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/externalworker"
)

// RunAgentExternalWorkerJoin enrolls a worker without contacting a Katl control
// plane or modifying the external cluster's configuration.
func RunAgentExternalWorkerJoin(ctx context.Context, request Request, workerName string, bootstrap []byte, deps AgentBootstrapDependencies) (Result, error) {
	if !request.Inventory.ExternalControlPlane {
		return Result{}, fmt.Errorf("external worker join requires an external control plane")
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: request.Inventory})
	if err != nil {
		return Result{}, err
	}
	var target inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.Name == workerName {
			target = node
			break
		}
	}
	if target.Name == "" {
		return Result{}, fmt.Errorf("worker %q is not in the configuration", workerName)
	}
	plan.Nodes = []inventory.PlannedNode{target}
	result := Result{Plan: plan}
	if deps.Connector == nil {
		return result, fmt.Errorf("katlc agent connector is required")
	}
	if len(bootstrap) > 0 {
		if _, err := externalworker.Bootstrap(bootstrap, plan.ControlPlaneEndpoint); err != nil {
			return result, err
		}
	}
	statuses, err := checkAgentReadiness(ctx, plan, deps.Connector)
	if err != nil {
		return result, err
	}
	result.Readiness = readinessFromStatuses(plan, statuses)
	if err := inventory.Error(result.Readiness); err != nil {
		return result, err
	}
	bindBootstrapPlan(&plan, statuses)
	ref, err := submitAndWaitWorkerJoin(ctx, plan.Nodes[0], plan, statuses[workerName], workerJoinMaterial{Bootstrap: bootstrap}, deps)
	if err != nil {
		return result, fmt.Errorf("external worker join on %s: %s", workerName, inventory.Redact(err.Error()))
	}
	result.addOperationPhase("worker-join", workerName, inventory.ActionWorkerJoin, "passed", ref)
	return result, nil
}
