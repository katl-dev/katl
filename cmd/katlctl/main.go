package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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
var wipeNodeKubectlRunner cluster.KubectlCommandRunner = execWipeNodeKubectlRunner{}
var newWipeClusterConnector = func(token string) cluster.AgentConnector {
	return cluster.TCPAgentConnector{AuthToken: strings.TrimSpace(token), AuthTokenForNode: agentTokenForNode}
}

const (
	configBundleCreator      = "katlctl config bundle"
	clusterBootstrapCreator  = "katlctl cluster bootstrap"
	wipeClusterOperationKind = "destructive-reset"
	wipeAcknowledgementText  = "I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity."
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
		Use:           "katlctl",
		Short:         "KatlOS operator client",
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("command is required")
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
	clusterCmd.AddCommand(newClusterEnrollCommand(ctx, stdout, stderr))
	clusterCmd.AddCommand(newClusterBootstrapCommand(ctx, stdout, stderr))
	clusterUpgradeCmd := &cobra.Command{Use: "upgrade", Short: "Cluster upgrade operations"}
	clusterUpgradeCmd.AddCommand(newKubernetesUpgradeCommand(ctx, stdout, stderr))
	clusterCmd.AddCommand(clusterUpgradeCmd)
	clusterCmd.AddCommand(newKubeadmControlPlaneConfigCommand(ctx, stdout, stderr))
	clusterWipeCmd := newWipeClusterCommand(ctx, stdout, stderr, "katlctl cluster wipe")
	clusterWipeCmd.AddCommand(newWipeNodeCommand(ctx, stdout, stderr, "katlctl cluster wipe node"))
	clusterCmd.AddCommand(clusterWipeCmd)
	cmd.AddCommand(clusterCmd)
	kubernetesCmd := &cobra.Command{Use: "kubernetes", Short: "Kubernetes lifecycle operations"}
	kubernetesUpgradeCmd := newKubernetesUpgradeCommand(ctx, stdout, stderr)
	kubernetesUpgradeCmd.Use = "upgrade VERSION"
	kubernetesCmd.AddCommand(kubernetesUpgradeCmd)
	cmd.AddCommand(kubernetesCmd)

	configCmd := &cobra.Command{Use: "config", Short: "Katl configuration operations"}
	configCmd.AddCommand(newConfigInitCommand(stdout, stderr))
	configCmd.AddCommand(newConfigPathCommand(stdout, stderr))
	configCmd.AddCommand(newConfigTopologyCommand(stdout, stderr))
	configCmd.AddCommand(newConfigValidateCommand(stdout, stderr))
	configCmd.AddCommand(newConfigSchemaCommand(stdout, stderr))
	configCmd.AddCommand(newConfigBundleCommand(stdout, stderr))
	configCmd.AddCommand(newConfigRenderNodeCommand(stdout, stderr))
	configCmd.AddCommand(newConfigApplyCommand(ctx, stdout, stderr))
	cmd.AddCommand(configCmd)
	cmd.AddCommand(newInstallCommand(ctx, stdout, stderr))
	cmd.AddCommand(newOperationCommand(ctx, stdout, stderr))

	hostCmd := &cobra.Command{Use: "host", Short: "KatlOS host lifecycle operations"}
	hostCmd.AddCommand(newHostUpgradeCommand(ctx, stdout, stderr))
	cmd.AddCommand(hostCmd)

	wipeCmd := &cobra.Command{
		Use:   "wipe",
		Short: "Compatibility aliases for destructive wipe commands",
	}
	legacyClusterWipeCmd := newWipeClusterCommand(ctx, stdout, stderr, "katlctl cluster wipe")
	legacyClusterWipeCmd.Use = "cluster"
	legacyClusterWipeCmd.Short = "Compatibility alias for katlctl cluster wipe"
	wipeCmd.AddCommand(legacyClusterWipeCmd)
	wipeCmd.AddCommand(newWipeNodeCommand(ctx, stdout, stderr, "katlctl wipe node"))
	cmd.AddCommand(wipeCmd)

	return cmd
}

