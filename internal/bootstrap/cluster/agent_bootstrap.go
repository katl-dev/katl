package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/kubeconfig"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const (
	defaultAgentPort           = "9443"
	agentAPIVersion            = operation.APIVersion
	agentSubmitOperationKind   = "SubmitOperationRequest"
	agentJoinMaterialKind      = "CreateWorkerJoinMaterialRequest"
	agentBootstrapInitKind     = "bootstrap-init"
	agentExpectedGeneration0ID = "0"
)

type AgentBootstrapDependencies struct {
	Connector       AgentConnector
	Actor           string
	WatchTimeout    time.Duration
	PollInterval    time.Duration
	OperationWait   time.Duration
	BootstrapRunner BootstrapRunner
	Progress        func(AgentBootstrapProgress)
}

type AgentBootstrapProgress struct {
	Node        string
	OperationID string
	Kind        string
	Phase       string
	Terminal    bool
	Result      string
	NextAction  string
}

type AgentConnector interface {
	Connect(ctx context.Context, node inventory.PlannedNode) (AgentConnection, error)
}

type AgentConnection struct {
	Endpoint string
	Client   AgentClient
	Close    func() error
}

type AgentClient interface {
	GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest, ...grpc.CallOption) (*agentapi.NodeStatus, error)
	SubmitOperation(context.Context, *agentapi.SubmitOperationRequest, ...grpc.CallOption) (*agentapi.OperationAccepted, error)
	CreateWorkerJoinMaterial(context.Context, *agentapi.CreateWorkerJoinMaterialRequest, ...grpc.CallOption) (*agentapi.CreateWorkerJoinMaterialResponse, error)
	GetOperation(context.Context, *agentapi.GetOperationRequest, ...grpc.CallOption) (*agentapi.OperationStatus, error)
	ListOperations(context.Context, *agentapi.ListOperationsRequest, ...grpc.CallOption) (*agentapi.ListOperationsResponse, error)
	WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error)
}

type TCPAgentConnector struct {
	DefaultPort string
	DialTimeout time.Duration
}

func (c TCPAgentConnector) Connect(ctx context.Context, node inventory.PlannedNode) (AgentConnection, error) {
	if node.Access.Method != "agent" {
		return AgentConnection{}, fmt.Errorf("node %q access method %q is not supported by katlc agent transport", node.Name, node.Access.Method)
	}
	endpoint := AgentEndpoint(node.Address, valueOrDefault(c.DefaultPort, defaultAgentPort))
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialCtx := ctx
	if c.DialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, c.DialTimeout)
		defer cancel()
		opts = append(opts, grpc.WithBlock())
	}
	conn, err := grpc.DialContext(dialCtx, endpoint, opts...)
	if err != nil {
		return AgentConnection{}, err
	}
	return AgentConnection{
		Endpoint: endpoint,
		Client:   agentapi.NewKatlcAgentClient(conn),
		Close:    conn.Close,
	}, nil
}

func AgentEndpoint(address, defaultPort string) string {
	address = strings.TrimSpace(address)
	if hasPort(address) {
		return address
	}
	return net.JoinHostPort(address, valueOrDefault(defaultPort, defaultAgentPort))
}

