package apiproxy

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckReadyVerifiesTLSAndReadiness(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer api.Close()
	cert, err := x509.ParseCertificate(api.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	server := Server{Config: Config{TLSName: "example.com", CAFile: caPath, CheckTimeout: time.Second}}
	if err := server.checkReady(context.Background(), Backend{Name: "cp-1", Address: api.Listener.Addr().String()}); err != nil {
		t.Fatalf("checkReady() error = %v", err)
	}
	server.Config.TLSName = "wrong.example"
	if err := server.checkReady(context.Background(), Backend{Name: "cp-1", Address: api.Listener.Addr().String()}); err == nil {
		t.Fatal("checkReady() error = nil for wrong TLS identity")
	}
}

func TestServeConnectionForwardsWithoutTerminatingTLS(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	pool := newPool([]Backend{{Name: "cp-1", Address: backend.Addr().String(), Local: true}})
	pool.update("cp-1", true, "ready", time.Now())
	server := Server{Config: Config{CheckTimeout: time.Second}, pool: pool}
	client, proxy := net.Pipe()
	defer client.Close()
	go server.serveConnection(context.Background(), proxy)
	payload := []byte{0x16, 0x03, 0x03, 0x00, 0x04, 0xde, 0xad, 0xbe, 0xef}
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("forwarded payload = %x, want %x", got, payload)
	}
}

func TestReadinessCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer api.Close()
	defer close(release)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	server := Server{Config: Config{TLSName: "example.com", CAFile: caPath, CheckTimeout: time.Minute}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.checkReadyAddress(ctx, api.Listener.Addr().String()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("readiness request did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled readiness check succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled readiness check is still blocked reading response")
	}
}

func TestForwardCancellation(t *testing.T) {
	client, proxyClient := net.Pipe()
	upstream, proxyUpstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()
	defer proxyClient.Close()
	defer proxyUpstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { (&Server{}).forward(ctx, proxyClient, proxyUpstream); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarding did not stop on cancellation")
	}
	for _, peer := range []net.Conn{client, upstream} {
		if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("peer read = %v, want EOF", err)
		}
	}
}

func TestRunShutdown(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	}))
	api.Listener.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:6443")
	if err != nil {
		t.Fatal(err)
	}
	api.Listener = listener
	api.StartTLS()
	defer api.Close()
	defer close(release)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	server := Server{Config: Config{
		TLSName: "example.com", CAFile: caPath, CheckTimeout: time.Minute,
		CanonicalEndpoint: api.Listener.Addr().String(),
		Listeners:         []Listener{{Address: "127.0.0.1:7445", Exposure: ExposureNodeLocal}},
		Backends:          []Backend{{Name: "cp-1", Address: api.Listener.Addr().String(), Local: true}},
	}, StatusPath: filepath.Join(dir, "status.json")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	for range 2 {
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("proxy exited: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("readiness checks did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not stop")
	}
	data, err := os.ReadFile(server.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Canonical.LastChecked.IsZero() || len(status.Backends) != 1 || status.Backends[0].LastChecked.IsZero() {
		t.Fatalf("Run returned before checks recorded their final status: %s", data)
	}
	listener, err = net.Listen("tcp", "127.0.0.1:7445")
	if err != nil {
		t.Fatalf("Run returned without releasing listener: %v", err)
	}
	listener.Close()
}
