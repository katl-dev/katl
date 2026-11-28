package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
)

func TestManagementEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		issuerRefused func(error) bool
		want          ManagementEvidence
	}{
		{name: "successful call", want: ManagementConfirmed},
		{name: "issuer rejection", err: grpcstatus.Error(codes.Unavailable, "handshake failed"), issuerRefused: func(error) bool { return true }, want: ManagementRefusedExplicitly},
		{name: "late issuer rejection", err: managementRPCInconclusive{cause: context.DeadlineExceeded}, issuerRefused: func(error) bool { return true }, want: ManagementInconclusive},
		{name: "alert in error text", err: grpcstatus.Error(codes.Unavailable, "tls: unknown certificate authority"), want: ManagementInconclusive},
		{name: "Unavailable without dial evidence", err: grpcstatus.Error(codes.Unavailable, "dial tcp: connect: connection refused"), want: ManagementInconclusive},
		{name: "RPC deadline", err: grpcstatus.Error(codes.DeadlineExceeded, "deadline exceeded"), want: ManagementInconclusive},
		{name: "RPC authentication refusal", err: grpcstatus.Error(codes.Unauthenticated, "unauthenticated"), want: ManagementInconclusive},
		{name: "RPC authentication refusal does not identify the issuer", err: grpcstatus.Error(codes.Unauthenticated, "unauthenticated"), issuerRefused: func(error) bool { return false }, want: ManagementInconclusive},
		{name: "EOF", err: io.EOF, want: ManagementInconclusive},
		{name: "reset", err: syscall.ECONNRESET, want: ManagementInconclusive},
		{name: "refusal", err: syscall.ECONNREFUSED, want: ManagementInconclusive},
		{name: "deadline", err: context.DeadlineExceeded, want: ManagementInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyManagementEvidence(tc.err, nil, tc.issuerRefused); got != tc.want {
				t.Errorf("evidence = %v, want %v", got, tc.want)
			}
		})
	}
}

type countingAgent struct {
	agentapi.UnimplementedKatlcAgentServer
	calls   atomic.Int32
	status  error
	onApply func()
}

func (a *countingAgent) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	a.calls.Add(1)
	return &agentapi.NodeStatus{InventoryNodeName: "cp-1"}, a.status
}

func (a *countingAgent) ApplyGeneration(context.Context, *agentapi.GenerationApplyRequest) (*agentapi.OperationAccepted, error) {
	a.calls.Add(1)
	if a.onApply != nil {
		a.onApply()
	}
	return &agentapi.OperationAccepted{OperationId: "op-1"}, a.status
}

func (a *countingAgent) WatchOperation(_ *agentapi.WatchOperationRequest, stream grpc.ServerStreamingServer[agentapi.OperationEvent]) error {
	a.calls.Add(1)
	return stream.Send(&agentapi.OperationEvent{})
}

func (a *countingAgent) StageHostUpgradeArtifact(stream grpc.ClientStreamingServer[agentapi.StageHostUpgradeArtifactRequest, agentapi.HostUpgradeArtifactStaged]) error {
	a.calls.Add(1)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	return stream.SendAndClose(&agentapi.HostUpgradeArtifactStaged{})
}

// connTracker remembers the last connection a listener accepted so a handler
// can reset it.
type connTracker struct {
	net.Listener
	delay time.Duration
	mu    sync.Mutex
	last  net.Conn
}

func (l *connTracker) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		time.Sleep(l.delay)
		l.mu.Lock()
		l.last = conn
		l.mu.Unlock()
	}
	return conn, err
}

func (l *connTracker) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tcp, ok := l.last.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = l.last.Close()
}

func serve(t *testing.T, agent *countingAgent, delay time.Duration) *connTracker {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tracker := &connTracker{Listener: listener, delay: delay}
	server := grpc.NewServer()
	agentapi.RegisterKatlcAgentServer(server, agent)
	t.Cleanup(server.Stop)
	go func() { _ = server.Serve(tracker) }()
	return tracker
}

// gatedProgress opens the gate on the first progress line, which is when a
// restarting agent is simulated to begin listening.
type gatedProgress struct {
	mu   sync.Mutex
	text strings.Builder
	open atomic.Bool
}

func (g *gatedProgress) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open.Store(true)
	return g.text.Write(p)
}

func (g *gatedProgress) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.text.String()
}

