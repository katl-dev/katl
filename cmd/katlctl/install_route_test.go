package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

// routeInstallers makes installer connections to the map's keys, as
// host:port, reach the mapped listener addresses. Other dials fail, so a
// request to an unexpected port is an error.
func routeInstallers(t *testing.T, routes map[string]string) {
	t.Helper()
	previous := installerDialContext
	installerDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, ok := routes[address]
		if !ok {
			return nil, fmt.Errorf("no fake installer at %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	t.Cleanup(func() { installerDialContext = previous })
}

func TestInstallStatusDialsInstallerPort(t *testing.T) {
	server := httptest.NewServer(handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{}).Handler())
	defer server.Close()
	// The agent port in management.address is not the installer's.
	routeInstallers(t, map[string]string{"10.99.0.11:8080": server.Listener.Addr().String()})
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	source := strings.Replace(configBundleSource(), "address: 10.0.0.11", "address: 10.99.0.11:19443", 1)
	source = strings.Replace(source, "managementAuthentication: mtls", "managementAuthentication: mtls\n  managementIdentity: unavailable-secret.yaml", 1)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"install", "status", "--config", path, "--node", "cp-1", "-o", "json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report installHandoffReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Endpoint != "http://10.99.0.11:8080" {
		t.Fatalf("endpoint = %s", report.Endpoint)
	}
}
