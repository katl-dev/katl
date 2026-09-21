package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type contextSaveOptions struct {
	configInput     string
	contextPath     string
	contextName     string
	replacementNode []string
	selectedNodes   []string
	timeout         time.Duration
	output          string
}

type contextSaveNodeReport struct {
	Name               string `json:"name"`
	ManagementEndpoint string `json:"managementEndpoint"`
	Connected          bool   `json:"connected"`
	EnrollmentID       string `json:"enrollmentID"`
	MachineID          string `json:"machineID"`
	Replaced           bool   `json:"replaced,omitempty"`
}

type contextSaveReport struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Context    string                  `json:"context"`
	ConfigPath string                  `json:"configPath"`
	Nodes      []contextSaveNodeReport `json:"nodes"`
}

func newContextSaveCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := contextSaveOptions{timeout: 15 * time.Second, output: "text"}
	cmd := &cobra.Command{
		Use:   "save",
		Short: "Save installed KatlOS nodes as the current workstation context",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runContextSave(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configInput, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.contextPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "context name; defaults to the cluster name")
	cmd.Flags().StringArrayVar(&opts.replacementNode, "replace-node", nil, "deprecated: trusted reinstalls are detected automatically")
	_ = cmd.Flags().MarkHidden("replace-node")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time to verify each node")
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
	return cmd
}

type contextFileOptions struct {
	path   string
	output string
}

func addContextFileFlags(cmd *cobra.Command, opts *contextFileOptions) {
	cmd.Flags().StringVar(&opts.path, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	addOutputFlag(cmd, &opts.output, "text", "text", "json")
}

func contextFilePath(path string) (string, error) {
	if path = strings.TrimSpace(path); path != "" {
		return path, nil
	}
	return workstation.ConfigPath()
}

func loadContexts(path string) (workstation.Config, string, error) {
	resolved, err := contextFilePath(path)
	if err != nil {
		return workstation.Config{}, "", err
	}
	cfg, err := workstation.Load(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return workstation.Config{}, resolved, fmt.Errorf("no saved katlctl contexts; create one with 'katlctl context save --config cluster.yaml'")
		}
		return workstation.Config{}, resolved, err
	}
	return cfg, resolved, nil
}

func newContextListCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "list", Short: "List saved workstation contexts", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		cfg, _, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(cfg.Contexts)
		}
		if opts.output != "text" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		contexts := append([]workstation.Context(nil), cfg.Contexts...)
		sort.Slice(contexts, func(i, j int) bool { return contexts[i].Name < contexts[j].Name })
		w := newTable(stdout)
		w.row("CURRENT", "CONTEXT", "CLUSTER")
		for _, ctx := range contexts {
			current := ""
			if ctx.Name == cfg.CurrentContext {
				current = "*"
			}
			w.row(current, ctx.Name, ctx.Cluster)
		}
		return w.flush()
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func newContextCurrentCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "current", Short: "Print the current workstation context", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		cfg, _, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		if cfg.CurrentContext == "" {
			return fmt.Errorf("no current context; select one with 'katlctl context use NAME'")
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(map[string]string{"currentContext": cfg.CurrentContext})
		}
		if opts.output != "text" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		_, err = fmt.Fprintln(stdout, cfg.CurrentContext)
		return err
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func newContextDeleteCommand(stdout io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Short:   "Remove a workstation shortcut without changing the cluster",
		Long:    "Delete a saved context and its unused local node bindings. Cluster secrets and nodes are untouched. Deleting the current context leaves no current selection; it never switches to another cluster.",
		Example: "katlctl context delete homelab",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, path, err := loadContexts(opts.path)
			if err != nil {
				return err
			}
			index := slices.IndexFunc(cfg.Contexts, func(item workstation.Context) bool { return item.Name == args[0] })
			if index < 0 {
				return fmt.Errorf("context %q was not found; run 'katlctl context list'", args[0])
			}
			cluster := cfg.Contexts[index].Cluster
			cfg.Contexts = slices.Delete(cfg.Contexts, index, index+1)
			if !slices.ContainsFunc(cfg.Contexts, func(item workstation.Context) bool { return item.Cluster == cluster }) {
				cfg.Clusters = slices.DeleteFunc(cfg.Clusters, func(item workstation.Cluster) bool { return item.Name == cluster })
			}
			if cfg.CurrentContext == args[0] {
				cfg.CurrentContext = ""
			}
			if err := workstation.Save(path, cfg); err != nil {
				return err
			}
			if opts.output == "json" {
				return writeJSON(stdout, map[string]string{"deleted": args[0], "currentContext": cfg.CurrentContext})
			}
			_, err = fmt.Fprintf(stdout, "Deleted context %s; cluster and secrets unchanged.\n", args[0])
			return err
		},
	}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func newContextUseCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "use NAME", Short: "Select the current workstation context", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_ = stderr
		if opts.output != "text" && opts.output != "json" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		cfg, path, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(args[0])
		found := false
		for _, ctx := range cfg.Contexts {
			found = found || ctx.Name == name
		}
		if !found {
			return fmt.Errorf("context %q was not found; run 'katlctl context list'", name)
		}
		cfg.CurrentContext = name
		if err := workstation.Save(path, cfg); err != nil {
			return err
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(map[string]string{"currentContext": name})
		}
		_, err = fmt.Fprintf(stdout, "Current context is now %s\n", name)
		return err
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func prepareContext(ctx context.Context, opts contextSaveOptions, stderr io.Writer) (workstation.Config, contextSaveReport, error) {
	if opts.output != "text" && opts.output != "json" {
		return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.timeout <= 0 {
		return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--timeout must be positive")
	}
	config, err := loadKatlConfig(opts.configInput, "katlctl context save", configbundle.PlanningInputs{}, stderr)
	if err != nil {
		return workstation.Config{}, contextSaveReport{}, err
	}
	bundle := config.Bundle
	inv := bundle.Manifest.Cluster.BootstrapInventory
	replacements := make(map[string]struct{}, len(opts.replacementNode))
	for _, value := range opts.replacementNode {
		name := strings.TrimSpace(value)
		if name == "" {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--replace-node requires a node name")
		}
		replacements[name] = struct{}{}
	}
	for name := range replacements {
		found := false
		for _, node := range inv.Nodes {
			found = found || node.Name == name
		}
		if !found {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--replace-node %q is not present in --config", name)
		}
	}
	configPath := strings.TrimSpace(opts.contextPath)
	if configPath == "" {
		configPath, err = workstation.ConfigPath()
		if err != nil {
			return workstation.Config{}, contextSaveReport{}, err
		}
	}
	contextName := strings.TrimSpace(opts.contextName)
	if contextName == "" {
		contextName = bundle.Manifest.ClusterName
	}
	management, err := managementClientForConfig(opts.configInput, bundle.Manifest.ClusterName)
	if err != nil {
		return workstation.Config{}, contextSaveReport{}, err
	}
	clusterProfile := workstation.Cluster{Name: bundle.Manifest.ClusterName, ControlPlaneEndpoint: inv.ControlPlaneEndpoint, Management: &management}
	report := contextSaveReport{APIVersion: "katl.dev/v1alpha1", Kind: "ContextSaveReport", Context: contextName, ConfigPath: configPath}
	cfg := workstation.Config{}
	if existing, loadErr := loadManagementContext(ctx, configPath); loadErr == nil {
		cfg = existing
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return workstation.Config{}, contextSaveReport{}, loadErr
	}
	known := make(map[string]workstation.Node)
	for _, cluster := range cfg.Clusters {
		if cluster.Name == bundle.Manifest.ClusterName {
			for _, node := range cluster.Nodes {
				known[node.Name] = node
			}
		}
	}

	for _, name := range opts.selectedNodes {
		found := false
		for _, node := range inv.Nodes {
			found = found || node.Name == name
		}
		if !found {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("node %q is not present in --config", name)
		}
	}

	for _, node := range inv.Nodes {
		endpoint := net.JoinHostPort(strings.TrimSpace(node.Address), "9443")
		if len(opts.selectedNodes) > 0 && !containsString(opts.selectedNodes, node.Name) {
			previous := known[node.Name]
			clusterProfile.Nodes = append(clusterProfile.Nodes, workstation.Node{
				Name: node.Name, ManagementEndpoint: endpoint, SystemRole: node.SystemRole,
				EnrollmentID: previous.EnrollmentID, MachineID: previous.MachineID,
			})
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		nodeCtx := withManagementDial(requestCtx, node.Name, &management)
		conn, err := dialKatlcAgent(nodeCtx, endpoint)
		if err != nil {
			cancel()
			if managementVerificationTimedOut(err) {
				return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint %s: timed out after %s; check the address and node reachability or increase --timeout", node.Name, endpoint, opts.timeout)
			}
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint: %w", node.Name, err)
		}
		status, statusErr := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
		closeErr := conn.Close()
		cancel()
		if statusErr != nil {
			if managementVerificationTimedOut(statusErr) {
				return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint %s: timed out after %s; check the address and node reachability or increase --timeout", node.Name, endpoint, opts.timeout)
			}
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint: %w", node.Name, statusErr)
		}
		if closeErr != nil {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("close node %s management endpoint: %w", node.Name, closeErr)
		}
		if strings.TrimSpace(status.GetMachineId()) == "" {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint: agent did not report a machine identity", node.Name)
		}
		if strings.TrimSpace(status.GetEnrollmentId()) == "" {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint: agent did not report an enrollment identity", node.Name)
		}
		if got := strings.TrimSpace(status.GetInventoryNodeName()); got != node.Name {
			return workstation.Config{}, contextSaveReport{}, fmt.Errorf("verify node %s management endpoint: address answered as enrolled node %q", node.Name, got)
		}
		replaced := false
		if previous, ok := known[node.Name]; ok && previous.EnrollmentID != "" && (previous.EnrollmentID != status.GetEnrollmentId() || previous.MachineID != status.GetMachineId()) {
			// Authentication follows the selected mode. Instance IDs bind an
			// operation to one installation without preventing later reinstalls.
			replaced = true
		}
		clusterProfile.Nodes = append(clusterProfile.Nodes, workstation.Node{
			Name: node.Name, ManagementEndpoint: endpoint, SystemRole: node.SystemRole,
			EnrollmentID: status.GetEnrollmentId(), MachineID: status.GetMachineId(),
		})
		report.Nodes = append(report.Nodes, contextSaveNodeReport{Name: node.Name, ManagementEndpoint: endpoint, Connected: true, EnrollmentID: status.GetEnrollmentId(), MachineID: status.GetMachineId(), Replaced: replaced})
	}

	cfg = cfg.UpsertCluster(contextName, clusterProfile)
	return cfg, report, nil
}

func runContextSave(ctx context.Context, opts contextSaveOptions, stdout, stderr io.Writer) error {
	cfg, report, err := prepareContext(ctx, opts, stderr)
	if err != nil {
		return err
	}
	if err := workstation.Save(report.ConfigPath, cfg); err != nil {
		return err
	}
	if opts.output == "text" {
		var replaced []string
		for _, node := range report.Nodes {
			if node.Replaced {
				replaced = append(replaced, node.Name)
			}
		}
		sort.Strings(replaced)
		suffix := ""
		if len(replaced) != 0 {
			suffix = "; replaced enrollment for " + strings.Join(replaced, ", ")
		}
		_, err := fmt.Fprintf(stdout, "Saved context %s with %d node(s)%s\n", report.Context, len(report.Nodes), suffix)
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode context save report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func managementVerificationTimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || grpcstatus.Code(err) == codes.DeadlineExceeded
}

type contextRebindOptions struct {
	contextPath string
	contextName string
	nodeName    string
	endpoint    string
	timeout     time.Duration
}

func newContextRebindCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := contextRebindOptions{timeout: 15 * time.Second}
	cmd := &cobra.Command{Use: "rebind", Short: "Verify an enrolled node and save its new management address", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		return runContextRebind(ctx, opts, stdout)
	}}
	cmd.Flags().StringVar(&opts.contextPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "saved context name")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "enrolled inventory node name")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "new node address: IP, hostname, host:port, or tcp:// URL")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time to verify the proposed address")
	return cmd
}