func refusedDial(addr string) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// dialManagement connects to endpoint through ManagementDialOptions. While
// listening is non-nil and false, dials fail as a refused connection does.
func dialManagement(ctx context.Context, t *testing.T, endpoint string, creds credentials.TransportCredentials, listening *atomic.Bool) agentapi.KatlcAgentClient {
	t.Helper()
	options := ManagementDialOptions(ctx, "cp-1", endpoint, creds)
	if listening != nil {
		options = append(options, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			if !listening.Load() {
				return nil, refusedDial(addr)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}))
	}
	conn, err := grpc.NewClient("passthrough:///"+endpoint, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentapi.NewKatlcAgentClient(conn)
}

func shortWait(t *testing.T, bound time.Duration) {
	t.Helper()
	old := managementReady
	t.Cleanup(func() { managementReady = old })
	managementReady.bound, managementReady.interval = bound, 20*time.Millisecond
}

// Each call shape a katlctl command uses reaches the agent once it listens, as
// node status, upgrade planning, apply and upgrade staging do.
func TestManagementCallsWaitForListener(t *testing.T) {
	calls := map[string]func(context.Context, agentapi.KatlcAgentClient) error{
		"status": func(ctx context.Context, client agentapi.KatlcAgentClient) error {
			_, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			return err
		},
		"apply": func(ctx context.Context, client agentapi.KatlcAgentClient) error {
			_, err := client.ApplyGeneration(ctx, &agentapi.GenerationApplyRequest{})
			return err
		},
		"watch": func(ctx context.Context, client agentapi.KatlcAgentClient) error {
			stream, err := client.WatchOperation(ctx, &agentapi.WatchOperationRequest{})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
		"stage": func(ctx context.Context, client agentapi.KatlcAgentClient) error {
			stream, err := client.StageHostUpgradeArtifact(ctx)
			if err != nil {
				return err
			}
			if err := stream.Send(&agentapi.StageHostUpgradeArtifactRequest{}); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			_, err = stream.CloseAndRecv()
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			shortWait(t, 10*time.Second)
			agent := &countingAgent{}
			endpoint := serve(t, agent, 0).Addr().String()
			progress := &gatedProgress{}
			ctx := WithManagementProgress(context.Background(), progress)
			client := dialManagement(ctx, t, endpoint, insecure.NewCredentials(), &progress.open)

			if err := call(ctx, client); err != nil {
				t.Fatalf("call: %v", err)
			}

			if got := agent.calls.Load(); got != 1 {
				t.Fatalf("agent handled %d calls, want exactly 1", got)
			}
			if !strings.Contains(progress.String(), "Waiting for node cp-1 management endpoint "+endpoint) {
				t.Fatalf("no progress while waiting:\n%s", progress.String())
			}
		})
	}
}

// With nothing listening the call gives up after the bound with guidance,
// whether the bound or the caller's deadline ends the wait.
func TestManagementCallRefusedGivesGuidance(t *testing.T) {
	tests := map[string]struct {
		bound, deadline time.Duration
		wantInErr       string
	}{
		"bound":    {bound: 300 * time.Millisecond, deadline: 10 * time.Second},
		"deadline": {bound: 30 * time.Second, deadline: 300 * time.Millisecond, wantInErr: "context deadline exceeded"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				shortWait(t, test.bound)
				progress := &gatedProgress{}
				ctx, cancel := context.WithTimeout(WithManagementProgress(context.Background(), progress), test.deadline)
				defer cancel()
				const endpoint = "node.test:9443"
				client := dialManagement(ctx, t, endpoint, insecure.NewCredentials(), &atomic.Bool{})

				start := time.Now()
				_, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

				if err == nil {
					t.Fatal("call succeeded with nothing listening")
				}
				if time.Since(start) > 5*time.Second {
					t.Fatalf("call took %s, want it bounded", time.Since(start))
				}
				for _, want := range []string{"cp-1", endpoint, "refused connections", "systemctl status katlc-agent", "rerun", test.wantInErr} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
				if progress.String() == "" {
					t.Error("no progress before giving up")
				}
			})
		})
	}
}

