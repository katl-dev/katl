package workstation

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/managementidentity"
	"gopkg.in/yaml.v3"
)

type Config struct {
	CurrentContext string    `json:"currentContext" yaml:"currentContext"`
	Contexts       []Context `json:"contexts" yaml:"contexts"`
	Clusters       []Cluster `json:"clusters" yaml:"clusters"`
}

type Context struct {
	Name    string `json:"name" yaml:"name"`
	Cluster string `json:"cluster" yaml:"cluster"`
}

type Cluster struct {
	ExternalControlPlane bool                                  `json:"externalControlPlane,omitempty" yaml:"externalControlPlane,omitempty"`
	Name                 string                                `json:"name" yaml:"name"`
	ControlPlaneEndpoint string                                `json:"controlPlaneEndpoint,omitempty" yaml:"controlPlaneEndpoint"`
	Nodes                []Node                                `json:"nodes" yaml:"nodes"`
	Management           *managementidentity.ClientCredentials `json:"management,omitempty" yaml:"management,omitempty"`
}

type Node struct {
	Name               string `json:"name" yaml:"name"`
	ManagementEndpoint string `json:"managementEndpoint" yaml:"managementEndpoint"`
	// ConfiguredEndpoint is the cluster configuration's management.address
	// when the context was saved. Rebinding changes ManagementEndpoint but not
	// this field, so a difference from the current configuration shows that
	// the configuration was edited since.
	ConfiguredEndpoint string               `json:"configuredEndpoint,omitempty" yaml:"configuredEndpoint,omitempty"`
	SystemRole         inventory.SystemRole `json:"systemRole" yaml:"systemRole"`
	EnrollmentID       string               `json:"enrollmentID,omitempty" yaml:"enrollmentID,omitempty"`
	MachineID          string               `json:"machineID,omitempty" yaml:"machineID,omitempty"`
}

type Source string

const (
	SourceConfigContext     Source = "config-context"
	SourceExplicitInventory Source = "explicit-inventory"
	SourceExplicitPlan      Source = "explicit-plan"
)

type Topology struct {
	ExternalControlPlane bool                                  `json:"externalControlPlane,omitempty"`
	ContextName          string                                `json:"contextName,omitempty"`
	ClusterName          string                                `json:"clusterName"`
	ControlPlaneEndpoint inventory.HostPort                    `json:"controlPlaneEndpoint,omitzero"`
	Nodes                []TopologyNode                        `json:"nodes"`
	Management           *managementidentity.ClientCredentials `json:"-"`
}

type TopologyNode struct {
	Name               string               `json:"name"`
	ManagementEndpoint inventory.HostPort   `json:"managementEndpoint"`
	SystemRole         inventory.SystemRole `json:"systemRole"`
	EnrollmentID       string               `json:"enrollmentID,omitempty"`
	MachineID          string               `json:"machineID,omitempty"`
}

type ResolvedTopology struct {
	Source Source `json:"source"`
	Topology
}

type ResolveRequest struct {
	ConfigPath        string
	ContextName       string
	ExplicitInventory *inventory.Inventory
	ExplicitPlan      *inventory.Plan
}

// TestPathCheck is installed by TestMain to reject workstation paths outside
// the test sandbox before using them.
var TestPathCheck func(string) error

func ConfigPath() (string, error) {
	path, err := ResolvePath(os.Getenv, os.UserConfigDir)
	if err == nil && TestPathCheck != nil {
		if err := TestPathCheck(path); err != nil {
			return "", err
		}
	}
	return path, err
}

func ResolvePath(getenv func(string) string, userConfigDir func() (string, error)) (string, error) {
	if path := strings.TrimSpace(getenv("KATLCTL_CONFIG")); path != "" {
		return filepath.Clean(path), nil
	}
	if dir := strings.TrimSpace(getenv("KATLCTL_CONFIG_DIR")); dir != "" {
		return filepath.Join(filepath.Clean(dir), "katlctl.yaml"), nil
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate katlctl config directory: %w", err)
	}
	return filepath.Join(dir, "katl", "katlctl.yaml"), nil
}

