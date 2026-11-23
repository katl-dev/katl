package inventory

import (
	"encoding/json"
	"net/netip"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseManagementAddress(t *testing.T) {
	for _, tc := range []struct {
		input    string
		endpoint string
		text     string
	}{
		{input: "10.0.0.11", endpoint: "10.0.0.11:9443", text: "10.0.0.11"},
		{input: " 10.0.0.11:19443 ", endpoint: "10.0.0.11:19443", text: "10.0.0.11:19443"},
		{input: "node-1.lab.example", endpoint: "node-1.lab.example:9443", text: "node-1.lab.example"},
		{input: "node-1.lab.example:19443", endpoint: "node-1.lab.example:19443", text: "node-1.lab.example:19443"},
		{input: "tcp://10.0.0.11", endpoint: "10.0.0.11:9443", text: "10.0.0.11"},
		{input: "tcp://10.0.0.11:19443/", endpoint: "10.0.0.11:19443", text: "10.0.0.11:19443"},
		{input: "fd00::11", endpoint: "[fd00::11]:9443", text: "fd00::11"},
		{input: "[fd00::11]", endpoint: "[fd00::11]:9443", text: "fd00::11"},
		{input: "[fd00::11]:19443", endpoint: "[fd00::11]:19443", text: "[fd00::11]:19443"},
		{input: "::ffff:10.0.0.11", endpoint: "10.0.0.11:9443", text: "10.0.0.11"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			address, err := ParseManagementAddress(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := address.Endpoint().String(); got != tc.endpoint {
				t.Errorf("endpoint = %q, want %q", got, tc.endpoint)
			}
			if got := address.String(); got != tc.text {
				t.Errorf("text = %q, want %q", got, tc.text)
			}
		})
	}
	for _, input := range []string{"", "https://10.0.0.11", "tcp://user@10.0.0.11", "10.0.0.11:0", "10.0.0.11:70000", "node_1", "fe80::1%eth0", "[10.0.0.11:9443]:9443"} {
		t.Run("rejects "+input, func(t *testing.T) {
			if address, err := ParseManagementAddress(input); err == nil {
				t.Fatalf("ParseManagementAddress(%q) = %v, want error", input, address)
			}
		})
	}
}

// Recorded values keep every host an earlier release accepted, which
// ParseManagementAddress refuses for new input, and re-encode unchanged.
func TestDecodeRecordedAddresses(t *testing.T) {
	for _, tc := range []struct {
		input    string
		endpoint string
	}{
		{input: "cp_1.home.arpa", endpoint: "cp_1.home.arpa:9443"},
		{input: "worker_1.home.arpa:19443", endpoint: "worker_1.home.arpa:19443"},
		{input: "fe80::1%eth0", endpoint: "[fe80::1%eth0]:9443"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if _, err := ParseManagementAddress(tc.input); err == nil {
				t.Fatalf("ParseManagementAddress(%q) succeeded; the case no longer covers a host only decoding accepts", tc.input)
			}
			var address ManagementAddress
			if err := address.UnmarshalText([]byte(tc.input)); err != nil {
				t.Fatal(err)
			}
			if got := address.Endpoint().String(); got != tc.endpoint {
				t.Errorf("endpoint = %q, want %q", got, tc.endpoint)
			}
			if got, _ := address.MarshalText(); string(got) != tc.input {
				t.Errorf("re-encoded = %q, want %q", got, tc.input)
			}
		})
	}

	// v2026.11.1 saved this endpoint for a management.address naming a port.
	endpoint, err := DecodeHostPort("[worker_1.home.arpa:19443]:9443")
	if err != nil {
		t.Fatal(err)
	}
	if got := endpoint.String(); got != "[worker_1.home.arpa:19443]:9443" {
		t.Errorf("endpoint = %q, want it unchanged", got)
	}

	for _, input := range []string{"cp_1.home.arpa", "http://10.0.0.11:9443", "10.0.0.11:0"} {
		if endpoint, err := DecodeHostPort(input); err == nil {
			t.Errorf("DecodeHostPort(%q) = %v, want error", input, endpoint)
		}
	}
}

