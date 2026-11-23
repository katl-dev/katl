package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// trialNode models the node side of the trust transition: one address that
// serves plaintext until a trial begins, then only the staged credentials, as
// katlc-agent does.
type trialNode struct {
	agentapi.UnimplementedKatlcAgentServer
	t    *testing.T
	name string

	mu        sync.Mutex
	listener  *bufconn.Listener
	server    *grpc.Server
	mode      managementidentity.Authentication
	authority string
	staged    managementidentity.NodeCredentials
	trial     *agentapi.ManagementTransition
	begins    int
	confirms  int
	// rebinds tracks listener restarts still in progress.
	rebinds sync.WaitGroup
	// controlPlane makes the node a configured control plane, so its etcd
	// and API proxy count towards health.
	controlPlane  bool
	kubernetes    *agentapi.KubernetesStatus
	etcdHealthy   uint32
	apiProxyState string
	endpoint      *agentapi.ControlPlaneEndpointStatus
	// Fault knobs.
	clockOffset      time.Duration
	omitNodeTime     bool
	legacy           bool
	unreachableTrial bool
	loseConfirmReply bool
	healthState      string
	// afterBegin runs once a Begin request has armed the trial.
	afterBegin func(context.Context)
	// afterConfirm runs, with the node locked, once a confirm commits.
	afterConfirm func(*trialNode)
	// interceptRPC runs inside the production dial options.
	interceptRPC grpc.UnaryClientInterceptor
}

func newTrialNode(t *testing.T, name string) *trialNode {
	t.Helper()
	node := &trialNode{t: t, name: name, mode: managementidentity.TrustedNetwork, healthState: bootHealthHealthy, etcdHealthy: 3, apiProxyState: "ready"}
	node.serve()
	t.Cleanup(func() {
		node.rebinds.Wait()
		node.mu.Lock()
		defer node.mu.Unlock()
		node.server.Stop()
	})
	return node
}

// serve starts a gRPC listener with the credentials its state selects. The
// in-memory listener allows synctest to advance time while gRPC waits for I/O.
func (n *trialNode) serve() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	var options []grpc.ServerOption
	if n.mode == managementidentity.MutualTLS || n.trial.GetState() == "trial" {
		credentialsForNode := n.staged
		if n.unreachableTrial && n.trial.GetState() == "trial" {
			// A listener whose server certificate this workstation does not
			// trust stands for a trial the workstation cannot reach.
			other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other"})
			if err != nil {
				n.t.Error(err)
				return
			}
			node, _, err := managementidentity.EnsureNode(&other, n.name, time.Now(), nil)
			if err != nil {
				n.t.Error(err)
				return
			}
			credentialsForNode = node
		}
		config, err := transport.ServerTLSConfigForNode(credentialsForNode)
		if err != nil {
			n.t.Error(err)
			return
		}
		options = append(options, grpc.Creds(trialTLSCredentials{credentials.NewTLS(config)}))
	}
	n.listener = bufconn.Listen(32 << 10)
	n.server = grpc.NewServer(options...)
	agentapi.RegisterKatlcAgentServer(n.server, n)
	go n.server.Serve(n.listener)
}

func (n *trialNode) dial(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
	return n.dialWithOptions(ctx, endpoint)
}

func (n *trialNode) dialWithOptions(ctx context.Context, endpoint string, extra ...grpc.DialOption) (katlcAgentConnection, error) {
	identity := transport.ClientCredentialsFromContext(ctx)
	if identity == nil {
		return katlcAgentConnection{}, fmt.Errorf("no management credentials for test node %s", n.name)
	}
	creds, err := transport.ClientCredentialsForNode(*identity, n.name)
	if err != nil {
		return katlcAgentConnection{}, err
	}
	return n.dialWithTransport(ctx, endpoint, creds, extra...)
}

func (n *trialNode) dialWithTransport(ctx context.Context, endpoint string, creds credentials.TransportCredentials, extra ...grpc.DialOption) (katlcAgentConnection, error) {
	// A bufconn dial holds the listener it starts with, so it cannot reach
	// the replacement listener if it races with an in-progress restart.
	n.rebinds.Wait()
	n.mu.Lock()
	listener := n.listener
	n.mu.Unlock()

	options := transport.ManagementDialOptions(ctx, n.name, endpoint, creds)
	if n.interceptRPC != nil {
		options = append(options, grpc.WithChainUnaryInterceptor(n.interceptRPC))
	}
	options = append(options, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	options = append(options, extra...)
	conn, err := grpc.NewClient("passthrough:///"+endpoint, options...)
	if err != nil {
		return katlcAgentConnection{}, err
	}
	return katlcAgentConnection{Client: agentapi.NewKatlcAgentClient(conn), Close: conn.Close}, nil
}

// rebind re-serves the address once in-flight calls finish, like the agent's
// listener loop after a trial begins or reverts.
func (n *trialNode) rebind() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rebindLocked()
}

func (n *trialNode) rebindLocked() {
	server := n.server
	n.rebinds.Add(1)
	go func() {
		defer n.rebinds.Done()
		server.GracefulStop()
		n.serve()
	}()
}

func (n *trialNode) revert(reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.trial = &agentapi.ManagementTransition{State: "reverted", TrialId: n.trial.GetTrialId(), TargetAuthority: n.trial.GetTargetAuthority(), RevertReason: reason}
	n.rebindLocked()
}

func (n *trialNode) GetEtcdStatus(context.Context, *agentapi.GetEtcdStatusRequest) (*agentapi.EtcdStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := &agentapi.EtcdStatus{LocalMemberId: "member-" + n.name, Quorum: 2, HealthyMembers: n.etcdHealthy}
	for _, id := range []string{"member-" + n.name, "member-b", "member-c"} {
		status.Members = append(status.Members, &agentapi.EtcdMember{Id: id, Name: id, Healthy: true})
	}
	return status, nil
}

func (n *trialNode) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := enrolledStatus(n.name, "enrollment-"+n.name, "machine-"+n.name)
	status.BootHealthState = n.healthState
	if n.controlPlane {
		status.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "control-plane", KubeletActive: true, NodeReady: true, ControlPlaneComponentsReady: true}
		status.ApiProxy = &agentapi.APIProxyStatus{State: n.apiProxyState, LocalApiEligible: true}
	}
	if n.kubernetes != nil {
		status.Kubernetes = proto.Clone(n.kubernetes).(*agentapi.KubernetesStatus)
		status.ApiProxy = &agentapi.APIProxyStatus{State: n.apiProxyState}
	}
	if n.endpoint != nil {
		status.ControlPlaneEndpoint = proto.Clone(n.endpoint).(*agentapi.ControlPlaneEndpointStatus)
	}
	if !n.legacy {
		status.Management = &agentapi.ManagementStatus{
			Authentication: string(n.mode), Authority: n.authority,
			Transition: proto.Clone(n.trial).(*agentapi.ManagementTransition),
			NodeTime:   time.Now().Add(n.clockOffset).UTC().Format(time.RFC3339Nano),
		}
		if n.trial == nil {
			status.Management.Transition = nil
		}
		if n.omitNodeTime {
			status.Management.NodeTime = ""
		}
	}
	return status, nil
}

