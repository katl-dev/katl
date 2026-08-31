package apiproxy

import (
	"context"
	"crypto/x509"
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