func TestParseKubernetesAddress(t *testing.T) {
	got, err := ParseKubernetesAddress("::ffff:10.0.0.11")
	if err != nil || got.String() != "10.0.0.11" {
		t.Fatalf("ParseKubernetesAddress(v4-mapped) = %v, %v; want 10.0.0.11", got, err)
	}
	if got, err := ParseKubernetesAddress(""); err != nil || got.IsValid() {
		t.Fatalf("ParseKubernetesAddress(\"\") = %v, %v; want unset", got, err)
	}
	for _, input := range []string{"node-1.lab", "127.0.0.1", "fe80::1%eth0", "10.0.0.11:6443"} {
		if _, err := ParseKubernetesAddress(input); err == nil {
			t.Errorf("ParseKubernetesAddress(%q) succeeded, want error", input)
		}
	}
}

// Inventories written by v2026.11.1 carry addresses as strings, and a
// management address without a port means the default agent port.
func TestInventoryDecodesReleasedFormats(t *testing.T) {
	released := `{"controlPlaneEndpoint":"api.lab.example:6443","kubernetesVersion":"v1.36.1","nodes":[` +
		`{"name":"cp-1","address":"10.0.0.11","kubernetesAddress":"10.0.0.11","systemRole":"control-plane","access":{"method":"agent"},"kubeadmConfig":{"ref":"control-plane","path":"/etc/katl/kubeadm/control-plane/config.yaml","intent":"control-plane"},"kubernetesVersion":"v1.36.1"},` +
		`{"name":"worker-1","address":"198.18.0.21:19443","systemRole":"worker","access":{"method":"agent"},"kubeadmConfig":{"ref":"worker","path":"/etc/katl/kubeadm/worker/config.yaml","intent":"worker"},"kubernetesVersion":"v1.36.1"}]}`
	var fromJSON Inventory
	if err := json.Unmarshal([]byte(released), &fromJSON); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != released {
		t.Fatalf("re-encoded inventory changed:\n%s\nwant:\n%s", encoded, released)
	}

	var fromYAML Inventory
	if err := yaml.Unmarshal([]byte(`controlPlaneEndpoint: api.lab.example:6443
nodes:
- name: cp-1
  address: 10.0.0.11
  kubernetesAddress: 10.0.0.11
- name: worker-1
  address: 198.18.0.21:19443
`), &fromYAML); err != nil {
		t.Fatal(err)
	}
	for _, inv := range []Inventory{fromJSON, fromYAML} {
		if got := inv.ControlPlaneEndpoint.String(); got != "api.lab.example:6443" {
			t.Errorf("control-plane endpoint = %q", got)
		}
		if got := inv.Nodes[0].ManagementEndpoint().String(); got != "10.0.0.11:9443" {
			t.Errorf("cp-1 management endpoint = %q, want the default agent port", got)
		}
		if got := inv.Nodes[1].ManagementEndpoint().String(); got != "198.18.0.21:19443" {
			t.Errorf("worker-1 management endpoint = %q, want its explicit port", got)
		}
		if inv.Nodes[1].KubernetesAddress.IsValid() {
			t.Errorf("worker-1 Kubernetes address = %v, want unset", inv.Nodes[1].KubernetesAddress)
		}
	}

	for _, bad := range []string{`{"nodes":[{"address":"http://10.0.0.11"}]}`, `{"controlPlaneEndpoint":"api.lab.example"}`} {
		if err := json.Unmarshal([]byte(bad), &Inventory{}); err == nil {
			t.Errorf("decoding %s succeeded, want error", bad)
		}
	}
}