func TestManagementRPCInconclusive(t *testing.T) {
	timings := []struct {
		name  string
		delay time.Duration
		cause error
	}{
		{name: "at deadline", delay: time.Second, cause: context.DeadlineExceeded},
		{name: "after deadline", delay: 2 * time.Second, cause: context.DeadlineExceeded},
		{name: "after cancellation", cause: context.Canceled},
	}
	results := []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "RPC error", err: grpcstatus.Error(codes.Unavailable, "connection reset")},
		{name: "EOF", err: io.EOF},
	}
	calls := []struct {
		name   string
		stream func(grpc.ClientStream) error
	}{
		{name: "unary"},
		{name: "stream open"},
		{name: "stream header", stream: func(s grpc.ClientStream) error {
			header, err := s.Header()
			if header != nil {
				return fmt.Errorf("exposed header after the call budget: %v", header)
			}
			return err
		}},
		{name: "stream send", stream: func(s grpc.ClientStream) error {
			return s.SendMsg(&agentapi.StageHostUpgradeArtifactRequest{})
		}},
		{name: "stream receive", stream: func(s grpc.ClientStream) error {
			return s.RecvMsg(&agentapi.HostUpgradeArtifactStaged{})
		}},
		{name: "stream close", stream: grpc.ClientStream.CloseSend},
	}
	for _, call := range calls {
		for _, timing := range timings {
			for _, result := range results {
				t.Run(call.name+"/"+timing.name+"/"+result.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancel(t.Context())
						defer cancel()
						if timing.cause == context.DeadlineExceeded {
							ctx = managementDeadlineContext{Context: ctx, deadline: time.Now().Add(time.Second)}
						}
						finish := func() error {
							if timing.cause == context.Canceled {
								cancel()
							} else {
								time.Sleep(timing.delay)
							}
							return result.err
						}

						options := ManagementDialOptions(WithoutManagementWait(ctx), "cp-1", "unused", insecure.NewCredentials())
						options = append(options,
							grpc.WithChainUnaryInterceptor(func(context.Context, string, any, any, *grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error {
								return finish()
							}),
							grpc.WithChainStreamInterceptor(func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, grpc.Streamer, ...grpc.CallOption) (grpc.ClientStream, error) {
								stream := managementResultStream{finish: finish}
								if call.stream == nil {
									return stream, finish()
								}
								return stream, nil
							}),
						)
						conn, err := grpc.NewClient("passthrough:///unused", options...)
						if err != nil {
							t.Fatal(err)
						}
						defer conn.Close()

						if call.name == "unary" {
							_, err = agentapi.NewKatlcAgentClient(conn).GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
						} else {
							var stream grpc.ClientStream
							stream, err = conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, agentapi.KatlcAgent_StageHostUpgradeArtifact_FullMethodName)
							if call.stream != nil {
								if err != nil {
									t.Fatalf("open stream before budget ended: %v", err)
								}
								err = call.stream(stream)
							}
						}
						if !errors.Is(err, ErrManagementRPCInconclusive) || !errors.Is(err, timing.cause) {
							t.Fatalf("result = %v, want an inconclusive outcome wrapping %v", err, timing.cause)
						}
					})
				})
			}
		}
	}
}

// A deadline can pass before the context reports cancellation.
type managementDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c managementDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

type managementResultStream struct {
	grpc.ClientStream
	finish func() error
}

func (s managementResultStream) Header() (metadata.MD, error) {
	if err := s.finish(); err != nil {
		return nil, err
	}
	return metadata.Pairs("evidence", "answer"), nil
}

func (s managementResultStream) SendMsg(any) error { return s.finish() }
func (s managementResultStream) RecvMsg(any) error { return s.finish() }
func (s managementResultStream) CloseSend() error  { return s.finish() }

// Failures that waiting cannot cure surface at once and unchanged: a handshake
// that fails, and an agent that answers with an error that merely mentions a
// refused connection.
func TestManagementCallDoesNotRetryOtherErrors(t *testing.T) {
	tests := map[string]struct {
		creds   credentials.TransportCredentials
		status  error
		wantErr string
	}{
		"handshake": {creds: credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}), wantErr: "handshake"},
		"agent error": {
			creds:   insecure.NewCredentials(),
			status:  grpcstatus.Error(codes.Unavailable, "dial tcp 10.0.0.9:6443: connect: connection refused"),
			wantErr: "10.0.0.9:6443",
		},
		"denied": {creds: insecure.NewCredentials(), status: grpcstatus.Error(codes.PermissionDenied, "connection refused by policy"), wantErr: "policy"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			shortWait(t, 10*time.Second)
			agent := &countingAgent{status: test.status}
			endpoint := serve(t, agent, 0).Addr().String()
			progress := &gatedProgress{}
			ctx := WithManagementProgress(context.Background(), progress)
			client := dialManagement(ctx, t, endpoint, test.creds, nil)

			start := time.Now()
			_, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("failure took %s, want it immediate", time.Since(start))
			}
			if agent.calls.Load() > 1 {
				t.Fatalf("agent handled %d calls, want at most 1", agent.calls.Load())
			}
			if progress.String() != "" {
				t.Fatalf("reported waiting for a non-transient error:\n%s", progress.String())
			}
		})
	}
}