type hostUpgradeOptions struct {
	version             string
	endpoint            string
	agentTokenFile      string
	workstationConfig   string
	contextName         string
	nodeName            string
	imageURL            string
	imageLocalRef       string
	candidateGeneration string
	clientRequestID     string
	actor               string
	plan                bool
	noWait              bool
	waitTimeout         time.Duration
	output              string
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
	opts := hostUpgradeOptions{actor: "katlctl host upgrade", waitTimeout: 30 * time.Minute, output: "json"}
	cmd := &cobra.Command{
		Use:   "upgrade VERSION",
		Short: "Upgrade one KatlOS host and verify its next boot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.version = args[0]
			}
			return runHostUpgrade(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "katlc agent TCP endpoint host:port")
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().StringVar(&opts.workstationConfig, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name in the selected context")
	cmd.Flags().StringVar(&opts.imageURL, "image-url", "", "HTTPS KatlOS upgrade image URL")
	cmd.Flags().StringVar(&opts.imageLocalRef, "image-local-ref", "", "relative KatlOS image reference under the node artifact store")
	cmd.Flags().StringVar(&opts.candidateGeneration, "candidate-generation", "", "candidate generation id")
	cmd.Flags().Lookup("image-url").Hidden = true
	cmd.Flags().Lookup("image-local-ref").Hidden = true
	cmd.Flags().Lookup("candidate-generation").Hidden = true
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.actor, "actor", opts.actor, "operation actor")
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "validate without accepting an operation")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the node accepts the operation")
	cmd.Flags().DurationVar(&opts.waitTimeout, "timeout", opts.waitTimeout, "overall operation wait timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func runHostUpgrade(ctx context.Context, opts hostUpgradeOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if opts.waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	direct := strings.TrimSpace(opts.version) != ""
	if direct {
		if strings.TrimSpace(opts.imageURL) != "" || strings.TrimSpace(opts.imageLocalRef) != "" || strings.TrimSpace(opts.candidateGeneration) != "" {
			return fmt.Errorf("VERSION cannot be combined with expert image or candidate flags")
		}
		version, err := katlOSVersion(opts.version)
		if err != nil {
			return err
		}
		opts.version, opts.candidateGeneration = version, "katlos-"+version
		if opts.noWait {
			return fmt.Errorf("--no-wait is unavailable for a managed VERSION upgrade")
		}
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	request := operation.HostUpgrade{
		ImageURL:              strings.TrimSpace(opts.imageURL),
		ImageLocalRef:         strings.TrimSpace(opts.imageLocalRef),
		CandidateGenerationID: strings.TrimSpace(opts.candidateGeneration),
	}
	if !direct {
		if err := operation.ValidateHostUpgrade(request); err != nil {
			return err
		}
	}
	target, err := resolveManagementTarget(managementTargetOptions{
		configPath: opts.workstationConfig, contextName: opts.contextName, nodeName: opts.nodeName,
		endpoint: opts.endpoint, agentTokenFile: opts.agentTokenFile,
	})
	if err != nil {
		return err
	}
	conn, err := dialKatlcAgent(ctx, target.endpoint, target.token)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return fmt.Errorf("read node status: %w", err)
	}
	if direct {
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
			return writeJSON(stdout, hostUpgradeReport{Node: node, Version: opts.version, Image: request.ImageURL, Result: operation.ResultSucceeded, BootHealth: generation.HealthStateHealthy})
		}
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
	if direct {
		report := hostUpgradeReport{Node: target.nodeName, Version: opts.version, Image: request.ImageURL, Result: "planned", BootHealth: "not-run"}
		if report.Node == "" {
			report.Node = target.endpoint
		}
		if opts.plan {
			return writeJSON(stdout, report)
		}
		terminal, err := waitAcceptedOperationStatus(ctx, conn.Client, accepted, opts.waitTimeout, stderr)
		if err != nil {
			report.Result = "failed"
			_ = writeJSON(stdout, report)
			return err
		}
		if err := operationResultError(terminal); err != nil {
			report.Result = terminal.GetResult()
			_ = writeJSON(stdout, report)
			return err
		}
		agentStart := status.GetAgentStartId()
		if err := requestNodeReboot(ctx, conn.Client, status.GetMachineId(), request.CandidateGenerationID); err != nil {
			report.Result = "staged"
			_ = writeJSON(stdout, report)
			return fmt.Errorf("reboot node %s: %w", report.Node, err)
		}
		_ = conn.Close()
		bootCtx, cancel := context.WithTimeout(ctx, opts.waitTimeout)
		verifiedConn, _, err := waitNodeBootHealth(bootCtx, report.Node, target.endpoint, target.token, agentStart, request.CandidateGenerationID, stderr)
		cancel()
		if err != nil {
			report.Result = "failed"
			report.Rebooted = true
			report.BootHealth = "failed"
			_ = writeJSON(stdout, report)
			return err
		}
		_ = verifiedConn.Close()
		report.Result = operation.ResultSucceeded
		report.Rebooted = true
		report.BootHealth = generation.HealthStateHealthy
		return writeJSON(stdout, report)
	}
	if opts.plan || opts.noWait {
		return writeOperationAccepted(stdout, accepted)
	}
	return waitAcceptedOperation(ctx, conn.Client, accepted, opts.waitTimeout, stdout, stderr)
}

func katlOSVersion(input string) (string, error) {
	version := strings.TrimPrefix(strings.TrimSpace(input), "v")
	if !katlOSReleasePattern.MatchString(version) {
		return "", fmt.Errorf("VERSION %q must look like 2026.7.0-alpha.9", input)
	}
	return version, nil
}