func RunAgentBootstrap(ctx context.Context, request Request, deps AgentBootstrapDependencies) (Result, error) {
	inv := request.Inventory
	if strings.TrimSpace(request.ControlPlaneEndpoint) != "" {
		inv.ControlPlaneEndpoint = strings.TrimSpace(request.ControlPlaneEndpoint)
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{
		Inventory:       inv,
		InitNode:        request.InitNode,
		AddressOverride: request.AddressOverrides,
	})
	if err != nil {
		return Result{}, err
	}
	result := Result{Plan: plan, DryRun: request.DryRun}
	emitAgentProgress(deps, AgentBootstrapProgress{Phase: "planning"})
	result.addPhase("plan", "", "", "passed")
	bootstrapInput := mergeBootstrap(planBootstrap(plan.Bootstrap), request.Bootstrap)
	nodeManifests, err := nodeLabelManifests(plan)
	if err != nil {
		return result, err
	}
	bootstrapInput.Manifests = append(nodeManifests, bootstrapInput.Manifests...)
	bootstrap, err := prepareBootstrap(bootstrapInput)
	if err != nil {
		return result, err
	}
	if (bootstrap.enabled() || plan.ControlPlaneEndpointManaged) && deps.BootstrapRunner == nil {
		return result, errors.New("bootstrap handoff runner is required")
	}
	if err := validateAgentPlan(plan); err != nil {
		return result, err
	}
	if deps.Connector == nil {
		return result, errors.New("katlc agent connector is required")
	}
	emitAgentProgress(deps, AgentBootstrapProgress{Phase: "checking-node-readiness"})
	statuses, err := checkAgentReadiness(ctx, plan, deps.Connector)
	if err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, errors.New(inventory.Redact(err.Error()))
	}
	result.Readiness = readinessFromStatuses(plan, statuses)
	if err := inventory.Error(result.Readiness); err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, err
	}
	result.addPhase("readiness", "", "", "passed")
	if request.DryRun {
		result.addPhase("dry-run", "", "", "passed")
		return result, nil
	}
	initNode, err := findInitNode(plan)
	if err != nil {
		return result, err
	}
	status := statuses[initNode.Name]
	emitAgentProgress(deps, AgentBootstrapProgress{Node: initNode.Name, Kind: agentBootstrapInitKind, Phase: "submitting"})
	initResult, err := submitAndWaitBootstrapInit(ctx, initNode, plan, status, deps)
	if err != nil {
		result.addOperationPhase("bootstrap-init", initNode.Name, inventory.ActionInit, "failed", initResult.Operation)
		return result, fmt.Errorf("bootstrap-init operation on %s: %s", initNode.Name, inventory.Redact(err.Error()))
	}
	result.addOperationPhase("bootstrap-init", initNode.Name, inventory.ActionInit, "passed", initResult.Operation)
	stableEndpointReady := false
	if plan.ControlPlaneEndpointManaged {
		emitAgentProgress(deps, AgentBootstrapProgress{Phase: "checking-stable-endpoint"})
		endpointResult, err := verifyManagedControlPlaneEndpoint(ctx, deps.BootstrapRunner, initNode, plan, initResult.Credentials)
		if err != nil {
			result.addPhase("stable-endpoint", "", "", "failed")
			return result, fmt.Errorf("wait for managed control-plane endpoint: %s", inventory.Redact(err.Error()))
		}
		if !endpointResult.StableEndpointReady {
			result.addPhase("stable-endpoint", "", "", "failed")
			return result, errors.New("managed control-plane endpoint check completed without confirming readiness")
		}
		stableEndpointReady = true
		result.addPhase("stable-endpoint", "", "", "passed")
	}
	for _, node := range controlPlaneJoinNodes(plan) {
		emitAgentProgress(deps, AgentBootstrapProgress{Node: node.Name, Kind: "bootstrap-join-control-plane", Phase: "creating-join-material"})
		material, err := createControlPlaneJoinMaterial(ctx, initNode, node, statuses[initNode.Name], initResult.Operation.ID, initResult.Credentials, plan.ControlPlaneEndpointManaged, deps)
		if err != nil {
			result.addPhase("control-plane-join", node.Name, inventory.ActionControlPlaneJoin, "failed")
			return result, fmt.Errorf("control-plane join material for %s: %s", node.Name, inventory.Redact(err.Error()))
		}
		operationRef, err := submitAndWaitControlPlaneJoin(ctx, node, plan, statuses[node.Name], material, deps)
		if err != nil {
			result.addOperationPhase("control-plane-join", node.Name, inventory.ActionControlPlaneJoin, "failed", operationRef)
			return result, fmt.Errorf("control-plane join operation on %s: %s", node.Name, inventory.Redact(err.Error()))
		}
		result.addOperationPhase("control-plane-join", node.Name, inventory.ActionControlPlaneJoin, "passed", operationRef)
	}
	for _, node := range workerNodes(plan) {
		emitAgentProgress(deps, AgentBootstrapProgress{Node: node.Name, Kind: "bootstrap-join-worker", Phase: "creating-join-material"})
		material, err := createWorkerJoinMaterial(ctx, initNode, node, statuses[initNode.Name], initResult.Operation.ID, deps)
		if err != nil {
			result.addPhase("worker-join", node.Name, inventory.ActionWorkerJoin, "failed")
			return result, fmt.Errorf("worker join material for %s: %s", node.Name, inventory.Redact(err.Error()))
		}
		operationRef, err := submitAndWaitWorkerJoin(ctx, node, plan, statuses[node.Name], material, deps)
		if err != nil {
			result.addOperationPhase("worker-join", node.Name, inventory.ActionWorkerJoin, "failed", operationRef)
			return result, fmt.Errorf("worker join operation on %s: %s", node.Name, inventory.Redact(err.Error()))
		}
		result.addOperationPhase("worker-join", node.Name, inventory.ActionWorkerJoin, "passed", operationRef)
	}
	if bootstrap.enabled() {
		emitAgentProgress(deps, AgentBootstrapProgress{Phase: "applying-bootstrap-manifests"})
		bootstrapResult, err := deps.BootstrapRunner.RunUserBootstrap(ctx, BootstrapRequest{
			Server:         bootstrapServer(initNode, plan),
			StableEndpoint: bootstrap.StableEndpoint,
			Credentials:    initResult.Credentials,
			PreWaits:       bootstrap.preWaits(),
			Manifests:      bootstrap.Manifests,
			Waits:          bootstrap.waitsWithEndpoint(),
		})
		if err != nil {
			result.addPhase("user-bootstrap", "", "", "failed")
			return result, fmt.Errorf("user bootstrap handoff: %s", inventory.Redact(err.Error()))
		}
		result.Bootstrap = bootstrapResult
		stableEndpointReady = stableEndpointReady || bootstrapResult.StableEndpointReady
		result.addPhase("user-bootstrap", "", "", "passed")
	}
	emitAgentProgress(deps, AgentBootstrapProgress{Phase: "writing-kubeconfig"})
	kubeconfigResult, err := kubeconfig.Write(kubeconfig.Request{
		Path:      request.KubeconfigOut,
		Overwrite: request.OverwriteKubeconfig,
		Endpoint: kubeconfig.EndpointSelection{
			InitialEndpoint:      endpointForNode(initNode),
			ControlPlaneEndpoint: plan.ControlPlaneEndpoint,
			StableEndpoint:       bootstrap.StableEndpoint,
			StableEndpointReady:  stableEndpointReady,
		},
		ClusterName:              valueOrDefault(request.ClusterName, "katl"),
		ContextName:              valueOrDefault(request.ContextName, "katl"),
		UserName:                 valueOrDefault(request.UserName, "katl-admin"),
		CertificateAuthorityData: initResult.Credentials.CertificateAuthorityData,
		ClientCertificateData:    initResult.Credentials.ClientCertificateData,
		ClientKeyData:            initResult.Credentials.ClientKeyData,
	})
	if err != nil {
		result.addPhase("kubeconfig", "", "", "failed")
		return result, err
	}
	result.Kubeconfig = kubeconfigResult
	result.NextStep = kubeconfigResult.NextStep()
	result.addPhase("kubeconfig", "", "", "passed")
	return result, nil
}

