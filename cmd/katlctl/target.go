package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/spf13/cobra"
)

type managementTargetOptions struct {
	clusterConfigPath string
	configPath        string
	contextName       string
	nodeName          string
	endpoint          string
}

// guidanceFlags are the flags that address the same node in a suggested
// command. A saved context file is found through KATLCTL_CONFIG.
func (opts managementTargetOptions) guidanceFlags() string {
	switch {
	case strings.TrimSpace(opts.clusterConfigPath) != "":
		return " --config " + strings.TrimSpace(opts.clusterConfigPath)
	case strings.TrimSpace(opts.contextName) != "":
		return " --context " + strings.TrimSpace(opts.contextName)
	case strings.TrimSpace(opts.endpoint) != "":
		return " --endpoint " + strings.TrimSpace(opts.endpoint)
	}
	return ""
}

type managementTarget struct {
	nodeName     string
	endpoint     inventory.HostPort
	enrollmentID string
	machineID    string
	credentials  *managementidentity.ClientCredentials
	observed     *transport.NodeIdentity
}

func addManagementTargetFlags(cmd *cobra.Command, opts *managementTargetOptions) {
	cmd.Flags().StringVar(&opts.clusterConfigPath, "config", "", "ClusterConfig YAML or compiled Katl config bundle")
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "saved katlctl context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name from --config or a saved context; optional for one node")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "node address override: IP, hostname, host:port, or tcp:// URL")
}

func resolveManagementTarget(ctx context.Context, opts managementTargetOptions) (managementTarget, error) {
	endpoint, err := parseEndpointFlag(opts.endpoint)
	if err != nil {
		return managementTarget{}, err
	}
	if configPath := strings.TrimSpace(opts.clusterConfigPath); configPath != "" {
		resolved, err := resolveClusterConfigTopology(ctx, configPath)
		if err != nil {
			return managementTarget{}, err
		}
		target, err := targetFromTopology(resolved.Topology, opts.nodeName)
		if err != nil {
			return managementTarget{}, err
		}
		if enrolled, ok := enrolledTarget(ctx, strings.TrimSpace(opts.configPath), strings.TrimSpace(opts.contextName), resolved.ClusterName, target.nodeName); ok && target.credentials == nil {
			target = enrolled
		}
		if !endpoint.IsZero() {
			target.endpoint = endpoint
		}
		if target.endpoint.IsZero() {
			return managementTarget{}, fmt.Errorf("node %q has no management.address in --config; set it or pass --endpoint ADDRESS", target.nodeName)
		}
		return target, nil
	}

	useContext := strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.contextName) != "" || endpoint.IsZero()
	if !useContext {
		if nodeName := strings.TrimSpace(opts.nodeName); nodeName != "" {
			resolved, resolveErr := workstation.ResolveTopology(workstation.ResolveRequest{})
			if resolveErr != nil && !errors.Is(resolveErr, os.ErrNotExist) {
				// An unreadable saved config must not silently downgrade the node to a
				// plaintext connection.
				return managementTarget{}, fmt.Errorf("resolve saved katlctl config for node %q: %w; fix it, or pass --config or --context to select the node explicitly", nodeName, resolveErr)
			}
			if resolveErr == nil {
				target, targetErr := targetFromTopology(resolved.Topology, nodeName)
				if targetErr == nil && target.enrollmentID != "" {
					target.endpoint = endpoint
					return target, nil
				}
			}
		}
		return managementTarget{nodeName: strings.TrimSpace(opts.nodeName), endpoint: endpoint, credentials: &managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}}, nil
	}
	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{ConfigPath: strings.TrimSpace(opts.configPath), ContextName: strings.TrimSpace(opts.contextName)})
	if err != nil {
		if endpoint.IsZero() && errors.Is(err, os.ErrNotExist) {
			return managementTarget{}, fmt.Errorf("no node address source: use --config cluster.yaml or --endpoint ADDRESS; for shorter repeated commands, first run 'katlctl context save --config cluster.yaml'")
		}
		return managementTarget{}, err
	}
	target, err := targetFromTopology(topology.Topology, opts.nodeName)
	if err != nil {
		return managementTarget{}, err
	}
	if !endpoint.IsZero() {
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
		inv, err := configbundle.SourceTopology(source)
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("read --config %s: %w", path, err)
		}
		return inv, nil
	}
	bundle, bundleErr := configbundle.ReadBundle(bytes.NewReader(data), "")
	if bundleErr != nil {
		return inventory.Inventory{}, fmt.Errorf("read --config %s as ClusterConfig YAML or Katl config bundle: YAML: %v; bundle: %w", path, sourceErr, bundleErr)
	}
	return bundle.Manifest.Cluster.BootstrapInventory, nil
}

