package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

var runBootstrap = cluster.Run
var runAgentBootstrap = cluster.RunAgentBootstrap
var runAgentWorkerJoin = cluster.RunAgentWorkerJoin
var dialVMTestAgent = vmtest.DialAgent
var dialKatlcAgent = dialKatlcAgentTCP
var operatorKubectlRunner cluster.KubectlCommandRunner = execWipeNodeKubectlRunner{}
var newWipeClusterConnector = func() cluster.AgentConnector {
	return cluster.TCPAgentConnector{}
}

const (
	configBundleCreator      = "katlctl config bundle"
	clusterBootstrapCreator  = "katlctl cluster bootstrap"
	wipeClusterOperationKind = "destructive-reset"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katlctl: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := newKatlctlCommand(ctx, stdout, stderr)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func newKatlctlCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "katlctl",
		Short: "Install and manage KatlOS clusters",
		Long: `katlctl installs and manages KatlOS nodes and their Kubernetes cluster.

Start with "katlctl install discover" for a waiting installer or
"katlctl context show" to inspect the current saved cluster.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Version = fmt.Sprintf("version=%s commit=%s date=%s", version, commit, date)
	cmd.SetVersionTemplate("katlctl {{.Version}}\n")
	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print katlctl version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintf(stdout, "katlctl version=%s commit=%s date=%s\n", version, commit, date)
			return nil
		},
	})

	clusterCmd := &cobra.Command{Use: "cluster", Short: "Cluster lifecycle operations"}
	clusterCmd.AddCommand(newClusterStatusCommand(ctx, stdout, stderr))
	clusterCmd.AddCommand(newClusterBootstrapCommand(ctx, stdout, stderr))
	clusterCmd.AddCommand(newWipeClusterCommand(ctx, stdout, stderr, "katlctl cluster wipe"))
	cmd.AddCommand(clusterCmd)

	kubernetesCmd := &cobra.Command{Use: "kubernetes", Short: "Kubernetes lifecycle operations"}
	kubernetesCmd.AddCommand(newKubernetesUpgradeCommand(ctx, stdout, stderr))
	kubeadmConfigCmd := newKubeadmControlPlaneConfigCommand(ctx, stdout, stderr)
	kubeadmConfigCmd.Hidden = true
	kubernetesCmd.AddCommand(kubeadmConfigCmd)
	cmd.AddCommand(kubernetesCmd)

	configCmd := &cobra.Command{Use: "config", Short: "Create and compile ClusterConfig"}
	configCmd.AddCommand(newConfigInitCommand(ctx, stdout, stderr))
	configCmd.AddCommand(newConfigValidateCommand(stdout, stderr))
	configCmd.AddCommand(newConfigSchemaCommand(stdout, stderr))
	configCmd.AddCommand(newConfigBundleCommand(stdout, stderr))
	renderNodeCmd := newConfigRenderNodeCommand(stdout, stderr)
	renderNodeCmd.Hidden = true
	configCmd.AddCommand(renderNodeCmd)
	cmd.AddCommand(configCmd)

	contextCmd := &cobra.Command{Use: "context", Short: "Save and inspect workstation contexts"}
	contextCmd.AddCommand(newContextSaveCommand(ctx, stdout, stderr))
	contextCmd.AddCommand(newConfigPathCommand(stdout, stderr))
	contextCmd.AddCommand(newContextListCommand(stdout, stderr))
	contextCmd.AddCommand(newContextCurrentCommand(stdout, stderr))
	contextCmd.AddCommand(newContextUseCommand(stdout, stderr))
	topologyCmd := newConfigTopologyCommand(stdout, stderr)
	topologyCmd.Use = "show"
	topologyCmd.Short = "Show the selected cluster and its nodes"
	contextCmd.AddCommand(topologyCmd)
	cmd.AddCommand(contextCmd)

	cmd.AddCommand(newInstallCommand(ctx, stdout, stderr))
	cmd.AddCommand(newOperationCommand(ctx, stdout, stderr))

	nodeCmd := &cobra.Command{Use: "node", Short: "Manage individual KatlOS nodes"}
	nodeCmd.AddCommand(newHostStatusCommand(ctx, stdout, stderr))
	nodeCmd.AddCommand(newHostRebootCommand(ctx, stdout, stderr))
	nodeCmd.AddCommand(newHostUpgradeCommand(ctx, stdout, stderr))
	nodeCmd.AddCommand(newConfigApplyCommand(ctx, stdout, stderr))
	nodeCmd.AddCommand(newWipeNodeCommand(ctx, stdout, stderr, "katlctl node wipe"))
	cmd.AddCommand(nodeCmd)

	configureCommandGroups(cmd)
	setMinimumInvocationExamples(cmd)
	return cmd
}

func configureCommandGroups(root *cobra.Command) {
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		if command != root && command.HasSubCommands() && command.Run == nil && command.RunE == nil {
			command.Args = rejectUnknownSubcommand
			command.RunE = func(command *cobra.Command, _ []string) error {
				return command.Help()
			}
		}
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
}

func rejectUnknownSubcommand(command *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	message := fmt.Sprintf("unknown command %q for %q", args[0], command.CommandPath())
	if command.SuggestionsMinimumDistance <= 0 {
		command.SuggestionsMinimumDistance = 2
	}
	if suggestions := command.SuggestionsFor(args[0]); len(suggestions) > 0 {
		message += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return errors.New(message)
}

func setMinimumInvocationExamples(root *cobra.Command) {
	examples := map[string]string{
		"katlctl":                     "katlctl install discover",
		"katlctl version":             "katlctl version",
		"katlctl cluster":             "katlctl cluster bootstrap --config cluster.yaml",
		"katlctl cluster status":      "katlctl cluster status --config cluster.yaml",
		"katlctl context save":        "katlctl context save --config cluster.yaml",
		"katlctl cluster bootstrap":   "katlctl cluster bootstrap --config cluster.yaml",
		"katlctl cluster wipe":        "katlctl cluster wipe --config cluster.yaml --all",
		"katlctl kubernetes":          "katlctl kubernetes upgrade v1.36.1 --config cluster.yaml",
		"katlctl kubernetes upgrade":  "katlctl kubernetes upgrade v1.36.1 --config cluster.yaml",
		"katlctl config":              "katlctl config validate cluster.yaml",
		"katlctl config init":         "katlctl config init cluster.yaml --node cp-1=control-plane,192.0.2.10,/dev/disk/by-id/ata-root",
		"katlctl config validate":     "katlctl config validate cluster.yaml",
		"katlctl config schema":       "katlctl config schema",
		"katlctl config bundle":       "katlctl config bundle cluster.yaml --output cluster.katlcfg",
		"katlctl config render-node":  "katlctl config render-node --config cluster.yaml --node cp-1 --desired-version 1",
		"katlctl context":             "katlctl context show",
		"katlctl context path":        "katlctl context path",
		"katlctl context list":        "katlctl context list",
		"katlctl context current":     "katlctl context current",
		"katlctl context use":         "katlctl context use homelab",
		"katlctl context show":        "katlctl context show",
		"katlctl install":             "katlctl install discover",
		"katlctl install discover":    "katlctl install discover",
		"katlctl install apply":       "katlctl install apply --config cluster.yaml",
		"katlctl install status":      "katlctl install status",
		"katlctl operations":          "katlctl operations list --config cluster.yaml --node cp-1",
		"katlctl operations status":   "katlctl operations status OPERATION_ID --config cluster.yaml --node cp-1",
		"katlctl operations list":     "katlctl operations list --config cluster.yaml --node cp-1",
		"katlctl node":                "katlctl node status cp-1 --config cluster.yaml",
		"katlctl node status":         "katlctl node status cp-1 --config cluster.yaml",
		"katlctl node reboot":         "katlctl node reboot cp-1 --config cluster.yaml",
		"katlctl node upgrade":        "katlctl node upgrade 2026.7.0 cp-1 --config cluster.yaml",
		"katlctl node apply":          "katlctl node apply cp-1 --config cluster.yaml",
		"katlctl node apply validate": "katlctl node apply validate --config cluster.yaml --node cp-1",
		"katlctl node apply status":   "katlctl node apply status --node cp-1",
		"katlctl node wipe":           "katlctl node wipe worker-1 --config cluster.yaml --kubeconfig kubeconfig",
	}
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		command.Example = examples[command.CommandPath()]
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
}

type hostUpgradeOptions struct {
	version         string
	target          managementTargetOptions
	clientRequestID string
	actor           string
	plan            bool
	waitTimeout     time.Duration
	output          string
}

type hostUpgradeReport struct {
	Node       string `json:"node"`
	Version    string `json:"version"`
	Image      string `json:"image"`
	Result     string `json:"result"`
	Rebooted   bool   `json:"rebooted"`
	BootHealth string `json:"bootHealth"`
}

var katlOSReleasePattern = regexp.MustCompile(`^[0-9]{4}\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

func newHostUpgradeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostUpgradeOptions{actor: "katlctl node upgrade", waitTimeout: 30 * time.Minute, output: "text"}
	cmd := &cobra.Command{
		Use:   "upgrade VERSION [NODE]",
		Short: "Upgrade one KatlOS node and verify its next boot",
		Long: `Upgrade one KatlOS node to a published KatlOS release, reboot it, and verify that it returns healthy.

Use the same ClusterConfig used to install the node. --endpoint can override its recorded address when DHCP or local routing changed. A saved katlctl context is optional shorthand for repeated commands.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 {
				return command.Help()
			}
			opts.version = args[0]
			if len(args) == 2 {
				if err := selectHostNode(&opts.target.nodeName, args[1:]); err != nil {
					return err
				}
			}
			return runHostUpgrade(ctx, opts, stdout, stderr)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.actor, "actor", opts.actor, "operation actor")
	cmd.Flags().Lookup("actor").Hidden = true
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "validate without accepting an operation")
	cmd.Flags().DurationVar(&opts.waitTimeout, "timeout", opts.waitTimeout, "overall operation wait timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func runHostUpgrade(ctx context.Context, opts hostUpgradeOptions, stdout, stderr io.Writer) error {
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	version, err := katlOSVersion(opts.version)
	if err != nil {
		return err
	}
	opts.version = version
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	request := operation.HostUpgrade{
		CandidateGenerationID: "katlos-" + version,
	}
	target, err := resolveManagementTarget(opts.target)
	if err != nil {
		return err
	}
	conn, err := dialKatlcAgent(ctx, target.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return fmt.Errorf("read node status: %w", err)
	}
	current, err := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: status.GetCurrentGenerationId()})
	if err != nil {
		return fmt.Errorf("read current node generation: %w", err)
	}
	architecture, err := nodeArtifactArchitecture(current)
	if err != nil {
		return err
	}
	request.ImageURL = katlOSReleaseURL(opts.version, architecture)
	if err := operation.ValidateHostUpgrade(request); err != nil {
		return err
	}
	if current.GetGenerationId() == request.CandidateGenerationID && current.GetCommitState() == generation.CommitStateCommitted && current.GetBootState() == generation.BootStateGood && current.GetHealthState() == generation.HealthStateHealthy {
		node := target.nodeName
		if node == "" {
			node = target.endpoint
		}
		return writeHostUpgradeReport(stdout, opts.output, hostUpgradeReport{Node: node, Version: opts.version, Image: request.ImageURL, Result: operation.ResultSucceeded, BootHealth: generation.HealthStateHealthy})
	}
	accepted, err := conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion:                  operation.APIVersion,
		Kind:                        "SubmitOperationRequest",
		ClientRequestId:             requestID,
		OperationKind:               "host-upgrade",
		Actor:                       strings.TrimSpace(opts.actor),
		ExpectedMachineId:           status.GetMachineId(),
		ExpectedCurrentGenerationId: status.GetCurrentGenerationId(),
		DryRun:                      opts.plan,
		HostUpgrade: &agentapi.HostUpgradeOperationRequest{
			ImageUrl:              request.ImageURL,
			ImageLocalRef:         request.ImageLocalRef,
			CandidateGenerationId: request.CandidateGenerationID,
		},
	})
	if err != nil {
		return err
	}
	report := hostUpgradeReport{Node: target.nodeName, Version: opts.version, Image: request.ImageURL, Result: "planned", BootHealth: "not-run"}
	if report.Node == "" {
		report.Node = target.endpoint
	}
	if opts.plan {
		return writeHostUpgradeReport(stdout, opts.output, report)
	}
	terminal, err := waitAcceptedOperationStatus(ctx, conn.Client, accepted, opts.waitTimeout, stderr)
	if err != nil {
		report.Result = "failed"
		_ = writeHostUpgradeReport(stdout, opts.output, report)
		return err
	}
	if err := operationResultError(terminal); err != nil {
		report.Result = terminal.GetResult()
		_ = writeHostUpgradeReport(stdout, opts.output, report)
		return err
	}
	agentStart := status.GetAgentStartId()
	if err := requestNodeReboot(ctx, conn.Client, opts.actor, status.GetMachineId(), request.CandidateGenerationID); err != nil {
		report.Result = "staged"
		_ = writeHostUpgradeReport(stdout, opts.output, report)
		return fmt.Errorf("reboot node %s: %w", report.Node, err)
	}
	_ = conn.Close()
	bootCtx, cancel := context.WithTimeout(ctx, opts.waitTimeout)
	verifiedConn, _, err := waitNodeBootHealth(bootCtx, report.Node, target.endpoint, agentStart, request.CandidateGenerationID, stderr)
	cancel()
	if err != nil {
		report.Result = "failed"
		report.Rebooted = true
		report.BootHealth = "failed"
		_ = writeHostUpgradeReport(stdout, opts.output, report)
		return err
	}
	_ = verifiedConn.Close()
	report.Result = operation.ResultSucceeded
	report.Rebooted = true
	report.BootHealth = generation.HealthStateHealthy
	return writeHostUpgradeReport(stdout, opts.output, report)
}

func writeHostUpgradeReport(stdout io.Writer, output string, report hostUpgradeReport) error {
	if output == "json" {
		return writeJSON(stdout, report)
	}
	if report.Result == "planned" {
		_, err := fmt.Fprintf(stdout, "%s can upgrade to KatlOS %s\n", report.Node, report.Version)
		return err
	}
	if report.Result == operation.ResultSucceeded {
		_, err := fmt.Fprintf(stdout, "%s runs KatlOS %s; health %s\n", report.Node, report.Version, report.BootHealth)
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s KatlOS %s upgrade result: %s\n", report.Node, report.Version, report.Result)
	return err
}

func katlOSVersion(input string) (string, error) {
	version := strings.TrimPrefix(strings.TrimSpace(input), "v")
	if !katlOSReleasePattern.MatchString(version) {
		return "", fmt.Errorf("VERSION %q must look like 2026.7.0-alpha.9", input)
	}
	return version, nil
}

func nodeArtifactArchitecture(current *agentapi.Generation) (string, error) {
	if architecture, ok := supportedArtifactArchitecture(current.GetRuntimeArchitecture()); ok {
		return architecture, nil
	}
	for _, ref := range current.GetSysexts() {
		if architecture, ok := supportedArtifactArchitecture(ref.GetArchitecture()); ok {
			return architecture, nil
		}
	}
	return "", fmt.Errorf("current node generation does not report a supported artifact architecture")
}

func supportedArtifactArchitecture(value string) (string, bool) {
	switch strings.TrimSpace(value) {
	case "x86_64":
		return "x86_64", true
	case "aarch64":
		return "aarch64", true
	case "amd64":
		return "x86_64", true
	case "arm64":
		return "aarch64", true
	default:
		return "", false
	}
}

func katlOSReleaseURL(version, architecture string) string {
	tag := "v" + version
	name := "katlos-upgrade-" + version + "-" + architecture + ".squashfs"
	return "https://github.com/katl-dev/katl/releases/download/" + tag + "/" + name
}

func writeJSON(stdout io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

type wipeClusterOptions struct {
	command           string
	selectedNodes     stringList
	configPath        string
	inventoryPath     string
	workstationConfig string
	contextName       string
	all               bool
	allowPartial      bool
	clientRequestID   string
	planOnly          bool
	noWait            bool
	timeout           string
	output            string
}

func newWipeClusterCommand(ctx context.Context, stdout, stderr io.Writer, commandName string) *cobra.Command {
	opts := wipeClusterOptions{command: commandName, output: "text"}
	cmd := &cobra.Command{
		Use:   "wipe",
		Short: "Destructively reset cluster nodes for installer-media reinstall",
		Long:  "Erase KatlOS boot artifacts from the selected cluster nodes. Every wiped node must boot installer media or PXE before it can be used again.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runWipeClusterOptions(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster inventory")
	cmd.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.workstationConfig, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("inventory").Hidden = true
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().BoolVar(&opts.all, "all", false, "select every node in the inventory")
	cmd.Flags().BoolVar(&opts.allowPartial, "allow-partial-cluster", false, "allow a partial cluster target set")
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().BoolVar(&opts.planOnly, "plan", false, "print the destructive wipe plan without accepting node-local operations")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after nodes accept their operations")
	cmd.Flags().StringVar(&opts.timeout, "timeout", "30m", "operation and wait timeout duration")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "output format: text or json")
	cmd.Flags().Var(&opts.selectedNodes, "node", "inventory node name to wipe; may be repeated")
	return cmd
}

func runWipeClusterOptions(ctx context.Context, opts wipeClusterOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	waitTimeout, err := time.ParseDuration(opts.timeout)
	if err != nil || waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be a positive duration")
	}
	if opts.all && len(opts.selectedNodes.values) > 0 {
		return fmt.Errorf("--all and --node cannot be combined")
	}

	targets, partial, err := resolveWipeClusterTargets(opts)
	if err != nil {
		return err
	}
	report := newWipeClusterReport(opts.planOnly, partial, targets)
	report.Output = opts.output
	report.Command = opts.command
	if partial && !opts.allowPartial {
		report.Refusals = append(report.Refusals, "partial cluster wipe requires --allow-partial-cluster")
		if printErr := printWipeClusterReport(stdout, report); printErr != nil {
			return printErr
		}
		return fmt.Errorf("partial cluster wipe requires --allow-partial-cluster")
	}

	connector := newWipeClusterConnector()
	if connector == nil {
		return fmt.Errorf("katlc agent connector is required")
	}
	if err := preflightWipeCluster(ctx, connector, &report, targets); err != nil {
		if printErr := printWipeClusterReport(stdout, report); printErr != nil {
			return printErr
		}
		return err
	}
	if opts.planOnly {
		report.NodeLocalOperations = plannedWipeClusterOperations(targets)
		return printWipeClusterReport(stdout, report)
	}
	submitErr := submitWipeCluster(ctx, connector, &report, targets, requestID, strings.TrimSpace(opts.timeout), opts.noWait, waitTimeout, stderr)
	if printErr := printWipeClusterReport(stdout, report); printErr != nil {
		return printErr
	}
	return submitErr
}

func resolveWipeClusterTargets(opts wipeClusterOptions) ([]inventory.PlannedNode, bool, error) {
	hasExplicitTopology := strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.inventoryPath) != ""
	if !hasExplicitTopology {
		topology, err := workstation.ResolveTopology(workstation.ResolveRequest{
			ConfigPath:  strings.TrimSpace(opts.workstationConfig),
			ContextName: strings.TrimSpace(opts.contextName),
		})
		if err != nil {
			return nil, false, fmt.Errorf("resolve cluster from workstation context: %w", err)
		}
		plan := inventory.Plan{Nodes: make([]inventory.PlannedNode, 0, len(topology.Nodes))}
		for _, node := range topology.Nodes {
			host, _, err := net.SplitHostPort(node.ManagementEndpoint)
			if err != nil {
				return nil, false, fmt.Errorf("node %q management endpoint: %w", node.Name, err)
			}
			plan.Nodes = append(plan.Nodes, inventory.PlannedNode{
				Name:       node.Name,
				Address:    host,
				SystemRole: node.SystemRole,
				Access:     inventory.Access{Method: "agent"},
			})
		}
		return wipeClusterTargets(plan, opts.all, opts.selectedNodes.values)
	}

	inv, err := loadWipeInventory(opts.configPath, opts.inventoryPath)
	if err != nil {
		return nil, false, err
	}
	inv, err = overlayWipeContext(inv, opts.workstationConfig, opts.contextName)
	if err != nil {
		return nil, false, err
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: inv})
	if err != nil {
		return nil, false, err
	}
	return wipeClusterTargets(plan, opts.all, opts.selectedNodes.values)
}

type wipeNodeOptions struct {
	command           string
	selectedNodes     stringList
	configPath        string
	inventoryPath     string
	workstationConfig string
	contextName       string
	kubeconfigPath    string
	clientRequestID   string
	planOnly          bool
	noWait            bool
	timeout           string
	output            string
}

func newWipeNodeCommand(ctx context.Context, stdout, stderr io.Writer, commandName string) *cobra.Command {
	opts := wipeNodeOptions{command: commandName, output: "text"}
	cmd := &cobra.Command{
		Use:   "wipe NODE",
		Short: "Remove one node and reset it for installer-media reinstall",
		Long:  "Remove one worker from Kubernetes and erase its KatlOS boot artifacts. The node must boot installer media or PXE before it can be used again.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 {
				return command.Help()
			}
			opts.selectedNodes.values = []string{args[0]}
			return runWipeNodeOptions(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster inventory")
	cmd.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.workstationConfig, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("inventory").Hidden = true
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.kubeconfigPath, "kubeconfig", "", "path to operator kubeconfig")
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().BoolVar(&opts.planOnly, "plan", false, "print the destructive wipe plan without accepting node-local operation")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the node accepts the operation")
	cmd.Flags().StringVar(&opts.timeout, "timeout", "30m", "operation and wait timeout duration")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "output format: text or json")
	return cmd
}

func runWipeNodeOptions(ctx context.Context, opts wipeNodeOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if len(opts.selectedNodes.values) != 1 {
		return fmt.Errorf("exactly one --node is required")
	}
	if !opts.planOnly && strings.TrimSpace(opts.kubeconfigPath) == "" {
		return fmt.Errorf("--kubeconfig is required")
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	waitTimeout, err := time.ParseDuration(opts.timeout)
	if err != nil || waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be a positive duration")
	}

	target, partial, err := resolveWipeNodeTarget(opts)
	if err != nil {
		return err
	}
	report := newWipeNodeReport(opts.planOnly, partial, target)
	report.Output = opts.output
	report.Command = opts.command
	if target.SystemRole == inventory.RoleControlPlane {
		report.KubernetesCleanup = "refused"
		report.Nodes = append(report.Nodes, wipeClusterNodeResult{Node: target.Name, Result: "refused"})
		report.Refusals = append(report.Refusals, "single control-plane wipe requires etcd membership coordination before node-local reset")
		if printErr := printWipeNodeReport(stdout, report); printErr != nil {
			return printErr
		}
		return fmt.Errorf("single control-plane wipe requires etcd membership coordination")
	}

	connector := newWipeClusterConnector()
	if connector == nil {
		return fmt.Errorf("katlc agent connector is required")
	}
	if err := preflightWipeCluster(ctx, connector, &report.wipeClusterReport, []inventory.PlannedNode{target}); err != nil {
		if printErr := printWipeNodeReport(stdout, report); printErr != nil {
			return printErr
		}
		return err
	}
	if opts.planOnly {
		if strings.TrimSpace(opts.kubeconfigPath) == "" {
			report.KubernetesCleanup = "unknown"
		}
		report.NodeLocalOperations = []wipeClusterNodeLocalOperation{wipeNodeOperation(target)}
		return printWipeNodeReport(stdout, report)
	}

	cleanup := cleanupWipeNodeKubernetes(ctx, strings.TrimSpace(opts.kubeconfigPath), target, strings.TrimSpace(opts.timeout))
	report.KubernetesCleanup = cleanup.Status
	report.KubernetesDiagnostics = cleanup.Diagnostics
	if cleanup.Status == "recovery-required" {
		if printErr := printWipeNodeReport(stdout, report); printErr != nil {
			return printErr
		}
		return fmt.Errorf("Kubernetes cleanup failed before node-local wipe")
	}

	submitErr := submitWipeCluster(ctx, connector, &report.wipeClusterReport, []inventory.PlannedNode{target}, requestID, strings.TrimSpace(opts.timeout), opts.noWait, waitTimeout, stderr)
	if printErr := printWipeNodeReport(stdout, report); printErr != nil {
		return printErr
	}
	return submitErr
}

func resolveWipeNodeTarget(opts wipeNodeOptions) (inventory.PlannedNode, bool, error) {
	hasExplicitTopology := strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.inventoryPath) != ""
	if !hasExplicitTopology {
		topology, err := workstation.ResolveTopology(workstation.ResolveRequest{
			ConfigPath:  strings.TrimSpace(opts.workstationConfig),
			ContextName: strings.TrimSpace(opts.contextName),
		})
		if err != nil {
			return inventory.PlannedNode{}, false, fmt.Errorf("resolve node from workstation context: %w", err)
		}
		for _, node := range topology.Nodes {
			if node.Name != opts.selectedNodes.values[0] {
				continue
			}
			host, _, err := net.SplitHostPort(node.ManagementEndpoint)
			if err != nil {
				return inventory.PlannedNode{}, false, fmt.Errorf("node %q management endpoint: %w", node.Name, err)
			}
			return inventory.PlannedNode{
				Name:       node.Name,
				Address:    host,
				SystemRole: node.SystemRole,
				Access:     inventory.Access{Method: "agent"},
			}, len(topology.Nodes) > 1, nil
		}
		return inventory.PlannedNode{}, false, fmt.Errorf("node %q is not in context %q", opts.selectedNodes.values[0], topology.ContextName)
	}

	inv, err := loadWipeInventory(opts.configPath, opts.inventoryPath)
	if err != nil {
		return inventory.PlannedNode{}, false, err
	}
	inv, err = overlayWipeContext(inv, opts.workstationConfig, opts.contextName)
	if err != nil {
		return inventory.PlannedNode{}, false, err
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: inv})
	if err != nil {
		return inventory.PlannedNode{}, false, err
	}
	targets, partial, err := wipeClusterTargets(plan, false, opts.selectedNodes.values)
	if err != nil {
		return inventory.PlannedNode{}, false, err
	}
	return targets[0], partial, nil
}

func loadWipeInventory(configPath, inventoryPath string) (inventory.Inventory, error) {
	inputs := 0
	for _, value := range []string{configPath, inventoryPath} {
		if strings.TrimSpace(value) != "" {
			inputs++
		}
	}
	if inputs != 1 {
		return inventory.Inventory{}, fmt.Errorf("exactly one of --config or --inventory is required")
	}
	if strings.TrimSpace(inventoryPath) != "" {
		return loadInventory(inventoryPath)
	}
	config, err := loadKatlConfig(configPath, "katlctl cluster wipe", configbundle.PlanningInputs{})
	if err != nil {
		return inventory.Inventory{}, err
	}
	return config.Bundle.Manifest.Cluster.BootstrapInventory, nil
}

func overlayWipeContext(inv inventory.Inventory, configPath, contextName string) (inventory.Inventory, error) {
	if strings.TrimSpace(configPath) == "" && strings.TrimSpace(contextName) == "" {
		return inv, nil
	}
	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{ConfigPath: strings.TrimSpace(configPath), ContextName: strings.TrimSpace(contextName)})
	if err != nil {
		return inventory.Inventory{}, err
	}
	byName := make(map[string]workstation.TopologyNode, len(topology.Nodes))
	for _, node := range topology.Nodes {
		byName[node.Name] = node
	}
	for index := range inv.Nodes {
		node, ok := byName[inv.Nodes[index].Name]
		if !ok {
			return inventory.Inventory{}, fmt.Errorf("node %q from wipe input is missing from context %q", inv.Nodes[index].Name, topology.ContextName)
		}
		host, _, err := net.SplitHostPort(node.ManagementEndpoint)
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("node %q management endpoint: %w", node.Name, err)
		}
		inv.Nodes[index].Address = host
		inv.Nodes[index].Access = inventory.Access{Method: "agent"}
	}
	return inv, nil
}

type wipeClusterReport struct {
	Output              string                          `json:"-"`
	APIVersion          string                          `json:"apiVersion"`
	Kind                string                          `json:"kind"`
	Command             string                          `json:"command"`
	Plan                bool                            `json:"plan"`
	PartialCluster      bool                            `json:"partialCluster"`
	Targets             []wipeClusterTarget             `json:"targets"`
	KubernetesCleanup   string                          `json:"kubernetesCleanup"`
	NodeLocalOperations []wipeClusterNodeLocalOperation `json:"nodeLocalOperations"`
	WipedState          []string                        `json:"wipedState"`
	PreservedState      []string                        `json:"preservedState"`
	Refusals            []string                        `json:"refusals,omitempty"`
	Nodes               []wipeClusterNodeResult         `json:"nodes,omitempty"`
}

type wipeNodeReport struct {
	wipeClusterReport
	KubernetesDiagnostics []string `json:"kubernetesDiagnostics,omitempty"`
}

type wipeClusterTarget struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	SystemRole string `json:"systemRole"`
}

type wipeClusterNodeLocalOperation struct {
	Node                   string   `json:"node"`
	OperationKind          string   `json:"operationKind"`
	ResetScope             string   `json:"resetScope"`
	TargetGenerationID     string   `json:"targetGenerationID,omitempty"`
	DiscardClusterIdentity bool     `json:"discardClusterIdentity"`
	WipeSurfaces           []string `json:"wipeSurfaces"`
}

type wipeClusterNodeResult struct {
	Node          string   `json:"node"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Accepted      bool     `json:"accepted"`
	OperationID   string   `json:"operationID,omitempty"`
	OperationKind string   `json:"operationKind,omitempty"`
	Phase         string   `json:"phase,omitempty"`
	Terminal      bool     `json:"terminal,omitempty"`
	Result        string   `json:"result,omitempty"`
	Diagnostics   []string `json:"diagnostics,omitempty"`
}

func newWipeClusterReport(planOnly bool, partial bool, nodes []inventory.PlannedNode) wipeClusterReport {
	report := wipeClusterReport{
		APIVersion:        operation.APIVersion,
		Kind:              "WipeClusterReport",
		Command:           "katlctl cluster wipe",
		Plan:              planOnly,
		PartialCluster:    partial,
		KubernetesCleanup: "not-attempted",
		WipedState: []string{
			"katlos-boot-artifacts",
			"disk-boot-path",
		},
		PreservedState: []string{
			"existing-kubernetes-state-until-installer-reinstall",
			"existing-kubelet-etcd-cni-and-container-runtime-state-until-installer-reinstall",
			"existing-generation-operation-and-node-identity-state-until-installer-reinstall",
			"off-node-artifacts",
			"operator-workstations",
			"external-backups",
			"external-load-balancers",
			"non-target-disks",
		},
	}
	for _, node := range nodes {
		report.Targets = append(report.Targets, wipeClusterTarget{
			Name:       node.Name,
			Address:    node.Address,
			SystemRole: string(node.SystemRole),
		})
	}
	return report
}

func newWipeNodeReport(planOnly bool, partial bool, node inventory.PlannedNode) wipeNodeReport {
	report := wipeNodeReport{wipeClusterReport: newWipeClusterReport(planOnly, partial, []inventory.PlannedNode{node})}
	report.Kind = "WipeNodeReport"
	report.Command = "katlctl node wipe"
	if planOnly && strings.TrimSpace(report.KubernetesCleanup) == "not-attempted" {
		report.KubernetesCleanup = "planned"
	}
	return report
}

func wipeClusterTargets(plan inventory.Plan, all bool, selected []string) ([]inventory.PlannedNode, bool, error) {
	if !all && len(selected) == 0 {
		return nil, false, fmt.Errorf("--all or at least one --node is required")
	}
	byName := make(map[string]inventory.PlannedNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		byName[node.Name] = node
	}
	if all {
		return append([]inventory.PlannedNode(nil), plan.Nodes...), false, nil
	}
	targets := make([]inventory.PlannedNode, 0, len(selected))
	seen := make(map[string]struct{}, len(selected))
	for _, name := range selected {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, false, fmt.Errorf("--node value is required")
		}
		if _, ok := seen[name]; ok {
			return nil, false, fmt.Errorf("duplicate --node %q", name)
		}
		seen[name] = struct{}{}
		node, ok := byName[name]
		if !ok {
			return nil, false, fmt.Errorf("--node %q is not in the inventory", name)
		}
		targets = append(targets, node)
	}
	return targets, len(targets) != len(plan.Nodes), nil
}