// RunAgentWorkerJoin joins one fresh worker to an already initialized cluster.
// It deliberately does not run bootstrap-init or rewrite operator kubeconfig.
func RunAgentWorkerJoin(ctx context.Context, request Request, workerName string, deps AgentBootstrapDependencies) (Result, error) {
	inv := request.Inventory
	if strings.TrimSpace(request.ControlPlaneEndpoint) != "" {
		inv.ControlPlaneEndpoint = strings.TrimSpace(request.ControlPlaneEndpoint)
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: inv, InitNode: request.InitNode, AddressOverride: request.AddressOverrides})
	if err != nil {
		return Result{}, err
	}
	result := Result{Plan: plan, DryRun: request.DryRun}
	result.addPhase("plan", "", "", "passed")
	if err := validateAgentPlan(plan); err != nil {
		return result, err
	}
	if deps.Connector == nil {
		return result, errors.New("katlc agent connector is required")
	}
	statuses, err := checkAgentReadiness(ctx, plan, deps.Connector)
	if err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, errors.New(inventory.Redact(err.Error()))
	}
	result.Readiness = readinessFromStatuses(plan, statuses)
	if err := inventory.Error(result.Readiness); err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, err
	}
	result.addPhase("readiness", "", "", "passed")
	if request.DryRun {
		result.addPhase("dry-run", "", "", "passed")
		return result, nil
	}
	initNode, err := findInitNode(plan)
	if err != nil {
		return result, err
	}
	var worker inventory.PlannedNode
	for _, candidate := range workerNodes(plan) {
		if candidate.Name == strings.TrimSpace(workerName) {
			worker = candidate
			break
		}
	}
	if worker.Name == "" {
		return result, fmt.Errorf("worker %q is not a worker join target", workerName)
	}
	material, err := createWorkerJoinMaterial(ctx, initNode, worker, statuses[initNode.Name], "existing-cluster", deps)
	if err != nil {
		result.addPhase("worker-join", worker.Name, inventory.ActionWorkerJoin, "failed")
		return result, fmt.Errorf("worker join material for %s: %s", worker.Name, inventory.Redact(err.Error()))
	}
	operationRef, err := submitAndWaitWorkerJoin(ctx, worker, plan, statuses[worker.Name], material, deps)
	if err != nil {
		result.addOperationPhase("worker-join", worker.Name, inventory.ActionWorkerJoin, "failed", operationRef)
		return result, fmt.Errorf("worker join operation on %s: %s", worker.Name, inventory.Redact(err.Error()))
	}
	result.addOperationPhase("worker-join", worker.Name, inventory.ActionWorkerJoin, "passed", operationRef)
	return result, nil
}

