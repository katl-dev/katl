package inventory

import (
	"net"
	"net/netip"
	"strconv"

	"github.com/katl-dev/katl/internal/apiproxy"
)

const (
	// ManagementPort is where the katlc agent listens when a management
	// address names no port.
	ManagementPort = 9443
	// KubernetesPort is kube-apiserver's secure port on each control plane.
	KubernetesPort = apiproxy.DefaultBackendPort
	// EtcdPeerPort is the stacked etcd member's peer port.
	EtcdPeerPort = 2380
)

// TLSEndpoint is an address to dial and the name its serving certificate is
// verified against.
type TLSEndpoint struct {
	Address    HostPort
	ServerName Host
}

// URL returns the endpoint as an https:// URL for kubeconfigs and kubectl.
func (e TLSEndpoint) URL() string {
	return "https://" + e.Address.String()
}

// ManagementEndpoint returns where the workstation reaches the node's agent.
func (n Node) ManagementEndpoint() HostPort {
	return n.Address.Endpoint()
}

// ManagementEndpoint returns where the workstation reaches the node's agent.
func (n PlannedNode) ManagementEndpoint() HostPort {
	return n.Address.Endpoint()
}

// KubernetesHost returns the host other nodes use to reach this node's
// kube-apiserver and etcd member: its Kubernetes address, or its management
// host when none is declared. Unlike API proxy addresses it keeps a DNS
// management name, because the dialer resolves it.
func (n PlannedNode) KubernetesHost() Host {
	if n.KubernetesAddress.IsValid() {
		return HostFromAddr(n.KubernetesAddress)
	}
	return n.Address.Host
}

// KubernetesEndpoint returns the node's kube-apiserver as other nodes reach
// it.
func (n PlannedNode) KubernetesEndpoint() HostPort {
	return HostPort{Host: n.KubernetesHost(), Port: KubernetesPort}
}

// EtcdPeerURL returns the peer URL other members reach this node's stacked
// etcd member at.
func (n PlannedNode) EtcdPeerURL() string {
	return "https://" + net.JoinHostPort(n.KubernetesHost().String(), strconv.Itoa(EtcdPeerPort))
}

// WorkstationKubernetesEndpoint returns node's kube-apiserver as the
// workstation reaches it. The workstation reaches a node only at its
// management host, which NAT may translate and which need not be in the
// serving certificate, so the endpoint carries the name to verify.
func (p Plan) WorkstationKubernetesEndpoint(node PlannedNode) TLSEndpoint {
	return TLSEndpoint{
		Address:    HostPort{Host: node.Address.Host, Port: KubernetesPort},
		ServerName: kubernetesServerName(p.ControlPlaneEndpoint, node.KubernetesHost()),
	}
}

// WorkstationProxyEndpoint returns node's API proxy listener as the
// workstation reaches it. The proxy forwards TLS to kube-apiserver
// unchanged, so it is verified like WorkstationKubernetesEndpoint.
func (p Plan) WorkstationProxyEndpoint(node PlannedNode) TLSEndpoint {
	return TLSEndpoint{
		Address:    HostPort{Host: node.Address.Host, Port: apiproxy.DefaultPort},
		ServerName: kubernetesServerName(p.ControlPlaneEndpoint, node.KubernetesHost()),
	}
}

// kubernetesServerName returns the name every kube-apiserver certificate in
// the cluster carries: the control-plane endpoint's host, which kubeadm adds
// to each one, or for a cluster without one, the address its single control
// plane advertises.
func kubernetesServerName(controlPlaneEndpoint HostPort, controlPlane Host) Host {
	if !controlPlaneEndpoint.IsZero() {
		return controlPlaneEndpoint.Host
	}
	return controlPlane
}

func endpoint(host netip.Addr, port int) string {
	return net.JoinHostPort(host.String(), strconv.Itoa(port))
}
