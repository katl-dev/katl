package cluster

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nettest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type listeningAgent struct {
	agentapi.UnimplementedKatlcAgentServer
	status *agentapi.NodeStatus
}

func (a listeningAgent) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	return a.status, nil
}

// listenOnFirstWrite starts serving once the first retry notice is written, as
// a restarting katlc-agent begins listening after the first refused attempt.
type listenOnFirstWrite struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	listen func()
}

func (w *listenOnFirstWrite) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer.Len() == 0 {
		w.listen()
	}
	return w.buffer.Write(p)
}

func (w *listenOnFirstWrite) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

// Cluster bootstrap reaches a node whose agent is restarting, as katlctl's
// other management commands do, instead of failing the readiness preflight.
func TestRunAgentBootstrapWaitsForRestartingAgent(t *testing.T) {
	port := nettest.ReservePort(t)
	status := readyAgentStatus("machine-cp-1")
	status.InventoryNodeName = "cp-1"
	status.EnrollmentId = "install-1"
	server := grpc.NewServer()
	agentapi.RegisterKatlcAgentServer(server, listeningAgent{status: status})
	t.Cleanup(server.Stop)
	progress := &listenOnFirstWrite{listen: func() {
		if listener, err := port.Listen(); err == nil {
			go func() { _ = server.Serve(listener) }()
		}
	}}
	ctx := transport.WithManagementProgress(context.Background(), progress)

	_, err := RunAgentBootstrap(ctx, Request{
		Inventory:        validSingleNodeInventory(t),
		AddressOverrides: map[string]inventory.ManagementAddress{"cp-1": mustManagementAddress(t, port.Endpoint)},
		DryRun:           true,
	}, AgentBootstrapDependencies{Connector: TCPAgentConnector{Credentials: &managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}}})

	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if !strings.Contains(progress.String(), "Waiting for node cp-1 management endpoint "+port.Endpoint) {
		t.Fatalf("no progress while waiting:\n%s", progress.String())
	}
}

// Cluster bootstrap explains a trusted-network client reaching an mTLS node
// as katlctl's other management commands do.
func TestTCPAgentConnectorExplainsMutualTLSNode(t *testing.T) {
	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, _, err := transport.ServerTLSConfigForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	t.Cleanup(server.Stop)
	go func() { _ = server.Serve(listener) }()
	connector := TCPAgentConnector{Credentials: &managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := connector.Connect(ctx, inventory.PlannedNode{Name: "cp-1", Address: mustManagementAddress(t, listener.Addr().String()), Access: inventory.Access{Method: "agent"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

	if err == nil || !strings.Contains(err.Error(), "likely serves mTLS") || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("error = %v", err)
	}
}