func (n *trialNode) BeginManagementTransition(ctx context.Context, req *agentapi.BeginManagementTransitionRequest) (*agentapi.ManagementTransition, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.legacy {
		return nil, grpcstatus.Error(codes.Unimplemented, "unknown method")
	}
	if req.GetExpectedInventoryNodeName() != n.name || req.GetExpectedMachineId() != "machine-"+n.name || req.GetExpectedAuthentication() != string(managementidentity.TrustedNetwork) {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "unexpected begin request %v", req)
	}
	target, err := managementidentity.CAFingerprint(req.GetCaCertificate())
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	n.begins++
	n.staged = managementidentity.NodeCredentials{CACertificate: req.GetCaCertificate(), ServerCertificate: req.GetServerCertificate(), ServerPrivateKey: req.GetServerPrivateKey()}
	n.trial = &agentapi.ManagementTransition{
		State: "trial", TrialId: fmt.Sprintf("trial-%d", n.begins), TargetAuthority: target,
		Deadline: time.Now().Add(n.clockOffset + time.Duration(req.GetTrialSeconds())*time.Second).UTC().Format(time.RFC3339),
	}
	result := proto.Clone(n.trial).(*agentapi.ManagementTransition)
	n.rebindLocked()
	if n.afterBegin != nil {
		n.afterBegin(ctx)
	}
	return result, nil
}

func (n *trialNode) ConfirmManagementTransition(_ context.Context, req *agentapi.ConfirmManagementTransitionRequest) (*agentapi.ManagementTransition, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.trial.GetState() != "trial" || req.GetTargetAuthority() != n.trial.GetTargetAuthority() {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "no management transition to authority %s is in progress", req.GetTargetAuthority())
	}
	n.confirms++
	n.mode, n.authority = managementidentity.MutualTLS, n.trial.GetTargetAuthority()
	n.trial.State, n.trial.Deadline = "committed", ""
	if n.afterConfirm != nil {
		n.afterConfirm(n)
	}
	if n.loseConfirmReply {
		n.loseConfirmReply = false
		return nil, grpcstatus.Error(codes.Unavailable, "connection reset before the reply")
	}
	return proto.Clone(n.trial).(*agentapi.ManagementTransition), nil
}

func (n *trialNode) state() (managementidentity.Authentication, string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode, n.trial.GetState(), n.begins
}

type enableMTLSWorld struct {
	configPath  string
	secretsPath string
	nodes       []*trialNode
}

func testEnableMTLSWithVirtualTime(t *testing.T, names []string, test func(*testing.T, enableMTLSWorld)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		poll := enableMTLSPoll
		world := newEnableMTLSWorld(t, names...)
		// Virtual time can use the production poll interval.
		enableMTLSPoll = poll
		test(t, world)
	})
}

func newEnableMTLSWorld(t *testing.T, names ...string) enableMTLSWorld {
	t.Helper()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(t.TempDir(), "katlctl.yaml"))
	oldPoll := enableMTLSPoll
	enableMTLSPoll = 20 * time.Millisecond
	t.Cleanup(func() { enableMTLSPoll = oldPoll })

	// Each node has its own configured address on the default management
	// port; the dialer reaches the node's test listener behind it.
	world := enableMTLSWorld{}
	listeners := map[string]*trialNode{}
	var nodes strings.Builder
	for i, name := range names {
		node := newTrialNode(t, name)
		world.nodes = append(world.nodes, node)
		address := fmt.Sprintf("192.0.2.%d", i+11)
		listeners[net.JoinHostPort(address, "9443")] = node
		fmt.Fprintf(&nodes, "    - name: %s\n      controlPlane: %t\n      management:\n        address: %s\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-%s-root\n", name, !strings.HasPrefix(name, "worker-"), address, name)
	}
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		node, ok := listeners[endpoint]
		if !ok {
			return katlcAgentConnection{}, fmt.Errorf("no test node at %s", endpoint)
		}
		return node.dial(ctx, endpoint)
	}
	dir := t.TempDir()
	world.configPath = filepath.Join(dir, "cluster.yaml")
	world.secretsPath = filepath.Join(dir, defaultManagementSecrets)
	config := `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  managementAuthentication: trusted-network
  controlPlaneEndpoint:
    host: api.katl.test
    port: 6443
  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  nodes:
` + nodes.String()
	if err := os.WriteFile(world.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return world
}

func (w enableMTLSWorld) enable(args ...string) (string, string, error) {
	return w.enableContext(context.Background(), args...)
}