func runContextRebind(ctx context.Context, opts contextRebindOptions, stdout io.Writer) error {
	if strings.TrimSpace(opts.nodeName) == "" || strings.TrimSpace(opts.endpoint) == "" {
		return fmt.Errorf("--node and --endpoint are required")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	cfg, path, err := loadContexts(opts.contextPath)
	if err != nil {
		return err
	}
	topology, err := cfg.SelectedTopology(opts.contextName)
	if err != nil {
		return err
	}
	var expected workstation.TopologyNode
	found := false
	for _, node := range topology.Nodes {
		if node.Name == strings.TrimSpace(opts.nodeName) {
			expected, found = node, true
			break
		}
	}
	if !found {
		return fmt.Errorf("node %q was not found in context %q", opts.nodeName, topology.ContextName)
	}
	target := managementTarget{nodeName: expected.Name, endpoint: expected.ManagementEndpoint, enrollmentID: expected.EnrollmentID, machineID: expected.MachineID, credentials: topology.Management}
	if err := requireEnrolledTarget(target); err != nil {
		return err
	}
	endpoint, err := normalizeManagementAddress(opts.endpoint)
	if err != nil {
		return err
	}
	conn, err := dialKatlcAgent(withManagementTarget(requestCtx, target), endpoint)
	if err != nil {
		return fmt.Errorf("connect to proposed address %s: %w", endpoint, err)
	}
	status, statusErr := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	closeErr := conn.Close()
	if statusErr != nil {
		return fmt.Errorf("verify proposed address %s: %w", endpoint, statusErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close proposed address %s: %w", endpoint, closeErr)
	}
	if err := verifyPlannedStatus(target, status); err != nil {
		return fmt.Errorf("verify proposed address %s: %w", endpoint, err)
	}
	for clusterIndex := range cfg.Clusters {
		if cfg.Clusters[clusterIndex].Name != topology.ClusterName {
			continue
		}
		for nodeIndex := range cfg.Clusters[clusterIndex].Nodes {
			if cfg.Clusters[clusterIndex].Nodes[nodeIndex].Name == expected.Name {
				cfg.Clusters[clusterIndex].Nodes[nodeIndex].ManagementEndpoint = endpoint
			}
		}
	}
	if err := workstation.Save(path, cfg); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Rebound %s from %s to %s after verifying enrollment and machine identity\n", expected.Name, expected.ManagementEndpoint, endpoint)
	return err
}
