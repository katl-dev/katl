package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestTLSResponseRequiresCompleteInitialSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    http2.Flags
		stream   uint32
		payload  []byte
		truncate bool
		want     bool
	}{
		{name: "server disables push", payload: []byte{0, 2, 0, 0, 0, 0}, want: true},
		{name: "unrecognized setting", payload: []byte{255, 255, 0, 0, 0, 1}, want: true},
		{name: "nonzero stream", stream: 1},
		{name: "acknowledgment", flags: http2.FlagSettingsAck},
		{name: "truncated setting", payload: []byte{0, 5, 0, 0, 64, 0}, truncate: true},
		{name: "server enables push", payload: []byte{0, 2, 0, 0, 0, 1}},
		{name: "invalid initial window size", payload: []byte{0, 4, 128, 0, 0, 0}},
		{name: "invalid maximum frame size", payload: []byte{0, 5, 0, 0, 0, 1}},
		{name: "invalid extended CONNECT setting", payload: []byte{0, 8, 0, 0, 0, 2}},
		{name: "invalid priority setting", payload: []byte{0, 9, 0, 0, 0, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := http2.NewFramer(&wire, nil).WriteRawFrame(http2.FrameSettings, tc.flags, tc.stream, tc.payload); err != nil {
				t.Fatal(err)
			}
			if tc.truncate {
				wire.Truncate(wire.Len() - 1)
			}
			if got := isPlaintextHTTP2(bufio.NewReader(&wire)); got != tc.want {
				t.Errorf("plaintext HTTP/2 = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagementPlaintextHTTP2UsesWireEvidence(t *testing.T) {
	clientBundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(clientBundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := ClientCredentialsForNode(client, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	serverBundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := managementidentity.EnsureNode(&serverBundle, "cp-1", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, _, err := ServerTLSConfigForNode(identity)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		want    bool
		options []grpc.ServerOption
	}{
		{name: "plaintext gRPC", want: true},
		{name: "untrusted certificate", options: []grpc.ServerOption{grpc.Creds(credentials.NewTLS(serverTLS))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(tc.options...)
			go server.Serve(listener)
			defer server.Stop()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx, observation := ObserveManagementConnection(WithoutManagementWait(ctx))
			endpoint := listener.Addr().String()
			conn, err := grpc.NewClient("passthrough:///"+endpoint, ManagementDialOptions(ctx, "cp-1", endpoint, creds)...)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
				t.Fatal("unexpected successful mTLS call")
			}
			if observed := observation.PlaintextHTTP2(); observed != tc.want {
				t.Errorf("plaintext HTTP/2 = %v, want %v", observed, tc.want)
			}
		})
	}
}

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

// A non-gRPC HTTP server on the management port also answers without TLS; the
// refusal must keep the underlying TLS error and name the address check.
func TestClientHandshakeReportsNonTLSHTTPServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

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
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = NewClientCredentials(clientTLS).ClientHandshake(ctx, server.Listener.Addr().String(), conn)

	var header tls.RecordHeaderError
	if err == nil || !errors.As(err, &header) {
		t.Fatalf("handshake error = %v, want wrapped tls.RecordHeaderError", err)
	}
	for _, want := range []string{"answered without TLS", "katlctl management enable-mtls", "managementAuthentication: trusted-network", "katlc agent on 9443"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("handshake error = %v, missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "reprovision") {
		t.Fatalf("handshake error = %v, still requires reprovisioning", err)
	}
}