func (w enableMTLSWorld) enableContext(ctx context.Context, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := run(ctx, append([]string{"management", "enable-mtls", "--config", w.configPath, "--timeout", "3s"}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func (w enableMTLSWorld) configMode(t *testing.T) (managementidentity.Authentication, string) {
	t.Helper()
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	mode, err := source.ManagementAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	return mode, source.Spec.ManagementIdentity
}

func TestEnableMTLSMovesClusterAndRerunsAsNoOp(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1", "cp-2")
	world.nodes[0].controlPlane = true
	var saved bytes.Buffer
	if err := run(context.Background(), []string{"context", "save", "--config", world.configPath}, &saved, &saved); err != nil {
		t.Fatalf("save trusted-network context: %v\n%s", err, saved.String())
	}

	stdout, stderr, err := world.enable()
	if err != nil {
		t.Fatalf("enable-mtls: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{"cp-1: mTLS confirmed (1/2)", "cp-2: mTLS confirmed (2/2)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("progress lacks %q:\n%s", want, stderr)
		}
	}
	if !strings.Contains(stdout, world.secretsPath) || !strings.Contains(stdout, "back this file up") {
		t.Errorf("stdout does not name the new secrets file to back up:\n%s", stdout)
	}
	bundle, err := readManagementIdentity(world.secretsPath)
	if err != nil {
		t.Fatalf("secrets file: %v", err)
	}
	info, err := managementidentity.Validate(bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range world.nodes {
		if mode, trial, _ := node.state(); mode != managementidentity.MutualTLS || trial != "committed" || node.authority != info.Fingerprint {
			t.Errorf("%s: mode %s trial %s authority %s, want mtls committed to %s", node.name, mode, trial, node.authority, info.Fingerprint)
		}
	}
	if mode, reference := world.configMode(t); mode != managementidentity.MutualTLS || reference != defaultManagementSecrets {
		t.Errorf("config = %s with identity %q, want mtls referencing %s", mode, reference, defaultManagementSecrets)
	}
	contextPath, err := workstation.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := workstation.Load(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if management := contexts.Clusters[0].Management; management == nil || management.Authentication == managementidentity.TrustedNetwork || management.CACertificate != bundle.CertificateAuthority.Certificate {
		t.Errorf("saved context management = %+v, want mTLS credentials", management)
	}
	var status bytes.Buffer
	if err := run(context.Background(), []string{"node", "status", "cp-2"}, &status, &status); err != nil || !strings.Contains(status.String(), "mtls") {
		t.Errorf("node status through the refreshed context: %v\n%s", err, status.String())
	}

	stdout, _, err = world.enable()
	if err != nil || !strings.Contains(stdout, "already uses mTLS") {
		t.Fatalf("rerun = %v, stdout:\n%s", err, stdout)
	}
	for _, node := range world.nodes {
		if _, _, begins := node.state(); begins != 1 {
			t.Errorf("%s began %d trials, want 1", node.name, begins)
		}
	}
}

func TestEnableMTLSPreflightChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		fault func(*trialNode)
		want  []string
	}{
		"unreachable":       {fault: func(n *trialNode) { n.mu.Lock(); n.server.Stop(); n.mu.Unlock() }, want: []string{"cp-2 at", "could not confirm the node uses trusted-network management; the node may still trust an mTLS authority", "check the management address and node reachability"}},
		"older node":        {fault: func(n *trialNode) { n.legacy = true }, want: []string{"cp-2: the installed KatlOS release cannot move management trust", "upgrade the node with `katlctl node upgrade`, then rerun"}},
		"missing node time": {fault: func(n *trialNode) { n.omitNodeTime = true }, want: []string{"management status did not report a valid node time", "upgrade the node with `katlctl node upgrade`, then rerun"}},
		"clock behind":      {fault: func(n *trialNode) { n.clockOffset = -6 * time.Minute }, want: []string{"cp-2's clock is 6m0s behind", "NTP"}},
		"etcd below quorum": {fault: func(n *trialNode) { n.controlPlane, n.etcdHealthy = true, 1 }, want: []string{"cp-2: could not confirm the node is healthy", "etcd has 1 healthy members, below quorum 2"}},
		"unhealthy":         {fault: func(n *trialNode) { n.healthState = "degraded" }, want: []string{"cp-2: could not confirm the node is healthy", "boot health is degraded"}},
	} {
		t.Run(name, func(t *testing.T) {
			world := newEnableMTLSWorld(t, "cp-1", "cp-2")
			tc.fault(world.nodes[1])
			before, err := os.ReadFile(world.configPath)
			if err != nil {
				t.Fatal(err)
			}

			_, _, err = world.enable()
			if err == nil || !strings.Contains(err.Error(), "no node was changed") {
				t.Fatalf("enable-mtls error = %v, want a preflight failure", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q: %v", want, err)
				}
			}
			if _, _, begins := world.nodes[0].state(); begins != 0 {
				t.Error("preflight failure began a trial on cp-1")
			}
			if _, err := os.Stat(world.secretsPath); !os.IsNotExist(err) {
				t.Errorf("preflight failure created secrets: %v", err)
			}
			after, err := os.ReadFile(world.configPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Error("preflight failure changed the ClusterConfig")
			}
		})
	}
}

func TestEnableMTLSDoesNotDowngradeAfterCertificateFailure(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	plaintext := world.nodes[0]
	tlsNode := newTrialNode(t, plaintext.name)
	tlsNode.serveForeignAuthority(t)

	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := managementidentity.EnsureNode(&bundle, plaintext.name, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
		t.Fatal(err)
	}
	plaintextDial := dialKatlcAgent
	plaintextProbes := 0
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		if transport.ClientCredentialsFromContext(ctx).Authentication != managementidentity.TrustedNetwork {
			return tlsNode.dial(ctx, endpoint)
		}
		plaintextProbes++
		return plaintextDial(ctx, endpoint)
	}

	_, _, err = world.enable()
	if err == nil || !strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority; the node may still trust its previous management authority") {
		t.Fatalf("enable-mtls = %v, want certificate verification failure", err)
	}
	plaintext.rebinds.Wait()
	plaintext.mu.Lock()
	defer plaintext.mu.Unlock()
	if plaintextProbes != 0 || plaintext.begins != 0 || plaintext.staged.ServerPrivateKey != "" {
		t.Errorf("verification failure allowed %d plaintext connections, %d Begin calls, key received = %t",
			plaintextProbes, plaintext.begins, plaintext.staged.ServerPrivateKey != "")
	}
}

func TestEnableMTLSDoesNotDowngradeOnRPCErrorText(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := managementidentity.EnsureNode(&bundle, node.name, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
		t.Fatal(err)
	}

	// Model a gRPC connection failure with peer-controlled text and no
	// authenticated peer or wire evidence. Plaintext would reach the node.
	const marker = "received plaintext HTTP/2 SETTINGS during TLS handshake"
	probe := &fakeKatlcAgentClient{nodeStatusErr: grpcstatus.Error(codes.Unavailable, marker)}
	plaintextDial := dialKatlcAgent
	plaintextProbes := 0
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		if transport.ClientCredentialsFromContext(ctx).Authentication != managementidentity.TrustedNetwork {
			return katlcAgentConnection{Client: probe, Close: func() error { return nil }}, nil
		}
		plaintextProbes++
		return plaintextDial(ctx, endpoint)
	}

	_, _, err = world.enable()

	node.rebinds.Wait()
	node.mu.Lock()
	defer node.mu.Unlock()
	if plaintextProbes != 0 || node.begins != 0 || node.staged.ServerPrivateKey != "" {
		t.Errorf("RPC error text allowed %d plaintext connections, %d Begin calls, key received = %t",
			plaintextProbes, node.begins, node.staged.ServerPrivateKey != "")
	}
	if err == nil || !strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority; the node may still trust its previous management authority") {
		t.Errorf("enable-mtls = %v, want an unclassified transport refusal", err)
	}
}

func TestEnableMTLSRefusesUnclassifiedTransport(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := managementidentity.EnsureNode(&bundle, node.name, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
		t.Fatal(err)
	}

	// A preface-gated listener answers real plaintext gRPC but closes a TLS
	// connection without sending any bytes.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(prefaceCredentials{insecure.NewCredentials()}))
	agentapi.RegisterKatlcAgentServer(server, node)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
		return dialKatlcAgentTCP(ctx, listener.Addr().String())
	}
	if _, err := enableMTLSStatus(t.Context(), enableMTLSNode{name: node.name, endpoint: mustHostPort(t, listener.Addr().String())}, trustedNetworkClient, time.Second); err != nil {
		t.Fatalf("plaintext listener did not answer status: %v", err)
	}

	_, _, err = world.enable()
	if err == nil || !strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority; the node may still trust its previous management authority") || !strings.Contains(err.Error(), "check the management address and node reachability") {
		t.Fatalf("enable-mtls = %v, want transport classification guidance", err)
	}
	if strings.Contains(err.Error(), "different management authority") || strings.Contains(err.Error(), "node name") {
		t.Errorf("unclassified transport blamed identity: %v", err)
	}
	if mode, _, begins := node.state(); mode != managementidentity.TrustedNetwork || begins != 0 {
		t.Errorf("node = %s after %d begins, want trusted-network with no trial", mode, begins)
	}
}

type prefaceCredentials struct {
	credentials.TransportCredentials
}

