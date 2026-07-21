package agent

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestManagedEndpointLifecycleFollowsGeneratedEnablement(t *testing.T) {
	root := t.TempDir()
	var calls [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		return ToolResult{}
	}

	paused, err := pauseManagedEndpoint(context.Background(), root, run)
	if err != nil || paused {
		t.Fatalf("pause without managed endpoint = %v, %v", paused, err)
	}
	if err := resumeManagedEndpoint(context.Background(), root, run); err != nil {
		t.Fatalf("resume without managed endpoint: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("commands without managed endpoint = %#v", calls)
	}

	writeTestFile(t, filepath.Join(root, bgpapivip.AdvertisementEnabledPath), "enabled\n")
	paused, err = pauseManagedEndpoint(context.Background(), root, run)
	if err != nil || !paused {
		t.Fatalf("pause managed endpoint = %v, %v", paused, err)
	}
	if err := resumeManagedEndpoint(context.Background(), root, run); err != nil {
		t.Fatalf("resume managed endpoint: %v", err)
	}
	want := [][]string{
		{"systemctl", "stop", endpointAdvertiserUnit},
		{endpointAdvertiserCommand, "withdraw"},
		{"systemctl", "start", endpointAdvertiserUnit},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("endpoint lifecycle commands = %#v, want %#v", calls, want)
	}
}

func TestManagedEndpointJoinLifecycleRemovesAndRestoresLocalVIP(t *testing.T) {
	root := t.TempDir()
	writeManagedEndpointTestConfig(t, root)
	discoveryPath := writeJoinDiscoveryTestConfig(t, root, "10.0.0.10")
	var calls [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		if reflect.DeepEqual(argv, []string{managedEndpointIP, "-json", "route", "get", "10.0.0.10"}) {
			return ToolResult{Stdout: []byte(`[{"dst":"10.0.0.10","dev":"enp1s0"}]`)}
		}
		return ToolResult{}
	}

	suspended, route, err := suspendManagedEndpointForJoin(context.Background(), root, discoveryPath, run)
	if err != nil || !suspended {
		t.Fatalf("suspendManagedEndpointForJoin() = %v, %v", suspended, err)
	}
	if err := removeManagedJoinRoute(context.Background(), route, run); err != nil {
		t.Fatalf("removeManagedJoinRoute(): %v", err)
	}
	if err := resumeManagedEndpointAfterJoin(context.Background(), root, run); err != nil {
		t.Fatalf("resumeManagedEndpointAfterJoin(): %v", err)
	}
	want := [][]string{
		{"systemctl", "stop", endpointAdvertiserUnit},
		{endpointAdvertiserCommand, "withdraw"},
		{managedEndpointInterface, "down", "katl-api0"},
		{managedEndpointIP, "address", "flush", "dev", "katl-api0", "to", "10.40.0.10/32"},
		{managedEndpointIP, "-json", "route", "get", "10.0.0.10"},
		{managedEndpointIP, "route", "add", "10.40.0.10/32", "via", "10.0.0.10", "dev", "enp1s0"},
		{managedEndpointKubectl, "--kubeconfig", filepath.Join(root, discoveryPath), "--server", "https://api.home.example:6443", "--request-timeout=10s", "get", "--raw=/version"},
		{managedEndpointIP, "route", "del", "10.40.0.10/32", "via", "10.0.0.10", "dev", "enp1s0"},
		{managedEndpointInterface, "up", "katl-api0"},
		{managedEndpointIP, "address", "replace", "10.40.0.10/32", "dev", "katl-api0"},
		{"systemctl", "start", endpointAdvertiserUnit},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("join endpoint lifecycle commands = %#v, want %#v", calls, want)
	}
}

func TestManagedEndpointJoinLifecycleFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeManagedEndpointTestConfig(t, root)
	discoveryPath := writeJoinDiscoveryTestConfig(t, root, "10.0.0.10")
	var calls int
	run := func(_ context.Context, _ []string, _ func(int)) ToolResult {
		calls++
		if calls == 3 {
			return ToolResult{Err: errors.New("networkctl failed"), ExitStatus: 1}
		}
		return ToolResult{}
	}

	suspended, route, err := suspendManagedEndpointForJoin(context.Background(), root, discoveryPath, run)
	if err == nil || suspended {
		t.Fatalf("suspendManagedEndpointForJoin() = %v, %v", suspended, err)
	}
	if route != nil {
		t.Fatalf("route = %#v, want nil", route)
	}
	if calls != 3 {
		t.Fatalf("commands after interface failure = %d, want 3", calls)
	}
}