// A connection the agent resets after it received a mutation may have been
// applied, so the call is not repeated and the error is the transport's own.
func TestManagementMutationNotRetriedAfterReset(t *testing.T) {
	shortWait(t, 10*time.Second)
	agent := &countingAgent{}
	tracker := serve(t, agent, 0)
	agent.onApply = tracker.reset
	progress := &gatedProgress{}
	ctx := WithManagementProgress(context.Background(), progress)
	client := dialManagement(ctx, t, tracker.Addr().String(), insecure.NewCredentials(), nil)

	_, err := client.ApplyGeneration(ctx, &agentapi.GenerationApplyRequest{})

	if grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v, want the transport's Unavailable", err)
	}
	if strings.Contains(err.Error(), "katlc-agent is not listening") {
		t.Fatalf("reset reported as a refused connection: %v", err)
	}
	if got := agent.calls.Load(); got != 1 {
		t.Fatalf("agent handled %d apply calls, want exactly 1", got)
	}
	if progress.String() != "" {
		t.Fatalf("waited after a reset:\n%s", progress.String())
	}
}

// An agent that is slow to complete the HTTP/2 handshake, as one starting under
// load is, is still reached.
func TestManagementCallSurvivesSlowHandshake(t *testing.T) {
	agent := &countingAgent{}
	endpoint := serve(t, agent, time.Second).Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := dialManagement(ctx, t, endpoint, insecure.NewCredentials(), nil)

	if _, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{}); err != nil {
		t.Fatalf("call with a handshake delayed by 1s: %v", err)
	}
	if agent.calls.Load() != 1 {
		t.Fatalf("agent handled %d calls, want 1", agent.calls.Load())
	}
}

func TestManagementReadinessPreservesRPCBudget(t *testing.T) {
	shortWait(t, 100*time.Millisecond)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		attempts := 0

		err := awaitManagementEndpoint(ctx, io.Discard, "cp-1", "unused", func(*peer.Peer) error {
			attempts++
			if attempts == 1 {
				return grpcstatus.Error(codes.Unavailable, "connect: connection refused")
			}
			time.Sleep(time.Second)
			return nil
		})

		if err != nil || attempts != 2 {
			t.Fatalf("wait returned %v after %d calls, want the successful RPC after one refusal", err, attempts)
		}
	})
}

// serveMutualTLS serves agent on a TLS listener whose certificate is issued
// for cp-1 and returns its endpoint and a client identity for the cluster.
func serveMutualTLS(t *testing.T, agent *countingAgent) (string, managementidentity.ClientCredentials) {
	t.Helper()
	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, _, err := ServerTLSConfigForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(identity, now)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentapi.RegisterKatlcAgentServer(server, agent)
	t.Cleanup(server.Stop)
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), client
}

// The dial owner tells a trusted-network client that the node requires mTLS
// immediately, with or without the wait, while an mTLS client reaches it.
func TestManagementDialOutcomes(t *testing.T) {
	trustedNetwork := managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}
	tests := map[string]struct {
		mutualTLS bool
		noWait    bool
		stream    bool
		wantErr   string
	}{
		"trusted-network client, mTLS node":                 {wantErr: "likely serves mTLS"},
		"trusted-network client, mTLS node, nowait":         {noWait: true, wantErr: "likely serves mTLS"},
		"trusted-network client, mTLS node, stream":         {stream: true, wantErr: "likely serves mTLS"},
		"trusted-network client, mTLS node, stream, nowait": {stream: true, noWait: true, wantErr: "likely serves mTLS"},
		"mTLS client, mTLS node":                            {mutualTLS: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			agent := &countingAgent{}
			endpoint, client := serveMutualTLS(t, agent)
			identity := trustedNetwork
			if test.mutualTLS {
				identity = client
			}
			creds, err := ClientCredentialsForNode(identity, "cp-1")
			if err != nil {
				t.Fatal(err)
			}
			progress := &gatedProgress{}
			ctx, cancel := context.WithTimeout(WithManagementProgress(context.Background(), progress), 10*time.Second)
			defer cancel()
			if test.noWait {
				ctx = WithoutManagementWait(ctx)
			}
			conn, err := grpc.NewClient("passthrough:///"+endpoint, ManagementDialOptions(ctx, "cp-1", endpoint, creds)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })

			if test.stream {
				var stream grpc.ServerStreamingClient[agentapi.OperationEvent]
				stream, err = agentapi.NewKatlcAgentClient(conn).WatchOperation(ctx, &agentapi.WatchOperationRequest{})
				if err == nil {
					_, err = stream.Recv()
				}
			} else {
				_, err = agentapi.NewKatlcAgentClient(conn).GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			}

			if test.wantErr == "" {
				if err != nil || agent.calls.Load() != 1 {
					t.Fatalf("error = %v, agent calls = %d; want one successful call", err, agent.calls.Load())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), "refused connections") {
				t.Errorf("mTLS node reported as refusing connections: %v", err)
			}
			if progress.String() != "" {
				t.Errorf("waited after an authentication failure:\n%s", progress.String())
			}
		})
	}
}

