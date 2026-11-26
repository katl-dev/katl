package inventory

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/katl-dev/katl/internal/apiproxy"
)

// APIProxyConfig returns the API proxy configuration for node self, or false
// when a control plane's Kubernetes address is not yet known.
func APIProxyConfig(nodes []Node, self string, canonicalEndpoint HostPort) (apiproxy.Config, bool, error) {
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
	kubernetesAddress netip.Addr
	address           ManagementAddress
}

// apiProxyConfig derives backends and the node-side listeners from Kubernetes
// addresses, which the nodes hold themselves. The workstation dials the
// management address, so a control plane also listens there when it is a
// different literal IP: on a multihomed node that address is local, and
// behind NAT the listener is free-bound and idle while translated
// connections arrive at the Kubernetes address.
func apiProxyConfig(members []proxyMember, self string, canonicalEndpoint HostPort) (apiproxy.Config, bool, error) {
	var selfMember *proxyMember
	var selfHost, firstHost netip.Addr
	backends := make([]apiproxy.Backend, 0, len(members))
	for i, member := range members {
		if member.name == self {
			selfMember = &members[i]
		}
		if member.role != RoleControlPlane {
			continue
		}
		host, known, err := proxyHost(member.kubernetesAddress, member.address)
		if err != nil {
			return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: %w", member.name, err)
		}
		if !known {
			return apiproxy.Config{}, false, nil
		}
		if member.name == self {
			selfHost = host
		}
		if !firstHost.IsValid() {
			firstHost = host
		}
		backends = append(backends, apiproxy.Backend{
			Name: member.name, Address: endpoint(host, KubernetesPort), Local: member.name == self,
		})
	}
	if selfMember == nil {
		return apiproxy.Config{}, false, fmt.Errorf("node %q is not in the inventory", self)
	}

	if canonicalEndpoint.IsZero() && firstHost.IsValid() {
		canonicalEndpoint = HostPort{Host: HostFromAddr(firstHost), Port: KubernetesPort}
	}
	if canonicalEndpoint.IsZero() {
		return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: control-plane endpoint is required", self)
	}
	tlsName := kubernetesServerName(canonicalEndpoint, Host{})

	listeners := []apiproxy.Listener{{Address: endpoint(netip.MustParseAddr("127.0.0.1"), apiproxy.DefaultPort), Exposure: apiproxy.ExposureNodeLocal}}
	if selfMember.role == RoleControlPlane {
		listeners = append(listeners, apiproxy.Listener{Address: endpoint(selfHost, apiproxy.DefaultPort), Exposure: apiproxy.ExposureWorkstation})
		if management, ok := managementHost(selfMember.address); ok && management != selfHost {
			listeners = append(listeners, apiproxy.Listener{Address: endpoint(management, apiproxy.DefaultPort), Exposure: apiproxy.ExposureWorkstation})
		}
	}
	config, err := apiproxy.Normalize(apiproxy.Config{
		TLSName: tlsName.String(), CanonicalEndpoint: canonicalEndpoint.String(), Listeners: listeners, Backends: backends,
	})
	if err != nil {
		return apiproxy.Config{}, false, fmt.Errorf("node %q API proxy: %w", self, err)
	}
	return config, true, nil
}

// proxyHost returns the address assigned to the node itself, which
// kube-apiserver serves on and node-side listeners bind. The management
// address may be translated by NAT, so it stands in only when no Kubernetes
// address is declared. known is false when neither address is set.
func proxyHost(kubernetesAddress netip.Addr, address ManagementAddress) (host netip.Addr, known bool, err error) {
	if kubernetesAddress.IsValid() {
		if err := validKubernetesAddr(kubernetesAddress); err != nil {
			return netip.Addr{}, false, fmt.Errorf("Kubernetes address %w", err)
		}
		return kubernetesAddress.Unmap(), true, nil
	}
	if address.IsZero() {
		return netip.Addr{}, false, nil
	}
	if management, ok := managementHost(address); ok {
		return management, true, nil
	}
	return netip.Addr{}, false, fmt.Errorf("management address %q is not a literal unicast IP; set nodes[].kubernetes.address to the node's own address", address.Host)
}

// managementHost returns the management address when it is a literal
// unicast IP that a listener can bind; a DNS name has no single address.
func managementHost(address ManagementAddress) (netip.Addr, bool) {
	addr, ok := address.Host.Addr()
	if !ok || !addr.IsGlobalUnicast() {
		return netip.Addr{}, false
	}
	return addr, true
}

// BackendKubernetesAddress returns the Kubernetes address of the control plane
// an API proxy backend reaches, the inverse of how APIProxyConfig renders that
// backend, or false when backend is not one it renders.
func BackendKubernetesAddress(backend string) (netip.Addr, bool) {
	endpoint, err := netip.ParseAddrPort(backend)
	if err != nil || endpoint.Port() != KubernetesPort {
		return netip.Addr{}, false
	}
	addr := endpoint.Addr().Unmap()
	if validKubernetesAddr(addr) != nil {
		return netip.Addr{}, false
	}
	return addr, true
}