func nodeArtifactArchitecture(current *agentapi.Generation) (string, error) {
	for _, ref := range current.GetSysexts() {
		architecture := strings.TrimSpace(ref.GetArchitecture())
		switch architecture {
		case "x86_64", "aarch64":
			return architecture, nil
		case "amd64":
			return "x86_64", nil
		case "arm64":
			return "aarch64", nil
		}
	}
	return "", fmt.Errorf("current node generation does not report a supported artifact architecture")
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
	sourcePath        string
	inventoryPath     string
	configBundlePath  string
	workstationConfig string
	contextName       string
	all               bool
	allowPartial      bool
	confirm           bool
	acknowledgement   string
	clientRequestID   string
	agentTokenFile    string
	planOnly          bool
	noWait            bool
	timeout           string
	output            string
}

func newWipeClusterCommand(ctx context.Context, stdout, stderr io.Writer, commandName string) *cobra.Command {
	opts := wipeClusterOptions{command: commandName, output: "json"}
	cmd := &cobra.Command{
		Use:   "wipe [SOURCE]",
		Short: "Destructively reset cluster nodes for installer-media reinstall",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.sourcePath = args[0]
			}
			return runWipeClusterOptions(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster inventory")
	cmd.Flags().StringVar(&opts.configBundlePath, "config-bundle", "", "path to a Katl config bundle")
	cmd.Flags().StringVar(&opts.workstationConfig, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().BoolVar(&opts.all, "all", false, "select every node in the inventory")
	cmd.Flags().BoolVar(&opts.allowPartial, "allow-partial-cluster", false, "allow a partial cluster target set")
	cmd.Flags().BoolVar(&opts.confirm, "confirm-destructive-wipe", false, "confirm destructive wipe")
	cmd.Flags().StringVar(&opts.acknowledgement, "acknowledge", "", "required destructive acknowledgement text")
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().BoolVar(&opts.planOnly, "plan", false, "print the destructive wipe plan without accepting node-local operations")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after nodes accept their operations")
	cmd.Flags().StringVar(&opts.timeout, "timeout", "30m", "operation and wait timeout duration")
	cmd.Flags().StringVar(&opts.output, "output", "json", "output format: json")
	cmd.Flags().Var(&opts.selectedNodes, "node", "inventory node name to wipe; may be repeated")
	return cmd
}

func runWipeClusterOptions(ctx context.Context, opts wipeClusterOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if !opts.planOnly {
		if !opts.confirm {
			return fmt.Errorf("--confirm-destructive-wipe is required")
		}
		if opts.acknowledgement != wipeAcknowledgementText {
			return fmt.Errorf("--acknowledge must exactly match the destructive wipe acknowledgement text")
		}
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

	inv, err := loadWipeInventory(opts.sourcePath, opts.configBundlePath, opts.inventoryPath)
	if err != nil {
		return err
	}
	inv, err = overlayWipeContext(inv, opts.workstationConfig, opts.contextName)
	if err != nil {
		return err
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: inv})
	if err != nil {
		return err
	}
	targets, partial, err := wipeClusterTargets(plan, opts.all, opts.selectedNodes.values)
	if err != nil {
		return err
	}
	report := newWipeClusterReport(opts.planOnly, partial, targets)
	report.AcknowledgementAccepted = opts.confirm && opts.acknowledgement == wipeAcknowledgementText
	report.Command = opts.command
	if partial && !opts.allowPartial {
		report.Refusals = append(report.Refusals, "partial cluster wipe requires --allow-partial-cluster")
		if printErr := printWipeClusterReport(stdout, report); printErr != nil {
			return printErr
		}
		return fmt.Errorf("partial cluster wipe requires --allow-partial-cluster")
	}

	token, err := readAgentToken(opts.agentTokenFile)
	if err != nil {
		return err
	}
	connector := newWipeClusterConnector(token)
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

type wipeNodeOptions struct {
	command           string
	selectedNodes     stringList
	sourcePath        string
	inventoryPath     string
	configBundlePath  string
	workstationConfig string
	contextName       string
	kubeconfigPath    string
	confirm           bool
	acknowledgement   string
	clientRequestID   string
	agentTokenFile    string
	planOnly          bool
	noWait            bool
	timeout           string
	output            string
}

func newWipeNodeCommand(ctx context.Context, stdout, stderr io.Writer, commandName string) *cobra.Command {
	opts := wipeNodeOptions{command: commandName, output: "json"}
	cmd := &cobra.Command{
		Use:   "node [SOURCE]",
		Short: "Destructively reset one node for installer-media reinstall",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.sourcePath = args[0]
			}
			return runWipeNodeOptions(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster inventory")
	cmd.Flags().StringVar(&opts.configBundlePath, "config-bundle", "", "path to a Katl config bundle")
	cmd.Flags().StringVar(&opts.workstationConfig, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.kubeconfigPath, "kubeconfig", "", "path to operator kubeconfig")
	cmd.Flags().BoolVar(&opts.confirm, "confirm-destructive-wipe", false, "confirm destructive wipe")
	cmd.Flags().StringVar(&opts.acknowledgement, "acknowledge", "", "required destructive acknowledgement text")
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().BoolVar(&opts.planOnly, "plan", false, "print the destructive wipe plan without accepting node-local operation")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the node accepts the operation")
	cmd.Flags().StringVar(&opts.timeout, "timeout", "30m", "operation and wait timeout duration")
	cmd.Flags().StringVar(&opts.output, "output", "json", "output format: json")
	cmd.Flags().Var(&opts.selectedNodes, "node", "inventory node name to wipe")
	return cmd
}

func runWipeNodeOptions(ctx context.Context, opts wipeNodeOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if len(opts.selectedNodes.values) != 1 {
		return fmt.Errorf("exactly one --node is required")
	}
	if !opts.planOnly && strings.TrimSpace(opts.kubeconfigPath) == "" {
		return fmt.Errorf("--kubeconfig is required")
	}
	if !opts.planOnly {
		if !opts.confirm {
			return fmt.Errorf("--confirm-destructive-wipe is required")
		}
		if opts.acknowledgement != wipeAcknowledgementText {
			return fmt.Errorf("--acknowledge must exactly match the destructive wipe acknowledgement text")
		}
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	waitTimeout, err := time.ParseDuration(opts.timeout)
	if err != nil || waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be a positive duration")
	}

	inv, err := loadWipeInventory(opts.sourcePath, opts.configBundlePath, opts.inventoryPath)
	if err != nil {
		return err
	}
	inv, err = overlayWipeContext(inv, opts.workstationConfig, opts.contextName)
	if err != nil {
		return err
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{Inventory: inv})
	if err != nil {
		return err
	}
	targets, partial, err := wipeClusterTargets(plan, false, opts.selectedNodes.values)
	if err != nil {
		return err
	}
	target := targets[0]
	report := newWipeNodeReport(opts.planOnly, partial, target)
	report.AcknowledgementAccepted = opts.confirm && opts.acknowledgement == wipeAcknowledgementText
	report.Command = opts.command
	if target.SystemRole == inventory.RoleControlPlane {
		report.KubernetesCleanup = "refused"
		report.Refusals = append(report.Refusals, "single control-plane wipe requires etcd membership coordination before node-local reset")
		if printErr := printWipeNodeReport(stdout, report); printErr != nil {
			return printErr
		}
		return fmt.Errorf("single control-plane wipe requires etcd membership coordination")
	}

	token, err := readAgentToken(opts.agentTokenFile)
	if err != nil {
		return err
	}
	connector := newWipeClusterConnector(token)
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

func loadWipeInventory(sourcePath, bundlePath, inventoryPath string) (inventory.Inventory, error) {
	inputs := 0
	for _, value := range []string{sourcePath, bundlePath, inventoryPath} {
		if strings.TrimSpace(value) != "" {
			inputs++
		}
	}
	if inputs != 1 {
		return inventory.Inventory{}, fmt.Errorf("exactly one cluster config SOURCE, --config-bundle, or --inventory is required")
	}
	if strings.TrimSpace(inventoryPath) != "" {
		return loadInventory(inventoryPath)
	}
	if strings.TrimSpace(sourcePath) != "" {
		archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
			SourcePath: sourcePath, KatlctlVersion: version, KatlctlCommit: commit, CreatedBy: "katlctl cluster wipe",
		})
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("compile cluster config: %w", err)
		}
		bundle, err := configbundle.ReadBundle(bytes.NewReader(archive), result.Digest)
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("read compiled cluster config: %w", err)
		}
		return bundle.Manifest.Cluster.BootstrapInventory, nil
	}
	bundle, err := configbundle.ReadBundleFile(bundlePath, "")
	if err != nil {
		return inventory.Inventory{}, err
	}
	return bundle.Manifest.Cluster.BootstrapInventory, nil
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
		inv.Nodes[index].Access = inventory.Access{Method: "agent", CredentialRef: node.CredentialRef}
	}
	return inv, nil
}

