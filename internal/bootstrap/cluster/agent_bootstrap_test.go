package cluster

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
)

func TestRunAgentBootstrapDryRunContactsAgentAndPropagatesOverride(t *testing.T) {
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1": {status: readyAgentStatus("machine-cp-1")},
	})
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:            validSingleNodeInventory(),
		AddressOverrides:     map[string]string{"cp-1": "cp-1.override.test"},
		ControlPlaneEndpoint: "api.override.test:6443",
		DryRun:               true,
	}, AgentBootstrapDependencies{Connector: connector})
	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if got := connector.connected; !reflect.DeepEqual(got, []string{"cp-1@cp-1.override.test"}) {
		t.Fatalf("connected nodes = %#v", got)
	}
	if len(connector.clients["cp-1"].submitRequests) != 0 {
		t.Fatalf("SubmitOperation requests = %#v, want none", connector.clients["cp-1"].submitRequests)
	}
	if result.Plan.AddressOverrides[0].Address != "cp-1.override.test" {
		t.Fatalf("address overrides = %#v", result.Plan.AddressOverrides)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "dry-run"}) {
		t.Fatalf("phases = %#v", got)
	}
}

func TestRunAgentBootstrapSubmitsControlPlaneJoin(t *testing.T) {
	inv := validSingleNodeInventory()
	inv.ControlPlaneEndpoint = "api.katl.test:6443"
	inv.Nodes = append(inv.Nodes, inventory.Node{
		Name:              "cp-2",
		Address:           "10.0.0.12",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent", CredentialRef: "agent/cp-2"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	cpClient := &fakeAgentClient{
		status: readyAgentStatusWithKinds("machine-cp-1", "bootstrap-init"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			RequestDigest: "digest-init",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
		},
		getStatus: &agentapi.OperationStatus{
			OperationId:     "bootstrap-init-1",
			Terminal:        true,
			Result:          "succeeded",
			AdminKubeconfig: adminKubeconfig(),
		},
		createMaterial: &agentapi.CreateWorkerJoinMaterialResponse{
			MaterialRef: "operation:bootstrap-init-1/control-plane:cp-2",
			WorkerJoinMaterial: &agentapi.WorkerJoinMaterial{
				JoinArgv: []string{
					"kubeadm",
					"join",
					"api.katl.test:6443",
					"--token",
					"abcdef.0123456789abcdef",
					"--discovery-token-ca-cert-hash",
					"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					"--control-plane",
					"--certificate-key",
					strings.Repeat("a", 64),
				},
				ExpiresAt: "2026-06-16T13:00:00Z",
			},
		},
	}
	cp2Client := &fakeAgentClient{
		status: readyAgentStatusWithKinds("machine-cp-2", "bootstrap-join-control-plane"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-join-control-plane-1",
			RequestDigest: "digest-cp-join",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-join-control-plane-1", Terminal: true, Result: "succeeded"},
		},
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1": cpClient,
		"cp-2": cp2Client,
	})
	out := filepath.Join(t.TempDir(), "operator.conf")
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:           inv,
		InitNode:            "cp-1",
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
	}, AgentBootstrapDependencies{Connector: connector, Actor: "test-actor"})
	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "bootstrap-init", "control-plane-join", "kubeconfig"}) {
		t.Fatalf("phases = %#v", got)
	}
	if result.Phases[2].OperationID != "bootstrap-init-1" || result.Phases[2].RequestDigest != "digest-init" || result.Phases[3].OperationID != "bootstrap-join-control-plane-1" || result.Phases[3].RequestDigest != "digest-cp-join" {
		t.Fatalf("operation phases = %#v", result.Phases)
	}
	if len(cpClient.createMaterialRequests) != 1 {
		t.Fatalf("CreateWorkerJoinMaterial requests = %d, want 1", len(cpClient.createMaterialRequests))
	}
	if got := cpClient.createMaterialRequests[0].RequestRef; got != "operation:bootstrap-init-1/control-plane:cp-2" {
		t.Fatalf("join material request ref = %q", got)
	}
	if len(cp2Client.submitRequests) != 1 {
		t.Fatalf("control-plane SubmitOperation requests = %d, want 1", len(cp2Client.submitRequests))
	}
	cp2Req := cp2Client.submitRequests[0]
	if cp2Req.OperationKind != "bootstrap-join-control-plane" || cp2Req.ExpectedMachineId != "machine-cp-2" {
		t.Fatalf("control-plane submit request = %#v", cp2Req)
	}
	if cp2Req.Bootstrap.JoinMaterialRef != "operation:bootstrap-init-1/control-plane:cp-2" {
		t.Fatalf("join material ref = %q", cp2Req.Bootstrap.JoinMaterialRef)
	}
}