// heldAgent answers status only after the caller's deadline.
type heldAgent struct {
	agentapi.UnimplementedKatlcAgentServer
}

func (heldAgent) GetNodeStatus(ctx context.Context, _ *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// A real management call yields evidence from what its connection observed:
// unreachable only when no TCP connection was established, and a named cause
// only for typed handshake evidence.
func TestManagementEvidenceFromConnection(t *testing.T) {
	trustedNetwork := managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}
	type setup struct {
		endpoint string
		identity managementidentity.ClientCredentials
		node     string
		dialer   func(context.Context, string) (net.Conn, error)
		cancel   bool
	}
	tests := map[string]struct {
		setup func(*testing.T) setup
		want  ManagementEvidence
	}{
		"nothing listening": {setup: func(t *testing.T) setup {
			listener := listenTCP(t)
			endpoint := listener.Addr().String()
			_ = listener.Close()
			return setup{endpoint: endpoint, identity: trustedNetwork}
		}, want: ManagementUnreachable},
		"dial blocks until deadline": {setup: func(t *testing.T) setup {
			return setup{endpoint: "192.0.2.1:9443", identity: trustedNetwork, dialer: blockingDial}
		}, want: ManagementUnreachable},
		"dial blocks until cancelled": {setup: func(t *testing.T) setup {
			return setup{endpoint: "192.0.2.1:9443", identity: trustedNetwork, dialer: blockingDial, cancel: true}
		}, want: ManagementInconclusive},
		"foreign authority": {setup: func(t *testing.T) setup {
			endpoint, _ := serveMutualTLS(t, &countingAgent{})
			_, foreign := serveMutualTLS(t, &countingAgent{})
			return setup{endpoint: endpoint, identity: foreign}
		}, want: ManagementAuthorityMismatch},
		"plaintext client at mTLS node": {setup: func(t *testing.T) setup {
			endpoint, _ := serveMutualTLS(t, &countingAgent{})
			return setup{endpoint: endpoint, identity: trustedNetwork}
		}, want: ManagementRequiresTLS},
		"TLS client, listener accepts and closes": {setup: func(t *testing.T) setup {
			listener := listenTCP(t)
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			_, client := serveMutualTLS(t, &countingAgent{})
			return setup{endpoint: listener.Addr().String(), identity: client}
		}, want: ManagementInconclusive},
		"TLS client, plaintext HTTP/2 server": {setup: func(t *testing.T) setup {
			_, client := serveMutualTLS(t, &countingAgent{})
			return setup{endpoint: serve(t, &countingAgent{}, 0).Addr().String(), identity: client}
		}, want: ManagementInconclusive},
		"certificate for another node": {setup: func(t *testing.T) setup {
			endpoint, client := serveMutualTLS(t, &countingAgent{})
			return setup{endpoint: endpoint, identity: client, node: "cp-2"}
		}, want: ManagementInconclusive},
		"reply after deadline": {setup: func(t *testing.T) setup {
			listener := listenTCP(t)
			server := grpc.NewServer()
			agentapi.RegisterKatlcAgentServer(server, heldAgent{})
			t.Cleanup(server.Stop)
			go func() { _ = server.Serve(listener) }()
			return setup{endpoint: listener.Addr().String(), identity: trustedNetwork}
		}, want: ManagementInconclusive},
		"agent reports Unavailable": {setup: func(t *testing.T) setup {
			agent := &countingAgent{status: grpcstatus.Error(codes.Unavailable, "dial tcp 10.0.0.9:6443: connect: connection refused")}
			return setup{endpoint: serve(t, agent, 0).Addr().String(), identity: trustedNetwork}
		}, want: ManagementInconclusive},
	}
	for name, test := range tests {
		for _, wait := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/wait=%t", name, wait), func(t *testing.T) {
				shortWait(t, 300*time.Millisecond)
				s := test.setup(t)
				if s.node == "" {
					s.node = "cp-1"
				}
				creds, err := ClientCredentialsForNode(s.identity, s.node)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if s.cancel {
					time.AfterFunc(200*time.Millisecond, cancel)
				}
				if !wait {
					ctx = WithoutManagementWait(ctx)
				}
				options := ManagementDialOptions(ctx, s.node, s.endpoint, creds)
				if s.dialer != nil {
					options = append(options, grpc.WithContextDialer(s.dialer))
				}
				conn, err := grpc.NewClient("passthrough:///"+s.endpoint, options...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })

				_, err = agentapi.NewKatlcAgentClient(conn).GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

				if got := ClassifyManagementEvidence(err, nil, nil); got != test.want {
					t.Fatalf("evidence = %v, want %v; error: %v", got, test.want, err)
				}
			})
		}
	}
}

