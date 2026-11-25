package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// trialNode models the node side of the trust transition: one address that
// serves plaintext until a trial begins, then only the staged credentials, as
// katlc-agent does.
type trialNode struct {
	agentapi.UnimplementedKatlcAgentServer
	t       *testing.T
	name    string
	address string

	mu        sync.Mutex
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
	etcdHealthy   uint32
	apiProxyState string
	// Fault knobs.
	clockOffset      time.Duration
	legacy           bool
	unreachableTrial bool
	loseConfirmReply bool
	healthState      string
	// onBegin runs when a Begin request arrives, before the trial starts.
	onBegin func()
	// afterConfirm runs, with the node locked, once a confirm commits.
	afterConfirm func(*trialNode)
}

func newTrialNode(t *testing.T, name string) *trialNode {
	t.Helper()
	node := &trialNode{t: t, name: name, address: "127.0.0.1:0", mode: managementidentity.TrustedNetwork, healthState: bootHealthHealthy, etcdHealthy: 3, apiProxyState: "ready"}
	node.serve()
	t.Cleanup(func() {
		node.rebinds.Wait()
		node.mu.Lock()
		defer node.mu.Unlock()
		node.server.Stop()
	})
	return node
}

// serve binds the node's address with the credentials its state selects.
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
		options = append(options, grpc.Creds(credentials.NewTLS(config)))
	}
	var listener net.Listener
	var err error
	for range 100 {
		if listener, err = net.Listen("tcp", n.address); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		n.t.Error(err)
		return
	}
	n.address = listener.Addr().String()
	n.server = grpc.NewServer(options...)
	agentapi.RegisterKatlcAgentServer(n.server, n)
	go func() { _ = n.server.Serve(listener) }()
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
	if !n.legacy {
		status.Management = &agentapi.ManagementStatus{
			Authentication: string(n.mode), Authority: n.authority,
			Transition: proto.Clone(n.trial).(*agentapi.ManagementTransition),
			NodeTime:   time.Now().Add(n.clockOffset).UTC().Format(time.RFC3339Nano),
		}
		if n.trial == nil {
			status.Management.Transition = nil
		}
	}
	return status, nil
}

func (n *trialNode) BeginManagementTransition(_ context.Context, req *agentapi.BeginManagementTransitionRequest) (*agentapi.ManagementTransition, error) {
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
	if n.onBegin != nil {
		n.onBegin()
	}
	n.begins++
	n.staged = managementidentity.NodeCredentials{CACertificate: req.GetCaCertificate(), ServerCertificate: req.GetServerCertificate(), ServerPrivateKey: req.GetServerPrivateKey()}
	n.trial = &agentapi.ManagementTransition{
		State: "trial", TrialId: fmt.Sprintf("trial-%d", n.begins), TargetAuthority: target,
		Deadline: time.Now().Add(time.Duration(req.GetTrialSeconds()) * time.Second).UTC().Format(time.RFC3339),
	}
	result := proto.Clone(n.trial).(*agentapi.ManagementTransition)
	n.rebindLocked()
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
		fmt.Fprintf(&nodes, "    - name: %s\n      controlPlane: true\n      management:\n        address: %s\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-%s-root\n", name, address, name)
	}
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		node, ok := listeners[endpoint]
		if !ok {
			return katlcAgentConnection{}, fmt.Errorf("no test node at %s", endpoint)
		}
		node.mu.Lock()
		address := node.address
		node.mu.Unlock()
		return dialKatlcAgentTCP(ctx, address)
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
		"unreachable":       {fault: func(n *trialNode) { n.mu.Lock(); n.server.Stop(); n.mu.Unlock() }, want: []string{"cp-2 is unreachable", "fix it or remove it from the config"}},
		"older node":        {fault: func(n *trialNode) { n.legacy = true }, want: []string{"cp-2 runs a KatlOS release that cannot move management trust", "upgrade it first"}},
		"clock behind":      {fault: func(n *trialNode) { n.clockOffset = -6 * time.Minute }, want: []string{"cp-2's clock is 6m0s behind", "NTP"}},
		"etcd below quorum": {fault: func(n *trialNode) { n.controlPlane, n.etcdHealthy = true, 1 }, want: []string{"cp-2 is not healthy", "etcd has 1 healthy members, below quorum 2"}},
		"unhealthy":         {fault: func(n *trialNode) { n.healthState = "degraded" }, want: []string{"cp-2 is not healthy", "boot health is degraded"}},
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

