package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestOperationStatusQueriesEveryOperationKind(t *testing.T) {
	for _, kind := range []string{"host-upgrade", "bootstrap-init", "generation-apply", "destructive-reset"} {
		t.Run(kind, func(t *testing.T) {
			client := &fakeKatlcAgentClient{operationStatus: &agentapi.OperationStatus{
				OperationId:             "operation-1",
				OperationKind:           kind,
				RequestDigest:           strings.Repeat("a", 64),
				Phase:                   "complete",
				Terminal:                true,
				Result:                  "failed",
				RecoveryRequired:        true,
				FailureReason:           "operator recovery is required",
				ExternalMutationStarted: true,
			}}
			oldDial := dialKatlcAgent
			dialKatlcAgent = func(_ context.Context, endpoint, token string) (katlcAgentConnection, error) {
				if endpoint != "node.test:9443" || token != "agent-token" {
					t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			tokenPath := writeOperationToken(t)

			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{
				"operation", "status",
				"--endpoint", "node.test:9443",
				"--agent-token-file", tokenPath,
				"--operation-id", "operation-1",
				"--request-digest", strings.Repeat("a", 64),
				"--diagnostics", "verbose",
			}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			var got agentapi.OperationStatus
			if err := protojson.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("decode status: %v\n%s", err, stdout.String())
			}
			if got.GetOperationKind() != kind || !got.GetRecoveryRequired() || got.GetFailureReason() != "operator recovery is required" {
				t.Fatalf("status = %#v", &got)
			}
			if client.operationRequest.GetExpectedRequestDigest() != strings.Repeat("a", 64) || client.operationRequest.GetIncludeDiagnostics() != "verbose" {
				t.Fatalf("request = %#v", client.operationRequest)
			}
		})
	}
}

func TestFollowOperationUsesWatchSequence(t *testing.T) {
	terminal := &agentapi.OperationStatus{OperationId: "operation-1", OperationKind: "host-upgrade", Phase: "complete", LatestJournalSeq: 4, Terminal: true, Result: "succeeded"}
	client := &fakeOperationClient{streams: []*fakeOperationStream{{events: []*agentapi.OperationEvent{{OperationId: "operation-1", JournalSeq: 4, Terminal: true, Status: terminal}}}}}
	request := &agentapi.GetOperationRequest{OperationId: "operation-1", ExpectedRequestDigest: strings.Repeat("b", 64), IncludeDiagnostics: "verbose"}
	var stderr bytes.Buffer

	status, err := followOperation(context.Background(), client, request, &agentapi.OperationStatus{OperationId: "operation-1", OperationKind: "host-upgrade", Phase: "download", LatestJournalSeq: 3}, &stderr)
	if err != nil {
		t.Fatalf("followOperation() error = %v", err)
	}
	if status != terminal {
		t.Fatalf("status = %#v", status)
	}
	if len(client.watchRequests) != 1 || client.watchRequests[0].GetAfterJournalSeq() != 3 || client.watchRequests[0].GetExpectedRequestDigest() != request.ExpectedRequestDigest || client.watchRequests[0].GetIncludeDiagnostics() != "verbose" {
		t.Fatalf("watch requests = %#v", client.watchRequests)
	}
}

func TestFollowOperationFallsBackToPolling(t *testing.T) {
	terminal := &agentapi.OperationStatus{OperationId: "operation-1", OperationKind: "destructive-reset", Phase: "complete", LatestJournalSeq: 9, Terminal: true, Result: "succeeded"}
	client := &fakeOperationClient{watchErrs: []error{errors.New("stream unavailable")}, statuses: []*agentapi.OperationStatus{terminal}}
	var stderr bytes.Buffer

	status, err := followOperation(context.Background(), client, &agentapi.GetOperationRequest{OperationId: "operation-1"}, &agentapi.OperationStatus{OperationId: "operation-1", Phase: "accepted"}, &stderr)
	if err != nil {
		t.Fatalf("followOperation() error = %v", err)
	}
	if status != terminal || len(client.getRequests) != 1 {
		t.Fatalf("status=%#v get requests=%d", status, len(client.getRequests))
	}
	if !strings.Contains(stderr.String(), "falling back to authoritative status polling") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestFollowOperationTimeoutReturnsLastStatus(t *testing.T) {
	current := &agentapi.OperationStatus{OperationId: "operation-1", OperationKind: "bootstrap-init", Phase: "kubeadm-init"}
	client := &fakeOperationClient{watchErrs: []error{errors.New("stream unavailable")}, statuses: []*agentapi.OperationStatus{current}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	status, err := followOperation(ctx, client, &agentapi.GetOperationRequest{OperationId: "operation-1"}, current, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || status != current {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

func TestOperationStatusValidatesFlags(t *testing.T) {
	for _, args := range [][]string{
		{"operation", "status"},
		{"operation", "status", "--endpoint", "node:9443"},
		{"operation", "status", "--endpoint", "node:9443", "--operation-id", "op-1", "--diagnostics", "everything"},
		{"operation", "status", "--endpoint", "node:9443", "--operation-id", "op-1", "--timeout", "0s"},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, &stdout, &stderr); err == nil {
			t.Fatalf("args %v succeeded", args)
		}
	}
}

func TestOperationWatchReturnsFailureAfterStatus(t *testing.T) {
	client := &fakeKatlcAgentClient{operationStatus: &agentapi.OperationStatus{
		OperationId: "operation-1", Terminal: true, Result: "failed", FailureReason: "disk mutation requires recovery",
	}}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(context.Context, string, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"operation", "status", "--endpoint", "node:9443", "--operation-id", "operation-1", "--watch"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "disk mutation requires recovery") {
		t.Fatalf("run() error = %v", err)
	}
	var got agentapi.OperationStatus
	if decodeErr := protojson.Unmarshal(stdout.Bytes(), &got); decodeErr != nil || got.GetResult() != "failed" {
		t.Fatalf("status=%#v decode error=%v\n%s", &got, decodeErr, stdout.String())
	}
}

func TestKatlcAgentDialOptionsAuthenticateWatchStream(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	capture := &streamAuthorizationServer{authorization: make(chan string, 1)}
	agentapi.RegisterKatlcAgentServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	opts := append(katlcAgentDialOptions("stream-token"), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	conn, err := grpc.DialContext(context.Background(), "passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := agentapi.NewKatlcAgentClient(conn).WatchOperation(context.Background(), &agentapi.WatchOperationRequest{OperationId: "operation-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-capture.authorization:
		if got != "Bearer stream-token" {
			t.Fatalf("authorization = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("stream authorization was not observed")
	}
}

type fakeOperationClient struct {
	statuses      []*agentapi.OperationStatus
	getErrs       []error
	streams       []*fakeOperationStream
	watchErrs     []error
	getRequests   []*agentapi.GetOperationRequest
	watchRequests []*agentapi.WatchOperationRequest
}

func (c *fakeOperationClient) GetOperation(_ context.Context, req *agentapi.GetOperationRequest, _ ...grpc.CallOption) (*agentapi.OperationStatus, error) {
	c.getRequests = append(c.getRequests, req)
	var err error
	if len(c.getErrs) > 0 {
		err, c.getErrs = c.getErrs[0], c.getErrs[1:]
	}
	if len(c.statuses) == 0 {
		return nil, err
	}
	status := c.statuses[0]
	if len(c.statuses) > 1 {
		c.statuses = c.statuses[1:]
	}
	return status, err
}

func (c *fakeOperationClient) WatchOperation(_ context.Context, req *agentapi.WatchOperationRequest, _ ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error) {
	c.watchRequests = append(c.watchRequests, req)
	var err error
	if len(c.watchErrs) > 0 {
		err, c.watchErrs = c.watchErrs[0], c.watchErrs[1:]
	}
	if err != nil || len(c.streams) == 0 {
		return nil, err
	}
	stream := c.streams[0]
	c.streams = c.streams[1:]
	return stream, nil
}

type fakeOperationStream struct {
	grpc.ClientStream
	events []*agentapi.OperationEvent
	err    error
}

func (s *fakeOperationStream) Recv() (*agentapi.OperationEvent, error) {
	if len(s.events) == 0 {
		if s.err != nil {
			err := s.err
			s.err = nil
			return nil, err
		}
		return nil, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func writeOperationToken(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/agent.token"
	if err := os.WriteFile(path, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type streamAuthorizationServer struct {
	agentapi.UnimplementedKatlcAgentServer
	authorization chan string
}

func (s *streamAuthorizationServer) WatchOperation(_ *agentapi.WatchOperationRequest, stream agentapi.KatlcAgent_WatchOperationServer) error {
	values := metadata.ValueFromIncomingContext(stream.Context(), "authorization")
	if len(values) > 0 {
		s.authorization <- values[0]
	}
	return stream.Send(&agentapi.OperationEvent{OperationId: "operation-1", Terminal: true, Status: &agentapi.OperationStatus{OperationId: "operation-1", Terminal: true, Result: "succeeded"}})
}
