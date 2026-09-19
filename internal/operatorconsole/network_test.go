package operatorconsole

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestCollectedVRFRefresh(t *testing.T) {
	collector := Collector{
		Root: t.TempDir(),
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Name: "bond1", Flags: net.FlagUp}}, nil
		},
		Addrs: func(net.Interface) ([]net.Addr, error) {
			return []net.Addr{testAddr("10.1.1.10/24")}, nil
		},
		InterfaceVRFs: func(context.Context) (map[string]string, error) {
			return map[string]string{"bond1": "mgmt"}, nil
		},
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if len(snapshot.DisplayInterfaces) != 1 || snapshot.DisplayInterfaces[0].VRF != "mgmt" {
		t.Fatalf("network = %+v", snapshot.DisplayInterfaces)
	}

	collector.InterfaceVRFs = func(context.Context) (map[string]string, error) {
		return nil, errors.New("link dump unavailable")
	}
	collector.Collect(&snapshot)
	if len(snapshot.DisplayInterfaces) != 1 || snapshot.DisplayInterfaces[0].VRF != "" {
		t.Fatalf("failed observation retained stale VRF or lost addresses: %+v", snapshot.DisplayInterfaces)
	}
}

func TestInterfaceVRFs(t *testing.T) {
	got, err := decodeInterfaceVRFs([]byte(`[
 {"ifname":"bond0"},
 {"ifname":"bond1","master":"mgmt"},
 {"ifname":"eno1","master":"bond1"},
 {"ifname":"mgmt","linkinfo":{"info_kind":"vrf"}},
 {"ifname":"vlan20","master":"bridge0"},
 {"ifname":"bridge0","linkinfo":{"info_kind":"bridge"}},
 {"ifname":"missing","master":"gone"},
 {"ifname":"cycle1","master":"cycle2"},
 {"ifname":"cycle2","master":"cycle1"}
]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"bond1": "mgmt", "eno1": "mgmt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("VRFs = %v, want %v", got, want)
	}
	if _, err := decodeInterfaceVRFs([]byte(`not JSON`)); err == nil {
		t.Fatal("malformed link data accepted")
	}
}

func TestNetworkAlignment(t *testing.T) {
	for _, mode := range []Mode{ModeRuntime, ModeInstaller} {
		t.Run(string(mode), func(t *testing.T) {
			snapshot := Snapshot{Mode: mode, DisplayInterfaces: []NetworkInterface{
				{Name: "bond0", Addresses: []string{"10.254.1.1/31"}},
				{Name: "bond0.300", Addresses: []string{"10.254.3.1/29"}},
				{Name: "bond1", Addresses: []string{"10.1.1.10/24"}, VRF: "vrf-mgmt"},
			}}
			lines := strings.Split(string(renderDashboard(&snapshot, nil, 240, 40, false)), "\n")
			for _, want := range []string{
				"Network:          bond0: 10.254.1.1/31",
				"                  bond0.300: 10.254.3.1/29",
				"                  bond1: 10.1.1.10/24 (VRF: vrf-mgmt)",
			} {
				found := false
				for _, line := range lines {
					found = found || strings.HasPrefix(line, want)
				}
				if !found {
					t.Fatalf("missing aligned row %q in:\n%s", want, strings.Join(lines, "\n"))
				}
			}
		})
	}
}

func TestBootstrapGuidance(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		State:            installstatus.StateWaitingForClusterBootstrap,
		GenerationHealth: generation.HealthStateHealthy,
	}
	model := NewDashboardModel(&snapshot)
	if model.Host.State != PresentationProgressing || model.Host.Label != "Waiting for cluster bootstrap" {
		t.Fatalf("unbootstrapped host = %+v", model.Host)
	}
	got := string(renderDashboard(&snapshot, nil, 240, 40, false))
	if !strings.Contains(got, "Waiting for cluster bootstrap") || !strings.Contains(got, "katlctl cluster bootstrap --config <cluster.yaml>") {
		t.Fatalf("missing bootstrap guidance:\n%s", got)
	}
	snapshot.State = installstatus.StateRuntimeFailedNeedsRepair
	model = NewDashboardModel(&snapshot)
	if model.Kubernetes.NextAction != "" || model.Host.State != PresentationFailed {
		t.Fatalf("repair state obscured by bootstrap guidance: %+v", model)
	}
	snapshot.State = installstatus.StateWaitingForClusterBootstrap
	snapshot.StatusStale = true
	if model = NewDashboardModel(&snapshot); model.Kubernetes.NextAction != "" {
		t.Fatalf("stale status suggests bootstrap: %+v", model)
	}
}
