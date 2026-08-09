package transport

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc/test/bufconn"
)

func TestManagementTLSRequiresTheOperatorAndExpectedNode(t *testing.T) {
	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(identity, now)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := ServerTLSConfigForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := ClientTLSConfig(client, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if serverErr, clientErr := handshake(serverTLS, clientTLS); serverErr != nil || clientErr != nil {
		t.Fatalf("authenticated handshake: server=%v client=%v", serverErr, clientErr)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(client.CACertificate)) {
		t.Fatal("append management CA")
	}
	unauthenticated := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "cp-1"}
	if serverErr, clientErr := handshake(serverTLS, unauthenticated); serverErr == nil && clientErr == nil {
		t.Fatal("handshake without a client certificate succeeded")
	}

	wrongIdentity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	wrongClient, err := managementidentity.Client(wrongIdentity, now)
	if err != nil {
		t.Fatal(err)
	}
	wrongClientTLS, err := ClientTLSConfig(wrongClient, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if serverErr, clientErr := handshake(serverTLS, wrongClientTLS); serverErr == nil && clientErr == nil {
		t.Fatal("handshake from another cluster identity succeeded")
	}

	wrongNodeTLS, err := ClientTLSConfig(client, "cp-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, clientErr := handshake(serverTLS, wrongNodeTLS); clientErr == nil || !strings.Contains(clientErr.Error(), "cp-2") {
		t.Fatalf("wrong-node handshake error = %v", clientErr)
	}
}

func handshake(serverConfig, clientConfig *tls.Config) (error, error) {
	listener := bufconn.Listen(1 << 20)
	defer listener.Close()
	serverResult := make(chan error, 1)
	go func() {
		serverSide, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer serverSide.Close()
		serverResult <- tls.Server(serverSide, serverConfig).Handshake()
	}()
	clientSide, err := listener.Dial()
	if err != nil {
		return <-serverResult, err
	}
	defer clientSide.Close()
	clientErr := tls.Client(clientSide, clientConfig).Handshake()
	_ = clientSide.Close()
	return <-serverResult, clientErr
}
