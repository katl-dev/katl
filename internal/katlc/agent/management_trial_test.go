package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type testTrialClock struct {
	mu        sync.Mutex
	bootID    string
	sinceBoot time.Duration
	now       time.Time
}

func (c *testTrialClock) BootID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bootID, nil
}

func (c *testTrialClock) SinceBoot() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sinceBoot, nil
}

func (c *testTrialClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testTrialClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sinceBoot += d
	c.now = c.now.Add(d)
}

type managementTrialNode struct {
	root       string
	server     *Server
	clock      *testTrialClock
	enrollment nodeidentity.Enrollment
	bundle     managementidentity.Bundle
	node       managementidentity.NodeCredentials
	authority  string

	mu       sync.Mutex
	commands [][]string
	fail     func([]string) bool
}

func newManagementTrialNode(t *testing.T) *managementTrialNode {
	t.Helper()
	root := t.TempDir()
	machineID, err := nodeidentity.WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := nodeidentity.WriteEnrollment(root, "cp-1", machineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteManagementIdentity(root, "cp-1", managementidentity.NodeCredentials{Authentication: managementidentity.TrustedNetwork}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := managementidentity.IssueNode(bundle, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := managementidentity.CAFingerprint(issued.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	storeRoot := filepath.Join(root, "var/lib/katl/operations")
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := operation.NewStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	node := &managementTrialNode{
		root: root, clock: &testTrialClock{bootID: "boot-a", sinceBoot: time.Hour, now: now},
		enrollment: enrollment, bundle: bundle, authority: authority,
		node: managementidentity.NodeCredentials{CACertificate: issued.CACertificate, ServerCertificate: issued.ServerCertificate, ServerPrivateKey: issued.ServerPrivateKey},
	}
	node.server = NewServer(root, store)
	node.server.ManagementTrialClock = node.clock
	node.server.RunManagementTrial = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.commands = append(node.commands, slices.Clone(argv))
		if node.fail != nil && node.fail(argv) {
			return ToolResult{ExitStatus: 1, Stderr: []byte("systemd refused")}
		}
		return ToolResult{}
	}
	return node
}

func (n *managementTrialNode) beginRequest() *agentapi.BeginManagementTransitionRequest {
	return &agentapi.BeginManagementTransitionRequest{
		ApiVersion: APIVersion, Kind: BeginManagementTransitionRequestKind, Actor: "test",
		ExpectedEnrollmentId: n.enrollment.ID, ExpectedInventoryNodeName: "cp-1", ExpectedMachineId: n.enrollment.MachineID,
		ExpectedAuthentication: string(managementidentity.TrustedNetwork),
		CaCertificate:          n.node.CACertificate, ServerCertificate: n.node.ServerCertificate, ServerPrivateKey: n.node.ServerPrivateKey,
		TrialSeconds: 90,
	}
}

func (n *managementTrialNode) confirmRequest() *agentapi.ConfirmManagementTransitionRequest {
	return &agentapi.ConfirmManagementTransitionRequest{
		ApiVersion: APIVersion, Kind: ConfirmManagementTransitionRequestKind, Actor: "test",
		ExpectedEnrollmentId: n.enrollment.ID, ExpectedInventoryNodeName: "cp-1", ExpectedMachineId: n.enrollment.MachineID,
		TargetAuthority: n.authority,
	}
}

// takeCommands returns and forgets the systemd commands run so far.
func (n *managementTrialNode) takeCommands() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	commands := n.commands
	n.commands = nil
	return commands
}

func (n *managementTrialNode) mode(t *testing.T) managementidentity.Authentication {
	t.Helper()
	mode, err := nodeidentity.ManagementAuthentication(n.root)
	if err != nil {
		t.Fatal(err)
	}
	return mode
}

func isRevertTimer(argv []string, trialID string) bool {
	return len(argv) > 1 && argv[0] == "systemd-run" && argv[1] == "--unit="+managementTrialRevertUnitPrefix+trialID &&
		slices.Contains(argv, "--trial="+trialID) && slices.Contains(argv, "revert")
}

func isTimerStop(argv []string, trialID string) bool {
	return slices.Equal(argv, []string{"systemctl", "stop", managementTrialRevertUnitPrefix + trialID + ".timer"})
}

// serve runs the agent's listener loop, which serves whatever the node's
// management state selects, and returns its address.
func (n *managementTrialNode) serve(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- n.server.serveManagement(ctx, "tcp", address, func() error { close(ready); return nil })
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve = %v", err)
	}
	return address
}

// trialClient presents the replacement authority's operator certificate.
func (n *managementTrialNode) trialClient(t *testing.T, address string) agentapi.KatlcAgentClient {
	t.Helper()
	client, err := managementidentity.Client(n.bundle, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return n.dial(t, address, client)
}

func plaintextClient(t *testing.T, address string) agentapi.KatlcAgentClient {
	t.Helper()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentapi.NewKatlcAgentClient(conn)
}

// answers reports whether the listener completes a call from client. A
// confirm for an unknown authority is refused by the handler, so any answer
// other than Unavailable shows the connection was accepted.
func (n *managementTrialNode) answers(t *testing.T, client agentapi.KatlcAgentClient) bool {
	t.Helper()
	request := n.confirmRequest()
	request.TargetAuthority = "unknown"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := client.ConfirmManagementTransition(ctx, request)
	return status.Code(err) == codes.PermissionDenied
}

// eventually waits for the listener loop to reach a condition.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (n *managementTrialNode) dial(t *testing.T, address string, client managementidentity.ClientCredentials) agentapi.KatlcAgentClient {
	t.Helper()
	creds, err := transport.ClientCredentialsForNode(client, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentapi.NewKatlcAgentClient(conn)
}

func TestManagementTransitionCommitsOverTrialCredentials(t *testing.T) {
	node := newManagementTrialNode(t)
	node.server.managementTrialPoll = 10 * time.Millisecond
	begun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil || begun.State != "trial" || begun.TargetAuthority != node.authority || begun.Deadline == "" {
		t.Fatalf("begin = %+v, %v", begun, err)
	}
	commands := node.takeCommands()
	if len(commands) != 1 || !isRevertTimer(commands[0], begun.TrialId) || !slices.Contains(commands[0], "--on-active=90s") {
		t.Fatalf("begin ran %q; want only the revert timer for the trial window", commands)
	}
	if node.mode(t) != managementidentity.TrustedNetwork {
		t.Fatal("begin changed the installed mode")
	}

	address := node.serve(t)
	agent := node.trialClient(t, address)
	confirmed, err := agent.ConfirmManagementTransition(t.Context(), node.confirmRequest())
	if err != nil || confirmed.State != "committed" {
		t.Fatalf("confirm over the trial credentials = %+v, %v", confirmed, err)
	}
	commands = node.takeCommands()
	if len(commands) != 1 || !isTimerStop(commands[0], begun.TrialId) {
		t.Fatalf("confirm ran %q; want only the revert timer stopped", commands)
	}

	// A repeated confirmation, as after a lost response, succeeds again.
	if again, err := agent.ConfirmManagementTransition(t.Context(), node.confirmRequest()); err != nil || again.State != "committed" {
		t.Fatalf("repeated confirm = %+v, %v", again, err)
	}
	if commands := node.takeCommands(); len(commands) != 0 {
		t.Fatalf("repeated confirm ran %q", commands)
	}

	// The committed trial's deadline no longer applies.
	node.clock.advance(2 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if node.mode(t) != managementidentity.MutualTLS || !node.answers(t, agent) {
		t.Fatal("the agent stopped serving the committed credentials at the trial deadline")
	}
	installed, trial, err := managementServerTLS(node.root, node.clock)
	if err != nil || installed == nil || trial != nil {
		t.Fatalf("restarted agent serves %v, trial %+v, %v; want the installed mTLS credentials", installed, trial, err)
	}
	status, err := nodeManagementStatus(node.root)
	if err != nil || status.Authentication != "mtls" || status.Authority != node.authority || status.Transition != nil {
		t.Fatalf("management status = %+v, %v", status, err)
	}
}

// The agent serving a trial enforces its deadline itself, so a revert timer
// that never runs cannot leave installed clients locked out.
func TestServedTrialRevertsAtDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		// timerFirst reverts as the systemd timer would before the agent
		// notices the deadline.
		timerFirst bool
	}{
		{name: "without the revert timer"},
		{name: "after the revert timer", timerFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newManagementTrialNode(t)
			node.server.managementTrialPoll = 10 * time.Millisecond
			begun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
			if err != nil {
				t.Fatal(err)
			}
			address := node.serve(t)
			trialClient, plaintext := node.trialClient(t, address), plaintextClient(t, address)
			if !node.answers(t, trialClient) || node.answers(t, plaintext) {
				t.Fatal("the agent does not serve only the trial credentials")
			}
			node.takeCommands()

			const timerReason = "reverted by the systemd timer"
			if tc.timerFirst {
				if _, reverted, err := nodeidentity.RevertManagementTrial(node.root, begun.TrialId, timerReason, node.clock); err != nil || !reverted {
					t.Fatalf("timer revert = %v, %v", reverted, err)
				}
			}
			before, _, err := nodeidentity.ReadManagementTrial(node.root)
			if err != nil {
				t.Fatal(err)
			}
			node.clock.advance(2 * time.Minute)

			eventually(t, "installed clients reach the agent", func() bool { return node.answers(t, plaintext) })
			if node.answers(t, trialClient) {
				t.Fatal("the agent still serves the trial credentials after its deadline")
			}
			if node.mode(t) != managementidentity.TrustedNetwork {
				t.Fatal("the deadline committed the trial")
			}
			trial, _, err := nodeidentity.ReadManagementTrial(node.root)
			if err != nil || trial.State != nodeidentity.ManagementTrialReverted {
				t.Fatalf("trial = %+v, %v; want reverted", trial, err)
			}
			if tc.timerFirst && (trial.RevertReason != timerReason || !trial.RevertedAt.Equal(*before.RevertedAt)) {
				t.Fatalf("trial = %+v; want the timer's revert kept", trial)
			}
			if !tc.timerFirst && trial.RevertReason != managementTrialDeadlineReason {
				t.Fatalf("revert reason = %q", trial.RevertReason)
			}
			if commands := node.takeCommands(); len(commands) != 0 {
				t.Fatalf("the agent ran %q to end the trial; want it to re-serve without systemd", commands)
			}

			// The timer firing after the agent finds nothing left to revert.
			if _, reverted, err := nodeidentity.RevertManagementTrial(node.root, begun.TrialId, timerReason, node.clock); err != nil || reverted {
				t.Fatalf("late timer revert = %v, %v; want nothing to revert", reverted, err)
			}
		})
	}
}