func TestManagedEndpointJoinUsesExistingFabricRouteForOffLinkInit(t *testing.T) {
	root := t.TempDir()
	writeManagedEndpointTestConfig(t, root)
	discoveryPath := writeJoinDiscoveryTestConfig(t, root, "10.50.0.10")
	var calls [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		if reflect.DeepEqual(argv, []string{managedEndpointIP, "-json", "route", "get", "10.50.0.10"}) {
			return ToolResult{Stdout: []byte(`[{"dst":"10.50.0.10","gateway":"10.0.0.1","dev":"enp1s0"}]`)}
		}
		return ToolResult{}
	}

	suspended, route, err := suspendManagedEndpointForJoin(context.Background(), root, discoveryPath, run)
	if err != nil || !suspended || route != nil {
		t.Fatalf("suspendManagedEndpointForJoin() = %v, %#v, %v", suspended, route, err)
	}
	for _, call := range calls {
		if len(call) > 2 && call[0] == managedEndpointIP && call[1] == "route" && call[2] == "add" {
			t.Fatalf("off-link init installed a direct route: %#v", call)
		}
	}
}

func TestManagedEndpointJoinProbeFailureRemovesTemporaryRoute(t *testing.T) {
	root := t.TempDir()
	writeManagedEndpointTestConfig(t, root)
	discoveryPath := writeJoinDiscoveryTestConfig(t, root, "10.0.0.10")
	var calls [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		switch {
		case reflect.DeepEqual(argv, []string{managedEndpointIP, "-json", "route", "get", "10.0.0.10"}):
			return ToolResult{Stdout: []byte(`[{"dst":"10.0.0.10","dev":"enp1s0"}]`)}
		case len(argv) > 0 && argv[0] == managedEndpointKubectl:
			return ToolResult{Err: errors.New("API request timed out"), ExitStatus: 1}
		default:
			return ToolResult{}
		}
	}

	suspended, route, err := suspendManagedEndpointForJoin(context.Background(), root, discoveryPath, run)
	if err == nil || suspended || route != nil {
		t.Fatalf("suspendManagedEndpointForJoin() = %v, %#v, %v", suspended, route, err)
	}
	wantLast := []string{managedEndpointIP, "route", "del", "10.40.0.10/32", "via", "10.0.0.10", "dev", "enp1s0"}
	if !reflect.DeepEqual(calls[len(calls)-1], wantLast) {
		t.Fatalf("last command = %#v, want route cleanup %#v", calls[len(calls)-1], wantLast)
	}
}

func writeJoinDiscoveryTestConfig(t *testing.T, root, host string) string {
	t.Helper()
	path := "/run/katl/bootstrap-join/test/discovery.conf"
	writeTestFile(t, filepath.Join(root, path), "apiVersion: v1\nkind: Config\nclusters:\n  - name: katl-discovery\n    cluster:\n      server: https://"+host+":6443\n")
	return path
}

func TestManagedEndpointPauseFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, bgpapivip.AdvertisementEnabledPath), "enabled\n")
	run := func(_ context.Context, _ []string, _ func(int)) ToolResult {
		return ToolResult{Err: errors.New("service failed"), ExitStatus: 1}
	}

	paused, err := pauseManagedEndpoint(context.Background(), root, run)
	if err == nil || paused {
		t.Fatalf("pauseManagedEndpoint() = %v, %v", paused, err)
	}
}

func writeManagedEndpointTestConfig(t *testing.T, root string) {
	t.Helper()
	plan, err := bgpapivip.RenderNativeEtcFiles(bgpapivip.RenderRequest{
		NodeRole: "control-plane",
		Config: bgpapivip.Config{
			Endpoint:     bgpapivip.Endpoint{Host: "api.home.example", VIP: "10.40.0.10/32"},
			VIPInterface: bgpapivip.VIPInterface{Kind: "dummy", Name: "katl-api0", MTU: 1500},
			Routing:      bgpapivip.Routing{RouterID: "10.0.0.11", LocalASN: 64512, SourceAddress: "10.0.0.11", SourceInterface: "enp1s0"},
			FabricPeers: []bgpapivip.Peer{{
				Name:                  "router-a",
				Address:               "10.0.0.1",
				ASN:                   64500,
				AllowedExportPrefixes: []string{"10.40.0.10/32"},
			}},
		},
		LocalInterfaceAddresses: map[string][]string{"enp1s0": {"10.0.0.11/24"}},
	})
	if err != nil {
		t.Fatalf("render managed endpoint test config: %v", err)
	}
	for _, file := range plan.Files {
		if file.Path == bgpapivip.ConfigPath || file.Path == bgpapivip.AdvertisementEnabledPath {
			writeTestFile(t, filepath.Join(root, file.Path), file.Content)
		}
	}
}