type wipeClusterReport struct {
	APIVersion              string                          `json:"apiVersion"`
	Kind                    string                          `json:"kind"`
	Command                 string                          `json:"command"`
	Plan                    bool                            `json:"plan"`
	PartialCluster          bool                            `json:"partialCluster"`
	AcknowledgementAccepted bool                            `json:"acknowledgementAccepted"`
	Targets                 []wipeClusterTarget             `json:"targets"`
	KubernetesCleanup       string                          `json:"kubernetesCleanup"`
	NodeLocalOperations     []wipeClusterNodeLocalOperation `json:"nodeLocalOperations"`
	WipedState              []string                        `json:"wipedState"`
	PreservedState          []string                        `json:"preservedState"`
	Refusals                []string                        `json:"refusals,omitempty"`
	Nodes                   []wipeClusterNodeResult         `json:"nodes,omitempty"`
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
		APIVersion:              operation.APIVersion,
		Kind:                    "WipeClusterReport",
		Command:                 "katlctl cluster wipe",
		Plan:                    planOnly,
		PartialCluster:          partial,
		AcknowledgementAccepted: true,
		KubernetesCleanup:       "not-attempted",
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
	report.Command = "katlctl wipe node"
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
			result.Diagnostics = append(result.Diagnostics, "inventory node address is required")
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		if node.Access.Method != "agent" {
			result.Diagnostics = append(result.Diagnostics, fmt.Sprintf("inventory access method %q is not supported", node.Access.Method))
			report.Nodes = append(report.Nodes, result)
			failures = append(failures, node.Name)
			continue
		}
		conn, err := connector.Connect(ctx, node)
		if err != nil {
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
		if strings.HasSuffix(report.Command, "wipe node") {
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
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cluster wipe report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func printWipeNodeReport(stdout io.Writer, report wipeNodeReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wipe node report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
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
		output, err := wipeNodeKubectlRunner.Run(ctx, argv)
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
		Short: "Print the resolved katlctl config path",
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
	contextName string
	output      string
}

func newConfigTopologyCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := configTopologyOptions{output: "json"}
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Print the resolved workstation topology",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runConfigTopology(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl config context name")
	cmd.Flags().StringVar(&opts.output, "output", "json", "output format: json")
	return cmd
}

func runConfigTopology(opts configTopologyOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	resolved, err := workstation.ResolveTopology(workstation.ResolveRequest{
		ContextName: opts.contextName,
	})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(resolved, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal topology: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

type configBundleOptions struct {
	sourcePath string
	outputPath string
}

type nodeConfigInputOptions struct {
	sourcePath     string
	bundlePath     string
	nodeName       string
	sourceID       string
	desiredVersion string
}

type configValidationNode struct {
	Name       string `json:"name"`
	SystemRole string `json:"systemRole"`
}

type configValidationReport struct {
	APIVersion  string                 `json:"apiVersion"`
	Kind        string                 `json:"kind"`
	Source      string                 `json:"source"`
	ClusterName string                 `json:"clusterName"`
	Nodes       []configValidationNode `json:"nodes"`
}

func newConfigValidateCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "validate SOURCE",
		Short: "Validate and resolve a cluster config without writing a bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runConfigValidate(args[0], stdout, stderr)
		},
	}
}

