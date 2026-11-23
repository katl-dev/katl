package configbundle

import (
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/clusterplan"
)

// SourceTopology lowers a ClusterConfig's nodes, addresses and control-plane
// endpoint to inventory through the lowering Compile uses, so workstation
// commands and installed nodes agree on where each node is. It reads only
// the topology, so a configuration whose other fields are completed later in
// the install flow, such as extension release targets, still resolves.
func SourceTopology(source SourceConfig) (inventory.Inventory, error) {
	selection, err := lowerKubernetesSelection(source, "")
	if err != nil {
		return inventory.Inventory{}, err
	}
	config := clusterplan.Config{Spec: clusterplan.Spec{
		ControlPlaneEndpoint: source.Spec.ControlPlaneEndpoint,
		Kubernetes:           clusterplan.KubernetesSelection{ExternalEndpoint: selection.ExternalEndpoint},
	}}
	for _, node := range source.Spec.Nodes {
		config.Spec.Nodes = append(config.Spec.Nodes, clusterplan.Node{
			Name:       node.Name,
			SystemRole: sourceNodeRole(node),
			Overrides:  topologyLayer(node),
		})
	}
	topology, err := clusterplan.LowerTopology(config, nil)
	if err != nil {
		return inventory.Inventory{}, err
	}
	return topology.Inventory, nil
}

// topologyLayer is the part of a node's lowered layer that says where it is;
// LowerSource sets the same fields.
func topologyLayer(node SourceNode) clusterplan.NodeLayer {
	var layer clusterplan.NodeLayer
	layer.Bootstrap.Address = strings.TrimSpace(node.Management.Address)
	layer.Kubernetes.Address = strings.TrimSpace(node.Kubernetes.Address)
	return layer
}