func (c prefaceCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	reader := bufio.NewReader(conn)
	preface, err := reader.Peek(len(http2.ClientPreface))
	if err != nil {
		return nil, nil, err
	}
	if string(preface) != http2.ClientPreface {
		return nil, nil, fmt.Errorf("expected a plaintext HTTP/2 client preface")
	}
	return c.TransportCredentials.ServerHandshake(prefaceConn{Conn: conn, reader: reader})
}

type trialTLSCredentials struct {
	credentials.TransportCredentials
}

func (c trialTLSCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	reader := bufio.NewReader(conn)
	preface, err := reader.Peek(len(http2.ClientPreface))
	if err != nil {
		return nil, nil, err
	}
	if string(preface) == http2.ClientPreface {
		// bufconn rejects writes as soon as the peer closes. Read the initial
		// SETTINGS so the plaintext client can observe EOF on its read.
		_, _ = reader.Discard(len(preface))
		if _, err := http2.NewFramer(nil, reader).ReadFrame(); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("expected a TLS client hello")
	}
	return c.TransportCredentials.ServerHandshake(prefaceConn{Conn: conn, reader: reader})
}

type prefaceConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c prefaceConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// A node whose trial this workstation cannot reach is left to revert, the
// config keeps trusted-network, and a rerun with the same secrets finishes.
func TestEnableMTLSResumesAfterUnreachableTrialReverts(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1", "cp-2"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[1]
		node.unreachableTrial = true

		_, _, err := world.enable("--trial-window", "40s")
		if err == nil {
			t.Fatal("enable-mtls succeeded although cp-2's trial was unreachable")
		}
		for _, want := range []string{"cp-2: could not confirm the node accepts the replacement authority; the node may still trust the trusted network", "already on mTLS: cp-1", "keeps trusted-network", "katlctl management enable-mtls --config '" + world.configPath + "'"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Fatalf("config mode = %s before every node was confirmed", mode)
		}
		if mode, trial, _ := node.state(); mode != managementidentity.TrustedNetwork || trial != "trial" {
			t.Fatalf("cp-2 = %s %s after the connection timeout, want an unconfirmed trial", mode, trial)
		}

		node.rebinds.Wait()
		node.unreachableTrial = false
		node.revert("the transition was not confirmed before its deadline")
		node.rebinds.Wait()
		if mode, trial, _ := node.state(); mode != managementidentity.TrustedNetwork || trial != "reverted" {
			t.Fatalf("cp-2 = %s %s after revert, want trusted-network reverted", mode, trial)
		}

		_, stderr, err := world.enable()
		if err != nil {
			t.Fatalf("rerun: %v\n%s", err, stderr)
		}
		for _, want := range []string{"cp-1: already mTLS (1/2)", "earlier trial reverted (the transition was not confirmed before its deadline)", "cp-2: mTLS confirmed (2/2)"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("rerun progress lacks %q:\n%s", want, stderr)
			}
		}
		if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
			t.Errorf("config mode = %s after every node was confirmed", mode)
		}
		if _, _, begins := world.nodes[0].state(); begins != 1 {
			t.Errorf("cp-1 began %d trials, want 1", begins)
		}
	})
}

// A rerun reaches a node still on its trial over mTLS and confirms it without
// beginning again.
func TestEnableMTLSResumesLiveTrial(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.unreachableTrial = true
		if _, _, err := world.enable(); err == nil {
			t.Fatal("first run succeeded although the trial was unreachable")
		}

		node.mu.Lock()
		node.unreachableTrial = false
		node.mu.Unlock()
		node.rebind()
		node.rebinds.Wait()
		_, stderr, err := world.enable()
		if err != nil {
			t.Fatalf("rerun: %v\n%s", err, stderr)
		}
		if !strings.Contains(stderr, "cp-1: resuming its trial") {
			t.Errorf("rerun did not resume the trial:\n%s", stderr)
		}
		if mode, trial, begins := node.state(); mode != managementidentity.MutualTLS || trial != "committed" || begins != 1 {
			t.Errorf("node = %s %s after %d begins, want mtls committed after 1", mode, trial, begins)
		}
	})
}

func TestEnableMTLSDetectsLostConfirmReply(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	world.nodes[0].loseConfirmReply = true

	if _, stderr, err := world.enable(); err != nil {
		t.Fatalf("enable-mtls with a lost confirm reply: %v\n%s", err, stderr)
	}
	if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
		t.Errorf("config mode = %s after the committed node was observed", mode)
	}
	if world.nodes[0].confirms != 1 {
		t.Errorf("confirms = %d, want 1", world.nodes[0].confirms)
	}
}

func TestEnableMTLSInterruptedConfirmationCheck(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.afterConfirm = func(*trialNode) { cancel() }

	_, _, err := world.enableContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "could not confirm mTLS confirmation; the node may still trust the replacement authority") ||
		!strings.Contains(err.Error(), "Rerun katlctl management enable-mtls") {
		t.Fatalf("interrupted confirmation check = %v", err)
	}
	if strings.Contains(err.Error(), "returns to trusted-network") || strings.Contains(err.Error(), "management-trial revert") {
		t.Errorf("confirmation error promises a revert: %v", err)
	}
	if mode, trial, _ := node.state(); mode != managementidentity.MutualTLS || trial != "committed" {
		t.Fatalf("node state = %s %s, want committed mTLS", mode, trial)
	}
	if _, stderr, err := world.enable(); err != nil {
		t.Fatalf("rerun after interrupted confirmation: %v\n%s", err, stderr)
	}
}

func TestEnableMTLSRerunRepairsSavedContext(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"context", "save", "--config", world.configPath}, &out, &out); err != nil {
		t.Fatalf("save trusted-network context: %v\n%s", err, &out)
	}
	contextPath, err := workstation.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	oldContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := world.enable(); err != nil {
		t.Fatalf("enable-mtls: %v\n%s", err, stderr)
	}
	// Restore the context as it was just before the migration refreshed it.
	if err := os.WriteFile(contextPath, oldContext, 0o600); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, err := world.enable(); err != nil || !strings.Contains(stdout, "Refreshed the saved context for lab.") {
		t.Fatalf("rerun with stale context = %v\n%s%s", err, stdout, stderr)
	}
	out.Reset()
	if err := run(context.Background(), []string{"node", "status", "cp-1"}, &out, &out); err != nil || !strings.Contains(out.String(), "mtls") {
		t.Fatalf("node status through repaired context: %v\n%s", err, &out)
	}
}