func validateAgentPlan(plan inventory.Plan) error {
	for _, node := range plan.Nodes {
		if node.Access.Method != "agent" {
			return fmt.Errorf("node %q access method %q cannot use operation-backed bootstrap", node.Name, node.Access.Method)
		}
		switch node.Action {
		case inventory.ActionInit:
		case inventory.ActionControlPlaneJoin:
		case inventory.ActionWorkerJoin:
		default:
			return fmt.Errorf("node %q has unsupported bootstrap action %q", node.Name, node.Action)
		}
	}
	return nil
}

func checkAgentReadiness(ctx context.Context, plan inventory.Plan, connector AgentConnector) (map[string]*agentapi.NodeStatus, error) {
	statuses := make(map[string]*agentapi.NodeStatus, len(plan.Nodes))
	for _, node := range plan.Nodes {
		conn, err := connector.Connect(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("connect to katlc agent on %s: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		closeErr := closeAgent(conn)
		if err != nil {
			return nil, fmt.Errorf("get node status from %s: %w", node.Name, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close katlc agent connection to %s: %w", node.Name, closeErr)
		}
		statuses[node.Name] = status
	}
	return statuses, nil
}

func readinessFromStatuses(plan inventory.Plan, statuses map[string]*agentapi.NodeStatus) inventory.ReadinessReport {
	report := inventory.ReadinessReport{Ready: true, Nodes: make([]inventory.NodeReadiness, 0, len(plan.Nodes))}
	for _, node := range plan.Nodes {
		status := statuses[node.Name]
		nodeReport := inventory.NodeReadiness{Name: node.Name, Ready: true}
		if status == nil {
			nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "katlc-agent", Message: "node status is missing"})
		} else {
			if status.GetApiVersion() != agentAPIVersion {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "katlc-agent", Message: fmt.Sprintf("node reports API version %q", status.GetApiVersion())})
			}
			required := requiredAgentOperationKind(node.Action)
			if required != "" && !contains(status.GetSupportedOperationKinds(), required) {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "katlc-agent", Message: fmt.Sprintf("%s operation is not supported", required)})
			}
			if strings.TrimSpace(status.GetMachineId()) == "" {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "machine-id", Message: "node did not report a machine identity"})
			}
		}
		nodeReport.Ready = len(nodeReport.Diagnostics) == 0
		if !nodeReport.Ready {
			report.Ready = false
		}
		report.Nodes = append(report.Nodes, nodeReport)
	}
	return report
}

type bootstrapInitResult struct {
	Operation   operationReference
	Credentials AdminCredentials
}

type operationReference struct {
	ID string
}

type workerJoinMaterial struct {
	Ref      string
	Material *agentapi.WorkerJoinMaterial
}