func TestRunAgentBootstrapPreservesFailedOperationReference(t *testing.T) {
	client := &fakeAgentClient{
		status: readyAgentStatus("machine-cp-1"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-failed",
			RequestDigest: "digest-failed",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-failed", Terminal: true, Result: "failed", FailureReason: "kubeadm refused input"},
		},
	}
	result, err := RunAgentBootstrap(context.Background(), Request{Inventory: validSingleNodeInventory()}, AgentBootstrapDependencies{Connector: newFakeAgentConnector(map[string]*fakeAgentClient{"cp-1": client})})
	if err == nil || !strings.Contains(err.Error(), "kubeadm refused input") {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	failed := result.Phases[len(result.Phases)-1]
	if failed.Status != "failed" || failed.OperationID != "bootstrap-init-failed" || failed.RequestDigest != "digest-failed" {
		t.Fatalf("failed phase = %#v", failed)
	}
}

func TestRunAgentBootstrapReportsDaemonStatusFailureBeforeSubmit(t *testing.T) {
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1": {statusErr: errors.New("rpc failed with Bearer secret-token")},
	})
	result, err := RunAgentBootstrap(context.Background(), Request{Inventory: validSingleNodeInventory()}, AgentBootstrapDependencies{Connector: connector})
	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("RunAgentBootstrap() error = %v, want redacted daemon failure", err)
	}
	if len(connector.clients["cp-1"].submitRequests) != 0 {
		t.Fatalf("SubmitOperation requests = %#v, want none", connector.clients["cp-1"].submitRequests)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness"}) {
		t.Fatalf("phases = %#v", got)
	}
}

func TestRunAgentBootstrapSubmitsInitOperationAndWaits(t *testing.T) {
	inv := validSingleNodeInventory()
	bundleRef := "v1.36.1@sha256:" + strings.Repeat("b", 64)
	inv.KubernetesBundleSource = "https://artifacts.example.test/kubernetes"
	inv.KubernetesBundleRef = bundleRef
	client := &fakeAgentClient{
		status: readyAgentStatus("machine-cp-1"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			OperationKind: "bootstrap-init",
			RequestDigest: "digest-1",
			InitialStatus: &agentapi.OperationStatus{
				OperationId: "bootstrap-init-1",
				Phase:       "accepted",
			},
		},
		events: []*agentapi.OperationEvent{{
			OperationId: "bootstrap-init-1",
			JournalSeq:  1,
			Terminal:    true,
			Status: &agentapi.OperationStatus{
				OperationId: "bootstrap-init-1",
				Terminal:    true,
				Result:      "succeeded",
			},
		}},
		getStatus: &agentapi.OperationStatus{
			OperationId:     "bootstrap-init-1",
			Terminal:        true,
			Result:          "succeeded",
			AdminKubeconfig: adminKubeconfig(),
		},
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{"cp-1": client})
	out := filepath.Join(t.TempDir(), "operator.conf")
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:            inv,
		ControlPlaneEndpoint: "api.katl.test:6443",
		KubeconfigOut:        out,
		OverwriteKubeconfig:  true,
	}, AgentBootstrapDependencies{
		Connector:    connector,
		Actor:        "test-actor",
		Now:          func() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC) },
		WatchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "bootstrap-init", "kubeconfig"}) {
		t.Fatalf("phases = %#v", got)
	}
	if len(client.submitRequests) != 1 {
		t.Fatalf("SubmitOperation requests = %d, want 1", len(client.submitRequests))
	}
	req := client.submitRequests[0]
	if req.OperationKind != "bootstrap-init" || req.Actor != "test-actor" || req.ExpectedMachineId != "machine-cp-1" || req.ExpectedCurrentGenerationId != "0" {
		t.Fatalf("submit request = %#v", req)
	}
	if req.Bootstrap.InventoryNodeName != "cp-1" || req.Bootstrap.ControlPlaneEndpoint != "api.katl.test:6443" || req.Bootstrap.BootstrapProfileRef != "control-plane" {
		t.Fatalf("bootstrap request = %#v", req.Bootstrap)
	}
	if req.Bootstrap.KubernetesBundleSource != inv.KubernetesBundleSource || req.Bootstrap.KubernetesBundleRef != bundleRef {
		t.Fatalf("bootstrap bundle request = %#v", req.Bootstrap)
	}
	if result.Kubeconfig.Path != out || result.Kubeconfig.Server != "https://api.katl.test:6443" {
		t.Fatalf("kubeconfig result = %#v", result.Kubeconfig)
	}
}

