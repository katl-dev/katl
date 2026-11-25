package vmtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestHandoffBase(t *testing.T) {
	tests := []struct {
		address string
		want    string
	}{
		{address: "192.168.122.44", want: "http://192.168.122.44:8080"},
		{address: "fd00::44", want: "http://[fd00::44]:8080"},
	}
	for _, test := range tests {
		got, err := handoffBase(VMConfig{Network: VMNetworkConfig{Address: test.address}})
		if err != nil || got != test.want {
			t.Fatalf("handoffBase(%q) = %q, %v; want %q", test.address, got, err, test.want)
		}
	}
}

func TestHandoffBaseRequiresReservedAddress(t *testing.T) {
	if _, err := handoffBase(VMConfig{Network: VMNetworkConfig{MAC: "52:54:00:00:00:01"}}); err == nil || !strings.Contains(err.Error(), "reserved address") {
		t.Fatalf("handoffBase() error = %v, want it to require a reserved address", err)
	}
}

func TestProbeHandoff(t *testing.T) {
	waiting := httptest.NewServer(handoff.NewHandoffServer(nil).Handler())
	defer waiting.Close()
	// The installer serves its API while a boot-provided install runs.
	automaticServer := handoff.NewHandoffServer(nil)
	automaticServer.BeginAutomatic("cp-1")
	automatic := httptest.NewServer(automaticServer.Handler())
	defer automatic.Close()
	// A refusal before disk mutation returns the API to waiting.
	refusedServer := handoff.NewHandoffServer(nil)
	refusedServer.BeginAutomatic("cp-1")
	if !refusedServer.PrepareRetry(installstatus.New(installstatus.StateFailedBeforeMutation, time.Now())) {
		t.Fatal("refusal before disk mutation was not retryable")
	}
	refused := httptest.NewServer(refusedServer.Handler())
	defer refused.Close()
	// Another service answers the port, but not as the installer.
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	if err := probeHandoff(context.Background(), waiting.URL); err != nil {
		t.Fatalf("probe of a waiting handoff API: %v", err)
	}
	if err := probeHandoff(context.Background(), refused.URL); err != nil {
		t.Fatalf("probe of a handoff API waiting after a refusal: %v", err)
	}
	if err := probeHandoff(context.Background(), automatic.URL); err == nil {
		t.Fatal("probe accepted a handoff API that is running an automatic install")
	}
	if err := probeHandoff(context.Background(), other.URL); err == nil {
		t.Fatal("probe accepted a server that is not the handoff API")
	}
	if err := probeHandoff(context.Background(), closedURL); err == nil {
		t.Fatal("probe succeeded with nothing listening")
	}
}

func TestFirstInstallGuestHandoffRequiresReservedAddress(t *testing.T) {
	root := t.TempDir()
	_, vmConfig := vmFixture(t)
	result := Result{Artifacts: ArtifactPaths{HandoffRequest: filepath.Join(root, "req.json")}}

	_, err := configureGuestHandoff(result, FirstInstallConfig{Installer: InstallerBootConfig{VM: vmConfig}}, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved address") {
		t.Fatalf("configureGuestHandoff() error = %v, want it to require a reserved address", err)
	}
}