func preflightWipeCluster(ctx context.Context, connector cluster.AgentConnector, report *wipeClusterReport, targets []inventory.PlannedNode) error {
	var failures []string
	for _, node := range targets {
		result := wipeClusterNodeResult{Node: node.Name}
		if strings.TrimSpace(node.Address) == "" {
			result.Result = "refused"
			result.Diagnostics = append(result.Diagnostics, "inventory node address is required")
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		if node.Access.Method != "agent" {
			result.Result = "refused"
			result.Diagnostics = append(result.Diagnostics, fmt.Sprintf("inventory access method %q is not supported", node.Access.Method))
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		conn, err := connector.Connect(ctx, node)
		if err != nil {
			result.Result = "refused"
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(err.Error()))
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		result.Endpoint = conn.Endpoint
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		closeErr := closeAgentConnection(conn)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(err.Error()))
		} else {
			result.Diagnostics = append(result.Diagnostics, wipeClusterStatusDiagnostics(status)...)
		}
		if closeErr != nil {
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(closeErr.Error()))
		}
		if len(result.Diagnostics) > 0 {
			result.Result = "refused"
			failures = append(failures, node.Name)
		}
		report.Nodes = append(report.Nodes, result)
	}
	if len(failures) > 0 {
		report.Refusals = append(report.Refusals, "node-local preflight failed for: "+strings.Join(failures, ","))
		return fmt.Errorf("node-local preflight failed for: %s", strings.Join(failures, ","))
	}
	report.Nodes = nil
	return nil
}

