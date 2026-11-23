package inventory

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/apiproxy"
)

// natInventory places every node behind NAT: the workstation reaches each one
// at a 192.0.2.0/24 management address that no node has assigned, while the
// nodes themselves hold 10.0.1.0/24 Kubernetes addresses.
func natInventory(t *testing.T) Inventory {
	node := func(name string, role SystemRole, management, kubernetes string) Node {
		intent, ref := IntentControlPlane, "control-plane"
		if role == RoleWorker {
			intent, ref = IntentWorker, "worker"
		}
		return Node{
			Name: name, Address: mustManagementAddress(t, management), KubernetesAddress: mustKubernetesAddr(t, kubernetes), SystemRole: role,
			Access:            Access{Method: "agent"},
			KubeadmConfig:     KubeadmConfig{Ref: ref, Path: "/etc/katl/kubeadm/" + ref + "/config.yaml", Intent: intent},
			KubernetesVersion: "v1.36.1",
		}
	}
	return Inventory{
		ControlPlaneEndpoint: mustHostPort(t, "10.0.1.10:6443"),
		Nodes: []Node{
			node("cp-1", RoleControlPlane, "192.0.2.10", "10.0.1.10"),
			node("cp-2", RoleControlPlane, "192.0.2.11", "10.0.1.11"),
			node("worker-1", RoleWorker, "192.0.2.21", "10.0.1.21"),
		},
	}
}

// proxyTopologies are the address layouts the API proxy goldens in
// testdata/api-proxy cover. The single-address goldens are the v2026.11.1
// output; NAT and multihomed nodes differ only in whether the management
// address is assigned to the node, which the renderer cannot observe.
var proxyTopologies = []struct {
	name                               string
	cpManagement, cpKubernetes         string
	workerManagement, workerKubernetes string
}{
	{name: "single-address", cpManagement: "10.0.0.11", workerManagement: "10.0.0.21"},
	{name: "single-address", cpManagement: "10.0.0.11", cpKubernetes: "10.0.0.11", workerManagement: "10.0.0.21", workerKubernetes: "10.0.0.21"},
	// The workstation reaches the nodes through DNAT at 198.18.0.0/24.
	{name: "nat", cpManagement: "198.18.0.11", cpKubernetes: "10.0.0.11", workerManagement: "198.18.0.21", workerKubernetes: "10.0.0.21"},
	// The nodes hold both addresses on separate NICs.
	{name: "multihomed", cpManagement: "192.168.1.11", cpKubernetes: "10.0.0.11", workerManagement: "192.168.1.21", workerKubernetes: "10.0.0.21"},
}