func submitAndWaitBootstrapInit(ctx context.Context, node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, deps AgentBootstrapDependencies) (bootstrapInitResult, error) {
	conn, err := deps.Connector.Connect(ctx, node)
	if err != nil {
		return bootstrapInitResult{}, fmt.Errorf("connect to katlc agent: %w", err)
	}
	defer closeAgent(conn)
	req := bootstrapInitRequest(node, plan, status, deps)
	accepted, resumed, err := resumeBootstrapOperation(ctx, conn.Client, req.ClientRequestId, req.OperationKind)
	if err != nil {
		return bootstrapInitResult{}, err
	}
	if !resumed {
		accepted, err = conn.Client.SubmitOperation(ctx, req)
	} else {
		emitAgentProgress(deps, AgentBootstrapProgress{Node: node.Name, OperationID: accepted.GetOperationId(), Kind: req.OperationKind, Phase: "resuming"})
	}
	if err != nil {
		return bootstrapInitResult{}, fmt.Errorf("submit operation: %w", err)
	}
	result := bootstrapInitResult{Operation: operationReference{ID: accepted.GetOperationId()}}
	final, err := waitOperationTerminal(ctx, node.Name, conn.Client, accepted, deps)
	if err != nil {
		return result, err
	}
	if !final.GetTerminal() {
		return result, fmt.Errorf("operation %s did not reach terminal status", accepted.GetOperationId())
	}
	if final.GetResult() != "" && final.GetResult() != operation.ResultSucceeded {
		return result, fmt.Errorf("operation %s finished with result %s: %s", accepted.GetOperationId(), final.GetResult(), final.GetFailureReason())
	}
	output, err := conn.Client.GetOperation(ctx, &agentapi.GetOperationRequest{
		OperationId:           accepted.GetOperationId(),
		ExpectedRequestDigest: accepted.GetRequestDigest(),
		IncludeDiagnostics:    "bootstrap-output",
	})
	if err != nil {
		return result, fmt.Errorf("get bootstrap output for operation %s: %w", accepted.GetOperationId(), err)
	}
	credentials, err := parseAdminCredentials([]byte(output.GetAdminKubeconfig()))
	if err != nil {
		return result, err
	}
	result.Credentials = credentials
	return result, nil
}

func createControlPlaneJoinMaterial(ctx context.Context, initNode, controlPlane inventory.PlannedNode, status *agentapi.NodeStatus, initOperationID string, credentials AdminCredentials, managedEndpoint bool, deps AgentBootstrapDependencies) (workerJoinMaterial, error) {
	discovery := joinDiscoveryOverride{}
	if managedEndpoint {
		discovery.Endpoint = endpointForNode(initNode)
		discovery.CertificateAuthorityData = credentials.CertificateAuthorityData
	}
	return createJoinMaterial(ctx, initNode, controlPlane, status, initOperationID, deps, "control-plane", discovery)
}

func createWorkerJoinMaterial(ctx context.Context, initNode, worker inventory.PlannedNode, status *agentapi.NodeStatus, initOperationID string, deps AgentBootstrapDependencies) (workerJoinMaterial, error) {
	return createJoinMaterial(ctx, initNode, worker, status, initOperationID, deps, "worker", joinDiscoveryOverride{})
}

type joinDiscoveryOverride struct {
	Endpoint                 string
	CertificateAuthorityData string
}