func TestEnableMTLSLatePreflightFailure(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
		if err != nil {
			t.Fatal(err)
		}
		node := world.nodes[0]
		if _, _, err := managementidentity.EnsureNode(&bundle, node.name, time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
			t.Fatal(err)
		}

		// Ensure the TLS probe sees a plaintext SETTINGS frame before the
		// connection closes, so it has wire evidence for a fallback.
		listener := bufconn.Listen(32 << 10)
		defer listener.Close()
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
				_ = http2.NewFramer(conn, nil).WriteSettings()
				_, _ = io.Copy(io.Discard, conn)
			}
		}()
		dial := dialKatlcAgent
		delayed := false
		plaintextProbes := 0
		dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
			if transport.ClientCredentialsFromContext(ctx).Authentication == managementidentity.TrustedNetwork {
				plaintextProbes++
				return dial(ctx, endpoint)
			}
			if delayed {
				return dial(ctx, endpoint)
			}
			return node.dialWithOptions(
				ctx, endpoint,
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
				grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
					err := invoker(ctx, method, req, reply, cc, opts...)
					delayed = true
					// Keep the wire observation from this failed attempt
					// while delaying delivery of its result.
					_ = cc.Close()
					<-ctx.Done()
					return err
				}),
			)
		}
		_, _, err = world.enable("--timeout", "1s")
		if _, _, begins := node.state(); begins != 0 {
			t.Fatalf("began %d trials after the preflight failure arrived late", begins)
		}
		if plaintextProbes != 0 {
			t.Fatalf("made %d plaintext probes after the preflight failure arrived late", plaintextProbes)
		}
		if !delayed || err == nil || !strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority") || !strings.Contains(err.Error(), "Rerun katlctl management enable-mtls") {
			t.Fatalf("enable-mtls = %v, want guidance on the inconclusive mTLS preflight", err)
		}
	})
}

func TestEnableMTLSLateCommittedStatus(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1", "cp-2"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		delayed := false
		node.interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			mode, _, _ := node.state()
			if mode == managementidentity.MutualTLS && method == agentapi.KatlcAgent_GetNodeStatus_FullMethodName && err == nil {
				delayed = true
				_ = cc.Close()
				<-ctx.Done()
			}
			return err
		}

		stdout, stderr, err := world.enable("--timeout", "1s")
		if mode, trial, _ := node.state(); mode != managementidentity.MutualTLS || trial != "committed" {
			t.Fatalf("node = %s/%s, want committed mTLS: %v\n%s%s", mode, trial, err, stdout, stderr)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Fatalf("published %s after the committed status arrived late", mode)
		}
		if _, _, begins := world.nodes[1].state(); begins != 0 {
			t.Fatalf("began %d trials on cp-2 after the committed status arrived late", begins)
		}
		if !delayed || err == nil || !strings.Contains(err.Error(), "could not confirm mTLS confirmation; the node may still trust the replacement authority") ||
			!strings.Contains(err.Error(), "katlctl node status 'cp-1' --config '"+world.configPath+"'") ||
			strings.Contains(err.Error(), "management-trial revert") {
			t.Fatalf("enable-mtls = %v, want status guidance on the inconclusive confirmation", err)
		}
	})
}

func TestEnableMTLSLateContextRefresh(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		var out bytes.Buffer
		if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, &out, &out); err != nil {
			t.Fatalf("save trusted-network context: %v\n%s", err, &out)
		}
		contextPath, err := workstation.ConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(contextPath)
		if err != nil {
			t.Fatal(err)
		}
		node := world.nodes[0]
		delayed := false
		committedStatusChecks := 0
		node.interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			mode, _, _ := node.state()
			if err == nil && method == agentapi.KatlcAgent_GetNodeStatus_FullMethodName && mode == managementidentity.MutualTLS {
				committedStatusChecks++
				// The first two status calls check confirmation and health.
				if committedStatusChecks == 3 {
					delayed = true
					<-ctx.Done()
				}
			}
			return err
		}

		stdout, stderr, err := world.enable("--timeout", "1s")
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Fatalf("published %s after context verification arrived late: %v\n%s%s", mode, err, stdout, stderr)
		}
		after, readErr := os.ReadFile(contextPath)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("saved context changed after the refresh reply arrived late: %v", readErr)
		}
		if !delayed || err == nil || !strings.Contains(err.Error(), "could not confirm the saved context can reach the nodes over mTLS; the node may still trust the replacement authority") ||
			!strings.Contains(err.Error(), enableMTLSInvocation(world.configPath, world.secretsPath)) || strings.Contains(stdout, "Refreshed the saved context") {
			t.Fatalf("enable-mtls = %v\n%s\nwant context verification failure and rerun guidance", err, stdout)
		}
	})
}

func TestContextRebindLateStatus(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		var out bytes.Buffer
		if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, &out, &out); err != nil {
			t.Fatalf("save trusted-network context: %v\n%s", err, &out)
		}
		contextPath, err := workstation.ConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(contextPath)
		if err != nil {
			t.Fatal(err)
		}
		node := world.nodes[0]
		node.interceptRPC = delayManagementStatus
		dialKatlcAgent = node.dial

		err = run(t.Context(), []string{"context", "rebind", "--context-file", contextPath, "--node", node.name, "--endpoint", "192.0.2.91", "--timeout", "1s"}, &bytes.Buffer{}, &bytes.Buffer{})
		after, readErr := os.ReadFile(contextPath)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("saved endpoint changed after its verification reply arrived late: %v", readErr)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("context rebind = %v, want a deadline error", err)
		}
	})
}

func TestContextVerificationHandshakeFailure(t *testing.T) {
	for _, failure := range []string{"HTTP response", "untrusted certificate"} {
		t.Run(failure, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
				if _, stderr, err := world.enable(); err != nil {
					t.Fatalf("enable-mtls: %v\n%s", err, stderr)
				}
				save := []string{"context", "save", "--config", world.configPath}
				if err := run(t.Context(), save, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
				contextPath, err := workstation.ConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(contextPath)
				if err != nil {
					t.Fatal(err)
				}

				node := world.nodes[0]
				var handshakeError string
				switch failure {
				case "HTTP response":
					handshakeError = "first record does not look like a TLS handshake"
					dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
						return node.dialWithOptions(ctx, endpoint, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
							client, server := net.Pipe()
							go func() {
								defer server.Close()
								var header [5]byte
								if _, err := io.ReadFull(server, header[:]); err != nil {
									return
								}
								if _, err := io.CopyN(io.Discard, server, int64(binary.BigEndian.Uint16(header[3:]))); err == nil {
									_, _ = io.WriteString(server, "HTTP/1.1 404 Not Found\r\n\r\n")
								}
							}()
							return client, nil
						}))
					}
				case "untrusted certificate":
					handshakeError = "certificate signed by unknown authority"
					other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other"})
					if err != nil {
						t.Fatal(err)
					}
					credentials, _, err := managementidentity.EnsureNode(&other, node.name, time.Now(), nil)
					if err != nil {
						t.Fatal(err)
					}
					node.mu.Lock()
					node.staged = credentials
					node.rebindLocked()
					node.mu.Unlock()
					dialKatlcAgent = node.dial
				}
				rebind := []string{"context", "rebind", "--node", node.name, "--endpoint", "192.0.2.91"}
				for _, args := range [][]string{save, rebind} {
					err := run(t.Context(), args, io.Discard, io.Discard)
					if err == nil {
						t.Fatalf("%s accepted the endpoint", args[1])
					}
					for _, want := range []string{handshakeError, "could not confirm", "the node may still trust", "rerun katlctl context " + args[1]} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("%s error = %v, missing %q", args[1], err, want)
						}
					}
					for _, unsafe := range []string{"managementAuthentication: trusted-network", "restore the installed authority"} {
						if strings.Contains(err.Error(), unsafe) {
							t.Errorf("error suggests a trust change after a failed handshake: %v", err)
						}
					}
					after, readErr := os.ReadFile(contextPath)
					if readErr != nil || !bytes.Equal(before, after) {
						t.Fatalf("failed verification changed the context: %v", readErr)
					}
				}
			})
		})
	}
}

