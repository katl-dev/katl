package transport

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
)

// An mTLS client reaching a node that serves plaintext management must get the
// mode-mismatch refusal rather than a raw TLS record error.
func TestClientHandshakeReportsPlaintextNode(t *testing.T) {
	server := grpc.NewServer()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Stop()

	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(identity, now)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := ClientTLSConfig(client, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = NewClientCredentials(clientTLS).ClientHandshake(ctx, listener.Addr().String(), conn)
	if err == nil || !strings.Contains(err.Error(), "managementAuthentication: trusted-network") {
		t.Fatalf("handshake error = %v", err)
	}
}