func wipeClusterStatusDiagnostics(status *agentapi.NodeStatus) []string {
	if status == nil {
		return []string{"node status is missing"}
	}
	var diagnostics []string
	if status.GetApiVersion() != operation.APIVersion {
		diagnostics = append(diagnostics, fmt.Sprintf("node reports API version %q", status.GetApiVersion()))
	}
	if !containsString(status.GetSupportedOperationKinds(), wipeClusterOperationKind) {
		diagnostics = append(diagnostics, wipeClusterOperationKind+" operation is not supported")
	}
	if status.GetOperationLockHeld() {
		diagnostics = append(diagnostics, "operation lock is held by "+strings.Join(status.GetActiveOperationIds(), ","))
	}
	if strings.TrimSpace(status.GetMachineId()) == "" {
		diagnostics = append(diagnostics, "node did not report a machine identity")
	}
	return diagnostics
}

func plannedWipeClusterOperations(targets []inventory.PlannedNode) []wipeClusterNodeLocalOperation {
	operations := make([]wipeClusterNodeLocalOperation, 0, len(targets))
	for _, node := range targets {
		operations = append(operations, wipeClusterOperation(node))
	}
	return operations
}

func wipeClusterOperation(node inventory.PlannedNode) wipeClusterNodeLocalOperation {
	return wipeClusterNodeLocalOperation{
		Node:                   node.Name,
		OperationKind:          wipeClusterOperationKind,
		ResetScope:             "cluster",
		DiscardClusterIdentity: true,
		WipeSurfaces: []string{
			"katlos-boot-artifacts",
			"disk-boot-path",
		},
	}
}

