package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
)

// Workstation commands and the compiled bundle that nodes install must agree
// on where each node is and which endpoint the cluster serves its API at.
// Workstation commands list nodes in configuration order, and a bundle in
// name order.
func TestManagementInventoryMatchesCompile(t *testing.T) {
	node := func(name string, controlPlane bool, extra string) string {
		return "    - name: " + name + "\n      controlPlane: " + map[bool]string{true: "true", false: "false"}[controlPlane] + "\n" + extra +
			"      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-" + name + "\n"
	}
	management := func(address string) string { return "      management:\n        address: " + address + "\n" }
	kubernetes := func(address string) string { return "      kubernetes:\n        address: " + address + "\n" }
	endpoint := "  controlPlaneEndpoint:\n    host: api.katl.test\n    port: 6443\n"

	for _, tc := range []struct {
		name     string
		endpoint string
		nodes    string
		served   string
		order    []string
		refused  string
	}{
		{
			name:   "single control plane derives its endpoint",
			nodes:  node("cp-1", true, management("10.0.0.11")) + node("worker-1", false, management("10.0.0.21")),
			served: "10.0.0.11:6443",
		},
		{
			name: "NAT control plane with an agent port and a Kubernetes address",
			nodes: node("cp-1", true, management("198.18.0.11:19443")+kubernetes("10.0.0.11")) +
				node("worker-1", false, management("198.18.0.21:19443")+kubernetes("::ffff:10.0.0.21")),
			served: "10.0.0.11:6443",
		},
		{
			name:   "single control plane with a management hostname",
			nodes:  node("cp-1", true, management("cp_1.home.arpa")+kubernetes("10.0.0.11")),
			served: "10.0.0.11:6443",
		},
		{
			name:     "several control planes with an endpoint",
			endpoint: endpoint,
			nodes:    node("cp-2", true, management("10.0.0.12")) + node("cp-1", true, management("10.0.0.11")) + node("worker-1", false, management("node-21.lab.example")),
			served:   "api.katl.test:6443",
			order:    []string{"cp-2", "cp-1", "worker-1"},
		},
		{
			name:    "no nodes",
			nodes:   "    []\n",
			refused: "spec.nodes must not be empty",
		},
		{
			name:    "several control planes without an endpoint",
			nodes:   node("cp-1", true, management("10.0.0.11")) + node("cp-2", true, management("10.0.0.12")),
			refused: "spec.controlPlaneEndpoint is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cluster.yaml")
			source := "apiVersion: config.katl.dev/v1alpha1\nkind: ClusterConfig\nmetadata:\n  name: lab\nspec:\n  managementAuthentication: trusted-network\n" + tc.endpoint +
				"  kubernetes:\n    version: v1.36.1\n  defaults:\n    install:\n      systemDisk:\n        minSizeMiB: 32768\n    access:\n      ssh:\n        authorizedKeys:\n          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example\n" +
				"  nodes:\n" + tc.nodes
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}

			workstation, workstationErr := readManagementInventory(path)
			_, compiled, compileErr := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: path})
			if tc.refused != "" {
				for name, err := range map[string]error{"katlctl": workstationErr, "Compile": compileErr} {
					if err == nil || !strings.Contains(err.Error(), tc.refused) {
						t.Errorf("%s error = %v, want %q", name, err, tc.refused)
					}
				}
				return
			}
			if workstationErr != nil || compileErr != nil {
				t.Fatalf("katlctl error = %v, Compile error = %v", workstationErr, compileErr)
			}

			if got := workstation.ControlPlaneEndpoint.String(); got != tc.served {
				t.Errorf("control-plane endpoint = %q, want %q", got, tc.served)
			}
			got, want := topologyOf(workstation), topologyOf(compiled.Manifest.Cluster.BootstrapInventory)
			if tc.order != nil {
				var names []string
				for _, node := range workstation.Nodes {
					names = append(names, node.Name)
				}
				if !slices.Equal(names, tc.order) {
					t.Errorf("katlctl node order = %q, want configuration order %q", names, tc.order)
				}
			}
			slices.Sort(got.Nodes)
			slices.Sort(want.Nodes)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("katlctl inventory = %+v\nCompile inventory = %+v", got, want)
			}
		})
	}
}

type loweredTopology struct {
	ExternalControlPlane bool
	ControlPlaneEndpoint string
	Managed              bool
	Nodes                []string
}

// topologyOf renders the fields both lowerings own, so a disagreement reads
// as text.
func topologyOf(inv inventory.Inventory) loweredTopology {
	out := loweredTopology{ExternalControlPlane: inv.ExternalControlPlane, ControlPlaneEndpoint: inv.ControlPlaneEndpoint.String(), Managed: inv.ControlPlaneEndpointManaged}
	for _, node := range inv.Nodes {
		kubernetes := ""
		if node.KubernetesAddress.IsValid() {
			kubernetes = node.KubernetesAddress.String()
		}
		out.Nodes = append(out.Nodes, strings.Join([]string{node.Name, string(node.SystemRole), node.Address.String(), node.ManagementEndpoint().String(), kubernetes}, " "))
	}
	return out
}