func Load(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if TestPathCheck != nil {
		if err := TestPathCheck(path); err != nil {
			return Config{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read katlctl config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode katlctl config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Config{}, fmt.Errorf("decode katlctl config: multiple YAML documents are not supported")
	} else if err != io.EOF {
		return Config{}, fmt.Errorf("decode katlctl config: %w", err)
	}
	for _, cluster := range cfg.Clusters {
		if cluster.Management == nil || cluster.Management.Authentication == managementidentity.TrustedNetwork {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return Config{}, fmt.Errorf("inspect katlctl config: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return Config{}, fmt.Errorf("katlctl config %s contains management credentials and must be a mode-0600 regular file; run 'chmod 600 %s'", path, path)
		}
		break
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes a workstation configuration atomically.
func Save(path string, cfg Config) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("workstation context file path is required")
	}
	if TestPathCheck != nil {
		if err := TestPathCheck(path); err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode katlctl config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create katlctl config directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".katlctl-*.yaml")
	if err != nil {
		return fmt.Errorf("create katlctl config temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect katlctl config temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write katlctl config temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close katlctl config temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace katlctl config: %w", err)
	}
	return nil
}

// UpsertCluster installs or replaces one cluster and its context while
// retaining unrelated workstation profiles.
func (cfg Config) UpsertCluster(contextName string, cluster Cluster) Config {
	contextName = strings.TrimSpace(contextName)
	cluster.Name = strings.TrimSpace(cluster.Name)
	nextContexts := make([]Context, 0, len(cfg.Contexts)+1)
	for _, existing := range cfg.Contexts {
		if strings.TrimSpace(existing.Name) != contextName {
			nextContexts = append(nextContexts, existing)
		}
	}
	nextContexts = append(nextContexts, Context{Name: contextName, Cluster: cluster.Name})
	nextClusters := make([]Cluster, 0, len(cfg.Clusters)+1)
	for _, existing := range cfg.Clusters {
		if strings.TrimSpace(existing.Name) != cluster.Name {
			nextClusters = append(nextClusters, existing)
		}
	}
	nextClusters = append(nextClusters, cluster)
	cfg.CurrentContext = contextName
	cfg.Contexts = nextContexts
	cfg.Clusters = nextClusters
	return cfg
}

func (cfg Config) Validate() error {
	contexts := make(map[string]Context, len(cfg.Contexts))
	for _, ctx := range cfg.Contexts {
		ctx.Name = strings.TrimSpace(ctx.Name)
		ctx.Cluster = strings.TrimSpace(ctx.Cluster)
		if err := validateName("context", ctx.Name); err != nil {
			return err
		}
		if ctx.Cluster == "" {
			return fmt.Errorf("context %q cluster is required", ctx.Name)
		}
		if _, ok := contexts[ctx.Name]; ok {
			return fmt.Errorf("duplicate context name %q", ctx.Name)
		}
		contexts[ctx.Name] = ctx
	}
	clusters := make(map[string]Cluster, len(cfg.Clusters))
	for _, cluster := range cfg.Clusters {
		cluster.Name = strings.TrimSpace(cluster.Name)
		if err := validateName("cluster", cluster.Name); err != nil {
			return err
		}
		if _, ok := clusters[cluster.Name]; ok {
			return fmt.Errorf("duplicate cluster name %q", cluster.Name)
		}
		if err := validateCluster(cluster); err != nil {
			return err
		}
		clusters[cluster.Name] = cluster
	}
	for _, ctx := range contexts {
		if _, ok := clusters[ctx.Cluster]; !ok {
			return fmt.Errorf("context %q references unknown cluster %q", ctx.Name, ctx.Cluster)
		}
	}
	currentContext := strings.TrimSpace(cfg.CurrentContext)
	if currentContext == "" {
		return nil
	}
	if _, ok := contexts[currentContext]; !ok {
		return fmt.Errorf("currentContext %q references unknown context", currentContext)
	}
	return nil
}

func (cfg Config) SelectedTopology(contextName string) (Topology, error) {
	if err := cfg.Validate(); err != nil {
		return Topology{}, err
	}
	contextName = strings.TrimSpace(contextName)
	if contextName == "" {
		contextName = strings.TrimSpace(cfg.CurrentContext)
	}
	if contextName == "" {
		return Topology{}, fmt.Errorf("no current context; select one with 'katlctl context use NAME', or pass --config or --context")
	}
	contexts := make(map[string]Context, len(cfg.Contexts))
	for _, ctx := range cfg.Contexts {
		ctx.Name = strings.TrimSpace(ctx.Name)
		ctx.Cluster = strings.TrimSpace(ctx.Cluster)
		contexts[ctx.Name] = ctx
	}
	ctx, ok := contexts[contextName]
	if !ok {
		return Topology{}, fmt.Errorf("context %q was not found", contextName)
	}
	clusters := make(map[string]Cluster, len(cfg.Clusters))
	for _, cluster := range cfg.Clusters {
		cluster.Name = strings.TrimSpace(cluster.Name)
		clusters[cluster.Name] = cluster
	}
	cluster := clusters[ctx.Cluster]
	return topologyFromCluster(ctx.Name, cluster)
}

func ResolveTopology(req ResolveRequest) (ResolvedTopology, error) {
	if req.ExplicitPlan != nil {
		topology, err := topologyFromPlan(*req.ExplicitPlan)
		if err != nil {
			return ResolvedTopology{}, err
		}
		return ResolvedTopology{Source: SourceExplicitPlan, Topology: topology}, nil
	}
	if req.ExplicitInventory != nil {
		topology, err := topologyFromInventory(*req.ExplicitInventory)
		if err != nil {
			return ResolvedTopology{}, err
		}
		return ResolvedTopology{Source: SourceExplicitInventory, Topology: topology}, nil
	}
	path := strings.TrimSpace(req.ConfigPath)
	if path == "" {
		resolved, err := ConfigPath()
		if err != nil {
			return ResolvedTopology{}, err
		}
		path = resolved
	}
	cfg, err := Load(path)
	if err != nil {
		return ResolvedTopology{}, err
	}
	topology, err := cfg.SelectedTopology(req.ContextName)
	if err != nil {
		return ResolvedTopology{}, err
	}
	return ResolvedTopology{Source: SourceConfigContext, Topology: topology}, nil
}

func (t Topology) ControlPlaneNodes() []TopologyNode {
	var nodes []TopologyNode
	for _, node := range t.Nodes {
		if node.SystemRole == inventory.RoleControlPlane {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// topologyFromCluster parses a saved cluster's endpoints, which the file
// keeps as strings.
func topologyFromCluster(contextName string, cluster Cluster) (Topology, error) {
	clusterName := strings.TrimSpace(cluster.Name)
	topology := Topology{
		ExternalControlPlane: cluster.ExternalControlPlane,
		ContextName:          strings.TrimSpace(contextName),
		ClusterName:          clusterName,
		Nodes:                make([]TopologyNode, 0, len(cluster.Nodes)),
	}
	if cluster.Management != nil {
		if err := managementidentity.ValidateClient(*cluster.Management, time.Now().UTC()); err != nil {
			return Topology{}, fmt.Errorf("cluster %q management access: %w", clusterName, err)
		}
		credentials := *cluster.Management
		topology.Management = &credentials
	}
	if value := strings.TrimSpace(cluster.ControlPlaneEndpoint); value != "" {
		endpoint, err := inventory.ParseHostPort(value)
		if err != nil {
			return Topology{}, fmt.Errorf("cluster %s controlPlaneEndpoint: %w", clusterName, err)
		}
		topology.ControlPlaneEndpoint = endpoint
	}
	for _, node := range cluster.Nodes {
		name := strings.TrimSpace(node.Name)
		value := strings.TrimSpace(node.ManagementEndpoint)
		if value == "" {
			return Topology{}, fmt.Errorf("node %q managementEndpoint is required", name)
		}
		endpoint, err := inventory.ParseHostPort(value)
		if err != nil {
			return Topology{}, fmt.Errorf("node %s managementEndpoint: %w", name, err)
		}
		topology.Nodes = append(topology.Nodes, TopologyNode{
			Name:               name,
			ManagementEndpoint: endpoint,
			SystemRole:         inventory.SystemRole(strings.TrimSpace(string(node.SystemRole))),
			EnrollmentID:       strings.TrimSpace(node.EnrollmentID),
			MachineID:          strings.TrimSpace(node.MachineID),
		})
	}
	if err := topology.validate(); err != nil {
		return Topology{}, err
	}
	return topology, nil
}

func topologyFromInventory(inv inventory.Inventory) (Topology, error) {
	topology := Topology{
		ClusterName:          "inventory",
		ExternalControlPlane: inv.ExternalControlPlane,
		ControlPlaneEndpoint: inv.ControlPlaneEndpoint,
		Nodes:                make([]TopologyNode, 0, len(inv.Nodes)),
	}
	for _, node := range inv.Nodes {
		topology.Nodes = append(topology.Nodes, TopologyNode{
			Name:               strings.TrimSpace(node.Name),
			ManagementEndpoint: node.ManagementEndpoint(),
			SystemRole:         inventory.SystemRole(strings.TrimSpace(string(node.SystemRole))),
			EnrollmentID:       strings.TrimSpace(node.EnrollmentID),
			MachineID:          strings.TrimSpace(node.MachineID),
		})
	}
	return topology, topology.validate()
}

func topologyFromPlan(plan inventory.Plan) (Topology, error) {
	topology := Topology{
		ClusterName:          "plan",
		ExternalControlPlane: plan.ExternalControlPlane,
		ControlPlaneEndpoint: plan.ControlPlaneEndpoint,
		Nodes:                make([]TopologyNode, 0, len(plan.Nodes)),
	}
	for _, node := range plan.Nodes {
		topology.Nodes = append(topology.Nodes, TopologyNode{
			Name:               node.Name,
			ManagementEndpoint: node.ManagementEndpoint(),
			SystemRole:         node.SystemRole,
			EnrollmentID:       node.EnrollmentID,
			MachineID:          node.MachineID,
		})
	}
	return topology, topology.validate()
}

// Inventory returns the topology as agent-managed inventory. Each node's
// management address is its saved endpoint, port included.
func (t Topology) Inventory() inventory.Inventory {
	inv := inventory.Inventory{
		ExternalControlPlane: t.ExternalControlPlane,
		ControlPlaneEndpoint: t.ControlPlaneEndpoint,
		Nodes:                make([]inventory.Node, 0, len(t.Nodes)),
	}
	for _, node := range t.Nodes {
		inv.Nodes = append(inv.Nodes, node.InventoryNode())
	}
	return inv
}

// InventoryNode returns the node as an agent-managed inventory node.
func (n TopologyNode) InventoryNode() inventory.Node {
	return inventory.Node{
		Name:         n.Name,
		Address:      n.ManagementEndpoint.ManagementAddress(),
		SystemRole:   n.SystemRole,
		Access:       inventory.Access{Method: "agent"},
		EnrollmentID: n.EnrollmentID,
		MachineID:    n.MachineID,
	}
}

func validateCluster(cluster Cluster) error {
	_, err := topologyFromCluster("", cluster)
	return err
}

func (t Topology) validate() error {
	if len(t.Nodes) == 0 {
		return fmt.Errorf("cluster %q must contain at least one node", t.ClusterName)
	}
	seen := make(map[string]struct{}, len(t.Nodes))
	controlPlanes := 0
	for _, node := range t.Nodes {
		if err := validateName("node", node.Name); err != nil {
			return err
		}
		if _, ok := seen[node.Name]; ok {
			return fmt.Errorf("cluster %q has duplicate node name %q", t.ClusterName, node.Name)
		}
		seen[node.Name] = struct{}{}
		if node.ManagementEndpoint.IsZero() {
			return fmt.Errorf("node %q managementEndpoint is required", node.Name)
		}
		switch node.SystemRole {
		case inventory.RoleControlPlane:
			controlPlanes++
		case inventory.RoleWorker:
		default:
			return fmt.Errorf("node %q systemRole %q is unsupported", node.Name, node.SystemRole)
		}
		if (node.EnrollmentID == "") != (node.MachineID == "") {
			return fmt.Errorf("node %q enrollmentID and machineID must be set together", node.Name)
		}
	}
	if t.ExternalControlPlane && (controlPlanes != 0 || t.ControlPlaneEndpoint.IsZero()) {
		return fmt.Errorf("external cluster %q requires an API endpoint and only worker nodes", t.ClusterName)
	}
	if controlPlanes == 0 && !t.ExternalControlPlane {
		return fmt.Errorf("cluster %q must contain at least one control-plane node", t.ClusterName)
	}
	return nil
}

func validateName(kind, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(value) > 63 || !namePattern.MatchString(value) {
		return fmt.Errorf("%s name %q must be a single DNS-style label", kind, value)
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
