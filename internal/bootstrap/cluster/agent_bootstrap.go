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

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	defaultAgentPort           = "9443"
	agentAPIVersion            = operation.APIVersion
	agentSubmitOperationKind   = "SubmitOperationRequest"
	agentBootstrapInitKind     = "bootstrap-init"
	agentExpectedGeneration0ID = "0"
)

var ErrAgentKubeconfigUnsupported = errors.New("operation-backed kubeconfig export requires katlc agent API support")

type AgentBootstrapDependencies struct {
	Connector     AgentConnector
	Actor         string
	Now           func() time.Time
	WatchTimeout  time.Duration
	PollInterval  time.Duration
	OperationWait time.Duration
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
	GetOperation(context.Context, *agentapi.GetOperationRequest, ...grpc.CallOption) (*agentapi.OperationStatus, error)
	WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error)
}

type TCPAgentConnector struct {
	AuthToken   string
	DefaultPort string
	DialTimeout time.Duration
}

func (c TCPAgentConnector) Connect(ctx context.Context, node inventory.PlannedNode) (AgentConnection, error) {
	if node.Access.Method != "agent" {
		return AgentConnection{}, fmt.Errorf("node %q access method %q is not supported by katlc agent transport", node.Name, node.Access.Method)
	}
	endpoint := AgentEndpoint(node.Address, valueOrDefault(c.DefaultPort, defaultAgentPort))
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if strings.TrimSpace(c.AuthToken) != "" {
		token := strings.TrimSpace(c.AuthToken)
		opts = append(opts,
			grpc.WithUnaryInterceptor(bearerUnaryInterceptor(token)),
			grpc.WithStreamInterceptor(bearerStreamInterceptor(token)),
		)
	}
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

func bearerUnaryInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
	}
}

func bearerStreamInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), desc, cc, method, opts...)
	}
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
	result.addPhase("plan", "", "", "passed")
	bootstrap, err := prepareBootstrap(mergeBootstrap(planBootstrap(plan.Bootstrap), request.Bootstrap))
	if err != nil {
		return result, err
	}
	if bootstrap.enabled() {
		return result, fmt.Errorf("operation-backed user bootstrap is not supported until katlc exposes bootstrap kubeconfig output")
	}
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
	status := statuses[initNode.Name]
	accepted, final, err := submitAndWaitBootstrapInit(ctx, initNode, plan, status, deps)
	if err != nil {
		result.addPhase("bootstrap-init", initNode.Name, inventory.ActionInit, "failed")
		return result, fmt.Errorf("bootstrap-init operation on %s: %s", initNode.Name, inventory.Redact(err.Error()))
	}
	_ = accepted
	_ = final
	result.addPhase("bootstrap-init", initNode.Name, inventory.ActionInit, "passed")
	result.NextStep = "katlc agent accepted bootstrap-init; kubeconfig export is pending agent API support"
	result.addPhase("kubeconfig", "", "", "failed")
	return result, ErrAgentKubeconfigUnsupported
}

func validateAgentPlan(plan inventory.Plan) error {
	for _, node := range plan.Nodes {
		if node.Access.Method != "agent" {
			return fmt.Errorf("node %q access method %q cannot use operation-backed bootstrap", node.Name, node.Access.Method)
		}
		switch node.Action {
		case inventory.ActionInit:
		case inventory.ActionControlPlaneJoin:
			return fmt.Errorf("node %q requires %s, which is not supported until katlc exposes join material over the agent API", node.Name, node.Action)
		case inventory.ActionWorkerJoin:
			return fmt.Errorf("node %q requires %s, which is not supported until katlc exposes join material over the agent API", node.Name, node.Action)
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
			if !contains(status.GetSupportedOperationKinds(), agentBootstrapInitKind) {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "katlc-agent", Message: "bootstrap-init operation is not supported"})
			}
			if status.GetOperationLockHeld() {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, inventory.Diagnostic{Field: "katlc-agent", Message: fmt.Sprintf("operation lock is held by %s", strings.Join(status.GetActiveOperationIds(), ","))})
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

func submitAndWaitBootstrapInit(ctx context.Context, node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, deps AgentBootstrapDependencies) (*agentapi.OperationAccepted, *agentapi.OperationStatus, error) {
	conn, err := deps.Connector.Connect(ctx, node)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to katlc agent: %w", err)
	}
	defer closeAgent(conn)
	req := bootstrapInitRequest(node, plan, status, deps)
	accepted, err := conn.Client.SubmitOperation(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("submit operation: %w", err)
	}
	final, err := waitOperationTerminal(ctx, conn.Client, accepted, deps)
	if err != nil {
		return accepted, nil, err
	}
	if !final.GetTerminal() {
		return accepted, final, fmt.Errorf("operation %s did not reach terminal status", accepted.GetOperationId())
	}
	if final.GetResult() != "" && final.GetResult() != operation.ResultSucceeded {
		return accepted, final, fmt.Errorf("operation %s finished with result %s: %s", accepted.GetOperationId(), final.GetResult(), final.GetFailureReason())
	}
	return accepted, final, nil
}

func bootstrapInitRequest(node inventory.PlannedNode, plan inventory.Plan, status *agentapi.NodeStatus, deps AgentBootstrapDependencies) *agentapi.SubmitOperationRequest {
	return &agentapi.SubmitOperationRequest{
		ApiVersion:                  agentAPIVersion,
		Kind:                        agentSubmitOperationKind,
		ClientRequestId:             clientRequestID(node, deps.now()),
		OperationKind:               agentBootstrapInitKind,
		Actor:                       valueOrDefault(deps.Actor, "katlctl cluster bootstrap"),
		ExpectedMachineId:           strings.TrimSpace(status.GetMachineId()),
		ExpectedCurrentGenerationId: agentExpectedGeneration0ID,
		DryRun:                      false,
		Bootstrap: &agentapi.BootstrapOperationRequest{
			InventoryNodeName:        node.Name,
			SystemRole:               string(node.SystemRole),
			KubernetesPayloadVersion: node.KubernetesVersion,
			BootstrapProfileRef:      bootstrapProfileRef(node),
			ControlPlaneEndpoint:     plan.ControlPlaneEndpoint,
		},
	}
}

func waitOperationTerminal(ctx context.Context, client AgentClient, accepted *agentapi.OperationAccepted, deps AgentBootstrapDependencies) (*agentapi.OperationStatus, error) {
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
	if status != nil {
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
		seq = polled.GetLatestJournalSeq()
		select {
		case <-waitCtx.Done():
			return status, waitCtx.Err()
		case <-time.After(valueOrDefaultDuration(deps.PollInterval, 250*time.Millisecond)):
		}
	}
}

func closeAgent(conn AgentConnection) error {
	if conn.Close == nil {
		return nil
	}
	return conn.Close()
}

func clientRequestID(node inventory.PlannedNode, now time.Time) string {
	sum := sha256.Sum256([]byte(node.Name + "\x00" + node.Address + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return "katlctl-" + node.Name + "-" + hex.EncodeToString(sum[:])[:12]
}

func (deps AgentBootstrapDependencies) now() time.Time {
	if deps.Now != nil {
		return deps.Now().UTC()
	}
	return time.Now().UTC()
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