func createJoinMaterial(ctx context.Context, initNode, joinNode inventory.PlannedNode, status *agentapi.NodeStatus, initOperationID string, deps AgentBootstrapDependencies, role string, discovery joinDiscoveryOverride) (workerJoinMaterial, error) {
	conn, err := deps.Connector.Connect(ctx, initNode)
	if err != nil {
		return workerJoinMaterial{}, fmt.Errorf("connect to katlc agent: %w", err)
	}
	defer closeAgent(conn)
	requestRef := "operation:" + strings.TrimSpace(initOperationID) + "/" + role + ":" + joinNode.Name
	response, err := conn.Client.CreateWorkerJoinMaterial(ctx, &agentapi.CreateWorkerJoinMaterialRequest{
		ApiVersion:        agentAPIVersion,
		Kind:              agentJoinMaterialKind,
		Actor:             valueOrDefault(deps.Actor, "katlctl cluster bootstrap"),
		ExpectedMachineId: strings.TrimSpace(status.GetMachineId()),
		RequestRef:        requestRef,
	})
	if err != nil {
		return workerJoinMaterial{}, fmt.Errorf("create %s join material: %w", role, err)
	}
	material := response.GetWorkerJoinMaterial()
	if material == nil {
		return workerJoinMaterial{}, fmt.Errorf("agent did not return %s join material", role)
	}
	if len(material.GetJoinArgv()) == 0 {
		return workerJoinMaterial{}, fmt.Errorf("agent returned %s join material without argv", role)
	}
	if strings.TrimSpace(material.GetExpiresAt()) == "" {
		return workerJoinMaterial{}, fmt.Errorf("agent returned %s join material without expiry", role)
	}
	if strings.TrimSpace(discovery.Endpoint) != "" {
		material, err = joinMaterialWithDiscoveryEndpoint(material, discovery.Endpoint, discovery.CertificateAuthorityData)
		if err != nil {
			return workerJoinMaterial{}, fmt.Errorf("prepare %s join material: %w", role, err)
		}
	}
	ref := strings.TrimSpace(response.GetMaterialRef())
	if ref == "" {
		ref = requestRef
	}
	return workerJoinMaterial{Ref: ref, Material: material}, nil
}