func wipeNodeOperation(node inventory.PlannedNode) wipeClusterNodeLocalOperation {
	operation := wipeClusterOperation(node)
	operation.ResetScope = "node"
	return operation
}

func submitWipeCluster(ctx context.Context, connector cluster.AgentConnector, report *wipeClusterReport, targets []inventory.PlannedNode, clientRequestID string, timeout string, noWait bool, waitTimeout time.Duration, stderr io.Writer) error {
	var failures []string
	for _, node := range targets {
		result := wipeClusterNodeResult{Node: node.Name}
		conn, err := connector.Connect(ctx, node)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(err.Error()))
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		result.Endpoint = conn.Endpoint
		operationSpec := wipeClusterOperation(node)
		if report.Kind == "WipeNodeReport" {
			operationSpec = wipeNodeOperation(node)
		}
		actor := strings.TrimSpace(report.Command)
		if actor == "" {
			actor = "katlctl cluster wipe"
		}
		accepted, err := conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion:       operation.APIVersion,
			Kind:             "SubmitOperationRequest",
			ClientRequestId:  clientRequestID,
			OperationKind:    operationSpec.OperationKind,
			Actor:            actor,
			OperationTimeout: timeout,
			DestructiveReset: &agentapi.DestructiveResetOperationRequest{
				InventoryNodeName:      operationSpec.Node,
				ResetScope:             operationSpec.ResetScope,
				TargetGenerationId:     operationSpec.TargetGenerationID,
				DiscardClusterIdentity: operationSpec.DiscardClusterIdentity,
				WipeSurfaces:           operationSpec.WipeSurfaces,
			},
		})
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(err.Error()))
			failures = append(failures, node.Name)
		} else if accepted == nil {
			result.Diagnostics = append(result.Diagnostics, "agent did not return operation acceptance")
			failures = append(failures, node.Name)
		} else {
			result.Accepted = true
			if noWait {
				result.OperationID = accepted.GetOperationId()
			}
			result.OperationKind = accepted.GetOperationKind()
			if status := accepted.GetInitialStatus(); status != nil {
				result.Phase = status.GetPhase()
				result.Terminal = status.GetTerminal()
				result.Result = status.GetResult()
				if strings.TrimSpace(status.GetFailureReason()) != "" {
					result.Diagnostics = append(result.Diagnostics, inventory.Redact(status.GetFailureReason()))
				}
			}
			if !noWait {
				waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
				status := accepted.GetInitialStatus()
				if status == nil {
					status, err = conn.Client.GetOperation(waitCtx, &agentapi.GetOperationRequest{OperationId: accepted.GetOperationId(), IncludeDiagnostics: "normal"})
				}
				if err == nil && status != nil && !status.GetTerminal() {
					status, err = followOperation(waitCtx, conn.Client, &agentapi.GetOperationRequest{OperationId: accepted.GetOperationId(), IncludeDiagnostics: "normal"}, status, stderr)
				}
				cancel()
				if err != nil {
					result.Diagnostics = append(result.Diagnostics, inventory.Redact(err.Error()))
					failures = append(failures, node.Name)
				} else if status == nil {
					result.Diagnostics = append(result.Diagnostics, "agent returned an empty operation status")
					failures = append(failures, node.Name)
				} else {
					result.Phase = status.GetPhase()
					result.Terminal = status.GetTerminal()
					result.Result = status.GetResult()
					if resultErr := operationResultError(status); resultErr != nil {
						result.Diagnostics = append(result.Diagnostics, inventory.Redact(resultErr.Error()))
						failures = append(failures, node.Name)
					}
				}
			}
		}
		closeErr := closeAgentConnection(conn)
		if closeErr != nil {
			result.Diagnostics = append(result.Diagnostics, inventory.Redact(closeErr.Error()))
			failures = append(failures, node.Name)
		}
		report.Nodes = append(report.Nodes, result)
	}
	if len(failures) > 0 {
		return fmt.Errorf("destructive reset submission failed for: %s", strings.Join(failures, ","))
	}
	return nil
}

func closeAgentConnection(conn cluster.AgentConnection) error {
	if conn.Close == nil {
		return nil
	}
	return conn.Close()
}

func printWipeClusterReport(stdout io.Writer, report wipeClusterReport) error {
	if report.Output == "text" {
		return printWipeText(stdout, report)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cluster wipe report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func printWipeNodeReport(stdout io.Writer, report wipeNodeReport) error {
	if report.Output == "text" {
		return printWipeText(stdout, report.wipeClusterReport)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wipe node report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func printWipeText(stdout io.Writer, report wipeClusterReport) error {
	action := "wipe"
	if report.Plan {
		action = "wipe plan"
	}
	fmt.Fprintf(stdout, "%s:\n", action)
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tROLE\tADDRESS\tRESULT")
	results := make(map[string]string, len(report.Nodes))
	for _, node := range report.Nodes {
		result := node.Result
		if result == "" && node.Accepted {
			result = "accepted"
		}
		if result == "" {
			result = "planned"
		}
		results[node.Node] = result
	}
	for _, target := range report.Targets {
		result := results[target.Name]
		if result == "" {
			result = "planned"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", target.Name, target.SystemRole, target.Address, result)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, refusal := range report.Refusals {
		fmt.Fprintf(stdout, "Refused: %s\n", refusal)
	}
	return nil
}

type wipeNodeCleanupResult struct {
	Status      string
	Diagnostics []string
}

func cleanupWipeNodeKubernetes(ctx context.Context, kubeconfigPath string, node inventory.PlannedNode, timeout string) wipeNodeCleanupResult {
	result := wipeNodeCleanupResult{Status: "succeeded"}
	diagnostic := func(format string, args ...any) {
		result.Diagnostics = append(result.Diagnostics, inventory.Redact(fmt.Sprintf(format, args...)))
	}
	run := func(name string, args ...string) bool {
		argv := append([]string{"kubectl", "--kubeconfig", kubeconfigPath}, args...)
		output, err := operatorKubectlRunner.Run(ctx, argv)
		if err != nil {
			diagnostic("%s failed: %v", name, err)
			return false
		}
		if output.ExitStatus != 0 {
			diagnostic("%s failed: %s", name, strings.TrimSpace(output.Stderr))
			return false
		}
		return true
	}

	_ = run("cordon node", "cordon", node.Name)
	drainTimeout := strings.TrimSpace(timeout)
	if drainTimeout == "" {
		drainTimeout = "10m"
	}
	_ = run("drain node", "drain", node.Name, "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout="+drainTimeout)
	if !run("delete node", "delete", "node", node.Name, "--ignore-not-found=true") {
		result.Status = "recovery-required"
		return result
	}
	if len(result.Diagnostics) > 0 {
		result.Status = "best-effort"
	}
	return result
}

type execWipeNodeKubectlRunner struct{}

func (execWipeNodeKubectlRunner) Run(ctx context.Context, argv []string) (readiness.CommandResult, error) {
	if len(argv) == 0 {
		return readiness.CommandResult{}, fmt.Errorf("argv is required")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitStatus := int32(0)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitStatus = int32(exitErr.ExitCode())
		} else {
			return readiness.CommandResult{}, err
		}
	}
	return readiness.CommandResult{
		ExitStatus: exitStatus,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
	}, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func newConfigPathCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the workstation context file path",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigPath(stdout, stderr)
		},
	}
}

func runConfigPath(stdout, stderr io.Writer) error {
	_ = stderr
	path, err := workstationConfigPath()
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, path)
	return nil
}

func workstationConfigPath() (string, error) {
	return workstation.ConfigPath()
}

type configTopologyOptions struct {
	configPath  string
	contextName string
	output      string
}

func newConfigTopologyCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := configTopologyOptions{output: "text"}
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Print the resolved workstation topology",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigTopology(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "context name")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "output format: text or json")
	return cmd
}

func runConfigTopology(opts configTopologyOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" && opts.output != "text" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	resolved, err := workstation.ResolveTopology(workstation.ResolveRequest{
		ConfigPath:  opts.configPath,
		ContextName: opts.contextName,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no saved katlctl contexts; create one with 'katlctl context save --config cluster.yaml'")
		}
		return err
	}
	if opts.output == "text" {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "Context:\t%s\nCluster:\t%s\n", resolved.ContextName, resolved.ClusterName)
		fmt.Fprintln(w, "NODE\tROLE\tENDPOINT")
		for _, node := range resolved.Nodes {
			fmt.Fprintf(w, "%s\t%s\t%s\n", node.Name, node.SystemRole, node.ManagementEndpoint)
		}
		return w.Flush()
	}
	data, err := json.MarshalIndent(resolved, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal topology: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

type configBundleOptions struct {
	sourcePath          string
	outputPath          string
	katlosImageURL      string
	katlosImageMetadata string
}

type katlosImageArtifactMetadata struct {
	APIVersion       string `json:"apiVersion"`
	Kind             string `json:"kind"`
	ImageRole        string `json:"imageRole"`
	Format           string `json:"format"`
	Version          string `json:"version"`
	Architecture     string `json:"architecture"`
	RuntimeInterface string `json:"runtimeInterface"`
	SizeBytes        int64  `json:"sizeBytes"`
	SHA256           string `json:"sha256"`
}

type nodeConfigInputOptions struct {
	configPath     string
	nodeName       string
	sourceID       string
	desiredVersion string
}

type configValidationNode struct {
	Name         string `json:"name"`
	ControlPlane bool   `json:"controlPlane,omitempty"`
}

type configValidationReport struct {
	APIVersion  string                 `json:"apiVersion"`
	Kind        string                 `json:"kind"`
	Source      string                 `json:"source"`
	ClusterName string                 `json:"clusterName"`
	Nodes       []configValidationNode `json:"nodes"`
}

func newConfigValidateCommand(stdout, stderr io.Writer) *cobra.Command {
	output := "text"
	cmd := &cobra.Command{
		Use:   "validate SOURCE",
		Short: "Validate and resolve a cluster config without writing a bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runConfigValidate(args[0], output, stdout, stderr)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", output, "output format: text or json")
	return cmd
}

func runConfigValidate(sourcePath, output string, stdout, stderr io.Writer) error {
	_ = stderr
	if output != "text" && output != "json" {
		return fmt.Errorf("--output = %q, want text or json", output)
	}
	_, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
		SourcePath:     sourcePath,
		KatlctlVersion: version,
		KatlctlCommit:  commit,
		CreatedBy:      configBundleCreator,
	})
	if err != nil {
		return err
	}
	nodes := make([]configValidationNode, 0, len(result.Manifest.Nodes))
	for _, node := range result.Manifest.Nodes {
		nodes = append(nodes, configValidationNode{Name: node.Name, ControlPlane: node.SystemRole == string(inventory.RoleControlPlane)})
	}
	report := configValidationReport{
		APIVersion:  configbundle.APIVersion,
		Kind:        "ClusterConfigValidation",
		Source:      sourcePath,
		ClusterName: result.Manifest.ClusterName,
		Nodes:       nodes,
	}
	if output == "text" {
		_, err := fmt.Fprintf(stdout, "%s is valid for cluster %s (%d node(s))\n", sourcePath, report.ClusterName, len(report.Nodes))
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config validation report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func newConfigSchemaCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the ClusterConfig JSON Schema",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = stderr
			data, err := configbundle.SourceSchema()
			if err != nil {
				return err
			}
			_, err = stdout.Write(data)
			return err
		},
	}
}

