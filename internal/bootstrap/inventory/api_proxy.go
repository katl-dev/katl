package inventory

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/apiproxy"
)

// APIProxyConfig returns the API proxy configuration for node self, or false
// when a control plane's Kubernetes address is not yet known.
func APIProxyConfig(nodes []Node, self, canonicalEndpoint string) (apiproxy.Config, bool, error) {
	members := make([]proxyMember, 0, len(nodes))
	for _, node := range nodes {
		members = append(members, proxyMember{
			name: strings.TrimSpace(node.Name), role: SystemRole(strings.TrimSpace(string(node.SystemRole))),
			kubernetesAddress: node.KubernetesAddress, address: node.Address,
		})
	}
	return apiProxyConfig(members, strings.TrimSpace(self), canonicalEndpoint)
}

// APIProxyConfig returns the API proxy configuration for the planned node self.
func (p Plan) APIProxyConfig(self string) (apiproxy.Config, error) {
	members := make([]proxyMember, 0, len(p.Nodes))
	for _, node := range p.Nodes {
		members = append(members, proxyMember{
			name: node.Name, role: node.SystemRole, kubernetesAddress: node.KubernetesAddress, address: node.Address,
		})
	}
	config, known, err := apiProxyConfig(members, self, p.ControlPlaneEndpoint)
	if err != nil {
		return apiproxy.Config{}, err
	}
	if !known {
		return apiproxy.Config{}, fmt.Errorf("a control-plane node has no Kubernetes address")
	}
	return config, nil
}

type proxyMember struct {
	name              string
	role              SystemRole
	kubernetesAddress string
	address           string
}

// apiProxyConfig derives backends and the node-side listeners from Kubernetes
// addresses, which the nodes hold themselves. The workstation dials the
// management address, so a control plane also listens there when it is a
// different literal IP: on a multihomed node that address is local, and
// behind NAT the listener is free-bound and idle while translated
// connections arrive at the Kubernetes address.
func apiProxyConfig(members []proxyMember, self, canonicalEndpoint string) (apiproxy.Config, bool, error) {
	var selfMember *proxyMember
	var selfHost netip.Addr
	backends := make([]apiproxy.Backend, 0, len(members))
	for i, member := range members {
		if member.name == self {
			selfMember = &members[i]
		}
		if member.role != RoleControlPlane {
			continue
		}
		host, known, err := kubernetesHost(member.kubernetesAddress, member.address)
		if err != nil {
			return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: %w", member.name, err)
		}
		if !known {
			return apiproxy.Config{}, false, nil
		}
		if member.name == self {
			selfHost = host
		}
		backends = append(backends, apiproxy.Backend{
			Name: member.name, Address: endpoint(host, apiproxy.DefaultBackendPort), Local: member.name == self,
		})
	}
	if selfMember == nil {
		return apiproxy.Config{}, false, fmt.Errorf("node %q is not in the inventory", self)
	}

	canonicalEndpoint = strings.TrimSpace(canonicalEndpoint)
	if canonicalEndpoint == "" && len(backends) > 0 {
		canonicalEndpoint = backends[0].Address
	}
	tlsName, _, err := net.SplitHostPort(canonicalEndpoint)
	if err != nil {
		return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: control-plane endpoint %q must be host:port", self, canonicalEndpoint)
	}

	listeners := []apiproxy.Listener{{Address: endpoint(netip.MustParseAddr("127.0.0.1"), apiproxy.DefaultPort), Exposure: apiproxy.ExposureNodeLocal}}
	if selfMember.role == RoleControlPlane {
		listeners = append(listeners, apiproxy.Listener{Address: endpoint(selfHost, apiproxy.DefaultPort), Exposure: apiproxy.ExposureWorkstation})
		if management, ok := managementHost(selfMember.address); ok && management != selfHost {
			listeners = append(listeners, apiproxy.Listener{Address: endpoint(management, apiproxy.DefaultPort), Exposure: apiproxy.ExposureWorkstation})
		}
	}
	config, err := apiproxy.Normalize(apiproxy.Config{
		TLSName: tlsName, CanonicalEndpoint: canonicalEndpoint, Listeners: listeners, Backends: backends,
	})
	if err != nil {
		return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: %w", self, err)
	}
	return config, true, nil
}

// kubernetesHost returns the address assigned to the node itself, which
// kube-apiserver serves on and node-side listeners bind. The management
// address may be translated by NAT, so it stands in only when no Kubernetes
// address is declared. known is false when neither address is set.
func kubernetesHost(kubernetesAddress, address string) (host netip.Addr, known bool, err error) {
	value := strings.TrimSpace(kubernetesAddress)
	source := "Kubernetes address"
	if value == "" {
		value = strings.TrimSpace(address)
		if host, _, err := net.SplitHostPort(value); err == nil {
			value = host
		}
		source = "management address"
	}
	if value == "" {
		return netip.Addr{}, false, nil
	}
	parsed, err := netip.ParseAddr(value)
	if err != nil || parsed.Zone() != "" || !parsed.IsGlobalUnicast() {
		if source == "management address" {
			return netip.Addr{}, false, fmt.Errorf("%s %q is not a literal unicast IP; set nodes[].kubernetes.address to the node's own address", source, value)
		}
		return netip.Addr{}, false, fmt.Errorf("%s %q must be a literal unicast IP", source, value)
	}
	return parsed.Unmap(), true, nil
}

// managementHost returns the management address when it is a literal
// unicast IP that a listener can bind; a DNS name has no single address.
func managementHost(address string) (netip.Addr, bool) {
	value := strings.TrimSpace(address)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	parsed, err := netip.ParseAddr(value)
	if err != nil || parsed.Zone() != "" || !parsed.IsGlobalUnicast() {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}

// KubernetesHost returns the host other nodes use to reach this node's
// kube-apiserver and etcd member. Unlike API proxy addresses it keeps a DNS
// management name when no Kubernetes address is declared, because the
// dialer resolves it.
func (n PlannedNode) KubernetesHost() string {
	if address := strings.TrimSpace(n.KubernetesAddress); address != "" {
		return address
	}
	address := strings.TrimSpace(n.Address)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	return address
}

// KubernetesEndpoint returns the node's kube-apiserver at KubernetesHost.
func (n PlannedNode) KubernetesEndpoint() string {
	return net.JoinHostPort(n.KubernetesHost(), strconv.Itoa(apiproxy.DefaultBackendPort))
}

func endpoint(host netip.Addr, port int) string {
	return net.JoinHostPort(host.String(), strconv.Itoa(port))
}
