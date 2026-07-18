package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				if endpoint != "node.test:9443" {
					t.Fatalf("dial endpoint=%q", endpoint)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{
				"operations", "status",
				"--endpoint", "node.test:9443",
				"--operation-id", "operation-1",
				"--diagnostics", "verbose",
				"--output", "json",
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
			if got.GetRequestDigest() != "" || strings.Contains(stdout.String(), "requestDigest") {
				t.Fatalf("status exposed request digest: %s", stdout.String())
			}
			if client.operationRequest.GetExpectedRequestDigest() != "" || client.operationRequest.GetIncludeDiagnostics() != "verbose" {
				t.Fatalf("request = %#v", client.operationRequest)
			}
		})
	}
}

func TestOperationsListDiscoversRecentWork(t *testing.T) {
	client := &fakeKatlcAgentClient{operations: &agentapi.ListOperationsResponse{Operations: []*agentapi.OperationStatus{
		{OperationId: "active-1", OperationKind: "host-upgrade", RequestDigest: strings.Repeat("a", 64), Phase: "download"},
		{OperationId: "done-1", OperationKind: "generation-apply", RequestDigest: strings.Repeat("b", 64), Phase: "complete", Terminal: true, Result: "succeeded"},
	}}}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"operations", "list", "--endpoint", "node:9443", "--active", "--limit", "5", "--output", "json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	var got agentapi.ListOperationsResponse
	if err := protojson.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode operations: %v\n%s", err, stdout.String())
	}
	if len(got.Operations) != 2 || got.Operations[0].OperationId != "active-1" {
		t.Fatalf("operations = %#v", got.Operations)
	}
	if strings.Contains(stdout.String(), "requestDigest") {
		t.Fatalf("operations exposed request digest: %s", stdout.String())
	}
	if client.operationsRequest == nil || !client.operationsRequest.ActiveOnly || client.operationsRequest.Limit != 5 {
		t.Fatalf("list request = %#v", client.operationsRequest)
	}
}

func TestOperationsListTargetsClusterConfigWithoutContext(t *testing.T) {
	client := &fakeKatlcAgentClient{operations: &agentapi.ListOperationsResponse{}}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"operations", "list", "--config", writeClusterConfig(t)}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "ID") || client.operationsRequest == nil {
		t.Fatalf("stdout=%q request=%#v", stdout.String(), client.operationsRequest)
	}
}

func TestDetachedAcceptanceHidesNodeRecordPath(t *testing.T) {
	var stdout bytes.Buffer
	err := writeOperationAccepted(&stdout, &agentapi.OperationAccepted{
		OperationId: "op-1", OperationKind: "host-upgrade", RecordPath: "/var/lib/katl/operations/op-1/record.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "op-1") || strings.Contains(stdout.String(), "recordPath") || strings.Contains(stdout.String(), "/var/lib/katl") {
		t.Fatalf("detached output = %s", stdout.String())
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
		{"operations", "status", "--endpoint", "node:9443", "--operation-id", "op-1", "--diagnostics", "everything"},
		{"operations", "status", "--endpoint", "node:9443", "--operation-id", "op-1", "--timeout", "0s"},
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
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"operations", "status", "--endpoint", "node:9443", "--operation-id", "operation-1", "--watch", "--output", "json"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "disk mutation requires recovery") {
		t.Fatalf("run() error = %v", err)
	}
	var got agentapi.OperationStatus
	if decodeErr := protojson.Unmarshal(stdout.Bytes(), &got); decodeErr != nil || got.GetResult() != "failed" {
		t.Fatalf("status=%#v decode error=%v\n%s", &got, decodeErr, stdout.String())
	}
}

func TestKatlcAgentDialOptionsDoNotSendAuthorization(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	capture := &streamAuthorizationServer{authorization: make(chan string, 1)}
	agentapi.RegisterKatlcAgentServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	opts := append(katlcAgentDialOptions(), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
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
		if got != "" {
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

type streamAuthorizationServer struct {
	agentapi.UnimplementedKatlcAgentServer
	authorization chan string
}

func (s *streamAuthorizationServer) WatchOperation(_ *agentapi.WatchOperationRequest, stream agentapi.KatlcAgent_WatchOperationServer) error {
	values := metadata.ValueFromIncomingContext(stream.Context(), "authorization")
	value := ""
	if len(values) > 0 {
		value = values[0]
	}
	s.authorization <- value
	return stream.Send(&agentapi.OperationEvent{OperationId: "operation-1", Terminal: true, Status: &agentapi.OperationStatus{OperationId: "operation-1", Terminal: true, Result: "succeeded"}})
}