// A node whose trial this workstation cannot reach is left to revert, the
// config keeps trusted-network, and a rerun with the same secrets finishes.
func TestEnableMTLSResumesAfterUnreachableTrialReverts(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1", "cp-2")
	world.nodes[1].unreachableTrial = true

	_, _, err := world.enable("--trial-window", "40s")
	if err == nil {
		t.Fatal("enable-mtls succeeded although cp-2's trial was unreachable")
	}
	for _, want := range []string{"cp-2: did not answer over mTLS", "returns to trusted-network by itself", "already on mTLS: cp-1", "keeps trusted-network", "katlctl management enable-mtls --config '" + world.configPath + "'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
		t.Fatalf("config mode = %s before every node was confirmed", mode)
	}

	world.nodes[1].unreachableTrial = false
	world.nodes[1].revert("the transition was not confirmed before its deadline")
	world.nodes[1].rebinds.Wait()
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
}

// A rerun reaches a node still on its trial over mTLS and confirms it without
// beginning again.
func TestEnableMTLSResumesLiveTrial(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
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

func TestEnableMTLSResumeKeepsSecretsFile(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	world.nodes[0].unreachableTrial = true
	secrets := filepath.Join(t.TempDir(), "lab-secrets.yaml")

	_, _, err := world.enable("--output", secrets)
	if err == nil || !strings.Contains(err.Error(), "--output '"+secrets+"'") {
		t.Fatalf("resume hint does not name the secrets file: %v", err)
	}
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
			mode:   "trusted-network (mtls trial)", detail: "mTLS trial until 2026-11-25T12:00:00Z",
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
			report := newHostStatusReport("cp-1", "127.0.0.1:9443", status, &agentapi.Generation{GenerationId: "0"})
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.onBegin = cancel

	_, _, err := world.enableContext(ctx)
	if err == nil {
		t.Fatal("enable-mtls succeeded although it was interrupted")
	}
	for _, want := range []string{"cp-1: interrupted before the new authority was confirmed", "returns to trusted-network by itself by", "Rerun katlctl management enable-mtls"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, trial, _ := node.state(); trial != "trial" {
		t.Fatalf("node trial state = %q; the interrupted Begin should still have reached it", trial)
	}

	node.rebinds.Wait()
	node.mu.Lock()
	node.onBegin = nil
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

// A trial past its deadline that the node has not yet reverted cannot be
// confirmed; the operator waits for the revert rather than chasing the
// address.
func TestEnableMTLSWaitsForExpiredTrialToRevert(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
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
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	node.controlPlane = true
	node.afterConfirm = func(n *trialNode) { n.apiProxyState = "starting" }

	_, _, err := world.enable()
	if err == nil || !strings.Contains(err.Error(), "uses mTLS management now but is not healthy") || !strings.Contains(err.Error(), "Kubernetes API proxy: starting") {
		t.Fatalf("enable-mtls = %v, want the API proxy health failure", err)
	}
	if mode, _ := world.configMode(t); mode != managementidentity.TrustedNetwork {
		t.Fatalf("config mode = %s although a node failed its health check", mode)
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
			if err == nil || !strings.Contains(err.Error(), "no node was changed") || !strings.Contains(err.Error(), "put the secrets file it was moved to at "+world.secretsPath) {
				t.Fatalf("enable-mtls = %v, want a preflight refusal naming %s", err, world.secretsPath)
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