// A refused endpoint that never starts listening before the caller's deadline
// is unreachable, although the readiness wait ends the call with an
// inconclusive budget error.
func TestManagementEvidenceRefusedUntilDeadline(t *testing.T) {
	shortWait(t, 30*time.Second)
	listener := listenTCP(t)
	endpoint := listener.Addr().String()
	_ = listener.Close()
	creds, err := ClientCredentialsForNode(managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///"+endpoint, ManagementDialOptions(ctx, "cp-1", endpoint, creds)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = agentapi.NewKatlcAgentClient(conn).GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

	if !errors.Is(err, ErrManagementRPCInconclusive) || !strings.Contains(err.Error(), "refused connections") {
		t.Fatalf("error = %v, want the readiness wait to give up at the deadline", err)
	}
	if got := ClassifyManagementEvidence(err, nil, nil); got != ManagementUnreachable {
		t.Fatalf("evidence = %v, want unreachable", got)
	}
}

func blockingDial(ctx context.Context, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Carrying connection evidence does not change what callers see of a call's
// error: its status, its message and its identity.
func TestManagementCallErrorKeepsStatus(t *testing.T) {
	for name, inner := range map[string]error{
		"status":       grpcstatus.Error(codes.Unavailable, "connection error"),
		"wrapped":      fmt.Errorf("node likely serves mTLS: %w", grpcstatus.Error(codes.Unavailable, "server preface")),
		"inconclusive": managementRPCInconclusive{cause: context.DeadlineExceeded},
		"plain":        io.ErrUnexpectedEOF,
	} {
		t.Run(name, func(t *testing.T) {
			wrapped := error(&managementCallError{err: inner})

			if got, want := grpcstatus.Code(wrapped), grpcstatus.Code(inner); got != want {
				t.Errorf("code = %v, want %v", got, want)
			}
			if got, want := grpcstatus.Convert(wrapped).Message(), grpcstatus.Convert(inner).Message(); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
			_, gotOK := grpcstatus.FromError(wrapped)
			_, wantOK := grpcstatus.FromError(inner)
			if gotOK != wantOK {
				t.Errorf("FromError ok = %t, want %t", gotOK, wantOK)
			}
			if wrapped.Error() != inner.Error() || !errors.Is(wrapped, inner) {
				t.Errorf("error = %v, want it to be %v", wrapped, inner)
			}
			if errors.Is(inner, ErrManagementRPCInconclusive) != errors.Is(wrapped, ErrManagementRPCInconclusive) {
				t.Error("wrapping changed whether the call was inconclusive")
			}
		})
	}

	t.Run("stream end", func(t *testing.T) {
		endpoint := serve(t, &countingAgent{}, 0).Addr().String()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		client := dialManagement(ctx, t, endpoint, insecure.NewCredentials(), nil)
		stream, err := client.WatchOperation(ctx, &agentapi.WatchOperationRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != io.EOF {
			t.Fatalf("end of stream = %v, want io.EOF itself", err)
		}
	})
}
