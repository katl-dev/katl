package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
)

type countingAgent struct {
	agentapi.UnimplementedKatlcAgentServer
	calls  atomic.Int32
	status error
}

func (a *countingAgent) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	a.calls.Add(1)
	return &agentapi.NodeStatus{InventoryNodeName: "cp-1"}, a.status
}

func (a *countingAgent) ApplyGeneration(context.Context, *agentapi.GenerationApplyRequest) (*agentapi.OperationAccepted, error) {
	a.calls.Add(1)
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

// freeEndpoint returns an address nothing listens on.
func freeEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// serveAfter starts agent on endpoint after delay, as a restarting katlc-agent
// begins listening.
func serveAfter(t *testing.T, endpoint string, delay time.Duration, agent *countingAgent) {
	t.Helper()
	server := grpc.NewServer()
	agentapi.RegisterKatlcAgentServer(server, agent)
	t.Cleanup(server.Stop)
	go func() {
		time.Sleep(delay)
		if listener, err := net.Listen("tcp", endpoint); err == nil {
			_ = server.Serve(listener)
		}
	}()
}

func dialManagement(t *testing.T, endpoint string, creds credentials.TransportCredentials) agentapi.KatlcAgentClient {
	t.Helper()
	options := append(katlcAgentDialOptions(creds), managementDialOptions("cp-1", endpoint, true)...)
	conn, err := grpc.NewClient(endpoint, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentapi.NewKatlcAgentClient(conn)
}

func shortManagementWait(t *testing.T, bound time.Duration) *bytes.Buffer {
	t.Helper()
	oldWait, oldProgress := managementReadyWait, managementWaitProgress
	t.Cleanup(func() { managementReadyWait, managementWaitProgress = oldWait, oldProgress })
	managementReadyWait.bound, managementReadyWait.interval = bound, 20*time.Millisecond
	progress := &bytes.Buffer{}
	managementWaitProgress = progress
	return progress
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
			progress := shortManagementWait(t, 10*time.Second)
			endpoint := freeEndpoint(t)
			agent := &countingAgent{}
			serveAfter(t, endpoint, 300*time.Millisecond, agent)
			client := dialManagement(t, endpoint, insecure.NewCredentials())

			if err := call(context.Background(), client); err != nil {
				t.Fatalf("call: %v", err)
			}

			if got := agent.calls.Load(); got != 1 {
				t.Fatalf("agent handled %d calls, want exactly 1", got)
			}
			if !strings.Contains(progress.String(), "Waiting for node cp-1 management endpoint "+endpoint) {
				t.Fatalf("no progress while waiting:\n%s", progress)
			}
		})
	}
}

func TestManagementCallRefusedBeyondBound(t *testing.T) {
	progress := shortManagementWait(t, 300*time.Millisecond)
	endpoint := freeEndpoint(t)
	client := dialManagement(t, endpoint, insecure.NewCredentials())

	start := time.Now()
	_, err := client.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})

	if err == nil {
		t.Fatal("call succeeded with nothing listening")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("call took %s, want it bounded", time.Since(start))
	}
	for _, want := range []string{"cp-1", endpoint, "refused connections", "systemctl status katlc-agent", "rerun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if progress.Len() == 0 {
		t.Error("no progress before giving up")
	}
}

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
			progress := shortManagementWait(t, 10*time.Second)
			endpoint := freeEndpoint(t)
			agent := &countingAgent{status: test.status}
			serveAfter(t, endpoint, 0, agent)
			time.Sleep(200 * time.Millisecond)
			client := dialManagement(t, endpoint, test.creds)

			start := time.Now()
			_, err := client.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("failure took %s, want it immediate", time.Since(start))
			}
			if agent.calls.Load() > 1 {
				t.Fatalf("agent handled %d calls, want at most 1", agent.calls.Load())
			}
			if progress.Len() != 0 {
				t.Fatalf("reported waiting for a non-transient error:\n%s", progress)
			}
		})
	}
}