func delayManagementStatus(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	if err == nil && method == agentapi.KatlcAgent_GetNodeStatus_FullMethodName {
		<-ctx.Done()
	}
	return err
}

func TestEnableMTLSResumeKeepsSecretsFile(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		world.nodes[0].unreachableTrial = true
		secrets := filepath.Join(t.TempDir(), "lab-secrets.yaml")

		_, _, err := world.enable("--output", secrets)
		if err == nil || !strings.Contains(err.Error(), "--output '"+secrets+"'") {
			t.Fatalf("resume hint does not name the secrets file: %v", err)
		}
	})
}

func TestManagementStatusColumn(t *testing.T) {
	for name, tc := range map[string]struct {
		status *agentapi.ManagementStatus
		mode   string
		detail string
	}{
		"older node": {mode: "-"},
		"mtls":       {status: &agentapi.ManagementStatus{Authentication: "mtls", Authority: "abc"}, mode: "mtls"},
		"trial": {
			status: &agentapi.ManagementStatus{Authentication: "trusted-network", Transition: &agentapi.ManagementTransition{State: "trial", Deadline: "2026-11-25T12:00:00Z"}},
			mode:   "trusted-network (mtls trial)", detail: "mTLS trial is active",
		},
		"reverted": {
			status: &agentapi.ManagementStatus{Authentication: "trusted-network", Transition: &agentapi.ManagementTransition{State: "reverted", RevertReason: "deadline passed"}},
			mode:   "trusted-network", detail: "last mTLS trial reverted: deadline passed",
		},
		"committed": {status: &agentapi.ManagementStatus{Authentication: "mtls", Transition: &agentapi.ManagementTransition{State: "committed"}}, mode: "mtls"},
	} {
		t.Run(name, func(t *testing.T) {
			status := enrolledStatus("cp-1", "e", "m")
			status.Management = tc.status
			report := newHostStatusReport("cp-1", "127.0.0.1:9443", status, &agentapi.Generation{GenerationId: "0"}, time.Now())
			var out bytes.Buffer
			if err := writeHostStatus(&out, "text", report, false); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(out.String(), "\n")
			if fields := strings.Fields(lines[1]); !strings.HasSuffix(strings.Join(fields, " "), tc.mode) {
				t.Errorf("status row = %q, want management %q", lines[1], tc.mode)
			}
			if tc.detail == "" && strings.Contains(out.String(), "management:") || tc.detail != "" && !strings.Contains(out.String(), tc.detail) {
				t.Errorf("status = %q, want detail %q", out.String(), tc.detail)
			}
		})
	}
}

func (n *trialNode) GetGeneration(context.Context, *agentapi.GetGenerationRequest) (*agentapi.Generation, error) {
	return &agentapi.Generation{GenerationId: "generation-0"}, nil
}

// serveForeignAuthority moves the node to mTLS with an authority the
// workstation does not hold, as if an earlier run lost its secrets.
func (n *trialNode) serveForeignAuthority(t *testing.T) {
	t.Helper()
	other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _, err := managementidentity.EnsureNode(&other, n.name, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.mode, n.authority, n.staged = managementidentity.MutualTLS, "foreign", credentials
	n.rebindLocked()
	n.mu.Unlock()
	n.rebinds.Wait()
}

// useMTLSConfig makes the ClusterConfig say mtls with the default secrets
// file, as a hand edit or an earlier tool might have left it.
func (w enableMTLSWorld) useMTLSConfig(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "managementAuthentication: trusted-network", "managementAuthentication: mtls\n  managementIdentity: "+defaultManagementSecrets, 1)
	if err := os.WriteFile(w.configPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnableMTLSTrialWindowBounds(t *testing.T) {
	for _, window := range []string{"29s", "30m1s", "90500ms"} {
		t.Run("refuses "+window, func(t *testing.T) {
			world := newEnableMTLSWorld(t, "cp-1")
			_, _, err := world.enable("--trial-window", window)
			if err == nil || !strings.Contains(err.Error(), "whole number of seconds from 30s to 30m0s") {
				t.Fatalf("enable-mtls --trial-window %s = %v, want the node's bounds", window, err)
			}
			if _, err := os.Stat(world.secretsPath); !os.IsNotExist(err) {
				t.Errorf("refused window created secrets: %v", err)
			}
			if _, _, begins := world.nodes[0].state(); begins != 0 {
				t.Error("refused window began a trial")
			}
		})
	}
	for _, window := range []string{"30s", "30m"} {
		t.Run("accepts "+window, func(t *testing.T) {
			world := newEnableMTLSWorld(t, "cp-1")
			if _, stderr, err := world.enable("--trial-window", window); err != nil {
				t.Fatalf("enable-mtls --trial-window %s: %v\n%s", window, err, stderr)
			}
		})
	}
}

// A node clock behind by less than the certificate backdate still validates
// the new certificates, so preflight lets it through.
func TestEnableMTLSAcceptsClockWithinBackdate(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	world.nodes[0].clockOffset = -4 * time.Minute
	if _, stderr, err := world.enable(); err != nil {
		t.Fatalf("enable-mtls with a clock 4m behind: %v\n%s", err, stderr)
	}
}

func TestEnableMTLSInterruptedBegin(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	node.clockOffset = -4 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.afterBegin = func(context.Context) { cancel() }

	_, _, err := world.enableContext(ctx, "--trial-window", "45s")
	if err == nil {
		t.Fatal("enable-mtls succeeded although it was interrupted")
	}
	for _, want := range []string{"cp-1: could not confirm the mTLS trial started; the node may still trust the trusted network", "Rerun katlctl management enable-mtls"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, trial, _ := node.state(); trial != "trial" {
		t.Fatalf("node trial state = %q; the interrupted Begin should still have reached it", trial)
	}

	node.rebinds.Wait()
	node.mu.Lock()
	node.afterBegin = nil
	node.mu.Unlock()
	_, stderr, err := world.enable()
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "cp-1: resuming its trial") {
		t.Errorf("rerun did not resume the trial:\n%s", stderr)
	}
	if mode, _, begins := node.state(); mode != managementidentity.MutualTLS || begins != 1 {
		t.Errorf("node = %s after %d begins, want mtls after 1", mode, begins)
	}
}

func TestEnableMTLSRefusesTrialWithoutNodeTime(t *testing.T) {
	world, _ := newManagementStatusTrial(t, -4*time.Minute)
	node := world.nodes[0]
	node.mu.Lock()
	node.omitNodeTime = true
	node.mu.Unlock()

	_, _, err := world.enable()
	if err == nil || !strings.Contains(err.Error(), "upgrade the node with `katlctl node upgrade`, then rerun") {
		t.Fatalf("rerun = %v, want upgrade guidance", err)
	}
	if mode, trial, _ := node.state(); mode != managementidentity.TrustedNetwork || trial != "trial" {
		t.Errorf("rerun changed the trial: mode = %s, state = %s", mode, trial)
	}
	if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
		t.Errorf("rerun published %s without node time", mode)
	}
}

func TestEnableMTLSBeginTimeout(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err == nil && method == agentapi.KatlcAgent_BeginManagementTransition_FullMethodName {
				<-ctx.Done()
			}
			return err
		}

		_, _, err := world.enable("--timeout", "1s", "--trial-window", "45s")
		if mode, trial, begins := node.state(); mode != managementidentity.TrustedNetwork || trial != "trial" || begins != 1 {
			t.Fatalf("node = %s %s after %d begins, want one unconfirmed trial", mode, trial, begins)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Errorf("config mode = %s after Begin timed out", mode)
		}
		for _, want := range []string{
			"could not confirm the mTLS trial started; the node may still trust the trusted network",
			"Rerun katlctl management enable-mtls",
		} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if err != nil && strings.Contains(err.Error(), "by itself by") {
			t.Errorf("interrupted Begin reported an unverified trial deadline: %v", err)
		}
	})
}

