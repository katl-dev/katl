package agent

import (
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/installer/apivip"
)

func TestControlPlaneEndpointStatusReportsVIPOwnership(t *testing.T) {
	root := t.TempDir()
	writeEndpointConfig(t, root)
	live := apivip.Status{
		APIVersion:              apivip.StatusAPIVersion,
		Kind:                    apivip.StatusKind,
		VIPInterfaceReady:       true,
		LocalVIPOwned:           true,
		HealthState:             apivip.HealthHealthy,
		OwnershipState:          apivip.OwnershipOwned,
		LastOwnershipTransition: "2026-08-31T12:00:00Z",
	}
	data, err := apivip.MarshalStatus(live)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, apivip.LiveStatusPath), string(data))
	status, err := controlPlaneEndpointStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetEndpoint() != "api.home.example:6443" || status.GetVip() != "10.40.0.10/32" || status.GetState() != "active" || !status.GetLocalApiReady() || !status.GetLocalVipOwned() {
		t.Fatalf("status = %#v", status)
	}
}

func TestEndpointProductState(t *testing.T) {
	config, err := apivip.Normalize(apivip.Config{Endpoint: apivip.Endpoint{Host: "api.home.example", VIP: "10.40.0.10/32"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		live apivip.Status
		want string
	}{
		{name: "network", live: apivip.Status{}, want: "waiting-for-network"},
		{name: "CA", live: apivip.Status{VIPInterfaceReady: true, HealthFailure: "waiting for kubeadm API CA"}, want: "waiting-for-kubeadm-ca"},
		{name: "API", live: apivip.Status{VIPInterfaceReady: true}, want: "waiting-for-apiserver"},
		{name: "released", live: apivip.Status{VIPInterfaceReady: true, HealthState: apivip.HealthHealthy}, want: "released"},
		{name: "active", live: apivip.Status{VIPInterfaceReady: true, HealthState: apivip.HealthHealthy, LocalVIPOwned: true}, want: "active"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := endpointProductState(config, test.live); got != test.want {
				t.Fatalf("state = %q, want %q", got, test.want)
			}
		})
	}
}

func writeEndpointConfig(t *testing.T, root string) {
	t.Helper()
	plan, err := apivip.RenderNativeEtcFiles(apivip.RenderRequest{
		NodeRole: "control-plane",
		Config:   apivip.Config{Endpoint: apivip.Endpoint{Host: "api.home.example", VIP: "10.40.0.10/32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range plan.Files {
		if file.Path == apivip.ConfigPath || file.Path == apivip.OwnershipEnabledPath {
			writeTestFile(t, filepath.Join(root, file.Path), file.Content)
		}
	}
}
