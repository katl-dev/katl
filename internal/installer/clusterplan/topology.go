package clusterplan

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
)

// Topology is the part of a cluster configuration that says where its nodes
// are: names, roles, addresses and the control-plane endpoint.
type Topology struct {
	// Inventory holds each node's name, role, management address and
	// Kubernetes address, in configuration order, and the cluster's
	// control-plane endpoint. Address overrides are applied.
	Inventory        inventory.Inventory
	Endpoint         *controlplaneendpoint.Plan
	AddressOverrides []inventory.AddressOverride
}

// LowerTopology lowers a cluster configuration's topology to inventory. It
// is the one lowering that Compile and workstation commands share, so both
// derive the same addresses and control-plane endpoint from a configuration.
// addressOverrides replaces nodes' management addresses, as an operator does
// while a node moves.
func LowerTopology(config Config, addressOverrides map[string]string) (Topology, error) {
	if len(config.Spec.Nodes) == 0 {
		return Topology{}, fmt.Errorf("spec.nodes must not be empty")
	}
	unused := maps.Clone(addressOverrides)
	topology := Topology{Inventory: inventory.Inventory{ExternalControlPlane: config.Spec.Kubernetes.ExternalEndpoint != ""}}
	seen := make(map[string]struct{}, len(config.Spec.Nodes))
	for _, node := range config.Spec.Nodes {
		name := strings.TrimSpace(node.Name)
		if name == "" {
			return Topology{}, fmt.Errorf("node name is required")
		}
		if _, ok := seen[name]; ok {
			return Topology{}, fmt.Errorf("duplicate node name %q", name)
		}
		seen[name] = struct{}{}
		role := inventory.SystemRole(strings.TrimSpace(string(node.SystemRole)))
		if role != inventory.RoleControlPlane && role != inventory.RoleWorker {
			return Topology{}, fmt.Errorf("node %q systemRole %q is unsupported", name, node.SystemRole)
		}
		layer, err := mergedLayer(config.Spec.Defaults, node.Overrides)
		if err != nil {
			return Topology{}, fmt.Errorf("node %q: %w", name, err)
		}
		invNode := inventory.Node{Name: name, SystemRole: role}
		if value := strings.TrimSpace(layer.Bootstrap.Address); value != "" {
			if invNode.Address, err = inventory.ParseManagementAddress(value); err != nil {
				return Topology{}, fmt.Errorf("node %q management.address: %w", name, err)
			}
		}
		if invNode.KubernetesAddress, err = inventory.ParseKubernetesAddress(layer.Kubernetes.Address); err != nil {
			return Topology{}, fmt.Errorf("node %q kubernetes.address: %w", name, err)
		}
		if value, ok := unused[name]; ok {
			delete(unused, name)
			if strings.TrimSpace(value) == "" {
				return Topology{}, fmt.Errorf("address override for node %q is empty", name)
			}
			override, err := inventory.ParseManagementAddress(value)
			if err != nil {
				return Topology{}, fmt.Errorf("address override for node %q: %w", name, err)
			}
			topology.AddressOverrides = append(topology.AddressOverrides, inventory.AddressOverride{Node: name, Before: invNode.Address, Address: override})
			invNode.Address = override
		}
		topology.Inventory.Nodes = append(topology.Inventory.Nodes, invNode)
	}
	if len(unused) > 0 {
		names := slices.Sorted(maps.Keys(unused))
		return Topology{}, fmt.Errorf("address override references unknown node %q", names[0])
	}
	slices.SortFunc(topology.AddressOverrides, func(a, b inventory.AddressOverride) int { return strings.Compare(a.Node, b.Node) })

	endpoint, err := resolveControlPlaneEndpoint(config, topology.Inventory.Nodes)
	if err != nil {
		return Topology{}, err
	}
	if endpoint != nil {
		topology.Endpoint = endpoint
		topology.Inventory.ControlPlaneEndpointManaged = endpoint.Config.Advertisement != nil
		if topology.Inventory.ControlPlaneEndpoint, err = inventory.ParseHostPort(endpoint.Endpoint); err != nil {
			return Topology{}, fmt.Errorf("control-plane endpoint: %w", err)
		}
	}
	return topology, nil
}

// kubernetesAddressText renders a Kubernetes address for the install
// manifest, where an unset address is empty.
func kubernetesAddressText(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// resolveControlPlaneEndpoint returns the configured endpoint, or for a
// single control plane without one, that node's management host.
func resolveControlPlaneEndpoint(config Config, nodes []inventory.Node) (*controlplaneendpoint.Plan, error) {
	if external := config.Spec.Kubernetes.ExternalEndpoint; external != "" {
		endpoint, err := inventory.ParseHostPort(external)
		if err != nil {
			return nil, fmt.Errorf("spec.kubernetes.external.endpoint: %w", err)
		}
		plan, err := controlplaneendpoint.Normalize(controlplaneendpoint.Config{Host: endpoint.Host.String(), Port: int(endpoint.Port)})
		if err != nil {
			return nil, fmt.Errorf("spec.kubernetes.external.endpoint: %w", err)
		}
		return &plan, nil
	}
	var controlPlanes []inventory.Node
	for _, node := range nodes {
		if node.SystemRole == inventory.RoleControlPlane {
			controlPlanes = append(controlPlanes, node)
		}
	}
	if len(controlPlanes) == 0 {
		if config.Spec.ControlPlaneEndpoint != nil && controlplaneendpoint.Managed(*config.Spec.ControlPlaneEndpoint) {
			return nil, fmt.Errorf("managed controlPlaneEndpoint requires at least one control-plane node")
		}
		return nil, nil
	}
	endpoint := config.Spec.ControlPlaneEndpoint
	if endpoint == nil {
		first := controlPlanes[0]
		if len(controlPlanes) > 1 {
			// A cluster bootstrapped with one control plane already serves
			// the API at that node's address, so naming it keeps the
			// cluster's endpoint unchanged.
			err := errors.New("spec.controlPlaneEndpoint is required for a cluster with more than one control-plane node")
			if !first.Address.IsZero() {
				err = fmt.Errorf("%w; a cluster bootstrapped with one control plane serves its API at that node's address, so to add control planes to it set spec.controlPlaneEndpoint.host to that address, such as %s for %s", err, first.Address.Host, first.Name)
			}
			return nil, err
		}
		if first.Address.IsZero() {
			return nil, fmt.Errorf("spec.controlPlaneEndpoint is required when control-plane node %q has no bootstrap address", first.Name)
		}
		endpoint = &controlplaneendpoint.Config{Host: first.Address.Host.String()}
	}
	plan, err := controlplaneendpoint.Normalize(*endpoint)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}