func TestEnableMTLSRejectsReplyAfterDeadline(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		// Model an inherited deadline whose cancellation callback is delayed.
		ctx := deadlineOnlyContext{Context: t.Context(), deadline: time.Now().Add(time.Second)}
		world.nodes[0].interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err != nil {
				t.Fatalf("RPC failed before delay: %v", err)
			}
			time.Sleep(time.Second)
			if err := ctx.Err(); err != nil {
				t.Fatalf("request canceled before the reply: %v", err)
			}
			return nil
		}
		node := enableMTLSNode{name: "cp-1", endpoint: mustManagement(t, "192.0.2.11").Endpoint()}
		_, err := enableMTLSStatus(ctx, node, trustedNetworkClient, time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, transport.ErrManagementRPCInconclusive) {
			t.Fatalf("late RPC reply = %v, want deadline exceeded", err)
		}
	})
}

type deadlineOnlyContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineOnlyContext) Deadline() (time.Time, bool) { return c.deadline, true }

// A trial past its deadline that the node has not yet reverted cannot be
// confirmed; the operator waits for the revert rather than chasing the
// address.
func TestEnableMTLSWaitsForExpiredTrialToRevert(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.unreachableTrial = true
		if _, _, err := world.enable(); err == nil {
			t.Fatal("first run succeeded although the trial was unreachable")
		}

		node.mu.Lock()
		node.unreachableTrial = false
		node.trial.Deadline = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
		node.mu.Unlock()
		node.rebind()
		node.rebinds.Wait()
		_, _, err := world.enable()
		if err == nil || !strings.Contains(err.Error(), "has not returned to trusted-network yet; wait for it to revert, then rerun") {
			t.Fatalf("rerun = %v, want a wait for the revert", err)
		}
		if strings.Contains(err.Error(), "management.address") {
			t.Errorf("error blames the address: %v", err)
		}
		if node.confirms != 0 {
			t.Errorf("confirmed an expired trial %d time(s)", node.confirms)
		}
	})
}

func TestEnableMTLSCompletesAfterWriteBackFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	world := newEnableMTLSWorld(t, "cp-1")
	secrets := filepath.Join(t.TempDir(), "lab-secrets.yaml")
	dir := filepath.Dir(world.configPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, _, err := world.enable("--output", secrets)
	relative, relErr := filepath.Rel(dir, secrets)
	if relErr != nil {
		t.Fatal(relErr)
	}
	if err == nil {
		t.Fatal("enable-mtls succeeded although the ClusterConfig is read-only")
	}
	for _, want := range []string{
		"every node now uses mTLS management",
		"rerun katlctl management enable-mtls --config '" + world.configPath + "' --output '" + secrets + "'",
		"katlctl context save --config '" + world.configPath + "'",
		"\n  managementAuthentication: mtls\n  managementIdentity: " + filepath.ToSlash(relative),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
		t.Fatalf("config mode = %s although the write-back failed", mode)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := world.enable("--output", secrets)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "cp-1: already mTLS (1/1)") {
		t.Errorf("rerun progress:\n%s", stderr)
	}
	if mode, reference := world.configMode(t); mode != managementidentity.MutualTLS || reference != filepath.ToSlash(relative) {
		t.Errorf("config = %s with identity %q, want mtls referencing %s", mode, reference, relative)
	}
	if _, _, begins := world.nodes[0].state(); begins != 1 {
		t.Errorf("began %d trials, want 1", begins)
	}
}

// A control plane whose API proxy stops being ready after it moves fails the
// post-confirm health gate, and a rerun continues once it recovers.
func TestEnableMTLSChecksAPIProxyAfterConfirm(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.controlPlane = true
		node.afterConfirm = func(n *trialNode) { n.apiProxyState = "starting" }

		_, _, err := world.enable()
		if err == nil || !strings.Contains(err.Error(), "could not confirm the node is healthy over mTLS") || !strings.Contains(err.Error(), "Kubernetes API proxy: starting") {
			t.Fatalf("enable-mtls = %v, want the API proxy health failure", err)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Fatalf("config mode = %s although a node failed its health check", mode)
		}

		if _, _, err := world.enable(); err == nil || !strings.Contains(err.Error(), "Kubernetes API proxy: starting") {
			t.Fatalf("rerun with a degraded API proxy = %v", err)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
			t.Fatalf("rerun published %s while the API proxy was unhealthy", mode)
		}

		node.mu.Lock()
		node.apiProxyState = "ready"
		node.mu.Unlock()
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("rerun: %v\n%s", err, stderr)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
			t.Errorf("config mode = %s after the node recovered", mode)
		}
	})
}

func TestEnableMTLSChecksInstalledVIPAfterConfirm(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		// The ClusterConfig names the API endpoint without VIP advertisement.
		node := world.nodes[0]
		node.controlPlane = true
		node.endpoint = &agentapi.ControlPlaneEndpointStatus{
			Endpoint: "api.katl.test:6443", Vip: "192.0.2.10/32",
			State: "active", LocalApiReady: true, LocalVipOwned: true,
		}
		node.afterConfirm = func(n *trialNode) {
			n.endpoint.State = "released"
			n.endpoint.LocalVipOwned = false
		}

		for _, attempt := range []string{"initial migration", "rerun"} {
			if stdout, _, err := world.enable(); err == nil || !strings.Contains(err.Error(), "managed API endpoint is released") {
				t.Fatalf("%s = %v, want the installed VIP health failure\n%s", attempt, err, stdout)
			}
			if mode, trial, begins := node.state(); mode != managementidentity.MutualTLS || trial != "committed" || begins != 1 {
				t.Fatalf("%s: node = %s %s after %d begins, want one committed trial", attempt, mode, trial, begins)
			}
			if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
				t.Fatalf("%s published %s while the VIP was released", attempt, mode)
			}
		}

		node.mu.Lock()
		node.endpoint.State = "active"
		node.endpoint.LocalVipOwned = true
		node.mu.Unlock()
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("rerun after VIP ownership was restored: %v\n%s", err, stderr)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
			t.Errorf("config = %s after VIP ownership was restored, want mtls", mode)
		}
		if _, _, begins := node.state(); begins != 1 {
			t.Errorf("began %d trials, want 1", begins)
		}
	})
}

