package main

import (
	"bytes"
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestClusterApplySelectsConfiguredTrust(t *testing.T) {
	configPath := writeClusterConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	savedPath, err := workstation.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := workstation.Load(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientCredentials, err := managementidentity.Client(identity, now)
	if err != nil {
		t.Fatal(err)
	}
	saved.Clusters[0].Management = &clientCredentials
	saved.Clusters[0].Nodes[0].ManagementEndpoint = listener.Addr().String()
	otherIdentity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "old-lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	otherCredentials, err := managementidentity.Client(otherIdentity, now)
	if err != nil {
		t.Fatal(err)
	}
	saved.Clusters = append(saved.Clusters, workstation.Cluster{Name: "old-lab", Nodes: saved.Clusters[0].Nodes, Management: &otherCredentials})
	saved.Contexts = append(saved.Contexts, workstation.Context{Name: "old", Cluster: "old-lab"})
	saved.CurrentContext = "old"
	if err := workstation.Save(savedPath, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := managementDialForEndpoint(listener.Addr().String()); err == nil {
		t.Fatal("unscoped duplicate address was accepted")
	}

	serverTLS, err := transport.ServerTLSConfigForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", ChangedDomains: []string{configapply.DomainKubeadmConfig}},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "apply-1"},
		operationStatus: &agentapi.OperationStatus{OperationId: "apply-1", Terminal: true, Result: operation.ResultSucceeded},
		generation:      &agentapi.Generation{GenerationId: "generation-1", CommitState: "committed", HealthState: "healthy", ConfigApply: &agentapi.ConfigApplyStatus{SelectedKubeadmConfigName: "control-plane"}, Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}},
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentapi.RegisterKatlcAgentServer(server, &clusterApplyIdentityServer{client: client})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "127.0.0.1:9443" {
			t.Fatalf("unexpected node endpoint %q", endpoint)
		}
		// Route the fixture address to a local server; identity selection and TLS
		// authentication use the real client path.
		return dialKatlcAgentTCP(ctx, listener.Addr().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"cluster", "apply", "--config", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr.String())
	}
	connector := managementAgentConnector("lab")
	connection, err := connector.Connect(ctx, inventory.PlannedNode{Name: "cp-1", Address: listener.Addr().String(), Access: inventory.Access{Method: "agent"}})
	if err != nil {
		t.Fatalf("join connection: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{}); err != nil {
		t.Fatalf("join authentication: %v", err)
	}

	var applied []string
	for _, request := range client.submitRequests {
		if request.DryRun {
			continue
		}
		applied = append(applied, request.OperationKind)
		if request.KubeadmControlPlaneConfig != nil {
			applied = append(applied, request.KubeadmControlPlaneConfig.SupportedFieldDelta...)
		}
	}
	for _, required := range []string{"generation-apply", kubeadmConfigComponentControlPlane, kubeadmConfigComponentKubelet} {
		if !slices.Contains(applied, required) {
			t.Fatalf("authenticated apply did not reach %s: %v", required, applied)
		}
	}
}

type clusterApplyIdentityServer struct {
	agentapi.UnimplementedKatlcAgentServer
	client *fakeKatlcAgentClient
}

func (s *clusterApplyIdentityServer) GetNodeStatus(ctx context.Context, req *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	return s.client.GetNodeStatus(ctx, req)
}

func (s *clusterApplyIdentityServer) ValidateConfig(ctx context.Context, req *agentapi.ValidateConfigRequest) (*agentapi.ConfigValidationResult, error) {
	return s.client.ValidateConfig(ctx, req)
}

func (s *clusterApplyIdentityServer) GetGeneration(ctx context.Context, req *agentapi.GetGenerationRequest) (*agentapi.Generation, error) {
	return s.client.GetGeneration(ctx, req)
}

func (s *clusterApplyIdentityServer) SubmitOperation(ctx context.Context, req *agentapi.SubmitOperationRequest) (*agentapi.OperationAccepted, error) {
	return s.client.SubmitOperation(ctx, req)
}

func (s *clusterApplyIdentityServer) GetOperation(ctx context.Context, req *agentapi.GetOperationRequest) (*agentapi.OperationStatus, error) {
	return s.client.GetOperation(ctx, req)
}