func TestManagementTrialServesOnlyItsAuthority(t *testing.T) {
	node := newManagementTrialNode(t)
	if _, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest()); err != nil {
		t.Fatal(err)
	}
	address := node.serve(t)
	other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(other.Operator.Certificate), []byte(other.Operator.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	// Trust the trial's server certificate so only the client certificate
	// can be refused.
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(node.bundle.CertificateAuthority.Certificate))
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: "cp-1"}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := agentapi.NewKatlcAgentClient(conn).ConfirmManagementTransition(t.Context(), node.confirmRequest()); err == nil {
		t.Fatal("trial accepted a client certificate from another authority")
	}
	if node.mode(t) != managementidentity.TrustedNetwork {
		t.Fatal("a refused client changed the installed mode")
	}
}

func TestConfirmManagementTransitionRequiresTrialClient(t *testing.T) {
	node := newManagementTrialNode(t)
	if _, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest()); err != nil {
		t.Fatal(err)
	}
	other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "plaintext", ctx: t.Context()},
		{name: "other authority", ctx: peerContext(t, other)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := node.server.ConfirmManagementTransition(tc.ctx, node.confirmRequest())
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("confirm = %v; want permission denied", err)
			}
			if node.mode(t) != managementidentity.TrustedNetwork {
				t.Fatal("refused confirm changed the installed mode")
			}
		})
	}
	if _, err := node.server.ConfirmManagementTransition(peerContext(t, node.bundle), node.confirmRequest()); err != nil {
		t.Fatalf("confirm from the trial authority = %v", err)
	}
}