func runConfigValidate(sourcePath string, stdout, stderr io.Writer) error {
	_ = stderr
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
		nodes = append(nodes, configValidationNode{Name: node.Name, SystemRole: node.SystemRole})
	}
	report := configValidationReport{
		APIVersion:  configbundle.APIVersion,
		Kind:        "ClusterConfigValidation",
		Source:      sourcePath,
		ClusterName: result.Manifest.ClusterName,
		Nodes:       nodes,
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
	return cmd
}

func runConfigBundle(opts configBundleOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if strings.TrimSpace(opts.outputPath) == "" {
		return fmt.Errorf("--output is required")
	}
	result, err := configbundle.WriteArchive(opts.outputPath, configbundle.BuildRequest{
		SourcePath:     opts.sourcePath,
		KatlctlVersion: version,
		KatlctlCommit:  commit,
		CreatedBy:      configBundleCreator,
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
	cmd.Flags().StringVar(&opts.sourcePath, "source", "", "ClusterConfig source YAML")
	cmd.Flags().StringVar(&opts.bundlePath, "config-bundle", "", "Katl config bundle")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node to select from cluster intent")
	cmd.Flags().StringVar(&opts.sourceID, "source-id", "", "runtime configuration source id; defaults to the cluster name")
	cmd.Flags().StringVar(&opts.desiredVersion, "desired-version", "", "monotonic unsigned runtime configuration version")
}

func renderNodeConfig(opts nodeConfigInputOptions, mode string) ([]byte, error) {
	fromSource := strings.TrimSpace(opts.sourcePath) != ""
	fromBundle := strings.TrimSpace(opts.bundlePath) != ""
	if fromSource == fromBundle {
		return nil, fmt.Errorf("exactly one of --source or --config-bundle is required")
	}
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
	var selected configbundle.SelectedNodeMaterial
	var err error
	if fromSource {
		archive, _, buildErr := configbundle.BuildArchive(configbundle.BuildRequest{
			SourcePath:     opts.sourcePath,
			KatlctlVersion: version,
			KatlctlCommit:  commit,
			CreatedBy:      configBundleCreator,
		})
		if buildErr != nil {
			return nil, buildErr
		}
		selected, err = configbundle.ReadSelectedNode(bytes.NewReader(archive), readOptions)
	} else {
		selected, err = configbundle.ReadSelectedNodeFile(opts.bundlePath, readOptions)
	}
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
		SourceID:       sourceID,
		DesiredVersion: opts.desiredVersion,
		ApplyMode:      mode,
	})
}

