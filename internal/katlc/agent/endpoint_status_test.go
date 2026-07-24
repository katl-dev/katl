package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestControlPlaneEndpointStatusReportsBoundedProductState(t *testing.T) {
	root := t.TempDir()
	writeManagedEndpointConfig(t, root, true)
	live := bgpapivip.Status{
		APIVersion:                  bgpapivip.StatusAPIVersion,
		Kind:                        bgpapivip.StatusKind,
		EndpointHost:                "api.home.example",
		EndpointPort:                6443,
		VIPPrefix:                   "10.40.0.10/32",
		VIPInterfaceReady:           true,
		LocalVIPOwned:               true,
		HealthState:                 bgpapivip.HealthHealthy,
		AdvertisementState:          bgpapivip.AdvertisementAdvertised,
		BirdProcessActive:           true,
		BirdControlSocketReady:      true,
		LastAdvertisementTransition: "2026-07-19T12:00:00Z",
		PeerSummary: []bgpapivip.PeerRuntimeStatus{
			{Name: "10.0.0.1", Kind: "fabric", ASN: 64500, SessionState: "established", LocalAddress: "10.0.0.11", ExportedRoutes: 1},
			{Name: "cilium", Kind: "route-exchange", SessionState: "established", AcceptedRoutes: 3},
			{Name: "cilium", Kind: "route-exchange-export", SessionState: "up", ExportedRoutes: 3},
		},
	}
	data, err := bgpapivip.MarshalStatus(live)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, bgpapivip.LiveStatusPath), string(data))

	status, err := controlPlaneEndpointStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetEndpoint() != "api.home.example:6443" || status.GetVip() != "10.40.0.10/32" || status.GetState() != "advertised" || !status.GetLocalApiReady() || !status.GetLocalVipOwned() || !status.GetRouteOriginated() {
		t.Fatalf("endpoint status = %#v", status)
	}
	if status.GetSelectedSourceAddress() != "10.0.0.11" || status.GetRouterId() != "10.0.0.11" || status.GetLastTransitionTime() != "2026-07-19T12:00:00Z" {
		t.Fatalf("routing identity = %#v", status)
	}
	if len(status.GetPeers()) != 1 || !status.GetPeers()[0].GetRouteExported() || status.GetPeers()[0].GetAsn() != 64500 {
		t.Fatalf("peer status = %#v", status.GetPeers())
	}
	if len(status.GetRouteExchange()) != 1 || status.GetRouteExchange()[0].GetAcceptedRoutes() != 3 || status.GetRouteExchange()[0].GetExportedRoutes() != 3 {
		t.Fatalf("route exchange status = %#v", status.GetRouteExchange())
	}
}

func TestControlPlaneEndpointStatusDistinguishesStartupStates(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		live    *bgpapivip.Status
		want    string
	}{
		{name: "disabled", enabled: false, want: "disabled"},
		{name: "starting", enabled: true, want: "starting"},
		{name: "network", enabled: true, live: &bgpapivip.Status{VIPInterfaceReady: false}, want: "waiting-for-network"},
		{name: "ca", enabled: true, live: &bgpapivip.Status{VIPInterfaceReady: true, BirdProcessActive: true, BirdControlSocketReady: true, HealthFailure: "waiting for kubeadm API CA"}, want: "waiting-for-kubeadm-ca"},
		{name: "api", enabled: true, live: &bgpapivip.Status{VIPInterfaceReady: true, BirdProcessActive: true, BirdControlSocketReady: true, HealthState: bgpapivip.HealthUnhealthy}, want: "waiting-for-apiserver"},
		{name: "peer", enabled: true, live: &bgpapivip.Status{VIPInterfaceReady: true, BirdProcessActive: true, BirdControlSocketReady: true, HealthState: bgpapivip.HealthHealthy}, want: "waiting-for-peer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeManagedEndpointConfig(t, root, tt.enabled)
			if tt.live != nil {
				tt.live.APIVersion = bgpapivip.StatusAPIVersion
				tt.live.Kind = bgpapivip.StatusKind
				data, err := bgpapivip.MarshalStatus(*tt.live)
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(root, bgpapivip.LiveStatusPath), string(data))
			}
			status, err := controlPlaneEndpointStatus(root)
			if err != nil {
				t.Fatal(err)
			}
			if status.GetState() != tt.want {
				t.Fatalf("state = %q, want %q; status = %#v", status.GetState(), tt.want, status)
			}
		})
	}
}

func TestControlPlaneEndpointStatusIsAbsentForExternalEndpointNode(t *testing.T) {
	status, err := controlPlaneEndpointStatus(t.TempDir())
	if err != nil || status != nil {
		t.Fatalf("external endpoint status = %#v, %v", status, err)
	}
}

func writeManagedEndpointConfig(t *testing.T, root string, enabled bool) {
	t.Helper()
	content := `apiVersion: apps.katl.dev/v1alpha1
kind: BGPAPIEndpoint
spec:
  endpoint:
    host: api.home.example
    port: 6443
    vip: 10.40.0.10/32
    addressFamily: ipv4
  vipInterface:
    kind: dummy
    name: katl-api0
  routing:
    routerID: 10.0.0.11
    localASN: 64512
    sourceAddress: 10.0.0.11
  advertiseOn:
    roles: [control-plane]
  fabricPeers:
    - name: router-a
      address: 10.0.0.1
      asn: 64500
      allowedExportPrefixes: [10.40.0.10/32]
  routeExchange:
    - name: cilium
      listenPort: 179
      peerASN: 64512
      exportToFabric:
        - cidr: 10.50.0.0/16
  advertisement:
    enabled: `
	if enabled {
		content += "true\n"
	} else {
		content += "false\n"
	}
	content += `    startWithdrawn: true
    advertiseAfterHealthy: true
    withdrawOnFailure: true
  health: {}
`
	path := filepath.Join(root, bgpapivip.ConfigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
