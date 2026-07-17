package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type managementTargetOptions struct {
	clusterConfigPath string
	configPath        string
	contextName       string
	nodeName          string
	endpoint          string
}

type managementTarget struct {
	nodeName string
	endpoint string
}

func addManagementTargetFlags(cmd *cobra.Command, opts *managementTargetOptions) {
	cmd.Flags().StringVar(&opts.clusterConfigPath, "config", "", "ClusterConfig YAML or compiled Katl config bundle")
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "saved katlctl context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name from --config or a saved context; optional for one node")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "node address override: IP, hostname, host:port, or tcp:// URL")
}

func resolveManagementTarget(opts managementTargetOptions) (managementTarget, error) {
	endpoint, err := normalizeManagementAddress(opts.endpoint)
	if err != nil {
		return managementTarget{}, err
	}
	if configPath := strings.TrimSpace(opts.clusterConfigPath); configPath != "" {
		if strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.contextName) != "" {
			return managementTarget{}, fmt.Errorf("--config cannot be combined with --context-file or --context")
		}
		inv, err := readManagementInventory(configPath)
		if err != nil {
			return managementTarget{}, err
		}
		target, err := targetFromInventory(inv, opts.nodeName)
		if err != nil {
			return managementTarget{}, err
		}
		if endpoint != "" {
			target.endpoint = endpoint
		}
		if target.endpoint == "" {
			return managementTarget{}, fmt.Errorf("node %q has no bootstrap.address in --config; set it or pass --endpoint ADDRESS", target.nodeName)
		}
		return target, nil
	}

	useContext := strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.contextName) != "" || endpoint == ""
	if !useContext {
		return managementTarget{nodeName: strings.TrimSpace(opts.nodeName), endpoint: endpoint}, nil
	}
	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{ConfigPath: strings.TrimSpace(opts.configPath), ContextName: strings.TrimSpace(opts.contextName)})
	if err != nil {
		if endpoint == "" && errors.Is(err, os.ErrNotExist) {
			return managementTarget{}, fmt.Errorf("no node address source: use --config cluster.yaml or --endpoint ADDRESS; for shorter repeated commands, first run 'katlctl context save --config cluster.yaml'")
		}
		return managementTarget{}, err
	}
	target, err := targetFromTopology(topology.Topology, opts.nodeName)
	if err != nil {
		return managementTarget{}, err
	}
	if endpoint != "" {
		target.endpoint = endpoint
	}
	return target, nil
}

func readManagementInventory(path string) (inventory.Inventory, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return inventory.Inventory{}, fmt.Errorf("read --config %s: %w", path, err)
	}
	source, sourceErr := configbundle.DecodeSource(bytes.NewReader(data))
	if sourceErr == nil {
		if len(source.Spec.Nodes) == 0 {
			return inventory.Inventory{}, fmt.Errorf("read --config %s: spec.nodes must not be empty", path)
		}
		inv := inventory.Inventory{ControlPlaneEndpoint: strings.TrimSpace(source.Spec.ControlPlaneEndpoint)}
		seen := make(map[string]struct{}, len(source.Spec.Nodes))
		for _, sourceNode := range source.Spec.Nodes {
			name := strings.TrimSpace(sourceNode.Name)
			if name == "" {
				return inventory.Inventory{}, fmt.Errorf("read --config %s: spec.nodes[].name is required", path)
			}
			if _, exists := seen[name]; exists {
				return inventory.Inventory{}, fmt.Errorf("read --config %s: duplicate node name %q", path, name)
			}
			seen[name] = struct{}{}
			role := inventory.RoleWorker
			if sourceNode.ControlPlane {
				role = inventory.RoleControlPlane
			}
			inv.Nodes = append(inv.Nodes, inventory.Node{Name: name, Address: strings.TrimSpace(sourceNode.Bootstrap.Address), SystemRole: role})
		}
		return inv, nil
	}
	bundle, bundleErr := configbundle.ReadBundle(bytes.NewReader(data), "")
	if bundleErr != nil {
		return inventory.Inventory{}, fmt.Errorf("read --config %s as ClusterConfig YAML or Katl config bundle: YAML: %v; bundle: %w", path, sourceErr, bundleErr)
	}
	return bundle.Manifest.Cluster.BootstrapInventory, nil
}

func targetFromInventory(inv inventory.Inventory, selected string) (managementTarget, error) {
	selected = strings.TrimSpace(selected)
	if selected == "" {
		if len(inv.Nodes) != 1 {
			return managementTarget{}, fmt.Errorf("--node is required because --config contains %d nodes", len(inv.Nodes))
		}
		selected = inv.Nodes[0].Name
	}
	for _, node := range inv.Nodes {
		if node.Name == selected {
			endpoint := ""
			if strings.TrimSpace(node.Address) != "" {
				endpoint = cluster.AgentEndpoint(node.Address, "9443")
			}
			return managementTarget{nodeName: node.Name, endpoint: endpoint}, nil
		}
	}
	return managementTarget{}, fmt.Errorf("node %q was not found in --config; choose one of: %s", selected, inventoryNodeNames(inv.Nodes))
}

func inventoryNodeNames(nodes []inventory.Node) string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return strings.Join(names, ", ")
}

func targetFromTopology(topology workstation.Topology, selected string) (managementTarget, error) {
	nodeName := strings.TrimSpace(selected)
	if nodeName == "" {
		if len(topology.Nodes) != 1 {
			return managementTarget{}, fmt.Errorf("--node is required because context %q contains %d nodes", topology.ContextName, len(topology.Nodes))
		}
		nodeName = topology.Nodes[0].Name
	}
	for _, node := range topology.Nodes {
		if node.Name != nodeName {
			continue
		}
		return managementTarget{nodeName: nodeName, endpoint: node.ManagementEndpoint}, nil
	}
	return managementTarget{}, fmt.Errorf("node %q was not found in context %q", nodeName, topology.ContextName)
}

func normalizeManagementAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("--endpoint is invalid: %w", err)
		}
		if parsed.Scheme != "tcp" {
			return "", fmt.Errorf("--endpoint URL scheme must be tcp")
		}
		if parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("--endpoint must be a tcp:// host or host:port without credentials, path, query, or fragment")
		}
		value = parsed.Hostname()
		if port := parsed.Port(); port != "" {
			value = net.JoinHostPort(value, port)
		}
	}
	if ip := net.ParseIP(value); ip != nil {
		return net.JoinHostPort(ip.String(), "9443"), nil
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		if strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
			return "", fmt.Errorf("--endpoint must include a host and port")
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("--endpoint port must be a number from 1 to 65535")
		}
		return value, nil
	}
	if strings.Contains(value, ":") {
		return "", fmt.Errorf("--endpoint %q must be an IP, hostname, host:port, or tcp:// URL", value)
	}
	return net.JoinHostPort(value, "9443"), nil
}