func joinMaterialWithDiscoveryEndpoint(material *agentapi.WorkerJoinMaterial, endpoint, certificateAuthorityData string) (*agentapi.WorkerJoinMaterial, error) {
	argv := append([]string(nil), material.GetJoinArgv()...)
	if len(argv) < 3 || argv[0] != "kubeadm" || argv[1] != "join" {
		return nil, errors.New("join material must start with kubeadm join and an API endpoint")
	}
	endpoint = strings.TrimSpace(endpoint)
	if err := validateEndpointLike(endpoint); err != nil {
		return nil, fmt.Errorf("init-node discovery endpoint: %w", err)
	}
	token := strings.TrimSpace(flagValue(argv, "--token"))
	if token == "" {
		return nil, errors.New("join material is missing bootstrap token")
	}
	certificateAuthorityData = strings.TrimSpace(certificateAuthorityData)
	if certificateAuthorityData == "" {
		return nil, errors.New("join discovery is missing certificate authority data")
	}
	argv[2] = endpoint
	discoveryKubeconfig, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []any{map[string]any{
			"name": "katl-discovery",
			"cluster": map[string]any{
				"certificate-authority-data": certificateAuthorityData,
				"server":                     "https://" + endpoint,
			},
		}},
		"contexts": []any{map[string]any{
			"name": "katl-discovery",
			"context": map[string]any{
				"cluster": "katl-discovery",
				"user":    "katl-bootstrap",
			},
		}},
		"current-context": "katl-discovery",
		"users": []any{map[string]any{
			"name": "katl-bootstrap",
			"user": map[string]any{"token": token},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("render join discovery kubeconfig: %w", err)
	}
	return &agentapi.WorkerJoinMaterial{
		JoinArgv:            argv,
		ExpiresAt:           material.GetExpiresAt(),
		DiscoveryKubeconfig: discoveryKubeconfig,
	}, nil
}

func submitAndWaitControlPlaneJoin(ctx context.Context, node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, material workerJoinMaterial, deps AgentBootstrapDependencies) (operationReference, error) {
	return submitAndWaitJoin(ctx, node, plan, status, material, deps, "bootstrap-join-control-plane")
}

func submitAndWaitWorkerJoin(ctx context.Context, node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, material workerJoinMaterial, deps AgentBootstrapDependencies) (operationReference, error) {
	return submitAndWaitJoin(ctx, node, plan, status, material, deps, "bootstrap-join-worker")
}

func submitAndWaitJoin(ctx context.Context, node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, material workerJoinMaterial, deps AgentBootstrapDependencies, kind string) (operationReference, error) {
	conn, err := deps.Connector.Connect(ctx, node)
	if err != nil {
		return operationReference{}, fmt.Errorf("connect to katlc agent: %w", err)
	}
	defer closeAgent(conn)
	req := bootstrapOperationRequest(node, plan, status, deps, kind)
	req.Bootstrap.JoinMaterialRef = strings.TrimSpace(material.Ref)
	req.Bootstrap.WorkerJoinMaterial = material.Material
	accepted, resumed, err := resumeBootstrapOperation(ctx, conn.Client, req.ClientRequestId, req.OperationKind)
	if err != nil {
		return operationReference{}, err
	}
	if !resumed {
		accepted, err = conn.Client.SubmitOperation(ctx, req)
	} else {
		emitAgentProgress(deps, AgentBootstrapProgress{Node: node.Name, OperationID: accepted.GetOperationId(), Kind: req.OperationKind, Phase: "resuming"})
	}
	if err != nil {
		return operationReference{}, fmt.Errorf("submit operation: %w", err)
	}
	operationRef := operationReference{ID: accepted.GetOperationId()}
	final, err := waitOperationTerminal(ctx, node.Name, conn.Client, accepted, deps)
	if err != nil {
		return operationRef, err
	}
	if !final.GetTerminal() {
		return operationRef, fmt.Errorf("operation %s did not reach terminal status", accepted.GetOperationId())
	}
	if final.GetResult() != "" && final.GetResult() != operation.ResultSucceeded {
		return operationRef, fmt.Errorf("operation %s finished with result %s: %s", accepted.GetOperationId(), final.GetResult(), final.GetFailureReason())
	}
	return operationRef, nil
}

func bootstrapInitRequest(node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, deps AgentBootstrapDependencies) *agentapi.SubmitOperationRequest {
	return bootstrapOperationRequest(node, plan, status, deps, agentBootstrapInitKind)
}

func bootstrapOperationRequest(node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, deps AgentBootstrapDependencies, kind string) *agentapi.SubmitOperationRequest {
	return &agentapi.SubmitOperationRequest{
		ApiVersion:                  agentAPIVersion,
		Kind:                        agentSubmitOperationKind,
		ClientRequestId:             clientRequestID(node, plan, kind),
		OperationKind:               kind,
		Actor:                       valueOrDefault(deps.Actor, "katlctl cluster bootstrap"),
		ExpectedMachineId:           strings.TrimSpace(status.GetMachineId()),
		ExpectedCurrentGenerationId: agentExpectedGeneration0ID,
		DryRun:                      false,
		Bootstrap: &agentapi.BootstrapOperationRequest{
			InventoryNodeName:        node.Name,
			SystemRole:               string(node.SystemRole),
			KubernetesPayloadVersion: node.KubernetesVersion,
			KubernetesBundleSource:   plan.KubernetesBundleSource,
			KubernetesBundleRef:      plan.KubernetesBundleRef,
			BootstrapProfileRef:      bootstrapProfileRef(node),
			ControlPlaneEndpoint:     plan.ControlPlaneEndpoint,
		},
	}
}

func requiredAgentOperationKind(action inventory.BootstrapAction) string {
	switch action {
	case inventory.ActionInit:
		return "bootstrap-init"
	case inventory.ActionWorkerJoin:
		return "bootstrap-join-worker"
	case inventory.ActionControlPlaneJoin:
		return "bootstrap-join-control-plane"
	default:
		return ""
	}
}

func waitOperationTerminal(ctx context.Context, node string, client AgentClient, accepted *agentapi.OperationAccepted, deps AgentBootstrapDependencies) (*agentapi.OperationStatus, error) {
	if accepted == nil || strings.TrimSpace(accepted.GetOperationId()) == "" {
		return nil, fmt.Errorf("agent did not return an operation id")
	}
	waitCtx := ctx
	if deps.OperationWait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, deps.OperationWait)
		defer cancel()
	}
	status := accepted.GetInitialStatus()
	seq := int32(0)
	lastProgress := ""
	emitStatus := func(status *agentapi.OperationStatus) {
		if status == nil {
			return
		}
		progress := strings.Join([]string{status.GetOperationId(), status.GetOperationKind(), status.GetPhase(), fmt.Sprint(status.GetTerminal()), status.GetResult()}, "\x00")
		if progress == lastProgress {
			return
		}
		lastProgress = progress
		emitAgentProgress(deps, AgentBootstrapProgress{
			Node:        node,
			OperationID: status.GetOperationId(),
			Kind:        status.GetOperationKind(),
			Phase:       status.GetPhase(),
			Terminal:    status.GetTerminal(),
			Result:      status.GetResult(),
			NextAction:  status.GetNextAction(),
		})
	}
	if status != nil {
		emitStatus(status)
		seq = status.GetLatestJournalSeq()
		if status.GetTerminal() {
			return status, nil
		}
	}
	for {
		stream, err := client.WatchOperation(waitCtx, &agentapi.WatchOperationRequest{
			OperationId:           accepted.GetOperationId(),
			ExpectedRequestDigest: accepted.GetRequestDigest(),
			AfterJournalSeq:       seq,
			WatchTimeout:          durationString(valueOrDefaultDuration(deps.WatchTimeout, 5*time.Second)),
		})
		if err != nil {
			return nil, fmt.Errorf("watch operation %s: %w", accepted.GetOperationId(), err)
		}
		for {
			event, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("watch operation %s: %w", accepted.GetOperationId(), err)
			}
			seq = event.GetJournalSeq()
			if event.GetStatus() != nil {
				status = event.GetStatus()
				emitStatus(status)
			}
			if event.GetTerminal() {
				if status == nil {
					break
				}
				return status, nil
			}
		}
		polled, err := client.GetOperation(waitCtx, &agentapi.GetOperationRequest{
			OperationId:           accepted.GetOperationId(),
			ExpectedRequestDigest: accepted.GetRequestDigest(),
		})
		if err != nil {
			return nil, fmt.Errorf("get operation %s: %w", accepted.GetOperationId(), err)
		}
		if polled.GetTerminal() {
			return polled, nil
		}
		status = polled
		emitStatus(status)
		seq = polled.GetLatestJournalSeq()
		select {
		case <-waitCtx.Done():
			return status, waitCtx.Err()
		case <-time.After(valueOrDefaultDuration(deps.PollInterval, 250*time.Millisecond)):
		}
	}
}