type configBundleReport struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Output      string `json:"output"`
	ArchiveSize int64  `json:"archiveSizeBytes"`
}

func newConfigBundleCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := configBundleOptions{}
	cmd := &cobra.Command{
		Use:   "bundle SOURCE --output PATH",
		Short: "Compile a cluster config into a Katl config bundle",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("exactly one source config path is required")
			}
			opts.sourcePath = args[0]
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigBundle(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.outputPath, "output", "", "bundle output path")
	cmd.Flags().StringVar(&opts.katlosImageURL, "katlos-image-url", "", "published KatlOS install image URL for PXE")
	cmd.Flags().StringVar(&opts.katlosImageMetadata, "katlos-image-metadata", "", "published KatlOS install image metadata JSON for PXE")
	return cmd
}

func runConfigBundle(opts configBundleOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if strings.TrimSpace(opts.outputPath) == "" {
		return fmt.Errorf("--output is required")
	}
	katlosImage, err := loadKatlosImagePlanningInput(opts.katlosImageURL, opts.katlosImageMetadata)
	if err != nil {
		return err
	}
	result, err := configbundle.WriteArchive(opts.outputPath, configbundle.BuildRequest{
		SourcePath:     opts.sourcePath,
		KatlctlVersion: version,
		KatlctlCommit:  commit,
		CreatedBy:      configBundleCreator,
		Planning:       configbundle.PlanningInputs{KatlosImage: katlosImage},
	})
	if err != nil {
		return err
	}
	report := configBundleReport{
		APIVersion:  configbundle.APIVersion,
		Kind:        "ConfigBundleReport",
		Output:      opts.outputPath,
		ArchiveSize: result.ArchiveSize,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config bundle report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func loadKatlosImagePlanningInput(imageURL, metadataPath string) (manifest.KatlosImage, error) {
	imageURL = strings.TrimSpace(imageURL)
	metadataPath = strings.TrimSpace(metadataPath)
	if (imageURL == "") != (metadataPath == "") {
		return manifest.KatlosImage{}, fmt.Errorf("--katlos-image-url and --katlos-image-metadata must be used together")
	}
	if imageURL == "" {
		return manifest.KatlosImage{}, nil
	}
	parsedURL, err := url.Parse(imageURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return manifest.KatlosImage{}, fmt.Errorf("--katlos-image-url must be an http or https URL")
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return manifest.KatlosImage{}, fmt.Errorf("read KatlOS image metadata: %w", err)
	}
	var metadata katlosImageArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return manifest.KatlosImage{}, fmt.Errorf("decode KatlOS image metadata: %w", err)
	}
	if metadata.APIVersion != "katl.dev/v1alpha1" || metadata.Kind != "KatlOSImageArtifact" || metadata.ImageRole != "install" || metadata.Format != "squashfs" {
		return manifest.KatlosImage{}, fmt.Errorf("KatlOS image metadata must describe a katl.dev/v1alpha1 install SquashFS")
	}
	if metadata.SizeBytes <= 0 {
		return manifest.KatlosImage{}, fmt.Errorf("KatlOS image metadata sizeBytes must be positive")
	}
	return manifest.KatlosImage{
		URL:              imageURL,
		SHA256:           metadata.SHA256,
		SizeBytes:        uint64(metadata.SizeBytes),
		Version:          metadata.Version,
		Architecture:     metadata.Architecture,
		RuntimeInterface: metadata.RuntimeInterface,
		Role:             metadata.ImageRole,
	}, nil
}

func newConfigRenderNodeCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := nodeConfigInputOptions{}
	mode := generation.ApplyModeAuto
	cmd := &cobra.Command{
		Use:   "render-node",
		Short: "Render one node's runtime configuration from cluster intent",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = stderr
			data, err := renderNodeConfig(opts, mode)
			if err != nil {
				return err
			}
			_, err = stdout.Write(data)
			return err
		},
	}
	addNodeConfigInputFlags(cmd, &opts)
	cmd.Flags().StringVar(&mode, "mode", mode, "apply mode: auto, live, or next-boot")
	return cmd
}

func addNodeConfigInputFlags(cmd *cobra.Command, opts *nodeConfigInputOptions) {
	cmd.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node to select from cluster intent")
	cmd.Flags().StringVar(&opts.sourceID, "source-id", "", "runtime configuration source id; defaults to the cluster name")
	cmd.Flags().StringVar(&opts.desiredVersion, "desired-version", "", "monotonic unsigned runtime configuration version")
}

func renderNodeConfig(opts nodeConfigInputOptions, mode string) ([]byte, error) {
	if strings.TrimSpace(opts.nodeName) == "" {
		return nil, fmt.Errorf("--node is required")
	}
	if strings.TrimSpace(opts.desiredVersion) == "" {
		return nil, fmt.Errorf("--desired-version is required")
	}
	readOptions := configbundle.ReadOptions{
		NodeName:                opts.nodeName,
		AllowMissingKatlosImage: true,
	}
	config, err := loadKatlConfig(opts.configPath, configBundleCreator, configbundle.PlanningInputs{})
	if err != nil {
		return nil, err
	}
	selected, err := configbundle.ReadSelectedNode(bytes.NewReader(config.Archive), readOptions)
	if err != nil {
		return nil, err
	}
	sourceID := strings.TrimSpace(opts.sourceID)
	if sourceID == "" {
		sourceID = selected.BundleManifest.ClusterName
	}
	return configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{
		NodeName:       selected.Node.Name,
		Manifest:       selected.InstallManifest,
		KubeadmConfigs: selected.KubeadmConfigs,
		SourceID:       sourceID,
		DesiredVersion: opts.desiredVersion,
		ApplyMode:      mode,
	})
}

type configApplyOptions struct {
	endpoint            string
	workstationConfig   string
	contextName         string
	nodeConfig          nodeConfigInputOptions
	mode                string
	candidateGeneration string
	clientRequestID     string
	actor               string
	plan                bool
	noWait              bool
	waitTimeout         time.Duration
	output              string
}

var configApplyNow = func() time.Time { return time.Now().UTC() }

func newConfigApplyCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := configApplyOptions{mode: generation.ApplyModeAuto, actor: "katlctl node apply", output: "text"}
	cmd := &cobra.Command{
		Use:   "apply [NODE]",
		Short: "Validate or apply node configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				if err := selectHostNode(&opts.nodeConfig.nodeName, args); err != nil {
					return err
				}
			}
			return runConfigApply(ctx, opts, stdout, stderr)
		},
	}
	addConfigApplyFlags(cmd, &opts)

	validateOpts := configApplyOptions{mode: generation.ApplyModeAuto, actor: "katlctl node apply validate", plan: true, output: "json"}
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate node configuration without accepting an operation",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigApply(ctx, validateOpts, stdout, stderr)
		},
	}
	addConfigApplyFlags(validateCmd, &validateOpts)
	for _, name := range []string{"plan", "no-wait", "timeout", "client-request-id"} {
		if flag := validateCmd.Flags().Lookup(name); flag != nil {
			flag.Hidden = true
		}
	}
	validateCmd.Hidden = true
	cmd.AddCommand(validateCmd)
	statusCmd := newConfigApplyStatusCommand(ctx, stdout, stderr)
	statusCmd.Hidden = true
	cmd.AddCommand(statusCmd)
	return cmd
}

func addConfigApplyFlags(cmd *cobra.Command, opts *configApplyOptions) {
	if opts.waitTimeout == 0 {
		opts.waitTimeout = 30 * time.Minute
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "node address override: IP, hostname, host:port, or tcp:// URL")
	cmd.Flags().StringVar(&opts.workstationConfig, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	addNodeConfigInputFlags(cmd, &opts.nodeConfig)
	cmd.Flags().StringVar(&opts.mode, "mode", opts.mode, "apply mode: auto, live, or next-boot")
	cmd.Flags().StringVar(&opts.candidateGeneration, "candidate-generation", "", "candidate generation id")
	for _, name := range []string{"desired-version", "candidate-generation"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			flag.Hidden = true
		}
	}
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.actor, "actor", opts.actor, "operation actor")
	cmd.Flags().Lookup("actor").Hidden = true
	cmd.Flags().Lookup("source-id").Hidden = true
	cmd.Flags().BoolVar(&opts.plan, "plan", opts.plan, "validate and plan without accepting an operation")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the node accepts the operation")
	cmd.Flags().DurationVar(&opts.waitTimeout, "timeout", opts.waitTimeout, "overall operation wait timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "output format: text or json")
}

func runConfigApply(ctx context.Context, opts configApplyOptions, stdout, stderr io.Writer) error {
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.nodeConfig.configPath) == "" {
		return fmt.Errorf("--config is required")
	}
	targetConfig := opts.nodeConfig.configPath
	if isRenderedNodeConfig(targetConfig) {
		targetConfig = ""
	}
	target, err := resolveManagementTarget(managementTargetOptions{
		clusterConfigPath: targetConfig, configPath: opts.workstationConfig, contextName: opts.contextName,
		nodeName: opts.nodeConfig.nodeName, endpoint: opts.endpoint,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.nodeConfig.nodeName) == "" {
		opts.nodeConfig.nodeName = target.nodeName
	}
	if strings.TrimSpace(opts.candidateGeneration) == "" {
		opts.candidateGeneration = "config-" + strconv.FormatInt(configApplyNow().UnixNano(), 10)
	}
	configYAML, rendered, err := nodeConfigYAML(opts.nodeConfig, opts.mode)
	if err != nil {
		return err
	}
	if !rendered {
		if strings.TrimSpace(opts.nodeConfig.sourceID) != "" || strings.TrimSpace(opts.nodeConfig.desiredVersion) != "" {
			return fmt.Errorf("--source-id and --desired-version cannot be used with a pre-rendered NodeConfigurationChange")
		}
	}
	conn, err := dialKatlcAgent(ctx, target.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	if opts.plan {
		result, err := conn.Client.ValidateConfig(ctx, &agentapi.ValidateConfigRequest{
			ApiVersion:            operation.APIVersion,
			Kind:                  "ValidateConfigRequest",
			ClientRequestId:       requestID,
			Actor:                 opts.actor,
			ApplyMode:             opts.mode,
			CandidateGenerationId: opts.candidateGeneration,
			NodeName:              opts.nodeConfig.nodeName,
			ConfigYaml:            string(configYAML),
		})
		if err != nil {
			return err
		}
		if opts.output == "text" {
			if !result.Accepted {
				return fmt.Errorf("configuration for %s was rejected: %s", opts.nodeConfig.nodeName, firstNonEmpty(result.FailureReason, strings.Join(result.Diagnostics, "; ")))
			}
			if result.GetNoChanges() {
				fmt.Fprintf(stdout, "%s configuration already matches\n", opts.nodeConfig.nodeName)
				return nil
			}
			fmt.Fprintf(stdout, "%s configuration is valid; apply mode %s\n", opts.nodeConfig.nodeName, result.AcceptedApplyMode)
			for _, diagnostic := range result.Diagnostics {
				fmt.Fprintf(stdout, "- %s\n", diagnostic)
			}
			return nil
		}
		publicResult := proto.Clone(result).(*agentapi.ConfigValidationResult)
		publicResult.RequestDigest = ""
		data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(publicResult)
		if err != nil {
			return fmt.Errorf("marshal validation result: %w", err)
		}
		_, err = stdout.Write(append(data, '\n'))
		return err
	}
	requestedMode := strings.TrimSpace(opts.mode)
	req := &agentapi.GenerationApplyRequest{
		ApiVersion:            operation.APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       requestID,
		Actor:                 opts.actor,
		CandidateGenerationId: opts.candidateGeneration,
		NodeName:              opts.nodeConfig.nodeName,
		ConfigYaml:            string(configYAML),
	}
	var accepted *agentapi.OperationAccepted
	switch requestedMode {
	case generation.ApplyModeLive:
		accepted, err = conn.Client.ApplyGeneration(ctx, req)
	case generation.ApplyModeNextBoot:
		accepted, err = conn.Client.StageGeneration(ctx, req)
	case generation.ApplyModeAuto:
		result, err := conn.Client.ValidateConfig(ctx, &agentapi.ValidateConfigRequest{
			ApiVersion:            operation.APIVersion,
			Kind:                  "ValidateConfigRequest",
			ClientRequestId:       requestID,
			Actor:                 opts.actor,
			ApplyMode:             requestedMode,
			CandidateGenerationId: opts.candidateGeneration,
			NodeName:              opts.nodeConfig.nodeName,
			ConfigYaml:            string(configYAML),
		})
		if err != nil {
			return err
		}
		if !result.Accepted {
			if strings.TrimSpace(result.FailureReason) != "" {
				return fmt.Errorf("config validation rejected: %s", result.FailureReason)
			}
			return fmt.Errorf("config validation rejected: %s", strings.Join(result.Diagnostics, "; "))
		}
		if result.GetNoChanges() {
			if opts.output == "json" {
				publicResult := proto.Clone(result).(*agentapi.ConfigValidationResult)
				publicResult.RequestDigest = ""
				data, marshalErr := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(publicResult)
				if marshalErr != nil {
					return fmt.Errorf("marshal validation result: %w", marshalErr)
				}
				_, err = stdout.Write(append(data, '\n'))
				return err
			}
			fmt.Fprintf(stdout, "%s configuration already matches\n", opts.nodeConfig.nodeName)
			return nil
		}
		operationKind, err := configApplyOperationKind(result.AcceptedApplyMode)
		if err != nil {
			return err
		}
		accepted, err = conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion:      operation.APIVersion,
			Kind:            "SubmitOperationRequest",
			ClientRequestId: requestID,
			OperationKind:   operationKind,
			Actor:           opts.actor,
			ConfigApply: &agentapi.ConfigApplyOperationRequest{
				CandidateGenerationId: opts.candidateGeneration,
				ApplyMode:             requestedMode,
				NodeName:              opts.nodeConfig.nodeName,
				ConfigYaml:            string(configYAML),
			},
		})
	default:
		return fmt.Errorf("--mode must be %q, %q, or %q", generation.ApplyModeAuto, generation.ApplyModeLive, generation.ApplyModeNextBoot)
	}
	if err != nil {
		return err
	}
	if opts.noWait {
		return writeOperationAccepted(stdout, accepted)
	}
	terminal, err := waitAcceptedOperationStatus(ctx, conn.Client, accepted, opts.waitTimeout, stderr)
	if opts.output == "json" {
		if writeErr := writeMutationOperationStatus(stdout, terminal); writeErr != nil {
			return writeErr
		}
	} else if terminal != nil {
		fmt.Fprintf(stdout, "%s configuration result: %s (phase %s)\n", opts.nodeConfig.nodeName, terminal.GetResult(), terminal.GetPhase())
	}
	if err != nil {
		return err
	}
	return operationResultError(terminal)
}

