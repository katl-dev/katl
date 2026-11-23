package inventory

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/apiproxy"
)

// natInventory places every node behind NAT: the workstation reaches each one
// at a 192.0.2.0/24 management address that no node has assigned, while the
// nodes themselves hold 10.0.1.0/24 Kubernetes addresses.
func natInventory() Inventory {
	node := func(name string, role SystemRole, management, kubernetes string) Node {
		intent, ref := IntentControlPlane, "control-plane"
		if role == RoleWorker {
			intent, ref = IntentWorker, "worker"
		}
		return Node{
			Name: name, Address: management, KubernetesAddress: kubernetes, SystemRole: role,
			Access:            Access{Method: "agent"},
			KubeadmConfig:     KubeadmConfig{Ref: ref, Path: "/etc/katl/kubeadm/" + ref + "/config.yaml", Intent: intent},
			KubernetesVersion: "v1.36.1",
		}
	}
	return Inventory{
		ControlPlaneEndpoint: "10.0.1.10:6443",
		Nodes: []Node{
			node("cp-1", RoleControlPlane, "192.0.2.10", "10.0.1.10"),
			node("cp-2", RoleControlPlane, "192.0.2.11", "10.0.1.11"),
			node("worker-1", RoleWorker, "192.0.2.21", "10.0.1.21"),
		},
	}
}

func TestPlanAPIProxyConfig(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{Inventory: natInventory(), InitNode: "cp-1"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		node string
		want string
	}{
		{node: "cp-2", want: `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "APIProxyConfig",
  "tlsServerName": "10.0.1.10",
  "canonicalEndpoint": "10.0.1.10:6443",
  "caFile": "/etc/kubernetes/pki/ca.crt",
  "checkInterval": "2s",
  "checkTimeout": "2s",
  "listeners": [
    {
      "address": "10.0.1.11:7445",
      "exposure": "workstation"
    },
    {
      "address": "127.0.0.1:7445",
      "exposure": "node-local"
    }
  ],
  "backends": [
    {
      "name": "cp-1",
      "address": "10.0.1.10:6443"
    },
    {
      "name": "cp-2",
      "address": "10.0.1.11:6443",
      "local": true
    }
  ]
}
`},
		{node: "worker-1", want: `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "APIProxyConfig",
  "tlsServerName": "10.0.1.10",
  "canonicalEndpoint": "10.0.1.10:6443",
  "caFile": "/etc/kubernetes/pki/ca.crt",
  "checkInterval": "2s",
  "checkTimeout": "2s",
  "listeners": [
    {
      "address": "127.0.0.1:7445",
      "exposure": "node-local"
    }
  ],
  "backends": [
    {
      "name": "cp-1",
      "address": "10.0.1.10:6443"
    },
    {
      "name": "cp-2",
      "address": "10.0.1.11:6443"
    }
  ]
}
`},
	} {
		t.Run(tc.node, func(t *testing.T) {
			config, err := plan.APIProxyConfig(tc.node)
			if err != nil {
				t.Fatal(err)
			}
			got, err := apiproxy.Render(config)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("API proxy config:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// Install-time rendering and the bootstrap generation must agree, since the
// bootstrap generation replaces the installed configuration.
func TestAPIProxyConfigMatchesPlan(t *testing.T) {
	inv := natInventory()
	plan, err := PlanInventory(PlanRequest{Inventory: inv, InitNode: "cp-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range inv.Nodes {
		installed, known, err := APIProxyConfig(inv.Nodes, node.Name, inv.ControlPlaneEndpoint)
		if err != nil || !known {
			t.Fatalf("APIProxyConfig(%s) = known %v, error %v", node.Name, known, err)
		}
		planned, err := plan.APIProxyConfig(node.Name)
		if err != nil {
			t.Fatal(err)
		}
		installedJSON, _ := apiproxy.Render(installed)
		plannedJSON, _ := apiproxy.Render(planned)
		if installedJSON != plannedJSON {
			t.Fatalf("%s install-time config:\n%s\nbootstrap config:\n%s", node.Name, installedJSON, plannedJSON)
		}
	}
}

func TestAPIProxyConfigAddressFallback(t *testing.T) {
	nodes := []Node{{Name: "cp-1", Address: "192.0.2.10:9443", SystemRole: RoleControlPlane}}

	config, known, err := APIProxyConfig(nodes, "cp-1", "api.katl.test:6443")
	if err != nil || !known {
		t.Fatalf("APIProxyConfig() = known %v, error %v", known, err)
	}
	if got := config.Backends[0].Address; got != "192.0.2.10:6443" {
		t.Fatalf("backend without a Kubernetes address = %s, want the management host", got)
	}

	nodes[0].Address = ""
	if _, known, err := APIProxyConfig(nodes, "cp-1", "api.katl.test:6443"); err != nil || known {
		t.Fatalf("APIProxyConfig() without addresses = known %v, error %v; want unknown", known, err)
	}

	nodes[0].Address = "cp-1.katl.test"
	if _, _, err := APIProxyConfig(nodes, "cp-1", "api.katl.test:6443"); err == nil || !strings.Contains(err.Error(), "nodes[].kubernetes.address") {
		t.Fatalf("APIProxyConfig() with a DNS management address error = %v", err)
	}
}

func TestPlanInventoryRejectsInvalidKubernetesAddress(t *testing.T) {
	inv := natInventory()
	inv.Nodes[1].KubernetesAddress = "cp-2.katl.test"
	if _, err := PlanInventory(PlanRequest{Inventory: inv, InitNode: "cp-1"}); err == nil || !strings.Contains(err.Error(), `node "cp-2"`) {
		t.Fatalf("PlanInventory() error = %v", err)
	}
}