func emitAgentProgress(deps AgentBootstrapDependencies, progress AgentBootstrapProgress) {
	if deps.Progress != nil {
		deps.Progress(progress)
	}
}

func closeAgent(conn AgentConnection) error {
	if conn.Close == nil {
		return nil
	}
	return conn.Close()
}

func clientRequestID(node inventory.PlannedNode, plan inventory.Plan, kind string) string {
	identity := strings.Join([]string{
		node.Name,
		kind,
		string(node.SystemRole),
		node.KubernetesVersion,
		node.KubeadmConfig.Ref,
		node.KubeadmConfig.Path,
		plan.ControlPlaneEndpoint,
		plan.KubernetesBundleSource,
		plan.KubernetesBundleRef,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return "katlctl-" + node.Name + "-" + hex.EncodeToString(sum[:])[:12]
}

func resumeBootstrapOperation(ctx context.Context, client AgentClient, clientRequestID, kind string) (*agentapi.OperationAccepted, bool, error) {
	response, err := client.ListOperations(ctx, &agentapi.ListOperationsRequest{Limit: 100})
	if err != nil {
		return nil, false, fmt.Errorf("list operations before %s: %w", kind, err)
	}
	for _, status := range response.GetOperations() {
		if status.GetClientRequestId() != clientRequestID || status.GetOperationKind() != kind {
			continue
		}
		return &agentapi.OperationAccepted{
			OperationId:   status.GetOperationId(),
			OperationKind: status.GetOperationKind(),
			RequestDigest: status.GetRequestDigest(),
			InitialStatus: status,
		}, true, nil
	}
	return nil, false, nil
}

func bootstrapProfileRef(node inventory.PlannedNode) string {
	if strings.TrimSpace(node.KubeadmConfig.Ref) != "" {
		return strings.TrimSpace(node.KubeadmConfig.Ref)
	}
	parts := strings.Split(strings.Trim(strings.TrimSpace(node.KubeadmConfig.Path), "/"), "/")
	if len(parts) >= 4 && parts[0] == "etc" && parts[1] == "katl" && parts[2] == "kubeadm" {
		return parts[3]
	}
	return string(node.SystemRole)
}

func valueOrDefaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func durationString(value time.Duration) string {
	if value <= 0 {
		return ""
	}
	return value.String()
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
