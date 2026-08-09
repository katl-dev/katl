package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/managementidentity"
)

type shutdownTestDispatcher struct {
	called chan struct{}
}

func (d *shutdownTestDispatcher) Dispatch(context.Context, operation.OperationRecord) error {
	return nil
}

func (d *shutdownTestDispatcher) Shutdown(context.Context) error {
	close(d.called)
	return nil
}

func TestServeShutsDownDispatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dispatcher := &shutdownTestDispatcher{called: make(chan struct{})}
	root := t.TempDir()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "test", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "node-a", time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteManagementIdentity(root, "node-a", manifest.ManagementIdentity{
		CACertificate: node.CACertificate, ServerCertificate: node.ServerCertificate, ServerPrivateKey: node.ServerPrivateKey,
	}); err != nil {
		t.Fatal(err)
	}
	err = Serve(ctx, ServeConfig{
		Root:       root,
		Listen:     "tcp://127.0.0.1:0",
		Dispatcher: dispatcher,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve() error = %v, want context cancellation", err)
	}
	select {
	case <-dispatcher.called:
	default:
		t.Fatal("Serve did not shut down its dispatcher")
	}
}

func TestParseListenAcceptsTCPOnly(t *testing.T) {
	network, address, err := parseListen("tcp://0.0.0.0:9443")
	if err != nil {
		t.Fatal(err)
	}
	if network != "tcp" || address != "0.0.0.0:9443" {
		t.Fatalf("parseListen = %q %q, want tcp 0.0.0.0:9443", network, address)
	}

	for _, value := range []string{"unix:///run/katlc.sock", "localhost:9443", "tcp://"} {
		t.Run(value, func(t *testing.T) {
			if _, _, err := parseListen(value); err == nil {
				t.Fatalf("parseListen(%q) succeeded, want error", value)
			}
		})
	}
}