func TestConfirmAfterRevertAsksForRerun(t *testing.T) {
	node := newManagementTrialNode(t)
	// The listener checks the deadline before it serves the trial and then
	// not again for an hour, so the late confirm is what ends the trial.
	node.server.managementTrialPoll = time.Hour
	begun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil {
		t.Fatal(err)
	}
	address := node.serve(t)
	node.clock.advance(2 * time.Minute)
	node.takeCommands()

	_, err = node.trialClient(t, address).ConfirmManagementTransition(t.Context(), node.confirmRequest())
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("late confirm = %v; want a rerun request", err)
	}
	if node.mode(t) != managementidentity.TrustedNetwork {
		t.Fatal("late confirm committed the trial")
	}
	eventually(t, "installed clients reach the agent", func() bool { return node.answers(t, plaintextClient(t, address)) })
	if commands := node.takeCommands(); len(commands) != 1 || !isTimerStop(commands[0], begun.TrialId) {
		t.Fatalf("late confirm ran %q; want only the revert timer stopped", commands)
	}
}

// A trial begun on a running agent is served by the same process: restarting
// katlc-agent would rerun the boot units it requires.
func TestBeginManagementTransitionServesTrialInProcess(t *testing.T) {
	node := newManagementTrialNode(t)
	address := node.serve(t)
	plaintext := plaintextClient(t, address)
	begun, err := plaintext.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil || begun.State != "trial" {
		t.Fatalf("begin over the installed listener = %+v, %v", begun, err)
	}
	trialClient := node.trialClient(t, address)
	eventually(t, "the trial client reaches the agent", func() bool { return node.answers(t, trialClient) })
	if node.answers(t, plaintext) {
		t.Fatal("the agent still serves installed clients during the trial")
	}
	if commands := node.takeCommands(); len(commands) != 1 || !isRevertTimer(commands[0], begun.TrialId) {
		t.Fatalf("begin ran %q; want only the revert timer", commands)
	}

	// A repeated begin, as after a lost response, returns the served trial.
	again, err := trialClient.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil || again.TrialId != begun.TrialId || again.State != "trial" {
		t.Fatalf("repeated begin = %+v, %v", again, err)
	}
	if commands := node.takeCommands(); len(commands) != 0 {
		t.Fatalf("repeated begin ran %q", commands)
	}
}