func TestRunAgentBootstrapRunsUserBootstrapWithReturnedKubeconfig(t *testing.T) {
	client := &fakeAgentClient{
		status: readyAgentStatus("machine-cp-1"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			RequestDigest: "digest-1",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1"},
		},
		events: []*agentapi.OperationEvent{{
			OperationId: "bootstrap-init-1",
			JournalSeq:  1,
			Terminal:    true,
			Status:      &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
		}},
		getStatus: &agentapi.OperationStatus{
			OperationId:     "bootstrap-init-1",
			Terminal:        true,
			Result:          "succeeded",
			AdminKubeconfig: adminKubeconfig(),
		},
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{"cp-1": client})
	bootstrapRunner := &fakeBootstrapRunner{result: BootstrapResult{StableEndpointReady: true}}
	out := filepath.Join(t.TempDir(), "operator.conf")
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{
			StableEndpoint: "api.stable.test:6443",
			Manifests: []BootstrapManifest{{
				Path:    "cni.yaml",
				Content: []byte(validBootstrapManifest("cni")),
			}},
		},
	}, AgentBootstrapDependencies{Connector: connector, BootstrapRunner: bootstrapRunner})
	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "bootstrap-init", "user-bootstrap", "kubeconfig"}) {
		t.Fatalf("phases = %#v", got)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", len(bootstrapRunner.requests))
	}
	if bootstrapRunner.requests[0].Credentials.ClientKeyData != testKey {
		t.Fatalf("bootstrap credentials = %#v", bootstrapRunner.requests[0].Credentials)
	}
	if result.Kubeconfig.Server != "https://api.stable.test:6443" {
		t.Fatalf("kubeconfig server = %q, want stable endpoint", result.Kubeconfig.Server)
	}
}

func TestRunAgentBootstrapStopsAfterUserBootstrapFailure(t *testing.T) {
	client := &fakeAgentClient{
		status: readyAgentStatus("machine-cp-1"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			RequestDigest: "digest-1",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1"},
		},
		events: []*agentapi.OperationEvent{{
			OperationId: "bootstrap-init-1",
			JournalSeq:  1,
			Terminal:    true,
			Status:      &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
		}},
		getStatus: &agentapi.OperationStatus{
			OperationId:     "bootstrap-init-1",
			Terminal:        true,
			Result:          "succeeded",
			AdminKubeconfig: adminKubeconfig(),
		},
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{"cp-1": client})
	out := filepath.Join(t.TempDir(), "operator.conf")
	bootstrapRunner := &fakeBootstrapRunner{err: errors.New("rollout timed out")}
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{Waits: []BootstrapWait{{
			Kind:      BootstrapWaitRolloutStatus,
			Namespace: "katl-vmtest",
			Name:      "deployment/net-server",
		}}},
	}, AgentBootstrapDependencies{Connector: connector, BootstrapRunner: bootstrapRunner})
	if err == nil || !strings.Contains(err.Error(), "rollout timed out") {
		t.Fatalf("RunAgentBootstrap() error = %v, want user bootstrap failure", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "bootstrap-init", "user-bootstrap"}) {
		t.Fatalf("phases = %#v", got)
	}
	if result.Phases[len(result.Phases)-1].Status != "failed" {
		t.Fatalf("user-bootstrap phase = %#v, want failed", result.Phases[len(result.Phases)-1])
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("kubeconfig output stat error = %v, want not exist", statErr)
	}
}