func readProxyGolden(t *testing.T, topology, node string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "api-proxy", topology, node+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAPIProxyConfigTopologies(t *testing.T) {
	for _, topology := range proxyTopologies {
		nodes := []Node{
			{Name: "cp-1", Address: mustManagementAddress(t, topology.cpManagement), KubernetesAddress: mustKubernetesAddr(t, topology.cpKubernetes), SystemRole: RoleControlPlane},
			{Name: "worker-1", Address: mustManagementAddress(t, topology.workerManagement), KubernetesAddress: mustKubernetesAddr(t, topology.workerKubernetes), SystemRole: RoleWorker},
		}
		for _, node := range nodes {
			t.Run(topology.name+"/"+node.Name, func(t *testing.T) {
				want := readProxyGolden(t, topology.name, node.Name)

				config, known, err := APIProxyConfig(nodes, node.Name, mustHostPort(t, "10.0.0.11:6443"))
				if err != nil || !known {
					t.Fatalf("APIProxyConfig() = known %v, error %v", known, err)
				}
				if got, err := apiproxy.Render(config); err != nil || got != want {
					t.Fatalf("install-time config (error %v):\n%s\nwant:\n%s", err, got, want)
				}

				plan := Plan{ControlPlaneEndpoint: mustHostPort(t, "10.0.0.11:6443")}
				for _, n := range nodes {
					plan.Nodes = append(plan.Nodes, PlannedNode{Name: n.Name, Address: n.Address, KubernetesAddress: n.KubernetesAddress, SystemRole: n.SystemRole})
				}
				planned, err := plan.APIProxyConfig(node.Name)
				if err != nil {
					t.Fatal(err)
				}
				if got, err := apiproxy.Render(planned); err != nil || got != want {
					t.Fatalf("bootstrap config (error %v):\n%s\nwant:\n%s", err, got, want)
				}
			})
		}
	}
}

func TestAPIProxyConfigManagementListener(t *testing.T) {
	kubernetes := apiproxy.Listener{Address: "10.0.0.11:7445", Exposure: apiproxy.ExposureWorkstation}
	local := apiproxy.Listener{Address: "127.0.0.1:7445", Exposure: apiproxy.ExposureNodeLocal}
	second := apiproxy.Listener{Address: "192.168.1.11:7445", Exposure: apiproxy.ExposureWorkstation}
	for _, tc := range []struct {
		management string
		want       []apiproxy.Listener
	}{
		// A DNS name has no single address to bind.
		{management: "cp-1.katl.test", want: []apiproxy.Listener{kubernetes, local}},
		{management: "cp-1.katl.test:9443", want: []apiproxy.Listener{kubernetes, local}},
		// The agent port is not the proxy's; only the host counts.
		{management: "10.0.0.11:9443", want: []apiproxy.Listener{kubernetes, local}},
		{management: "192.168.1.11:9443", want: []apiproxy.Listener{kubernetes, local, second}},
		{management: "::ffff:10.0.0.11", want: []apiproxy.Listener{kubernetes, local}},
		{management: "::ffff:192.168.1.11", want: []apiproxy.Listener{kubernetes, local, second}},
	} {
		nodes := []Node{{Name: "cp-1", Address: mustManagementAddress(t, tc.management), KubernetesAddress: mustKubernetesAddr(t, "10.0.0.11"), SystemRole: RoleControlPlane}}
		config, _, err := APIProxyConfig(nodes, "cp-1", mustHostPort(t, "10.0.0.11:6443"))
		if err != nil {
			t.Fatalf("%s: %v", tc.management, err)
		}
		if !slices.Equal(config.Listeners, tc.want) {
			t.Errorf("%s listeners = %#v, want %#v", tc.management, config.Listeners, tc.want)
		}
	}
}

func TestAPIProxyConfigAddressFallback(t *testing.T) {
	nodes := []Node{{Name: "cp-1", Address: mustManagementAddress(t, "192.0.2.10:9443"), SystemRole: RoleControlPlane}}

	config, known, err := APIProxyConfig(nodes, "cp-1", mustHostPort(t, "api.katl.test:6443"))
	if err != nil || !known {
		t.Fatalf("APIProxyConfig() = known %v, error %v", known, err)
	}
	if got := config.Backends[0].Address; got != "192.0.2.10:6443" {
		t.Fatalf("backend without a Kubernetes address = %s, want the management host", got)
	}

	nodes[0].Address = ManagementAddress{}
	if _, known, err := APIProxyConfig(nodes, "cp-1", mustHostPort(t, "api.katl.test:6443")); err != nil || known {
		t.Fatalf("APIProxyConfig() without addresses = known %v, error %v; want unknown", known, err)
	}

	nodes[0].Address = mustManagementAddress(t, "cp-1.katl.test")
	if _, _, err := APIProxyConfig(nodes, "cp-1", mustHostPort(t, "api.katl.test:6443")); err == nil || !strings.Contains(err.Error(), "nodes[].kubernetes.address") {
		t.Fatalf("APIProxyConfig() with a DNS management address error = %v", err)
	}
}

func TestPlanInventoryRejectsInvalidKubernetesAddress(t *testing.T) {
	inv := natInventory(t)
	inv.Nodes[1].KubernetesAddress = netip.MustParseAddr("224.0.0.1")
	if _, err := PlanInventory(PlanRequest{Inventory: inv, InitNode: "cp-1"}); err == nil || !strings.Contains(err.Error(), `node "cp-2"`) {
		t.Fatalf("PlanInventory() error = %v", err)
	}
}