func isRenderedNodeConfig(path string) bool {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	var identity struct {
		Kind string `yaml:"kind"`
	}
	return yaml.Unmarshal(data, &identity) == nil && identity.Kind == configapply.NodeConfigurationChangeKind
}

func nodeConfigYAML(opts nodeConfigInputOptions, mode string) ([]byte, bool, error) {
	data, err := os.ReadFile(strings.TrimSpace(opts.configPath))
	if err != nil {
		return nil, false, fmt.Errorf("read --config %s: %w", opts.configPath, err)
	}
	var identity struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &identity); err == nil && identity.Kind == configapply.NodeConfigurationChangeKind {
		return data, false, nil
	}
	if strings.TrimSpace(opts.desiredVersion) == "" {
		opts.desiredVersion = strconv.FormatInt(configApplyNow().UnixNano(), 10)
	}
	rendered, err := renderNodeConfig(opts, mode)
	return rendered, true, err
}

func configApplyOperationKind(acceptedMode string) (string, error) {
	switch strings.TrimSpace(acceptedMode) {
	case generation.ApplyModeLive:
		return "generation-apply", nil
	case generation.ApplyModeNextBoot:
		return "generation-stage", nil
	default:
		return "", fmt.Errorf("config validation accepted unsupported apply mode %q", acceptedMode)
	}
}

type configApplyStatusOptions struct {
	root               string
	endpoint           string
	workstationConfig  string
	contextName        string
	nodeName           string
	generationID       string
	activeGeneration   string
	nextBootGeneration string
	output             string
}

func newConfigApplyStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := configApplyStatusOptions{root: "/", output: "json"}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report config apply state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigApplyStatus(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.root, "root", "/", "runtime root to inspect")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "katlc agent TCP endpoint host:port")
	cmd.Flags().StringVar(&opts.workstationConfig, "context-file", "", "workstation context file path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name in the selected context")
	cmd.Flags().StringVar(&opts.generationID, "generation", "", "generation id to query from katlc agent")
	cmd.Flags().StringVar(&opts.activeGeneration, "active-generation", "", "active generation id")
	cmd.Flags().StringVar(&opts.nextBootGeneration, "next-boot-generation", "", "next boot generation id")
	cmd.Flags().StringVar(&opts.output, "output", "json", "output format: json")
	return cmd
}

func runConfigApplyStatus(ctx context.Context, opts configApplyStatusOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	remote := strings.TrimSpace(opts.endpoint) != "" || strings.TrimSpace(opts.workstationConfig) != "" || strings.TrimSpace(opts.contextName) != "" || strings.TrimSpace(opts.nodeName) != ""
	if remote {
		target, err := resolveManagementTarget(managementTargetOptions{
			configPath: opts.workstationConfig, contextName: opts.contextName, nodeName: opts.nodeName,
			endpoint: opts.endpoint,
		})
		if err != nil {
			return err
		}
		conn, err := dialKatlcAgent(ctx, target.endpoint)
		if err != nil {
			return err
		}
		defer conn.Close()
		generationID := strings.TrimSpace(opts.generationID)
		if generationID == "" {
			status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if err != nil {
				return err
			}
			generationID = strings.TrimSpace(status.GetCurrentGenerationId())
			if generationID == "" {
				return fmt.Errorf("node %q did not report a current generation", target.nodeName)
			}
		}
		generation, err := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{
			GenerationId:       generationID,
			IncludeConfigApply: true,
		})
		if err != nil {
			return err
		}
		data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(generation)
		if err != nil {
			return fmt.Errorf("marshal generation status: %w", err)
		}
		_, err = stdout.Write(append(data, '\n'))
		return err
	}
	report, err := loadConfigApplyReport(opts.root, opts.activeGeneration, opts.nextBootGeneration)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config apply status: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

type configApplyReport struct {
	APIVersion           string                      `json:"apiVersion"`
	Kind                 string                      `json:"kind"`
	ActiveGenerationID   string                      `json:"activeGenerationID,omitempty"`
	NextBootGenerationID string                      `json:"nextBootGenerationID,omitempty"`
	Active               *configApplyGenerationState `json:"active,omitempty"`
	NextBoot             *configApplyGenerationState `json:"nextBoot,omitempty"`
}

type configApplyGenerationState struct {
	GenerationID          string                               `json:"generationID"`
	PreviousGenerationID  string                               `json:"previousGenerationID,omitempty"`
	RequestedApplyMode    string                               `json:"requestedApplyMode,omitempty"`
	AcceptedApplyMode     string                               `json:"acceptedApplyMode,omitempty"`
	ChangedDomains        []string                             `json:"changedDomains,omitempty"`
	Phase                 string                               `json:"phase,omitempty"`
	HealthState           string                               `json:"healthState,omitempty"`
	DomainActions         []generation.ConfigApplyDomainAction `json:"domainActions,omitempty"`
	DiagnosticArtifacts   []generation.DiagnosticArtifact      `json:"diagnosticArtifacts,omitempty"`
	RollbackTarget        string                               `json:"rollbackTargetGenerationID,omitempty"`
	RollbackResult        string                               `json:"rollbackResult,omitempty"`
	KubeadmActionRequired generation.KubeadmActionRequired     `json:"kubeadmActionRequired"`
	FailureReason         string                               `json:"failureReason,omitempty"`
}

func loadConfigApplyReport(root, activeGeneration, nextBootGeneration string) (configApplyReport, error) {
	activeGeneration = strings.TrimSpace(activeGeneration)
	nextBootGeneration = strings.TrimSpace(nextBootGeneration)
	if activeGeneration == "" && nextBootGeneration == "" {
		return configApplyReport{}, fmt.Errorf("--active-generation or --next-boot-generation is required")
	}
	report := configApplyReport{
		APIVersion:           generation.APIVersion,
		Kind:                 "ConfigApplyReport",
		ActiveGenerationID:   activeGeneration,
		NextBootGenerationID: nextBootGeneration,
	}
	if activeGeneration != "" {
		state, err := loadConfigApplyGenerationState(root, activeGeneration)
		if err != nil {
			return configApplyReport{}, fmt.Errorf("active generation %s: %w", activeGeneration, err)
		}
		report.Active = &state
	}
	if nextBootGeneration != "" {
		state, err := loadConfigApplyGenerationState(root, nextBootGeneration)
		if err != nil {
			return configApplyReport{}, fmt.Errorf("next-boot generation %s: %w", nextBootGeneration, err)
		}
		report.NextBoot = &state
	}
	return report, nil
}

func loadConfigApplyGenerationState(root, generationID string) (configApplyGenerationState, error) {
	metadataPath, err := generation.MetadataPath(root, generationID)
	if err != nil {
		return configApplyGenerationState{}, err
	}
	record, err := generation.ReadRecord(metadataPath)
	if err != nil {
		return configApplyGenerationState{}, err
	}
	state := configApplyGenerationState{
		GenerationID: generationID,
		HealthState:  record.HealthState,
	}
	if record.ConfigApply != nil {
		state.PreviousGenerationID = record.ConfigApply.PreviousGeneration
		state.RequestedApplyMode = record.ConfigApply.RequestedApplyMode
		state.AcceptedApplyMode = record.ConfigApply.AcceptedApplyMode
		state.ChangedDomains = append([]string(nil), record.ConfigApply.ChangedDomains...)
		state.KubeadmActionRequired = redactKubeadm(record.ConfigApply.Kubeadm)
	}
	statusPath, err := generation.ConfigApplyStatusPath(root, generationID)
	if err != nil {
		return configApplyGenerationState{}, err
	}
	status, err := generation.ReadConfigApplyStatus(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return configApplyGenerationState{}, err
	}
	status = redactStatus(status)
	state.PreviousGenerationID = firstNonEmpty(status.PreviousGeneration, state.PreviousGenerationID)
	state.RequestedApplyMode = firstNonEmpty(status.RequestedApplyMode, state.RequestedApplyMode)
	state.AcceptedApplyMode = firstNonEmpty(status.AcceptedApplyMode, state.AcceptedApplyMode)
	if len(status.ChangedDomains) > 0 {
		state.ChangedDomains = append([]string(nil), status.ChangedDomains...)
	}
	state.Phase = status.Phase
	state.HealthState = firstNonEmpty(status.HealthState, state.HealthState)
	state.DomainActions = append([]generation.ConfigApplyDomainAction(nil), status.DomainActions...)
	state.DiagnosticArtifacts = append([]generation.DiagnosticArtifact(nil), status.DiagnosticArtifacts...)
	if status.Rollback != nil {
		state.RollbackTarget = status.Rollback.TargetGenerationID
		state.RollbackResult = status.Rollback.Result
	}
	state.KubeadmActionRequired = redactKubeadm(status.Kubeadm)
	state.FailureReason = generation.RedactConfigApplyMessage(status.FailureReason)
	return state, nil
}