func TestRunAgentBootstrapMintsWorkerJoinMaterialAndSubmitsWorkerJoin(t *testing.T) {
	cpClient := &fakeAgentClient{
		status: readyAgentStatusWithKinds("machine-cp-1", "bootstrap-init"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			RequestDigest: "digest-init",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1"},
		},
		events: []*agentapi.OperationEvent{{
			OperationId: "bootstrap-init-1",
			JournalSeq:  1,
			Terminal:    true,
			Status:      &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
		}},
		getStatus: &agentapi.OperationStatus{
			OperationId:     "bootstrap-init-1",
			Terminal:        true,
			Result:          "succeeded",
			AdminKubeconfig: adminKubeconfig(),
		},
		createMaterial: &agentapi.CreateWorkerJoinMaterialResponse{
			MaterialRef: "operation:bootstrap-init-1/worker:worker-1",
			WorkerJoinMaterial: &agentapi.WorkerJoinMaterial{
				JoinArgv: []string{
					"kubeadm",
					"join",
					"api.katl.test:6443",
					"--token",
					"abcdef.0123456789abcdef",
					"--discovery-token-ca-cert-hash",
					"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				},
				ExpiresAt: "2026-06-16T13:00:00Z",
			},
		},
	}
	workerClient := &fakeAgentClient{
		status: readyAgentStatusWithKinds("machine-worker-1", "bootstrap-join-worker"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-join-worker-1",
			RequestDigest: "digest-worker",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-join-worker-1", Terminal: true, Result: "succeeded"},
		},
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1":     cpClient,
		"worker-1": workerClient,
	})
	out := filepath.Join(t.TempDir(), "operator.conf")
	result, err := RunAgentBootstrap(context.Background(), Request{
		Inventory:           validInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
	}, AgentBootstrapDependencies{
		Connector: connector,
		Actor:     "test-actor",
	})
	if err != nil {
		t.Fatalf("RunAgentBootstrap() error = %v", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "bootstrap-init", "worker-join", "kubeconfig"}) {
		t.Fatalf("phases = %#v", got)
	}
	if len(cpClient.createMaterialRequests) != 1 {
		t.Fatalf("CreateWorkerJoinMaterial requests = %d, want 1", len(cpClient.createMaterialRequests))
	}
	materialReq := cpClient.createMaterialRequests[0]
	if materialReq.Kind != agentJoinMaterialKind || materialReq.Actor != "test-actor" || materialReq.ExpectedMachineId != "machine-cp-1" || materialReq.RequestRef != "operation:bootstrap-init-1/worker:worker-1" {
		t.Fatalf("join material request = %#v", materialReq)
	}
	if len(workerClient.submitRequests) != 1 {
		t.Fatalf("worker SubmitOperation requests = %d, want 1", len(workerClient.submitRequests))
	}
	workerReq := workerClient.submitRequests[0]
	if workerReq.OperationKind != "bootstrap-join-worker" || workerReq.ExpectedMachineId != "machine-worker-1" {
		t.Fatalf("worker submit request = %#v", workerReq)
	}
	if workerReq.Bootstrap.JoinMaterialRef != "operation:bootstrap-init-1/worker:worker-1" {
		t.Fatalf("join material ref = %q", workerReq.Bootstrap.JoinMaterialRef)
	}
	if workerReq.Bootstrap.WorkerJoinMaterial == nil || workerReq.Bootstrap.WorkerJoinMaterial.ExpiresAt != "2026-06-16T13:00:00Z" {
		t.Fatalf("worker join material = %#v", workerReq.Bootstrap.WorkerJoinMaterial)
	}
}

func TestRunAgentBootstrapRedactsWorkerJoinMaterialFailure(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	cpClient := &fakeAgentClient{
		status: readyAgentStatusWithKinds("machine-cp-1", "bootstrap-init"),
		accepted: &agentapi.OperationAccepted{
			OperationId:   "bootstrap-init-1",
			RequestDigest: "digest-init",
			InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
		},
		getStatus:         &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded", AdminKubeconfig: adminKubeconfig()},
		createMaterialErr: errors.New("kubeadm token " + secret),
	}
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1":     cpClient,
		"worker-1": {status: readyAgentStatusWithKinds("machine-worker-1", "bootstrap-join-worker")},
	})
	_, err := RunAgentBootstrap(context.Background(), Request{Inventory: validInventory()}, AgentBootstrapDependencies{Connector: connector})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("RunAgentBootstrap() error = %v, want redacted join material failure", err)
	}
}

func TestRunAgentBootstrapRedactsSubmitFailure(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	connector := newFakeAgentConnector(map[string]*fakeAgentClient{
		"cp-1": {
			status:    readyAgentStatus("machine-cp-1"),
			submitErr: errors.New("kubeadm token " + secret),
		},
	})
	_, err := RunAgentBootstrap(context.Background(), Request{Inventory: validSingleNodeInventory()}, AgentBootstrapDependencies{Connector: connector})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("RunAgentBootstrap() error = %v, want redacted submit failure", err)
	}
}

type fakeAgentConnector struct {
	clients   map[string]*fakeAgentClient
	connected []string
}