func TestEnableMTLSAllowsExternalControlPlaneEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint *agentapi.ControlPlaneEndpointStatus
	}{
		{name: "no VIP configured"},
		{
			name: "VIP ownership disabled",
			endpoint: &agentapi.ControlPlaneEndpointStatus{
				Endpoint: "api.katl.test:6443", Vip: "192.0.2.10/32", State: "disabled",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newEnableMTLSWorld(t, "cp-1")
			node := world.nodes[0]
			node.controlPlane = true
			node.endpoint = tc.endpoint

			if _, stderr, err := world.enable(); err != nil {
				t.Fatalf("enable-mtls with an external endpoint: %v\n%s", err, stderr)
			}
			if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
				t.Errorf("config = %s with an external endpoint, want mtls", mode)
			}
		})
	}
}

func TestEnableMTLSRerunRequiresWorkerReady(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
		worker := world.nodes[1]
		worker.kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "worker", KubeletActive: true, NodeReady: true}
		worker.afterConfirm = func(n *trialNode) {
			n.kubernetes.State = "waiting-for-node"
			n.kubernetes.NodeReady = false
		}

		for _, attempt := range []string{"initial migration", "rerun"} {
			if _, _, err := world.enable(); err == nil || !strings.Contains(err.Error(), "Kubernetes node is not Ready") {
				t.Fatalf("%s = %v, want a worker readiness failure", attempt, err)
			}
			if mode, trial, begins := worker.state(); mode != managementidentity.MutualTLS || trial != "committed" || begins != 1 {
				t.Fatalf("%s: worker = %s %s after %d begins, want one committed trial", attempt, mode, trial, begins)
			}
			if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
				t.Fatalf("%s published %s with a NotReady worker", attempt, mode)
			}
		}

		worker.mu.Lock()
		worker.kubernetes.State = "ready"
		worker.kubernetes.NodeReady = true
		worker.mu.Unlock()
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("rerun after the worker recovered: %v\n%s", err, stderr)
		}
		if mode, _ := world.configMode(t); mode != managementidentity.MutualTLS {
			t.Errorf("config = %s after recovery, want mtls", mode)
		}
	})
}

func TestEnableMTLSRetainsHealthDiagnosisAtDeadline(t *testing.T) {
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	status := &agentapi.NodeStatus{
		InventoryNodeName: "cp-1",
		EnrollmentId:      "enrollment-1",
		MachineId:         "machine-1",
		BootHealthState:   bootHealthHealthy,
		Kubernetes:        &agentapi.KubernetesStatus{State: "ready", KubeletActive: true},
		ApiProxy:          &agentapi.APIProxyStatus{State: "starting"},
		Management:        &agentapi.ManagementStatus{Authentication: "mtls", Authority: "replacement"},
	}
	var client agentapi.KatlcAgentClient = &fakeKatlcAgentClient{nodeStatus: status}
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		conn := katlcAgentConnection{Client: client, Close: func() error { return nil }}
		client = &deadlineKatlcAgentClient{}
		return conn, nil
	}
	identity, err := transport.ObserveNode("cp-1", status)
	if err != nil {
		t.Fatal(err)
	}
	node := enableMTLSNode{name: "cp-1", identity: identity, requirement: nodeRecoveryRequirement{APIProxyReady: true}}

	synctest.Test(t, func(t *testing.T) {
		err := waitEnableMTLSHealthy(t.Context(), node, managementidentity.ClientCredentials{}, "replacement", 2*enableMTLSPoll)
		if err == nil || !strings.Contains(err.Error(), "Kubernetes API proxy: starting") {
			t.Fatalf("health check = %v, want the API proxy diagnosis", err)
		}
	})
}

// A config that already says mtls but whose nodes still serve trusted-network
// and whose secrets file does not exist is where 'config init --force' can
// leave an operator; enable-mtls starts from it.
func TestEnableMTLSFromMTLSConfigWithoutSecrets(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1", "cp-2")
	world.useMTLSConfig(t)

	stdout, stderr, err := world.enable()
	if err != nil {
		t.Fatalf("enable-mtls: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "Created management secrets at "+world.secretsPath) {
		t.Errorf("stdout does not name the new secrets:\n%s", stdout)
	}
	bundle, err := readManagementIdentity(world.secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := managementidentity.Validate(bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range world.nodes {
		if mode, _, _ := node.state(); mode != managementidentity.MutualTLS || node.authority != info.Fingerprint {
			t.Errorf("%s: mode %s authority %s, want mtls with %s", node.name, mode, node.authority, info.Fingerprint)
		}
	}
	if mode, reference := world.configMode(t); mode != managementidentity.MutualTLS || reference != defaultManagementSecrets {
		t.Errorf("config = %s with identity %q, want mtls referencing %s", mode, reference, defaultManagementSecrets)
	}
}

// When a node already serves an authority other than the one at the
// configured path, enable-mtls changes nothing and names that path, not
// --output, which an mtls config refuses.
func TestEnableMTLSRefusesNodeOnOtherAuthority(t *testing.T) {
	for name, existingSecrets := range map[string]bool{"no secrets": false, "other secrets": true} {
		t.Run(name, func(t *testing.T) {
			world := newEnableMTLSWorld(t, "cp-1", "cp-2")
			world.useMTLSConfig(t)
			if existingSecrets {
				bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range world.nodes {
					if _, _, err := managementidentity.EnsureNode(&bundle, node.name, time.Now(), nil); err != nil {
						t.Fatal(err)
					}
				}
				if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
					t.Fatal(err)
				}
			}
			world.nodes[1].serveForeignAuthority(t)

			_, _, err := world.enable()
			if err == nil || !strings.Contains(err.Error(), "no node was changed") || !strings.Contains(err.Error(), "could not confirm") {
				t.Fatalf("enable-mtls = %v, want an inconclusive preflight", err)
			}
			if strings.Contains(err.Error(), "--output") {
				t.Errorf("refusal suggests --output, which this config refuses: %v", err)
			}
			if _, _, begins := world.nodes[0].state(); begins != 0 {
				t.Error("refusal began a trial on cp-1")
			}
			if _, err := os.Stat(world.secretsPath); !existingSecrets && !os.IsNotExist(err) {
				t.Errorf("refusal created secrets: %v", err)
			}
		})
	}
}