func resolveClusterConfigTopology(ctx context.Context, path string) (workstation.ResolvedTopology, error) {
	inv, err := readManagementInventory(path)
	if err != nil {
		return workstation.ResolvedTopology{}, err
	}
	resolved, err := workstation.ResolveTopology(workstation.ResolveRequest{ExplicitInventory: &inv})
	if err != nil {
		return workstation.ResolvedTopology{}, err
	}
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return workstation.ResolvedTopology{}, fmt.Errorf("read --config %s: %w", path, err)
	}
	if source, sourceErr := configbundle.DecodeSource(bytes.NewReader(data)); sourceErr == nil {
		resolved.ClusterName = strings.TrimSpace(source.Metadata.Name)
		authentication, err := source.ManagementAuthentication()
		if err != nil {
			return workstation.ResolvedTopology{}, err
		}
		if source.Spec.ManagementIdentity != "" || authentication == managementidentity.TrustedNetwork {
			credentials, err := managementClientForConfig(path, source.Metadata.Name)
			if err != nil {
				return workstation.ResolvedTopology{}, err
			}
			resolved.Management = &credentials
			return resolved, nil
		}
		mergeEnrolledTopology(ctx, &resolved, "", "")
		return resolved, nil
	}
	bundle, err := configbundle.ReadBundle(bytes.NewReader(data), "")
	if err != nil {
		return workstation.ResolvedTopology{}, fmt.Errorf("read --config %s: %w", path, err)
	}
	resolved.ClusterName = strings.TrimSpace(bundle.Manifest.ClusterName)
	if bundle.Authentication == managementidentity.TrustedNetwork {
		resolved.Management = &managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}
		return resolved, nil
	}
	mergeEnrolledTopology(ctx, &resolved, "", "")
	return resolved, nil
}

func mergeEnrolledTopology(ctx context.Context, resolved *workstation.ResolvedTopology, configPath, contextName string) {
	if resolved == nil {
		return
	}
	for index := range resolved.Nodes {
		enrolled, ok := enrolledTarget(ctx, configPath, contextName, resolved.ClusterName, resolved.Nodes[index].Name)
		if !ok {
			continue
		}
		resolved.Nodes[index].ManagementEndpoint = enrolled.endpoint
		resolved.Nodes[index].EnrollmentID = enrolled.enrollmentID
		resolved.Nodes[index].MachineID = enrolled.machineID
		if resolved.Management == nil && enrolled.credentials != nil {
			credentials := *enrolled.credentials
			resolved.Management = &credentials
		}
	}
}

func targetFromTopology(topology workstation.Topology, selected string) (managementTarget, error) {
	source := "--config"
	if topology.ContextName != "" {
		source = fmt.Sprintf("context %q", topology.ContextName)
	}
	names := make([]string, 0, len(topology.Nodes))
	for _, node := range topology.Nodes {
		names = append(names, node.Name)
	}
	if len(names) == 0 {
		return managementTarget{}, fmt.Errorf("%s contains no nodes; add nodes to your ClusterConfig and use --config cluster.yaml", source)
	}

	nodeName := strings.TrimSpace(selected)
	if nodeName == "" {
		if len(topology.Nodes) != 1 {
			return managementTarget{}, fmt.Errorf("select a node with NODE or --node because %s contains %d nodes; choose one of: %s", source, len(topology.Nodes), strings.Join(names, ", "))
		}
		nodeName = topology.Nodes[0].Name
	}
	for _, node := range topology.Nodes {
		if node.Name != nodeName {
			continue
		}
		return managementTarget{nodeName: nodeName, endpoint: node.ManagementEndpoint, enrollmentID: node.EnrollmentID, machineID: node.MachineID, credentials: topology.Management}, nil
	}
	return managementTarget{}, fmt.Errorf("node %q was not found in %s; choose one of: %s", nodeName, source, strings.Join(names, ", "))
}