func newFakeAgentConnector(clients map[string]*fakeAgentClient) *fakeAgentConnector {
	if clients == nil {
		clients = make(map[string]*fakeAgentClient)
	}
	return &fakeAgentConnector{clients: clients}
}

func (c *fakeAgentConnector) Connect(_ context.Context, node inventory.PlannedNode) (AgentConnection, error) {
	c.connected = append(c.connected, node.Name+"@"+node.Address)
	client := c.clients[node.Name]
	if client == nil {
		client = &fakeAgentClient{status: readyAgentStatus("machine-" + node.Name)}
		c.clients[node.Name] = client
	}
	return AgentConnection{Endpoint: node.Address, Client: client}, nil
}

type fakeAgentClient struct {
	status                 *agentapi.NodeStatus
	statusErr              error
	submitRequests         []*agentapi.SubmitOperationRequest
	submitErr              error
	accepted               *agentapi.OperationAccepted
	createMaterialRequests []*agentapi.CreateWorkerJoinMaterialRequest
	createMaterial         *agentapi.CreateWorkerJoinMaterialResponse
	createMaterialErr      error
	events                 []*agentapi.OperationEvent
	getStatus              *agentapi.OperationStatus
	getErr                 error
	watchErr               error
}

func (c *fakeAgentClient) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest, ...grpc.CallOption) (*agentapi.NodeStatus, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	return c.status, nil
}

func (c *fakeAgentClient) SubmitOperation(_ context.Context, req *agentapi.SubmitOperationRequest, _ ...grpc.CallOption) (*agentapi.OperationAccepted, error) {
	c.submitRequests = append(c.submitRequests, req)
	if c.submitErr != nil {
		return nil, c.submitErr
	}
	if c.accepted != nil {
		return c.accepted, nil
	}
	return &agentapi.OperationAccepted{
		OperationId:   "operation-1",
		RequestDigest: "digest-1",
		InitialStatus: &agentapi.OperationStatus{OperationId: "operation-1", Terminal: true, Result: "succeeded"},
	}, nil
}

func (c *fakeAgentClient) CreateWorkerJoinMaterial(_ context.Context, req *agentapi.CreateWorkerJoinMaterialRequest, _ ...grpc.CallOption) (*agentapi.CreateWorkerJoinMaterialResponse, error) {
	c.createMaterialRequests = append(c.createMaterialRequests, req)
	if c.createMaterialErr != nil {
		return nil, c.createMaterialErr
	}
	if c.createMaterial != nil {
		return c.createMaterial, nil
	}
	return &agentapi.CreateWorkerJoinMaterialResponse{
		MaterialRef: req.GetRequestRef(),
		WorkerJoinMaterial: &agentapi.WorkerJoinMaterial{
			JoinArgv: []string{
				"kubeadm",
				"join",
				"api.katl.test:6443",
				"--token",
				"abcdef.0123456789abcdef",
				"--discovery-token-ca-cert-hash",
				"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			ExpiresAt: "2026-06-16T13:00:00Z",
		},
	}, nil
}

func (c *fakeAgentClient) GetOperation(context.Context, *agentapi.GetOperationRequest, ...grpc.CallOption) (*agentapi.OperationStatus, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.getStatus != nil {
		return c.getStatus, nil
	}
	return &agentapi.OperationStatus{OperationId: "operation-1", Terminal: true, Result: "succeeded"}, nil
}

func (c *fakeAgentClient) WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error) {
	if c.watchErr != nil {
		return nil, c.watchErr
	}
	return &fakeAgentWatch{events: append([]*agentapi.OperationEvent(nil), c.events...)}, nil
}

type fakeAgentWatch struct {
	grpc.ClientStream
	events []*agentapi.OperationEvent
}

func (w *fakeAgentWatch) Recv() (*agentapi.OperationEvent, error) {
	if len(w.events) == 0 {
		return nil, io.EOF
	}
	event := w.events[0]
	w.events = w.events[1:]
	return event, nil
}

func readyAgentStatus(machineID string) *agentapi.NodeStatus {
	return readyAgentStatusWithKinds(machineID, "bootstrap-init")
}

func readyAgentStatusWithKinds(machineID string, kinds ...string) *agentapi.NodeStatus {
	return &agentapi.NodeStatus{
		ApiVersion:              agentAPIVersion,
		MachineId:               machineID,
		SupportedOperationKinds: kinds,
	}
}