type configApplyOptions struct {
	endpoint            string
	agentTokenFile      string
	workstationConfig   string
	contextName         string
	configPath          string
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
	opts := configApplyOptions{mode: generation.ApplyModeAuto, actor: "katlctl config apply", output: "json"}
	cmd := &cobra.Command{
		Use:   "apply [SOURCE]",
		Short: "Validate or apply node configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				if strings.TrimSpace(opts.nodeConfig.sourcePath) != "" {
					return fmt.Errorf("SOURCE cannot be combined with --source")
				}
				opts.nodeConfig.sourcePath = args[0]
			}
			return runConfigApply(ctx, opts, stdout, stderr)
		},
	}
	addConfigApplyFlags(cmd, &opts)

	validateOpts := configApplyOptions{mode: generation.ApplyModeAuto, actor: "katlctl config apply validate", plan: true, output: "json"}
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
	cmd.AddCommand(validateCmd)
	cmd.AddCommand(newConfigApplyStatusCommand(ctx, stdout, stderr))
	return cmd
}

func addConfigApplyFlags(cmd *cobra.Command, opts *configApplyOptions) {
	if opts.waitTimeout == 0 {
		opts.waitTimeout = 30 * time.Minute
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "katlc agent TCP endpoint host:port")
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().StringVar(&opts.workstationConfig, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.configPath, "file", "", "pre-rendered NodeConfigurationChange YAML")
	addNodeConfigInputFlags(cmd, &opts.nodeConfig)
	cmd.Flags().StringVar(&opts.mode, "mode", opts.mode, "apply mode: auto, live, or next-boot")
	cmd.Flags().StringVar(&opts.candidateGeneration, "candidate-generation", "", "candidate generation id")
	for _, name := range []string{"source", "desired-version", "candidate-generation"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			flag.Hidden = true
		}
	}
	cmd.Flags().StringVar(&opts.clientRequestID, "client-request-id", "", "optional idempotency key for advanced retry control")
	cmd.Flags().Lookup("client-request-id").Hidden = true
	cmd.Flags().StringVar(&opts.actor, "actor", opts.actor, "operation actor")
	cmd.Flags().BoolVar(&opts.plan, "plan", opts.plan, "validate and plan without accepting an operation")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the node accepts the operation")
	cmd.Flags().DurationVar(&opts.waitTimeout, "timeout", opts.waitTimeout, "overall operation wait timeout")
	cmd.Flags().StringVar(&opts.output, "output", "json", "output format: json")
}

func runConfigApply(ctx context.Context, opts configApplyOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if opts.waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	requestID, err := clientRequestID(opts.clientRequestID)
	if err != nil {
		return err
	}
	fileInput := strings.TrimSpace(opts.configPath) != ""
	intentInput := strings.TrimSpace(opts.nodeConfig.sourcePath) != "" || strings.TrimSpace(opts.nodeConfig.bundlePath) != ""
	if fileInput == intentInput {
		return fmt.Errorf("exactly one of --file, --source, or --config-bundle is required")
	}
	target, err := resolveManagementTarget(managementTargetOptions{
		configPath: opts.workstationConfig, contextName: opts.contextName,
		nodeName: opts.nodeConfig.nodeName, endpoint: opts.endpoint, agentTokenFile: opts.agentTokenFile,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.nodeConfig.nodeName) == "" {
		opts.nodeConfig.nodeName = target.nodeName
	}
	if strings.TrimSpace(opts.nodeConfig.desiredVersion) == "" && intentInput {
		opts.nodeConfig.desiredVersion = strconv.FormatInt(configApplyNow().UnixNano(), 10)
	}
	if strings.TrimSpace(opts.candidateGeneration) == "" {
		opts.candidateGeneration = "config-" + strconv.FormatInt(configApplyNow().UnixNano(), 10)
	}
	var configYAML []byte
	if fileInput {
		if strings.TrimSpace(opts.nodeConfig.sourceID) != "" || strings.TrimSpace(opts.nodeConfig.desiredVersion) != "" {
			return fmt.Errorf("--source-id and --desired-version require --source or --config-bundle")
		}
		configYAML, err = os.ReadFile(opts.configPath)
		if err != nil {
			return fmt.Errorf("read config file: %w", err)
		}
	} else {
		configYAML, err = renderNodeConfig(opts.nodeConfig, opts.mode)
		if err != nil {
			return err
		}
	}
	conn, err := dialKatlcAgent(ctx, target.endpoint, target.token)
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
	return waitAcceptedOperation(ctx, conn.Client, accepted, opts.waitTimeout, stdout, stderr)
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
	agentTokenFile     string
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
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().StringVar(&opts.workstationConfig, "config", "", "katlctl workstation config path")
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
			endpoint: opts.endpoint, agentTokenFile: opts.agentTokenFile,
		})
		if err != nil {
			return err
		}
		conn, err := dialKatlcAgent(ctx, target.endpoint, target.token)
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
	sourcePath                             string
	inventoryPath                          string
	configBundlePath                       string
	initNode                               string
	joinWorker                             string
	controlPlaneEndpoint                   string
	kubernetesBundle                       string
	kubeconfigOut                          string
	overwriteKubeconfig                    bool
	dryRun                                 bool
	vmtestTranscriptDir                    string
	agentTokenFile                         string
	bootstrapManifestPaths                 stringList
	bootstrapPreWaitValues                 stringList
	bootstrapWaitValues                    stringList
	bootstrapStableEndpoint                string
	bootstrapStableEndpointBeforeManifests bool
}

func newClusterBootstrapCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := clusterBootstrapOptions{}
	cmd := &cobra.Command{
		Use:   "bootstrap [SOURCE]",
		Short: "Bootstrap Kubernetes from a cluster config or node inventory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.sourcePath = args[0]
			}
			return runClusterBootstrap(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "path to cluster bootstrap inventory")
	cmd.Flags().StringVar(&opts.configBundlePath, "config-bundle", "", "path to a Katl config bundle")
	cmd.Flags().StringVar(&opts.initNode, "init-node", "", "first control-plane node for kubeadm init")
	cmd.Flags().StringVar(&opts.joinWorker, "join-worker", "", "join one fresh worker to an already initialized cluster without rerunning kubeadm init")
	cmd.Flags().StringVar(&opts.controlPlaneEndpoint, "control-plane-endpoint", "", "control-plane endpoint host:port")
	cmd.Flags().StringVar(&opts.kubernetesBundle, "kubernetes-bundle", "", "Kubernetes OCI bundle image reference; an @sha256 manifest pin is optional")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig-out", "", "operator kubeconfig output path")
	cmd.Flags().BoolVar(&opts.overwriteKubeconfig, "overwrite-kubeconfig", false, "overwrite different existing kubeconfig")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print the bootstrap plan without running kubeadm")
	cmd.Flags().StringVar(&opts.vmtestTranscriptDir, "vmtest-transcript-dir", "", "directory for per-node vmtest agent transcript artifacts")
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().Var(&opts.addresses, "node-address", "node address override in node=address form")
	cmd.Flags().Var(&opts.bootstrapManifestPaths, "bootstrap-manifest", "ordered Kubernetes manifest file or bundle to apply after API readiness")
	cmd.Flags().Var(&opts.bootstrapPreWaitValues, "bootstrap-pre-wait", "pre-manifest wait: api-ready, nodes-ready, resource-exists[:namespace]:kind/name, condition[:namespace]:kind/name:Condition, rollout-status[:namespace]:kind/name, or pods-ready[:namespace]:selector")
	cmd.Flags().Var(&opts.bootstrapWaitValues, "bootstrap-wait", "post-bootstrap wait: api-ready, nodes-ready, resource-exists[:namespace]:kind/name, condition[:namespace]:kind/name:Condition, rollout-status[:namespace]:kind/name, or pods-ready[:namespace]:selector")
	cmd.Flags().StringVar(&opts.bootstrapStableEndpoint, "bootstrap-stable-endpoint", "", "stable API endpoint host:port to wait for before writing kubeconfig")
	cmd.Flags().BoolVar(&opts.bootstrapStableEndpointBeforeManifests, "bootstrap-stable-endpoint-before-manifests", false, "wait for stable API endpoint before applying bootstrap manifests")
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
		var token string
		token, err = readAgentToken(opts.agentTokenFile)
		if err != nil {
			return err
		}
		result, err := runAgentWorkerJoin(ctx, request, strings.TrimSpace(opts.joinWorker), agentBootstrapDependencies(token))
		printBootstrapResult(stdout, result)
		return err
	}
	var result cluster.Result
	if strings.TrimSpace(opts.vmtestTranscriptDir) != "" {
		result, err = runBootstrap(ctx, request, bootstrapDependencies(opts.vmtestTranscriptDir))
	} else {
		var token string
		token, err = readAgentToken(opts.agentTokenFile)
		if err != nil {
			return err
		}
		result, err = runAgentBootstrap(ctx, request, agentBootstrapDependencies(token))
	}
	printBootstrapResult(stdout, result)
	return err
}