// The workstation reaches a node only at its management host, which NAT may
// translate and a multihomed node holds on a second NIC. Neither is in the
// serving certificate, so each endpoint names the host to verify.
func TestWorkstationEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		management, kubernetes string
		controlPlaneEndpoint   string
		agent, apiServer       string
		installer              string
		proxy, serverName      string
		peer                   string
	}{
		{
			name: "nat", management: "198.18.0.11:19443", kubernetes: "10.0.0.11", controlPlaneEndpoint: "10.0.0.11:6443",
			agent: "198.18.0.11:19443", installer: "198.18.0.11:8080", apiServer: "198.18.0.11:6443", proxy: "198.18.0.11:7445", serverName: "10.0.0.11",
			peer: "https://10.0.0.11:2380",
		},
		{
			name: "multihomed with a DNS endpoint", management: "192.168.1.11", kubernetes: "10.0.0.11", controlPlaneEndpoint: "api.lab.example:6443",
			agent: "192.168.1.11:9443", installer: "192.168.1.11:8080", apiServer: "192.168.1.11:6443", proxy: "192.168.1.11:7445", serverName: "api.lab.example",
			peer: "https://10.0.0.11:2380",
		},
		{
			name: "single address without an endpoint", management: "10.0.0.11",
			agent: "10.0.0.11:9443", installer: "10.0.0.11:8080", apiServer: "10.0.0.11:6443", proxy: "10.0.0.11:7445", serverName: "10.0.0.11",
			peer: "https://10.0.0.11:2380",
		},
		{
			name: "NAT without an endpoint", management: "198.18.0.11", kubernetes: "10.0.0.11",
			agent: "198.18.0.11:9443", installer: "198.18.0.11:8080", apiServer: "198.18.0.11:6443", proxy: "198.18.0.11:7445", serverName: "10.0.0.11",
			peer: "https://10.0.0.11:2380",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := Node{
				Name: "cp-1", Address: mustManagementAddress(t, tc.management), SystemRole: RoleControlPlane,
				Access:            Access{Method: "agent"},
				KubeadmConfig:     KubeadmConfig{Ref: "control-plane", Intent: IntentControlPlane},
				KubernetesVersion: "v1.36.1",
			}
			if tc.kubernetes != "" {
				addr, err := ParseKubernetesAddress(tc.kubernetes)
				if err != nil {
					t.Fatal(err)
				}
				node.KubernetesAddress = addr
			}
			inv := Inventory{Nodes: []Node{node}}
			if tc.controlPlaneEndpoint != "" {
				endpoint, err := ParseHostPort(tc.controlPlaneEndpoint)
				if err != nil {
					t.Fatal(err)
				}
				inv.ControlPlaneEndpoint = endpoint
			}
			plan, err := PlanInventory(PlanRequest{Inventory: inv})
			if err != nil {
				t.Fatal(err)
			}
			planned := plan.Nodes[0]

			if got := planned.ManagementEndpoint().String(); got != tc.agent {
				t.Errorf("management endpoint = %q, want %q", got, tc.agent)
			}
			if got := node.InstallerEndpoint().String(); got != tc.installer {
				t.Errorf("installer endpoint = %q, want %q", got, tc.installer)
			}
			apiServer := plan.WorkstationKubernetesEndpoint(planned)
			if got := apiServer.Address.String(); got != tc.apiServer {
				t.Errorf("kube-apiserver = %q, want %q", got, tc.apiServer)
			}
			proxy := plan.WorkstationProxyEndpoint(planned)
			if got := proxy.Address.String(); got != tc.proxy {
				t.Errorf("API proxy = %q, want %q", got, tc.proxy)
			}
			for _, endpoint := range []TLSEndpoint{apiServer, proxy} {
				if got := endpoint.ServerName.String(); got != tc.serverName {
					t.Errorf("%s server name = %q, want %q", endpoint.Address, got, tc.serverName)
				}
			}
			if got := planned.EtcdPeerURL(); got != tc.peer {
				t.Errorf("etcd peer URL = %q, want %q", got, tc.peer)
			}
		})
	}
}

func mustManagementAddress(t *testing.T, value string) ManagementAddress {
	t.Helper()
	address, err := ParseManagementAddress(value)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func mustHostPort(t *testing.T, value string) HostPort {
	t.Helper()
	endpoint, err := ParseHostPort(value)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func mustKubernetesAddr(t *testing.T, value string) netip.Addr {
	t.Helper()
	addr, err := ParseKubernetesAddress(value)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