func redactStatus(status generation.ConfigApplyStatus) generation.ConfigApplyStatus {
	status.FailureReason = generation.RedactConfigApplyMessage(status.FailureReason)
	status.Kubeadm = redactKubeadm(status.Kubeadm)
	for i := range status.DomainActions {
		status.DomainActions[i].Diagnostic = generation.RedactConfigApplyMessage(status.DomainActions[i].Diagnostic)
	}
	for i := range status.DiagnosticArtifacts {
		status.DiagnosticArtifacts[i].Path = generation.RedactConfigApplyMessage(status.DiagnosticArtifacts[i].Path)
	}
	if status.Rollback != nil {
		status.Rollback.Reason = generation.RedactConfigApplyMessage(status.Rollback.Reason)
	}
	return status
}

func redactKubeadm(action generation.KubeadmActionRequired) generation.KubeadmActionRequired {
	action.Reason = generation.RedactConfigApplyMessage(action.Reason)
	return action
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type clusterBootstrapOptions struct {
	addresses                              addressOverrides
	configPath                             string
	inventoryPath                          string
	initNode                               string
	joinWorker                             string
	controlPlaneEndpoint                   string
	kubernetesBundle                       string
	kubeconfigOut                          string
	overwriteKubeconfig                    bool
	dryRun                                 bool
	vmtestTranscriptDir                    string
	bootstrapManifestPaths                 stringList
	bootstrapPreWaitValues                 stringList
	bootstrapWaitValues                    stringList
	bootstrapStableEndpoint                string
	bootstrapStableEndpointBeforeManifests bool
	verbose                                bool
}

func newClusterBootstrapCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := clusterBootstrapOptions{kubeconfigOut: "kubeconfig"}
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap Kubernetes from a ClusterConfig or config bundle",
		Long:  "Bootstrap Kubernetes from a ClusterConfig YAML manifest or compiled Katl config bundle. Katl detects the --config format internally.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runClusterBootstrap(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster bootstrap inventory")
	cmd.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().Lookup("inventory").Hidden = true
	cmd.Flags().StringVar(&opts.initNode, "init-node", "", "first control-plane node for kubeadm init")
	cmd.Flags().StringVar(&opts.joinWorker, "join-worker", "", "join one fresh worker to an already initialized cluster without rerunning kubeadm init")
	cmd.Flags().StringVar(&opts.controlPlaneEndpoint, "control-plane-endpoint", "", "control-plane endpoint host:port")
	cmd.Flags().StringVar(&opts.kubernetesBundle, "kubernetes-bundle", "", "Kubernetes OCI bundle image reference; an @sha256 manifest pin is optional")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig-out", opts.kubeconfigOut, "operator kubeconfig output path")
	cmd.Flags().BoolVar(&opts.overwriteKubeconfig, "overwrite-kubeconfig", false, "overwrite different existing kubeconfig")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print the bootstrap plan without running kubeadm")
	cmd.Flags().StringVar(&opts.vmtestTranscriptDir, "vmtest-transcript-dir", "", "directory for per-node vmtest agent transcript artifacts")
	for _, name := range []string{"control-plane-endpoint", "join-worker", "kubernetes-bundle", "vmtest-transcript-dir"} {
		cmd.Flags().Lookup(name).Hidden = true
	}
	cmd.Flags().Var(&opts.addresses, "node-address", "node address override in node=address form")
	cmd.Flags().Var(&opts.bootstrapManifestPaths, "bootstrap-manifest", "ordered Kubernetes manifest file or bundle to apply after API readiness")
	cmd.Flags().Var(&opts.bootstrapPreWaitValues, "bootstrap-pre-wait", "pre-manifest wait: api-ready, nodes-ready, resource-exists[:namespace]:kind/name, condition[:namespace]:kind/name:Condition, rollout-status[:namespace]:kind/name, or pods-ready[:namespace]:selector")
	cmd.Flags().Var(&opts.bootstrapWaitValues, "bootstrap-wait", "post-bootstrap wait: api-ready, nodes-ready, resource-exists[:namespace]:kind/name, condition[:namespace]:kind/name:Condition, rollout-status[:namespace]:kind/name, or pods-ready[:namespace]:selector")
	cmd.Flags().StringVar(&opts.bootstrapStableEndpoint, "bootstrap-stable-endpoint", "", "stable API endpoint host:port to wait for before writing kubeconfig")
	cmd.Flags().BoolVar(&opts.bootstrapStableEndpointBeforeManifests, "bootstrap-stable-endpoint-before-manifests", false, "wait for stable API endpoint before applying bootstrap manifests")
	cmd.Flags().BoolVarP(&opts.verbose, "verbose", "v", false, "show operation IDs and recovery details with bootstrap progress")
	return cmd
}

func runClusterBootstrap(ctx context.Context, opts clusterBootstrapOptions, stdout, stderr io.Writer) error {
	_ = stderr
	inv, err := bootstrapInventory(opts)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.kubernetesBundle) != "" {
		image, err := kubernetesbundle.ParseImageReference(opts.kubernetesBundle)
		if err != nil {
			return fmt.Errorf("--kubernetes-bundle: %w", err)
		}
		inv.KubernetesBundleSource = image.Source
		inv.KubernetesBundleRef = image.Value
	}
	bootstrap, err := parseUserBootstrap(opts.bootstrapManifestPaths.values, opts.bootstrapPreWaitValues.values, opts.bootstrapWaitValues.values, opts.bootstrapStableEndpoint, opts.bootstrapStableEndpointBeforeManifests)
	if err != nil {
		return err
	}
	request := cluster.Request{
		Inventory:            inv,
		InitNode:             opts.initNode,
		AddressOverrides:     opts.addresses.values,
		ControlPlaneEndpoint: opts.controlPlaneEndpoint,
		KubeconfigOut:        opts.kubeconfigOut,
		OverwriteKubeconfig:  opts.overwriteKubeconfig,
		DryRun:               opts.dryRun,
		Bootstrap:            bootstrap,
	}
	if strings.TrimSpace(opts.joinWorker) != "" {
		if strings.TrimSpace(opts.vmtestTranscriptDir) != "" {
			return fmt.Errorf("--join-worker requires katlc agent transport")
		}
		deps := agentBootstrapDependencies()
		deps.Progress = bootstrapProgressWriter(stderr, opts.verbose)
		result, err := runAgentWorkerJoin(ctx, request, strings.TrimSpace(opts.joinWorker), deps)
		printBootstrapResult(stdout, result)
		return err
	}
	var result cluster.Result
	if strings.TrimSpace(opts.vmtestTranscriptDir) != "" {
		result, err = runBootstrap(ctx, request, bootstrapDependencies(opts.vmtestTranscriptDir))
	} else {
		deps := agentBootstrapDependencies()
		deps.Progress = bootstrapProgressWriter(stderr, opts.verbose)
		result, err = runAgentBootstrap(ctx, request, deps)
	}
	printBootstrapResult(stdout, result)
	return err
}

func bootstrapProgressWriter(stderr io.Writer, verbose bool) func(cluster.AgentBootstrapProgress) {
	return func(progress cluster.AgentBootstrapProgress) {
		fmt.Fprint(stderr, "katlctl cluster bootstrap")
		if progress.Node != "" {
			fmt.Fprintf(stderr, " node=%s", progress.Node)
		}
		if progress.Kind != "" {
			fmt.Fprintf(stderr, " operation=%s", progress.Kind)
		}
		fmt.Fprintf(stderr, " phase=%s", progress.Phase)
		if progress.Terminal {
			fmt.Fprintf(stderr, " result=%s", fallbackText(progress.Result, "completed"))
		}
		if verbose && progress.OperationID != "" {
			fmt.Fprintf(stderr, " operation-id=%s", progress.OperationID)
		}
		if verbose && progress.NextAction != "" {
			fmt.Fprintf(stderr, " next=%q", progress.NextAction)
		}
		fmt.Fprintln(stderr)
	}
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func bootstrapInventory(opts clusterBootstrapOptions) (inventory.Inventory, error) {
	configPath := strings.TrimSpace(opts.configPath)
	inventoryPath := strings.TrimSpace(opts.inventoryPath)
	inputs := 0
	for _, value := range []string{configPath, inventoryPath} {
		if value != "" {
			inputs++
		}
	}
	if inputs != 1 {
		return inventory.Inventory{}, fmt.Errorf("exactly one of --config or --inventory is required")
	}
	if inventoryPath != "" {
		return loadInventory(inventoryPath)
	}
	if strings.TrimSpace(opts.controlPlaneEndpoint) != "" {
		return inventory.Inventory{}, fmt.Errorf("--control-plane-endpoint conflicts with the endpoint embedded in the cluster config")
	}
	config, err := loadKatlConfig(configPath, clusterBootstrapCreator, configbundle.PlanningInputs{KubernetesBundle: opts.kubernetesBundle})
	if err != nil {
		return inventory.Inventory{}, err
	}
	if !config.Source && strings.TrimSpace(opts.kubernetesBundle) != "" {
		return inventory.Inventory{}, fmt.Errorf("--kubernetes-bundle conflicts with the selection embedded in the compiled config bundle")
	}
	return config.Bundle.Manifest.Cluster.BootstrapInventory, nil
}

func bootstrapDependencies(vmtestTranscriptDir string) cluster.Dependencies {
	transport := vmtestAgentTransport{TranscriptDir: strings.TrimSpace(vmtestTranscriptDir)}
	return cluster.Dependencies{
		ReadinessChecker: readiness.Checker{Agent: transport},
		NodeRunner:       cluster.TransportRunner{Transport: transport},
		BootstrapRunner:  cluster.KubectlBootstrapRunner{},
	}
}

func agentBootstrapDependencies() cluster.AgentBootstrapDependencies {
	return cluster.AgentBootstrapDependencies{
		Connector:       cluster.TCPAgentConnector{},
		Actor:           "katlctl cluster bootstrap",
		BootstrapRunner: cluster.KubectlBootstrapRunner{},
	}
}

type katlcAgentConnection struct {
	Client agentapi.KatlcAgentClient
	Close  func() error
}

func dialKatlcAgentTCP(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return katlcAgentConnection{}, fmt.Errorf("katlc agent endpoint is required")
	}
	conn, err := grpc.DialContext(ctx, endpoint, katlcAgentDialOptions()...)
	if err != nil {
		return katlcAgentConnection{}, err
	}
	return katlcAgentConnection{
		Client: agentapi.NewKatlcAgentClient(conn),
		Close:  conn.Close,
	}, nil
}

func katlcAgentDialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
}

func printBootstrapResult(stdout io.Writer, result cluster.Result) {
	if len(result.Plan.Nodes) > 0 {
		fmt.Fprintf(stdout, "katlctl cluster bootstrap init-node=%s\n", result.Plan.InitNode)
		for _, override := range result.Plan.AddressOverrides {
			fmt.Fprintf(stdout, "katlctl cluster bootstrap address-override node=%s before=%s after=%s\n", override.Node, override.Before, override.Address)
		}
	}
	for _, phase := range result.Phases {
		if phase.Node != "" {
			fmt.Fprintf(stdout, "phase=%s node=%s status=%s\n", phase.Name, phase.Node, phase.Status)
			continue
		}
		fmt.Fprintf(stdout, "phase=%s status=%s\n", phase.Name, phase.Status)
	}
	if result.NextStep != "" {
		fmt.Fprintf(stdout, "next: %s\n", result.NextStep)
	}
}

func loadInventory(path string) (inventory.Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inventory.Inventory{}, fmt.Errorf("read inventory: %w", err)
	}
	var doc inventoryDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return inventory.Inventory{}, fmt.Errorf("decode inventory: %w", err)
	}
	if strings.TrimSpace(doc.KubernetesBundle) != "" {
		if _, err := kubernetesbundle.ParseImageReference(doc.KubernetesBundle); err != nil {
			return inventory.Inventory{}, fmt.Errorf("decode inventory kubernetesBundle: %w", err)
		}
	}
	return doc.inventory(), nil
}