func enrolledTarget(ctx context.Context, configPath, contextName, clusterName, nodeName string) (managementTarget, bool) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		resolved, err := workstation.ConfigPath()
		if err != nil {
			return managementTarget{}, false
		}
		path = resolved
	}
	cfg, err := loadManagementContext(ctx, path)
	if err != nil {
		return managementTarget{}, false
	}
	if contextName != "" {
		topology, err := cfg.SelectedTopology(contextName)
		if err != nil || topology.ClusterName != clusterName {
			return managementTarget{}, false
		}
		for _, node := range topology.Nodes {
			if node.Name == nodeName && node.EnrollmentID != "" {
				return managementTarget{nodeName: node.Name, endpoint: node.ManagementEndpoint, enrollmentID: node.EnrollmentID, machineID: node.MachineID, credentials: topology.Management}, true
			}
		}
		return managementTarget{}, false
	}
	for _, cluster := range cfg.Clusters {
		if strings.TrimSpace(cluster.Name) != clusterName {
			continue
		}
		for _, node := range cluster.Nodes {
			if strings.TrimSpace(node.Name) == nodeName && strings.TrimSpace(node.EnrollmentID) != "" {
				// The saved context file is the boundary for stored endpoints.
				endpoint, err := inventory.DecodeHostPort(node.ManagementEndpoint)
				if err != nil {
					continue
				}
				return managementTarget{nodeName: nodeName, endpoint: endpoint, enrollmentID: strings.TrimSpace(node.EnrollmentID), machineID: strings.TrimSpace(node.MachineID), credentials: cluster.Management}, true
			}
		}
	}
	return managementTarget{}, false
}

func requireEnrolledTarget(target managementTarget) error {
	if target.credentials != nil && target.nodeName != "" {
		return nil
	}
	if strings.TrimSpace(target.enrollmentID) == "" || strings.TrimSpace(target.machineID) == "" {
		return fmt.Errorf("node %q is not enrolled on this workstation; run 'katlctl context save --config cluster.yaml' before planning or changing it", target.nodeName)
	}
	return nil
}

func bindManagementStatus(target *managementTarget, status *agentapi.NodeStatus) error {
	if err := requireEnrolledTarget(*target); err != nil {
		return err
	}
	observed, err := transport.ObserveNode(target.nodeName, status)
	if err != nil {
		return err
	}
	if target.observed != nil {
		if err := target.observed.Verify(status); err != nil {
			return err
		}
	}
	if strings.TrimSpace(status.GetCurrentGenerationId()) == "" {
		return fmt.Errorf("node %q did not report its current generation", target.nodeName)
	}
	target.observed = &observed
	target.enrollmentID = status.GetEnrollmentId()
	target.machineID = status.GetMachineId()
	return nil
}

func verifyPlannedStatus(target managementTarget, status *agentapi.NodeStatus) error {
	expected, err := transport.ObserveNode(target.nodeName, &agentapi.NodeStatus{
		InventoryNodeName: target.nodeName, EnrollmentId: target.enrollmentID, MachineId: target.machineID,
	})
	if err != nil {
		return err
	}
	target.observed = &expected
	return bindManagementStatus(&target, status)
}

// parseEndpointFlag parses an --endpoint value: an IP, hostname, host:port, or
// tcp:// URL, with the agent port defaulted.
func parseEndpointFlag(value string) (inventory.HostPort, error) {
	if strings.TrimSpace(value) == "" {
		return inventory.HostPort{}, nil
	}
	address, err := inventory.ParseManagementAddress(value)
	if err != nil {
		return inventory.HostPort{}, fmt.Errorf("--endpoint: %w", err)
	}
	return address.Endpoint(), nil
}

// Configuration names the trust domain even when another saved cluster uses
// the same address. Carry that choice through each node connection and reboot.
func managementContextForNode(ctx context.Context, configPath, nodeName string) (context.Context, error) {
	if strings.TrimSpace(configPath) == "" {
		return ctx, nil
	}
	target, err := resolveManagementTarget(ctx, managementTargetOptions{clusterConfigPath: configPath, nodeName: nodeName})
	if err != nil {
		return nil, err
	}
	return withManagementTarget(ctx, target), nil
}

func managementContextForCluster(ctx context.Context, configPath, contextPath, contextName string) (context.Context, error) {
	var resolved workstation.ResolvedTopology
	var err error
	if configPath != "" {
		resolved, err = resolveClusterConfigTopology(ctx, configPath)
	} else if contextPath != "" || contextName != "" {
		resolved, err = workstation.ResolveTopology(workstation.ResolveRequest{ConfigPath: contextPath, ContextName: contextName})
	} else {
		resolved, err = workstation.ResolveTopology(workstation.ResolveRequest{})
		if err != nil {
			// Inventory-only callers can supply their own connector policy.
			return ctx, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if resolved.Management != nil {
		ctx = transport.WithClientCredentials(ctx, *resolved.Management)
	}
	return ctx, nil
}

// configuredManagementEndpoint returns the endpoint that the cluster
// configuration names for nodeName, or "" when it names none.
func configuredManagementEndpoint(configPath, nodeName string) (inventory.HostPort, error) {
	inv, err := readManagementInventory(configPath)
	if err != nil {
		return inventory.HostPort{}, err
	}
	for _, node := range inv.Nodes {
		if node.Name == strings.TrimSpace(nodeName) {
			return node.ManagementEndpoint(), nil
		}
	}
	return inventory.HostPort{}, nil
}
