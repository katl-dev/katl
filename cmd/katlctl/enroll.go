package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
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
	endpoints       map[string]inventory.HostPort
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
	opts := contextSaveOptions{timeout: defaultManagementTimeout, output: "text"}
	cmd := &cobra.Command{
		Use:   "save",
		Short: "Save installed KatlOS nodes as the current workstation context",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runContextSave(ctx, opts, stdout)
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

func prepareContext(ctx context.Context, opts contextSaveOptions, rerun string) (workstation.Config, contextSaveReport, error) {
	if opts.output != "text" && opts.output != "json" {
		return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.timeout <= 0 {
		return workstation.Config{}, contextSaveReport{}, fmt.Errorf("--timeout must be positive")
	}
	topology, err := resolveClusterConfigTopology(ctx, opts.configInput)
	if err != nil {
		return workstation.Config{}, contextSaveReport{}, err
	}
	management, err := managementClientForConfig(opts.configInput, topology.ClusterName)
	if err != nil {
		return workstation.Config{}, contextSaveReport{}, err
	}
	return prepareContextWithCredentials(ctx, opts, topology.ClusterName, management, func(err error) error {
		return fmt.Errorf("%w; check the address and node reachability, then rerun %s",
			managementConfirmationError("the node answers with the configured management credentials", contextPossibleTrust(&management), err), rerun)
	})
}

func prepareContextWithCredentials(ctx context.Context, opts contextSaveOptions, clusterName string, management managementidentity.ClientCredentials, confirmationError func(error) error) (workstation.Config, contextSaveReport, error) {
	// Observe the configured endpoints without compiling host payloads or
	// acquiring artifacts. Their target release is not known until this probe.
	inv, err := readManagementInventory(opts.configInput)
	if err != nil {
		return workstation.Config{}, contextSaveReport{}, err
	}
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
		contextName = clusterName
	}
	clusterProfile := workstation.Cluster{
		ExternalControlPlane: inv.ExternalControlPlane,
		Name:                 clusterName,
		ControlPlaneEndpoint: inv.ControlPlaneEndpoint.String(),
		Management:           &management,
	}
	report := contextSaveReport{APIVersion: "katl.dev/v1alpha1", Kind: "ContextSaveReport", Context: contextName, ConfigPath: configPath}
	cfg := workstation.Config{}
	if existing, loadErr := loadManagementContext(ctx, configPath); loadErr == nil {
		cfg = existing
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return workstation.Config{}, contextSaveReport{}, loadErr
	}
	known := make(map[string]workstation.Node)
	for _, cluster := range cfg.Clusters {
		if cluster.Name == clusterName {
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

	verificationFailure := func(node inventory.Node, endpoint inventory.HostPort, err error) error {
		var hint string
		_, overridden := opts.endpoints[node.Name]
		configured := node.ManagementEndpoint()
		switch {
		case !managementUnreachable(err):
		case !overridden && node.SystemRole == inventory.RoleWorker:
			hint = "; " + readdressHint(node.Name, endpoint, opts.configInput, savedEndpoint(configPath, clusterName, node.Name))
		case overridden && !configured.IsZero() && configured != endpoint:
			hint = "; " + movedHint(node.Name, configured, opts.configInput)
		}
		if managementVerificationTimedOut(err) {
			err = fmt.Errorf("timed out after %s: %w; try to increase --timeout", opts.timeout, err)
		}
		return fmt.Errorf("verify node %s management endpoint %s: %w%s", node.Name, endpoint, confirmationError(err), hint)
	}

	for _, node := range inv.Nodes {
		endpoint := node.ManagementEndpoint()
		if override, ok := opts.endpoints[node.Name]; ok {
			endpoint = override
		}
		if len(opts.selectedNodes) > 0 && !containsString(opts.selectedNodes, node.Name) {
			previous := known[node.Name]
			clusterProfile.Nodes = append(clusterProfile.Nodes, workstation.Node{
				Name: node.Name, ManagementEndpoint: endpoint.String(), ConfiguredEndpoint: configuredEndpoint(node).String(), SystemRole: node.SystemRole,
				EnrollmentID: previous.EnrollmentID, MachineID: previous.MachineID,
			})
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		status, err := contextEndpointStatus(withManagementDial(requestCtx, node.Name, &management), endpoint)
		cancel()
		if err != nil {
			return workstation.Config{}, contextSaveReport{}, verificationFailure(node, endpoint, err)
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
			Name: node.Name, ManagementEndpoint: endpoint.String(), ConfiguredEndpoint: configuredEndpoint(node).String(), SystemRole: node.SystemRole,
			EnrollmentID: status.GetEnrollmentId(), MachineID: status.GetMachineId(),
		})
		report.Nodes = append(report.Nodes, contextSaveNodeReport{Name: node.Name, ManagementEndpoint: endpoint.String(), Connected: true, EnrollmentID: status.GetEnrollmentId(), MachineID: status.GetMachineId(), Replaced: replaced})
	}

	cfg = cfg.UpsertCluster(contextName, clusterProfile)
	return cfg, report, nil
}

func runContextSave(ctx context.Context, opts contextSaveOptions, stdout io.Writer) error {
	rerun := contextSaveInvocation(opts.configInput)
	if name := strings.TrimSpace(opts.contextName); name != "" {
		rerun += " --context " + shellQuote(name)
	}
	if path := strings.TrimSpace(opts.contextPath); path != "" {
		rerun += " --context-file " + shellQuote(path)
	}
	cfg, report, err := prepareContext(ctx, opts, rerun)
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

func contextEndpointStatus(ctx context.Context, endpoint inventory.HostPort) (*agentapi.NodeStatus, error) {
	conn, err := dialKatlcAgent(transport.WithoutManagementAdvice(ctx), endpoint.String())
	if err != nil {
		return nil, err
	}
	status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	closeErr := conn.Close()
	if transport.ClassifyManagementEvidence(err, nil) != transport.ManagementConfirmed {
		return nil, err
	}
	return status, closeErr
}

func contextPossibleTrust(credentials *managementidentity.ClientCredentials) string {
	if credentials != nil && credentials.Authentication == managementidentity.TrustedNetwork {
		return "an mTLS authority"
	}
	return "a different management authority"
}

func managementUnreachable(err error) bool {
	return managementVerificationTimedOut(err) || grpcstatus.Code(err) == codes.Unavailable
}

// readdressHint names the re-address workflow for a worker that does not
// answer at its configured endpoint. A worker whose address is being changed
// keeps answering at its previous address until it reboots. The saved context
// is the only local record of that address, so without it the operator must
// supply the address.
func readdressHint(nodeName string, endpoint inventory.HostPort, configInput string, saved inventory.HostPort) string {
	const guide = "see docs/operations/change-cluster-nodes.md#re-address-a-worker"
	if !saved.IsZero() && saved != endpoint {
		return fmt.Sprintf("%s is not answering at %s, and the saved context last reached it at %s; if you are changing its address, run 'katlctl cluster apply --config %s --node %s --endpoint %s' (%s)", nodeName, endpoint, saved, configInput, nodeName, managementHost(saved), guide)
	}
	return fmt.Sprintf("if you are changing %s's address, run 'katlctl cluster apply --config %s --node %s --endpoint CURRENT_ADDRESS' with the address it answers at now (%s)", nodeName, configInput, nodeName, guide)
}

// movedHint names the step after a re-address reboot for a node that does not
// answer at the --endpoint address while its configuration places it at
// configured: a moved node answers there, so apply no longer needs --endpoint.
func movedHint(nodeName string, configured inventory.HostPort, configInput string) string {
	host := managementHost(configured)
	return fmt.Sprintf("if %s already moved to its management.address %s, finish with 'katlctl cluster apply --config %s --node %s' without --endpoint, after 'katlctl context rebind --node %s --endpoint %s' if you use a saved context (see docs/operations/change-cluster-nodes.md#re-address-a-worker)", nodeName, host, configInput, nodeName, nodeName, host)
}

// savedEndpoint returns the management endpoint the saved context records for
// a node, or the zero endpoint when there is none. It reads the file directly because
// commands that observe nodes replace the saved context in memory with their
// own observations.
func savedEndpoint(path, clusterName, nodeName string) inventory.HostPort {
	saved, err := workstation.Load(path)
	if err != nil {
		return inventory.HostPort{}
	}
	for _, cluster := range saved.Clusters {
		if cluster.Name != clusterName {
			continue
		}
		for _, node := range cluster.Nodes {
			if node.Name == nodeName {
				endpoint, err := inventory.DecodeHostPort(node.ManagementEndpoint)
				if err != nil {
					return inventory.HostPort{}
				}
				return endpoint
			}
		}
	}
	return inventory.HostPort{}
}

// managementHost drops the default agent port so a suggested command matches
// the documented form.
func managementHost(endpoint inventory.HostPort) string {
	if endpoint.Port == inventory.ManagementPort {
		return endpoint.Host.String()
	}
	return endpoint.String()
}

type contextRebindOptions struct {
	contextPath string
	contextName string
	nodeName    string
	endpoint    string
	timeout     time.Duration
}

func newContextRebindCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := contextRebindOptions{timeout: defaultManagementTimeout}
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
	endpoint, err := parseEndpointFlag(opts.endpoint)
	if err != nil {
		return err
	}
	status, err := contextEndpointStatus(withManagementTarget(requestCtx, target), endpoint)
	if err != nil {
		rerun := "katlctl context rebind --node " + shellQuote(target.nodeName) + " --endpoint " + shellQuote(endpoint.String())
		if name := strings.TrimSpace(opts.contextName); name != "" {
			rerun += " --context " + shellQuote(name)
		}
		if path := strings.TrimSpace(opts.contextPath); path != "" {
			rerun += " --context-file " + shellQuote(path)
		}
		if managementVerificationTimedOut(err) {
			err = fmt.Errorf("%w; try to increase --timeout", err)
		}
		return fmt.Errorf("verify proposed address %s: %w; check the address and node reachability, then rerun %s", endpoint,
			managementConfirmationError("the node answers at the proposed address with the saved management credentials", contextPossibleTrust(target.credentials), err), rerun)
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
				cfg.Clusters[clusterIndex].Nodes[nodeIndex].ManagementEndpoint = endpoint.String()
			}
		}
	}
	if err := workstation.Save(path, cfg); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Rebound %s from %s to %s after verifying enrollment and machine identity\n", expected.Name, expected.ManagementEndpoint, endpoint)
	return err
}

// configuredEndpoint returns the management endpoint the node's cluster
// configuration names, or the zero endpoint when it names none.
func configuredEndpoint(node inventory.Node) inventory.HostPort {
	return node.ManagementEndpoint()
}

// warnPendingReaddress warns, once per selected node, that the saved context
// reaches the node at an address other than the one its configuration now
// names. Such a node is either awaiting a planned move or has been moved by
// DHCP. The saved context's configuredEndpoint separates these from a
// configuration that was never edited: after 'context rebind' alone, the
// configuration still names the address recorded when the context was saved
// and is not a pending change. A node passed in endpoints is reached at that
// address by the operator's choice.
func warnPendingReaddress(ctx context.Context, configInput string, endpoints map[string]inventory.HostPort, selectedNodes []string, stderr io.Writer) {
	inv, err := readManagementInventory(configInput)
	if err != nil {
		return
	}
	topology, err := resolveClusterConfigTopology(ctx, configInput)
	if err != nil {
		return
	}
	contextPath, err := workstation.ConfigPath()
	if err != nil {
		return
	}
	saved, err := loadManagementContext(ctx, contextPath)
	if err != nil {
		return
	}
	// Parse each saved baseline once, where it is read from the file.
	baselines := map[string]inventory.HostPort{}
	for _, cluster := range saved.Clusters {
		if cluster.Name == topology.ClusterName {
			for _, node := range cluster.Nodes {
				if baseline, err := inventory.DecodeHostPort(node.ConfiguredEndpoint); err == nil {
					baselines[node.Name] = baseline
				}
			}
		}
	}
	used := map[string]inventory.HostPort{}
	for _, node := range topology.Nodes {
		used[node.Name] = node.ManagementEndpoint
	}
	for _, node := range inv.Nodes {
		if _, overridden := endpoints[node.Name]; overridden {
			continue
		}
		if len(selectedNodes) > 0 && !containsString(selectedNodes, node.Name) {
			continue
		}
		configured := configuredEndpoint(node)
		reached := used[node.Name]
		if configured.IsZero() || reached.IsZero() || configured == reached || baselines[node.Name].IsZero() || baselines[node.Name] == configured {
			continue
		}
		fmt.Fprintf(stderr, "warning: %s is configured at management.address %s, but the saved context reaches it at %s, so this command uses %s; "+
			"to complete a planned move, stage it with 'katlctl cluster apply --config %s --node %s --endpoint %s' and run 'katlctl node reboot %s --config %s --endpoint %s'; "+
			"if it already answers at %s, run 'katlctl context rebind --node %s --endpoint %s' (see docs/operations/change-cluster-nodes.md#re-address-a-worker)\n",
			node.Name, managementHost(configured), managementHost(reached), managementHost(reached),
			configInput, node.Name, managementHost(reached), node.Name, configInput, managementHost(reached),
			managementHost(configured), node.Name, managementHost(configured))
	}
}