// Operations started during a trial would race its commit or revert.
func TestManagementTrialRefusesMutations(t *testing.T) {
	node := newManagementTrialNode(t)
	begun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil {
		t.Fatal(err)
	}
	requests := []struct {
		name string
		call func() error
	}{
		{name: "reboot", call: func() error {
			_, err := node.server.Reboot(t.Context(), &agentapi.RebootRequest{
				ApiVersion: APIVersion, Kind: RebootRequestKind, Actor: "test",
				ExpectedEnrollmentId: node.enrollment.ID, ExpectedInventoryNodeName: "cp-1", ExpectedMachineId: node.enrollment.MachineID,
				ExpectedCurrentGenerationId: "generation-1", TargetGenerationId: "generation-1",
			})
			return err
		}},
		{name: "operation", call: func() error {
			_, err := node.server.SubmitOperation(t.Context(), &agentapi.SubmitOperationRequest{
				ApiVersion: APIVersion, Kind: RequestKind, Actor: "test", ClientRequestId: "request-1",
				OperationKind:        node.server.supportedOperationKinds()[0],
				ExpectedEnrollmentId: node.enrollment.ID, ExpectedInventoryNodeName: "cp-1", ExpectedMachineId: node.enrollment.MachineID,
				ExpectedCurrentGenerationId: "generation-1",
			})
			return err
		}},
	}
	const refusal = "management trust transition is in progress"
	for _, request := range requests {
		if err := request.call(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s during the trial = %v; want it refused", request.name, err)
		}
	}

	if _, _, err := nodeidentity.RevertManagementTrial(node.root, begun.TrialId, "test", node.clock); err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if err := request.call(); err == nil || strings.Contains(err.Error(), refusal) {
			t.Errorf("%s after the trial = %v; want only its own validation", request.name, err)
		}
	}
}

func TestBeginManagementTransitionFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *managementTrialNode)
	}{
		{name: "active operation", setup: func(t *testing.T, n *managementTrialNode) {
			if _, err := n.server.Store.Create(operation.OperationRecord{OperationID: "op-1", OperationKind: "test", Scope: "host-generation", RequestDigest: strings.Repeat("f", 64), ResourceLocks: []string{"generation-state.lock"}}, "accepted", time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mutation in progress", setup: func(t *testing.T, n *managementTrialNode) {
			lock, err := n.server.Store.AcquireMutationLock()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
		}},
		{name: "revert timer refused", setup: func(t *testing.T, n *managementTrialNode) {
			n.fail = func(argv []string) bool { return slices.Contains(argv, "revert") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newManagementTrialNode(t)
			tc.setup(t, node)

			_, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("begin = %v; want failed precondition", err)
			}
			trial, found, readErr := nodeidentity.ReadManagementTrial(node.root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if found {
				t.Fatalf("trial record = %+v; want none", trial)
			}
			if served, err := nodeidentity.SettleManagementTrial(node.root, node.clock); err != nil || served != nil {
				t.Fatalf("agent start would serve %+v, %v", served, err)
			}
		})
	}
}

func TestManagementServerTLSRevertsUnloadableTrial(t *testing.T) {
	node := newManagementTrialNode(t)
	if _, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest()); err != nil {
		t.Fatal(err)
	}
	_, _, keyPath := nodeidentity.ManagementVersionPaths(node.root, node.authority)
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}

	config, served, err := managementServerTLS(node.root, node.clock)
	if err != nil || config != nil || served != nil {
		t.Fatalf("agent start = %v, trial %+v, %v; want the installed plaintext listener", config, served, err)
	}
	trial, found, err := nodeidentity.ReadManagementTrial(node.root)
	if err != nil || !found || trial.State != nodeidentity.ManagementTrialReverted || !strings.Contains(trial.RevertReason, "could not load") {
		t.Fatalf("trial = %+v, %v, %v; want reverted for the load failure", trial, found, err)
	}
	status, err := nodeManagementStatus(node.root)
	if err != nil || status.Authentication != "trusted-network" || status.Transition.GetState() != "reverted" || status.Transition.GetRevertReason() == "" {
		t.Fatalf("management status = %+v, %v", status, err)
	}
	if rerun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest()); err != nil || rerun.State != "trial" {
		t.Fatalf("rerun after the damaged trial = %+v, %v", rerun, err)
	}
}

// peerContext presents a verified client certificate chain from bundle's
// authority, as the TLS listener does after a handshake.
func peerContext(t *testing.T, bundle managementidentity.Bundle) context.Context {
	t.Helper()
	parse := func(value string) *x509.Certificate {
		block, _ := pem.Decode([]byte(value))
		if block == nil {
			t.Fatal(errors.New("certificate PEM is empty"))
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return certificate
	}
	chain := []*x509.Certificate{parse(bundle.Operator.Certificate), parse(bundle.CertificateAuthority.Certificate)}
	return peer.NewContext(t.Context(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{chain}}}})
}