type inventoryDocument struct {
	ControlPlaneEndpoint string               `yaml:"controlPlaneEndpoint"`
	KubernetesVersion    string               `yaml:"kubernetesVersion"`
	KubernetesBundle     string               `yaml:"kubernetesBundle"`
	Bootstrap            *inventory.Bootstrap `yaml:"bootstrap"`
	Nodes                []nodeDocument       `yaml:"nodes"`
}

type nodeDocument struct {
	Name              string                `yaml:"name"`
	Address           string                `yaml:"address"`
	SystemRole        inventory.SystemRole  `yaml:"systemRole"`
	Access            accessDocument        `yaml:"access"`
	KubeadmConfig     kubeadmConfigDocument `yaml:"kubeadmConfig"`
	KubernetesVersion string                `yaml:"kubernetesVersion"`
}

type accessDocument struct {
	Method        string `yaml:"method"`
	User          string `yaml:"user"`
	CredentialRef string `yaml:"credentialRef"`
}

type kubeadmConfigDocument struct {
	Ref    string                  `yaml:"ref"`
	Path   string                  `yaml:"path"`
	Intent inventory.KubeadmIntent `yaml:"intent"`
}

func (d inventoryDocument) inventory() inventory.Inventory {
	nodes := make([]inventory.Node, 0, len(d.Nodes))
	for _, node := range d.Nodes {
		nodes = append(nodes, inventory.Node{
			Name:       node.Name,
			Address:    node.Address,
			SystemRole: node.SystemRole,
			Access: inventory.Access{
				Method:        node.Access.Method,
				User:          node.Access.User,
				CredentialRef: node.Access.CredentialRef,
			},
			KubeadmConfig: inventory.KubeadmConfig{
				Ref:    node.KubeadmConfig.Ref,
				Path:   node.KubeadmConfig.Path,
				Intent: node.KubeadmConfig.Intent,
			},
			KubernetesVersion: node.KubernetesVersion,
		})
	}
	result := inventory.Inventory{
		ControlPlaneEndpoint: d.ControlPlaneEndpoint,
		KubernetesVersion:    d.KubernetesVersion,
		Bootstrap:            d.Bootstrap,
		Nodes:                nodes,
	}
	if image, err := kubernetesbundle.ParseImageReference(d.KubernetesBundle); err == nil {
		result.KubernetesBundleSource = image.Source
		result.KubernetesBundleRef = image.Value
	}
	return result
}

type addressOverrides struct {
	values map[string]string
}

func (o *addressOverrides) String() string {
	if o == nil || len(o.values) == 0 {
		return ""
	}
	return fmt.Sprint(o.values)
}

func (o *addressOverrides) Set(value string) error {
	name, address, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(address) == "" {
		return fmt.Errorf("--node-address requires node=address")
	}
	if o.values == nil {
		o.values = make(map[string]string)
	}
	o.values[strings.TrimSpace(name)] = strings.TrimSpace(address)
	return nil
}

func (o *addressOverrides) Type() string {
	return "node=address"
}

type stringList struct {
	values []string
}

func (l *stringList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(l.values, ",")
}

func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value is required")
	}
	l.values = append(l.values, value)
	return nil
}

func (l *stringList) Type() string {
	return "string"
}

func parseUserBootstrap(manifestPaths, preWaitValues, waitValues []string, stableEndpoint string, stableEndpointBeforeManifests bool) (cluster.UserBootstrap, error) {
	bootstrap := cluster.UserBootstrap{
		StableEndpoint:                strings.TrimSpace(stableEndpoint),
		StableEndpointBeforeManifests: stableEndpointBeforeManifests,
	}
	for _, path := range manifestPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			return cluster.UserBootstrap{}, fmt.Errorf("bootstrap manifest path is required")
		}
		bootstrap.Manifests = append(bootstrap.Manifests, cluster.BootstrapManifest{Path: path})
	}
	for _, value := range preWaitValues {
		wait, err := parseBootstrapWait(value)
		if err != nil {
			return cluster.UserBootstrap{}, err
		}
		bootstrap.PreWaits = append(bootstrap.PreWaits, wait)
	}
	for _, value := range waitValues {
		wait, err := parseBootstrapWait(value)
		if err != nil {
			return cluster.UserBootstrap{}, err
		}
		bootstrap.Waits = append(bootstrap.Waits, wait)
	}
	return bootstrap, nil
}

func parseBootstrapWait(value string) (cluster.BootstrapWait, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	switch parts[0] {
	case cluster.BootstrapWaitAPIReady, cluster.BootstrapWaitNodesReady:
		if len(parts) != 1 {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait %q does not accept arguments", parts[0])
		}
		return cluster.ValidateBootstrapWait(cluster.BootstrapWait{Kind: parts[0]})
	case cluster.BootstrapWaitResourceExists:
		namespace, name, err := parseWaitResource(parts[1:])
		if err != nil {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait resource-exists: %w", err)
		}
		return cluster.ValidateBootstrapWait(cluster.BootstrapWait{Kind: parts[0], Namespace: namespace, Name: name})
	case cluster.BootstrapWaitRolloutStatus:
		namespace, name, err := parseWaitResource(parts[1:])
		if err != nil {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait rollout-status: %w", err)
		}
		return cluster.ValidateBootstrapWait(cluster.BootstrapWait{Kind: parts[0], Namespace: namespace, Name: name})
	case cluster.BootstrapWaitPodsReady:
		namespace, selector, err := parseWaitSelector(parts[1:])
		if err != nil {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait pods-ready: %w", err)
		}
		return cluster.ValidateBootstrapWait(cluster.BootstrapWait{Kind: parts[0], Namespace: namespace, Selector: selector})
	case cluster.BootstrapWaitCondition:
		if len(parts) != 3 && len(parts) != 4 {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait condition requires condition[:namespace]:kind/name:Condition")
		}
		namespace := ""
		name := parts[1]
		condition := parts[2]
		if len(parts) == 4 {
			namespace = parts[1]
			name = parts[2]
			condition = parts[3]
		}
		if strings.TrimSpace(name) == "" || strings.TrimSpace(condition) == "" {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait condition requires kind/name and Condition")
		}
		if err := validateWaitResourceTarget(name); err != nil {
			return cluster.BootstrapWait{}, fmt.Errorf("bootstrap wait condition: %w", err)
		}
		return cluster.ValidateBootstrapWait(cluster.BootstrapWait{Kind: parts[0], Namespace: namespace, Name: name, Condition: condition})
	default:
		return cluster.BootstrapWait{}, fmt.Errorf("unsupported bootstrap wait %q", value)
	}
}

func parseWaitSelector(parts []string) (string, string, error) {
	switch len(parts) {
	case 1:
		if strings.TrimSpace(parts[0]) == "" {
			return "", "", fmt.Errorf("selector is required")
		}
		return "", parts[0], nil
	case 2:
		if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return "", "", fmt.Errorf("namespace and selector are required")
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("requires pods-ready[:namespace]:selector")
	}
}

func parseWaitResource(parts []string) (string, string, error) {
	switch len(parts) {
	case 1:
		if strings.TrimSpace(parts[0]) == "" {
			return "", "", fmt.Errorf("kind/name is required")
		}
		if err := validateWaitResourceTarget(parts[0]); err != nil {
			return "", "", err
		}
		return "", parts[0], nil
	case 2:
		if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return "", "", fmt.Errorf("namespace and kind/name are required")
		}
		if err := validateWaitResourceTarget(parts[1]); err != nil {
			return "", "", err
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("requires resource-exists[:namespace]:kind/name")
	}
}

func validateWaitResourceTarget(name string) error {
	kind, resource, ok := strings.Cut(strings.TrimSpace(name), "/")
	if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(resource) == "" || strings.Contains(resource, "/") {
		return fmt.Errorf("target must be kind/name")
	}
	return nil
}

type vmtestAgentTransport struct {
	TranscriptDir string
}

func (t vmtestAgentTransport) RunCommand(ctx context.Context, node inventory.PlannedNode, req readiness.CommandRequest) (readiness.CommandResult, error) {
	client, err := t.dialNodeAgent(ctx, node)
	if err != nil {
		return readiness.CommandResult{}, err
	}
	defer client.Close()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := client.RunCommand(ctx, &vmtestpb.RunCommandRequest{
		Argv:            req.Argv,
		StdoutLimit:     req.StdoutLimit,
		StderrLimit:     req.StderrLimit,
		SensitiveOutput: req.SensitiveOutput,
	})
	if err != nil {
		return readiness.CommandResult{}, err
	}
	return readiness.CommandResult{
		ExitStatus:      result.ExitStatus,
		Stdout:          string(result.Stdout),
		Stderr:          string(result.Stderr),
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
	}, nil
}

func (t vmtestAgentTransport) ReadFile(ctx context.Context, node inventory.PlannedNode, req readiness.FileRequest) (readiness.FileResult, error) {
	client, err := t.dialNodeAgent(ctx, node)
	if err != nil {
		return readiness.FileResult{}, err
	}
	defer client.Close()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := client.ReadFile(ctx, &vmtestpb.ReadFileRequest{
		Path:      req.Path,
		MaxBytes:  req.MaxBytes,
		Sensitive: req.Sensitive,
	})
	if err != nil {
		return readiness.FileResult{}, err
	}
	return readiness.FileResult{
		Content:   result.Content,
		Truncated: result.Truncated,
		Redaction: result.Redaction,
	}, nil
}

func (t vmtestAgentTransport) WriteFile(ctx context.Context, node inventory.PlannedNode, req readiness.WriteFileRequest) (readiness.WriteFileResult, error) {
	client, err := t.dialNodeAgent(ctx, node)
	if err != nil {
		return readiness.WriteFileResult{}, err
	}
	defer client.Close()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := client.WriteFile(ctx, &vmtestpb.WriteFileRequest{
		Path:      req.Path,
		Content:   req.Content,
		Mode:      req.Mode,
		Sensitive: req.Sensitive,
	})
	if err != nil {
		return readiness.WriteFileResult{}, err
	}
	return readiness.WriteFileResult{
		SizeBytes: result.SizeBytes,
		Redaction: result.Redaction,
	}, nil
}

func (t vmtestAgentTransport) dialNodeAgent(ctx context.Context, node inventory.PlannedNode) (*vmtest.AgentClient, error) {
	if node.Access.Method != "agent" {
		return nil, fmt.Errorf("node %q access method %q is not supported by vmtest agent transport", node.Name, node.Access.Method)
	}
	cid, port, err := parseVSockCredentialRef(node.Access.CredentialRef)
	if err != nil {
		return nil, fmt.Errorf("node %q agent credentialRef: %w", node.Name, err)
	}
	return dialVMTestAgent(ctx, cid, port, t.transcriptPath(node))
}

func (t vmtestAgentTransport) transcriptPath(node inventory.PlannedNode) string {
	if t.TranscriptDir == "" {
		return ""
	}
	return filepath.Join(t.TranscriptDir, node.Name+".jsonl")
}

func parseVSockCredentialRef(value string) (uint32, uint32, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 3 || parts[0] != "vsock" {
		return 0, 0, fmt.Errorf("expected vsock:<cid>:<port>")
	}
	cid, err := parseUint32(parts[1], "cid")
	if err != nil {
		return 0, 0, err
	}
	port, err := parseUint32(parts[2], "port")
	if err != nil {
		return 0, 0, err
	}
	return cid, port, nil
}

func parseUint32(value, name string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a uint32: %w", name, err)
	}
	if parsed == 0 {
		return 0, fmt.Errorf("%s must be non-zero", name)
	}
	return uint32(parsed), nil
}
