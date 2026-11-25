package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// saveContextWithWorkerDial runs `context save` against a two-node
// configuration where worker-1 fails its first dials with workerErrs and then
// answers.
func saveContextWithWorkerDial(t *testing.T, workerErrs []error) (dials int, stderr string, err error) {
	t.Helper()
	path := writeMovedWorkerConfig(t, "")
	cp := healthyHostClient("machine-cp-1", "agent-1", "generation-1")
	cp.nodeStatus.InventoryNodeName = "cp-1"
	worker := movedWorkerClient("agent-1")
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != workerDesiredEndpoint {
			return katlcAgentConnection{Client: cp, Close: func() error { return nil }}, nil
		}
		dials++
		if dials <= len(workerErrs) {
			return katlcAgentConnection{}, workerErrs[dials-1]
		}
		return katlcAgentConnection{Client: worker, Close: func() error { return nil }}, nil
	}
	oldWait := managementReadyWait
	t.Cleanup(func() { managementReadyWait = oldWait })
	managementReadyWait.bound, managementReadyWait.interval = 200*time.Millisecond, 10*time.Millisecond

	var out, errOut bytes.Buffer
	err = run(context.Background(), []string{"context", "save", "--config", path}, &out, &errOut)
	return dials, errOut.String(), err
}

var refused = grpcstatus.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing: dial tcp 10.0.0.23:9443: connect: connection refused\"")

func TestContextSaveWaitsForListener(t *testing.T) {
	dials, stderr, err := saveContextWithWorkerDial(t, []error{refused, refused})

	if err != nil {
		t.Fatalf("context save: %v\n%s", err, stderr)
	}
	if dials != 3 {
		t.Fatalf("worker dialed %d times, want 3", dials)
	}
	if !strings.Contains(stderr, "Waiting for node worker-1 management endpoint "+workerDesiredEndpoint) {
		t.Fatalf("no progress for the waiting node:\n%s", stderr)
	}
}

func TestContextSaveRefusedBeyondBound(t *testing.T) {
	dials, stderr, err := saveContextWithWorkerDial(t, repeatError(refused, 1000))

	if err == nil {
		t.Fatalf("context save succeeded against a node that never listens\n%s", stderr)
	}
	if dials < 2 || dials > 100 {
		t.Fatalf("worker dialed %d times, want a bounded number of retries", dials)
	}
	for _, want := range []string{"worker-1", workerDesiredEndpoint, "refused connections", "systemctl status katlc-agent", "rerun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestContextSaveDoesNotRetryOtherErrors(t *testing.T) {
	for name, failure := range map[string]error{
		"handshake": grpcstatus.Error(codes.Unavailable, "authentication handshake failed: x509: certificate signed by unknown authority"),
		"denied":    grpcstatus.Error(codes.PermissionDenied, "connection refused by policy"),
	} {
		t.Run(name, func(t *testing.T) {
			dials, stderr, err := saveContextWithWorkerDial(t, []error{failure})

			if err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("error = %v, want %v", err, failure)
			}
			if dials != 1 {
				t.Fatalf("worker dialed %d times, want 1", dials)
			}
			if strings.Contains(stderr, "Waiting") {
				t.Fatalf("reported waiting for a non-transient error:\n%s", stderr)
			}
		})
	}
}

func repeatError(err error, n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
}
