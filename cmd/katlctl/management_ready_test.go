package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nettest"
	"google.golang.org/grpc"
	grpcstatus "google.golang.org/grpc/status"
)

// listenOnFirstWrite starts agent on port when the first byte is written, as a
// restarting katlc-agent begins listening after the first retry notice.
type listenOnFirstWrite struct {
	bytes.Buffer
	mu    sync.Mutex
	port  *nettest.Port
	agent agentapi.KatlcAgentServer
	once  sync.Once
	stop  func()
}

func (w *listenOnFirstWrite) Write(p []byte) (int, error) {
	w.once.Do(func() {
		listener, err := w.port.Listen()
		if err != nil {
			return
		}
		server := grpc.NewServer()
		agentapi.RegisterKatlcAgentServer(server, w.agent)
		go func() { _ = server.Serve(listener) }()
		w.stop = server.Stop
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Buffer.Write(p)
}

func trustedDial(t *testing.T) {
	t.Helper()
	old := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = old })
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		credentials := managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}
		return dialKatlcAgentTCP(withManagementDial(ctx, "node-a", &credentials), endpoint)
	}
}

// A command waits for a restarting agent and reports progress on the command's
// own stderr.
func TestCommandWaitsForRestartingAgent(t *testing.T) {
	trustedDial(t)
	port := nettest.ReservePort(t)
	stderr := &listenOnFirstWrite{port: port, agent: &clusterApplyIdentityServer{client: healthyHostClient("machine-a", "agent-a", "generation-0")}}
	t.Cleanup(func() {
		if stderr.stop != nil {
			stderr.stop()
		}
	})

	var stdout bytes.Buffer
	err := run(context.Background(), []string{"node", "status", "node-a", "--endpoint", port.Endpoint}, &stdout, stderr)

	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Waiting for node node-a management endpoint "+port.Endpoint) {
		t.Fatalf("stderr lacks progress:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "generation-0") {
		t.Fatalf("stdout lacks the status:\n%s", stdout.String())
	}
}

// Callers that poll for a node's state take the refusal at once, not after the
// wait that ordinary commands give a restarting agent.
func TestWithoutManagementWaitReachesDial(t *testing.T) {
	trustedDial(t)
	port := nettest.ReservePort(t)
	var progress bytes.Buffer
	ctx := transport.WithManagementProgress(transport.WithoutManagementWait(context.Background()), &progress)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := dialKatlcAgent(ctx, port.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

	if err == nil || strings.Contains(err.Error(), "katlc-agent is not listening") {
		t.Fatalf("error = %v, want the raw refusal", err)
	}
	if !strings.Contains(grpcstatus.Convert(err).Message(), "connection refused") {
		t.Fatalf("error = %v, want a refused connection", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second || progress.Len() != 0 {
		t.Fatalf("refusal took %s with progress %q, want an immediate silent failure", elapsed, progress.String())
	}
}