func bootstrapInventory(opts clusterBootstrapOptions) (inventory.Inventory, error) {
	sourcePath := strings.TrimSpace(opts.sourcePath)
	inventoryPath := strings.TrimSpace(opts.inventoryPath)
	bundlePath := strings.TrimSpace(opts.configBundlePath)
	inputs := 0
	for _, value := range []string{sourcePath, inventoryPath, bundlePath} {
		if value != "" {
			inputs++
		}
	}
	if inputs != 1 {
		return inventory.Inventory{}, fmt.Errorf("exactly one cluster config SOURCE, --config-bundle, or --inventory is required")
	}
	if inventoryPath != "" {
		return loadInventory(inventoryPath)
	}
	if strings.TrimSpace(opts.kubernetesBundle) != "" {
		return inventory.Inventory{}, fmt.Errorf("--kubernetes-bundle conflicts with the selection embedded in the cluster config")
	}
	if strings.TrimSpace(opts.controlPlaneEndpoint) != "" {
		return inventory.Inventory{}, fmt.Errorf("--control-plane-endpoint conflicts with the endpoint embedded in the cluster config")
	}
	if sourcePath != "" {
		archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
			SourcePath:     sourcePath,
			KatlctlVersion: version,
			KatlctlCommit:  commit,
			CreatedBy:      clusterBootstrapCreator,
		})
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("compile cluster config: %w", err)
		}
		bundle, err := configbundle.ReadBundle(bytes.NewReader(archive), result.Digest)
		if err != nil {
			return inventory.Inventory{}, fmt.Errorf("read compiled cluster config: %w", err)
		}
		return bundle.Manifest.Cluster.BootstrapInventory, nil
	}
	bundle, err := configbundle.ReadBundleFile(bundlePath, "")
	if err != nil {
		return inventory.Inventory{}, err
	}
	return bundle.Manifest.Cluster.BootstrapInventory, nil
}

func bootstrapDependencies(vmtestTranscriptDir string) cluster.Dependencies {
	transport := vmtestAgentTransport{TranscriptDir: strings.TrimSpace(vmtestTranscriptDir)}
	return cluster.Dependencies{
		ReadinessChecker: readiness.Checker{Agent: transport},
		NodeRunner:       cluster.TransportRunner{Transport: transport},
		BootstrapRunner:  cluster.KubectlBootstrapRunner{},
	}
}

func agentBootstrapDependencies(token string) cluster.AgentBootstrapDependencies {
	return cluster.AgentBootstrapDependencies{
		Connector:       cluster.TCPAgentConnector{AuthToken: strings.TrimSpace(token), AuthTokenForNode: agentTokenForNode},
		Actor:           "katlctl cluster bootstrap",
		BootstrapRunner: cluster.KubectlBootstrapRunner{},
	}
}

func agentTokenForNode(node inventory.PlannedNode) (string, error) {
	ref := strings.TrimSpace(node.Access.CredentialRef)
	if ref == "" {
		return "", nil
	}
	path, ok := strings.CutPrefix(ref, "file:")
	if !ok {
		return "", nil
	}
	return readAgentToken(path)
}

func readAgentToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agent token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("agent token file is empty: %s", path)
	}
	return token, nil
}

type katlcAgentConnection struct {
	Client agentapi.KatlcAgentClient
	Close  func() error
}

func dialKatlcAgentTCP(ctx context.Context, endpoint string, token string) (katlcAgentConnection, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return katlcAgentConnection{}, fmt.Errorf("katlc agent endpoint is required")
	}
	conn, err := grpc.DialContext(ctx, endpoint, katlcAgentDialOptions(token)...)
	if err != nil {
		return katlcAgentConnection{}, err
	}
	return katlcAgentConnection{
		Client: agentapi.NewKatlcAgentClient(conn),
		Close:  conn.Close,
	}, nil
}

func katlcAgentDialOptions(token string) []grpc.DialOption {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if strings.TrimSpace(token) != "" {
		authorization := "Bearer " + strings.TrimSpace(token)
		opts = append(opts,
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", authorization), method, req, reply, cc, opts...)
			}),
			grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				return streamer(metadata.AppendToOutgoingContext(ctx, "authorization", authorization), desc, cc, method, opts...)
			}),
		)
	}
	return opts
}

func printBootstrapResult(stdout io.Writer, result cluster.Result) {
	if len(result.Plan.Nodes) > 0 {
		fmt.Fprintf(stdout, "katlctl cluster bootstrap init-node=%s\n", result.Plan.InitNode)
		for _, override := range result.Plan.AddressOverrides {
			fmt.Fprintf(stdout, "katlctl cluster bootstrap address-override node=%s before=%s after=%s\n", override.Node, override.Before, override.Address)
		}
	}
	for _, phase := range result.Phases {
		operationFields := ""
		if phase.OperationID != "" {
			operationFields = fmt.Sprintf(" operation-id=%s", phase.OperationID)
		}
		if phase.Node != "" {
			fmt.Fprintf(stdout, "phase=%s node=%s status=%s%s\n", phase.Name, phase.Node, phase.Status, operationFields)
			continue
		}
		fmt.Fprintf(stdout, "phase=%s status=%s%s\n", phase.Name, phase.Status, operationFields)
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
