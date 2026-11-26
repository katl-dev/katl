package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestHostStatusUsesContextAndPrintsOperatorView(t *testing.T) {
	configPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: homelab
clusters:
- name: homelab
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.10:9443
    systemRole: control-plane
`)
	fake := healthyHostClient("machine-secret", "agent-secret", "generation-0")
	fake.nodeStatus.OperationLockHeld = true
	fake.nodeStatus.ActiveOperationIds = []string{"operation-secret"}
	fake.nodeStatus.BootTargetGenerationId = "generation-staged"
	fake.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{
		State: "waiting-for-node", Role: "control-plane", NodeName: "cp-1", KubeletActive: true,
		FailureReason: "Kubernetes node cp-1 is not Ready",
	}
	fake.nodeStatus.ControlPlaneEndpoint = &agentapi.ControlPlaneEndpointStatus{
		Endpoint: "api.home.example:6443", Vip: "10.40.0.10/32", State: "failed", FailureReason: "endpoint ownership unavailable",
	}
	fake.nodeStatus.Volumes = []*agentapi.VolumeStatus{{
		Name: "local-hostpath", TargetKind: "partition", MountPath: "/var/mnt/local-hostpath", Filesystem: "xfs", ActiveState: "failed",
		FailureDiagnostic: "mount: wrong fs type",
	}}
	fake.generation.RuntimeVersion = "2026.7.0-alpha.10"
	installKatlcDial(t, func(endpoint string) {
		if endpoint != "192.0.2.10:9443" {
			t.Fatalf("dial endpoint=%q", endpoint)
		}
	}, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "status", "cp-1", "--context-file", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"NODE", "HEALTH", "KUBERNETES", "KATLOS", "GENERATION", "NEXT BOOT", "ACTIVITY", "cp-1", "OK", "waiting-for-node", "Kubernetes node cp-1 is not Ready", "2026.7.0-alpha.10", "generation-0", "generation-staged", "busy", "VOLUME", "local-hostpath", "/var/mnt/local-hostpath", "mount: wrong fs type", "CONTROL PLANE ENDPOINT", "LOCAL VIP", "api.home.example:6443", "10.40.0.10/32", "failed", "endpoint ownership unavailable"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	for _, internal := range []string{"machine-secret", "agent-secret", "operation-secret"} {
		if strings.Contains(output, internal) {
			t.Fatalf("output exposes %q:\n%s", internal, output)
		}
	}
}

func TestHostStatusJSON(t *testing.T) {
	fake := healthyHostClient("machine-a", "agent-a", "generation-0")
	fake.nodeStatus.ControlPlaneEndpoint = &agentapi.ControlPlaneEndpointStatus{
		Endpoint: "api.home.example:6443", Vip: "10.40.0.10/32", State: "active", LocalApiReady: true, LocalVipOwned: true,
	}
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "status", "node-a", "--endpoint", "node-a.test:9443", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report hostStatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout.String())
	}
	if report.Node != "node-a" || report.Endpoint != "node-a.test:9443" || report.Health != "OK" || report.Generation != "generation-0" || report.Activity != "idle" {
		t.Fatalf("report = %#v", report)
	}
	if report.ControlPlaneEndpoint == nil || report.ControlPlaneEndpoint.State != "active" || !report.ControlPlaneEndpoint.LocalVIPOwned {
		t.Fatalf("control-plane endpoint report = %#v", report.ControlPlaneEndpoint)
	}
}

func TestHostStatusReportsSelectedFallbackAndBootFailure(t *testing.T) {
	fake := healthyHostClient("machine-a", "agent-a", "generation-0")
	fake.nodeStatus.CurrentGenerationId = "generation-1"
	fake.nodeStatus.SelectedGenerationId = "generation-0"
	fake.nodeStatus.BootTargetGenerationId = "generation-1"
	fake.nodeStatus.BootHealthState = "failed"
	fake.nodeStatus.BootHealthDiagnostic = "running generation generation-0 does not match durable boot evidence generation-1"
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "status", "node-a", "--endpoint", "node-a.test:9443"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"failed", "generation-0", "generation-1", "does not match durable boot evidence"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "\tOK\t") {
		t.Fatalf("output masks boot failure as healthy:\n%s", output)
	}
}

func TestHostRebootDefaultAllowsBootDeadmanRecovery(t *testing.T) {
	cmd := newHostRebootCommand(context.Background(), io.Discard, io.Discard)
	timeout, err := cmd.Flags().GetDuration("timeout")
	if err != nil {
		t.Fatal(err)
	}
	if timeout != 15*time.Minute {
		t.Fatalf("reboot timeout = %s, want 15m", timeout)
	}
}

func TestHostRebootHonorsBootTargetAndWaits(t *testing.T) {
	configPath := writeHostMutationContext(t, "machine-a")
	fake := healthyHostClient("machine-a", "before", "generation-0")
	fake.nodeStatus.BootTargetGenerationId = "generation-staged"
	fake.onReboot = func(req *agentapi.RebootRequest) {
		fake.nodeStatus.AgentStartId = "after"
		fake.nodeStatus.CurrentGenerationId = req.GetTargetGenerationId()
		fake.generation.GenerationId = req.GetTargetGenerationId()
	}
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "reboot", "node-a", "--context-file", configPath, "--timeout", "1s"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if len(fake.rebootRequests) != 1 {
		t.Fatalf("reboot requests = %d, want 1", len(fake.rebootRequests))
	}
	request := fake.rebootRequests[0]
	if request.GetActor() != "katlctl node reboot" || request.GetExpectedMachineId() != "machine-a" || request.GetTargetGenerationId() != "generation-staged" {
		t.Fatalf("reboot request = %#v", request)
	}
	if got := stdout.String(); got != "node-a rebooted successfully; health OK\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "Reboot scheduled for node-a") || strings.Contains(got, "agent=") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestHostRebootNoWaitJSON(t *testing.T) {
	configPath := writeHostMutationContext(t, "machine-a")
	fake := healthyHostClient("machine-a", "before", "generation-0")
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "reboot", "node-a", "--context-file", configPath, "--no-wait", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report hostRebootReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode reboot: %v\n%s", err, stdout.String())
	}
	if report.Node != "node-a" || report.Result != "scheduled" || report.Generation != "generation-0" || report.Health != "" {
		t.Fatalf("report = %#v", report)
	}
}

func TestHostRebootReportsUnhealthyReturn(t *testing.T) {
	configPath := writeHostMutationContext(t, "machine-a")
	fake := healthyHostClient("machine-a", "before", "generation-0")
	fake.onReboot = func(*agentapi.RebootRequest) {
		fake.nodeStatus.AgentStartId = "after"
		fake.nodeStatus.BootHealthState = "failed"
		fake.nodeStatus.BootHealthDiagnostic = "katl-boot-health.service failed in this boot; inspect the boot journal"
	}
	installKatlcDial(t, nil, fake)

	err := run(context.Background(), []string{"node", "reboot", "node-a", "--context-file", configPath, "--timeout", "1s"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "node-a failed boot health on generation generation-0: katl-boot-health.service failed in this boot") {
		t.Fatalf("run() error = %v, want unhealthy boot error", err)
	}
}

func TestHostRebootTimesOutWhenAgentDoesNotRestart(t *testing.T) {
	configPath := writeHostMutationContext(t, "machine-a")
	fake := healthyHostClient("machine-a", "before", "generation-0")
	installKatlcDial(t, nil, fake)
	oldInterval := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = oldInterval })

	err := run(context.Background(), []string{"node", "reboot", "node-a", "--context-file", configPath, "--timeout", "10ms"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "node node-a did not return healthy") {
		t.Fatalf("run() error = %v, want reboot timeout", err)
	}
}

func TestHostShutdownWaitsForManagementAPIToStop(t *testing.T) {
	for _, test := range []struct {
		name      string
		statusErr error
	}{
		{name: "connection refused"},
		{name: "agent error", statusErr: grpcstatus.Error(codes.Unavailable, "connection error: dial tcp: connect: connection refused")},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			agent := &shutdownTestAgent{
				status:    healthyHostClient("machine-a", "agent-a", "generation-0").nodeStatus,
				statusErr: test.statusErr,
				requests:  make(chan *agentapi.ShutdownRequest, 1),
			}
			if test.statusErr == nil {
				// Stop accepting connections while the existing RPC can still acknowledge shutdown.
				agent.stopListening = func() { _ = listener.Close() }
			}
			agentapi.RegisterKatlcAgentServer(server, agent)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)

			configPath := writeHostMutationContext(t, "machine-a")
			var stdout, stderr bytes.Buffer
			err = run(t.Context(), []string{"node", "shutdown", "node-a", "--context-file", configPath, "--endpoint", listener.Addr().String(), "--timeout", "1s"}, &stdout, &stderr)
			select {
			case request := <-agent.requests:
				if request.GetActor() != "katlctl node shutdown" || request.GetExpectedMachineId() != "machine-a" {
					t.Fatalf("shutdown request = %#v", request)
				}
			default:
				t.Fatalf("agent did not receive a shutdown request: %v", err)
			}
			if !strings.Contains(stderr.String(), "Shutdown scheduled for node-a") {
				t.Fatalf("stderr = %q", stderr.String())
			}

			if test.statusErr != nil {
				if !agent.repliedWithError.Load() {
					t.Fatal("the live agent did not return the application error during shutdown verification")
				}
				if err == nil || !strings.Contains(err.Error(), "Could not confirm shutdown") || !strings.Contains(err.Error(), "Check the node's console") || stdout.Len() != 0 {
					t.Fatalf("stdout = %q, err = %v, want an unconfirmed shutdown with console guidance", stdout.String(), err)
				}
				return
			}
			if err != nil || stdout.String() != "node-a shut down; management API is offline\n" {
				t.Fatalf("stdout = %q, err = %v, want offline confirmation", stdout.String(), err)
			}
		})
	}
}

type shutdownTestAgent struct {
	agentapi.UnimplementedKatlcAgentServer
	status           *agentapi.NodeStatus
	statusErr        error
	stopListening    func()
	requests         chan *agentapi.ShutdownRequest
	scheduled        atomic.Bool
	repliedWithError atomic.Bool
}

func (a *shutdownTestAgent) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	if a.scheduled.Load() && a.statusErr != nil {
		a.repliedWithError.Store(true)
		return nil, a.statusErr
	}
	return a.status, nil
}

func (a *shutdownTestAgent) Shutdown(_ context.Context, req *agentapi.ShutdownRequest) (*agentapi.ShutdownAccepted, error) {
	a.requests <- req
	a.scheduled.Store(true)
	if a.stopListening != nil {
		a.stopListening()
	}
	return &agentapi.ShutdownAccepted{Scheduled: true}, nil
}

func TestHostShutdownLateStatusFailure(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.interceptRPC = func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ grpc.UnaryInvoker, _ ...grpc.CallOption) error {
			<-ctx.Done()
			return grpcstatus.Error(codes.Unavailable, "connection error: dial tcp: connect: connection refused")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		target := managementTarget{nodeName: node.name, endpoint: mustManagement(t, "192.0.2.11").Endpoint(), credentials: &trustedNetworkClient}
		if err := waitNodeOffline(ctx, target); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown verification = %v, want deadline exceeded", err)
		}
	})
}

func TestHostStatusLatePlaintextRefusal(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("enable-mtls: %v\n%s", err, stderr)
		}
		node := world.nodes[0]
		node.interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoke(ctx, method, req, reply, cc, opts...)
			if err == nil {
				t.Fatal("the node answered the plaintext probe")
			}
			_ = cc.Close()
			deadline, _ := ctx.Deadline()
			time.Sleep(time.Until(deadline))
			return err
		}
		ctx, observation := transport.ObserveManagementConnection(deadlineOnlyContext{Context: t.Context(), deadline: time.Now().Add(time.Second)})
		endpoint := "192.0.2.11:9443"
		conn, err := node.dial(transport.WithoutManagementWait(withManagementDial(ctx, node.name, &trustedNetworkClient)), endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
			t.Fatal("node status fell back to mTLS based on an expired plaintext probe")
			return katlcAgentConnection{}, nil
		}

		_, _, err = readStatusReport(ctx, conn.Client, node.name, mustHostPort(t, endpoint), world.configPath, &trustedNetworkClient, observation)
		if !errors.Is(err, context.DeadlineExceeded) || !observation.PlaintextClosed() {
			t.Fatalf("node status = %v, want a deadline error after observing plaintext close", err)
		}
	})
}

func TestHostShutdownNoWaitJSON(t *testing.T) {
	configPath := writeHostMutationContext(t, "machine-a")
	fake := healthyHostClient("machine-a", "agent-a", "generation-0")
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "shutdown", "node-a", "--context-file", configPath, "--no-wait", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report hostShutdownReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode shutdown: %v\n%s", err, stdout.String())
	}
	if report.Node != "node-a" || report.Result != "scheduled" {
		t.Fatalf("report = %#v", report)
	}
}

func TestWaitNodeOfflineRetainsResolvedManagementIdentity(t *testing.T) {
	credentials := &managementidentity.ClientCredentials{}
	target := managementTarget{nodeName: "node-a", endpoint: mustHostPort(t, "override.test:9443"), credentials: credentials}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != target.endpoint.String() {
			t.Fatalf("dial endpoint = %q, want %q", endpoint, target.endpoint)
		}
		identity, ok := ctx.Value(managementDialIdentityKey{}).(managementDialIdentity)
		if !ok || identity.nodeName != target.nodeName || identity.credentials != credentials {
			t.Fatalf("management identity = %#v, want resolved target credentials", identity)
		}
		cancel()
		return katlcAgentConnection{}, context.Canceled
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	if err := waitNodeOffline(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitNodeOffline() error = %v, want cancellation", err)
	}
}

func TestHostManagementRejectsDuplicateNodeSelection(t *testing.T) {
	err := run(context.Background(), []string{"node", "status", "cp-1", "--node", "worker-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NODE cannot be combined with --node") {
		t.Fatalf("run() error = %v", err)
	}
}

func healthyHostClient(machineID, agentStartID, generationID string) *fakeKatlcAgentClient {
	return &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           machineID,
			EnrollmentId:        "enrollment-" + machineID,
			InventoryNodeName:   "node-a",
			AgentStartId:        agentStartID,
			CurrentGenerationId: generationID,
			BootHealthState:     "healthy",
			Kubernetes:          &agentapi.KubernetesStatus{State: "not-configured"},
		},
		generation: &agentapi.Generation{
			GenerationId: generationID,
			CommitState:  generation.CommitStateCommitted,
			BootState:    generation.BootStateGood,
			HealthState:  generation.HealthStateHealthy,
		},
	}
}

func writeHostMutationContext(t *testing.T, machineID string) string {
	t.Helper()
	return writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: homelab
clusters:
- name: homelab
  management:
    authentication: trusted-network
  nodes:
  - name: node-a
    managementEndpoint: node-a.test:9443
    systemRole: control-plane
    enrollmentID: enrollment-`+machineID+`
    machineID: `+machineID+`
`)
}

func installKatlcDial(t *testing.T, inspect func(endpoint string), client agentapi.KatlcAgentClient) {
	t.Helper()
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if inspect != nil {
			inspect(endpoint)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })
}
