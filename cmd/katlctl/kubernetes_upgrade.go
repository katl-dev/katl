package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type kubernetesUpgradeOptions struct {
	version       string
	configPath    string
	contextName   string
	inventoryPath string
	bundle        string
	plan          bool
	timeout       time.Duration
	output        string
}

type kubernetesUpgradeNodeReport struct {
	Name             string `json:"name"`
	Role             string `json:"role"`
	SourceVersion    string `json:"sourceVersion"`
	TargetVersion    string `json:"targetVersion"`
	Result           string `json:"result"`
	Phase            string `json:"phase,omitempty"`
	RecoveryRequired bool   `json:"recoveryRequired,omitempty"`
	NextAction       string `json:"nextAction,omitempty"`
}

type kubernetesUpgradeReport struct {
	Cluster       string                        `json:"cluster"`
	SourceVersion string                        `json:"sourceVersion"`
	TargetVersion string                        `json:"targetVersion"`
	Bundle        string                        `json:"bundle"`
	Plan          bool                          `json:"plan"`
	Nodes         []kubernetesUpgradeNodeReport `json:"nodes"`
	NextAction    string                        `json:"nextAction,omitempty"`
}

type kubernetesUpgradeTarget struct {
	node       workstation.TopologyNode
	role       string
	conn       katlcAgentConnection
	token      string
	machineID  string
	agentStart string
	generation string
	source     string
	candidate  string
}

var kubernetesUpgradeNow = func() time.Time { return time.Now().UTC() }
var kubernetesEndpointPollInterval = 2 * time.Second
var dialKubernetesEndpoint = func(ctx context.Context, endpoint string) error {
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return err
	}
	return conn.Close()
}

func newKubernetesUpgradeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := kubernetesUpgradeOptions{timeout: 25 * time.Minute, output: "json"}
	cmd := &cobra.Command{
		Use:   "kubernetes VERSION",
		Short: "Upgrade Kubernetes control planes and workers serially",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.version = args[0]
			}
			return runKubernetesUpgrade(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configPath, "config", "", "katlctl config path (defaults to the selected workstation context)")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl config context name")
	cmd.Flags().StringVar(&opts.inventoryPath, "inventory", "", "cluster inventory instead of a workstation context")
	cmd.Flags().StringVar(&opts.bundle, "bundle", "", "Kubernetes bundle image, for example ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1")
	cmd.Flags().Lookup("bundle").Hidden = true
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "validate the complete rollout without accepting operations")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "per-node operation timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	_ = stderr
	return cmd
}

func runKubernetesUpgrade(ctx context.Context, opts kubernetesUpgradeOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if opts.timeout > 25*time.Minute {
		return fmt.Errorf("--timeout must not exceed 25m")
	}
	bundle, err := kubernetesUpgradeBundle(opts.version, opts.bundle)
	if err != nil {
		return err
	}
	image, err := kubernetesbundle.ParseImageReference(bundle)
	if err != nil {
		return fmt.Errorf("--bundle: %w", err)
	}
	topology, err := resolveKubernetesUpgradeTopology(opts)
	if err != nil {
		return err
	}
	targets, err := connectKubernetesUpgradeTargets(ctx, topology, image.PayloadVersion)
	if err != nil {
		return err
	}
	defer func() {
		for _, target := range targets {
			if target.conn.Close != nil {
				_ = target.conn.Close()
			}
		}
	}()

	report := kubernetesUpgradeReport{Cluster: topology.ClusterName, TargetVersion: image.PayloadVersion, Bundle: image.Value, Plan: opts.plan}
	if len(targets) > 0 {
		report.SourceVersion = targets[0].source
	} else {
		report.SourceVersion = image.PayloadVersion
		report.NextAction = "every node already runs the selected Kubernetes version"
		return writeKubernetesUpgradeReport(stdout, report)
	}
	for _, target := range targets {
		body := kubernetesUpgradeBody(target, image)
		accepted, err := target.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest",
			ClientRequestId: "katlctl-plan-" + target.candidate, OperationKind: "kubeadm-upgrade",
			Actor: "katlctl cluster upgrade kubernetes", ExpectedMachineId: target.machineID,
			ExpectedCurrentGenerationId: target.generation, DryRun: true, KubernetesSysextUpdate: body,
		})
		if err != nil {
			return fmt.Errorf("plan node %s: %w", target.node.Name, err)
		}
		if accepted.InitialStatus == nil || accepted.InitialStatus.Phase != "accepted" && accepted.InitialStatus.Phase != "dry-run" {
			return fmt.Errorf("node %s did not accept the Kubernetes upgrade plan", target.node.Name)
		}
		report.Nodes = append(report.Nodes, kubernetesUpgradeNodeReport{Name: target.node.Name, Role: target.role, SourceVersion: target.source, TargetVersion: image.PayloadVersion, Result: "planned"})
	}
	if opts.plan {
		return writeKubernetesUpgradeReport(stdout, report)
	}

	report.Nodes = nil
	for i := range targets {
		target := &targets[i]
		accepted, err := target.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest",
			ClientRequestId: "katlctl-" + target.candidate, OperationKind: "kubeadm-upgrade",
			Actor: "katlctl cluster upgrade kubernetes", ExpectedMachineId: target.machineID,
			ExpectedCurrentGenerationId: target.generation, OperationTimeout: opts.timeout.String(),
			KubernetesSysextUpdate: kubernetesUpgradeBody(*target, image),
		})
		if err != nil {
			return fmt.Errorf("submit node %s: %w", target.node.Name, err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		terminal, err := waitKubernetesUpgrade(waitCtx, target.conn.Client, accepted.OperationId, target.node.Name, stderr)
		cancel()
		if err != nil {
			return fmt.Errorf("wait for node %s: %w", target.node.Name, err)
		}
		nodeReport := kubernetesUpgradeNodeReport{Name: target.node.Name, Role: target.role, SourceVersion: target.source, TargetVersion: image.PayloadVersion, Result: terminal.Result, Phase: terminal.Phase, RecoveryRequired: terminal.RecoveryRequired, NextAction: terminal.NextAction}
		report.Nodes = append(report.Nodes, nodeReport)
		if terminal.Result != operation.ResultSucceeded {
			_ = writeKubernetesUpgradeReport(stdout, report)
			return fmt.Errorf("Kubernetes upgrade stopped at node %s: %s", target.node.Name, terminal.FailureReason)
		}
		if err := requestNodeReboot(ctx, target.conn.Client, target.machineID, target.candidate); err != nil {
			report.Nodes[len(report.Nodes)-1].Result = "reboot-failed"
			_ = writeKubernetesUpgradeReport(stdout, report)
			return fmt.Errorf("Kubernetes upgrade stopped while rebooting node %s: %w", target.node.Name, err)
		}
		_ = target.conn.Close()
		target.conn = katlcAgentConnection{}
		bootCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		conn, boot, err := waitNodeBootHealth(bootCtx, target.node.Name, target.node.ManagementEndpoint, target.token, target.agentStart, target.candidate, stderr)
		cancel()
		if err != nil {
			report.Nodes[len(report.Nodes)-1].Result = "boot-health-failed"
			report.Nodes[len(report.Nodes)-1].RecoveryRequired = true
			_ = writeKubernetesUpgradeReport(stdout, report)
			return fmt.Errorf("Kubernetes upgrade stopped after node %s reboot: %w", target.node.Name, err)
		}
		target.conn = conn
		if kubernetesGenerationVersion(boot.Generation) != image.PayloadVersion {
			report.Nodes[len(report.Nodes)-1].Result = "boot-health-failed"
			_ = writeKubernetesUpgradeReport(stdout, report)
			return fmt.Errorf("Kubernetes upgrade stopped at node %s: booted generation does not contain Kubernetes %s", target.node.Name, image.PayloadVersion)
		}
		if target.node.SystemRole == inventory.RoleControlPlane {
			readyCtx, cancel := context.WithTimeout(ctx, opts.timeout)
			err := waitKubernetesEndpoint(readyCtx, topology.ControlPlaneEndpoint, target.node.Name, stderr)
			cancel()
			if err != nil {
				report.Nodes[len(report.Nodes)-1].Result = "cluster-health-failed"
				_ = writeKubernetesUpgradeReport(stdout, report)
				return fmt.Errorf("Kubernetes upgrade stopped after node %s reboot: %w", target.node.Name, err)
			}
		}
		report.Nodes[len(report.Nodes)-1].Phase = "boot-healthy"
		report.Nodes[len(report.Nodes)-1].NextAction = ""
	}
	report.NextAction = "rollout complete; every upgraded node rebooted into a healthy generation"
	return writeKubernetesUpgradeReport(stdout, report)
}

func waitKubernetesEndpoint(ctx context.Context, endpoint, nodeName string, stderr io.Writer) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("cluster control-plane endpoint is not configured")
	}
	for {
		if err := dialKubernetesEndpoint(ctx, endpoint); err == nil {
			_, _ = fmt.Fprintf(stderr, "kubernetes upgrade node=%s control-plane-ready endpoint=%s\n", nodeName, endpoint)
			return nil
		}
		timer := time.NewTimer(kubernetesEndpointPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("control-plane endpoint %s did not return after node %s reboot: %w", endpoint, nodeName, ctx.Err())
		case <-timer.C:
		}
	}
}

func kubernetesUpgradeBundle(version, explicit string) (string, error) {
	version = strings.TrimSpace(version)
	explicit = strings.TrimSpace(explicit)
	if version != "" && explicit != "" {
		return "", fmt.Errorf("VERSION cannot be combined with --bundle")
	}
	if explicit != "" {
		return explicit, nil
	}
	if version == "" {
		return "", fmt.Errorf("VERSION is required")
	}
	version = strings.TrimPrefix(version, "v")
	if !strings.Contains(version, "-katl.") {
		version += "-katl.1"
	}
	return "ghcr.io/katl-dev/kubernetes:v" + version, nil
}

func kubernetesGenerationVersion(value *agentapi.Generation) string {
	if value == nil {
		return ""
	}
	for _, ref := range value.Sysexts {
		if ref.GetName() == "kubernetes" {
			return strings.TrimSpace(ref.GetPayloadVersion())
		}
	}
	return ""
}

func waitKubernetesUpgrade(ctx context.Context, client agentapi.KatlcAgentClient, operationID, nodeName string, stderr io.Writer) (*agentapi.OperationStatus, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastPhase := ""
	for {
		status, err := client.GetOperation(ctx, &agentapi.GetOperationRequest{OperationId: operationID, IncludeDiagnostics: "normal"})
		if err != nil {
			return nil, err
		}
		if status.Phase != lastPhase {
			lastPhase = status.Phase
			if _, err := fmt.Fprintf(stderr, "kubernetes upgrade node=%s phase=%s\n", nodeName, status.Phase); err != nil {
				return nil, err
			}
		}
		if status.Terminal {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func resolveKubernetesUpgradeTopology(opts kubernetesUpgradeOptions) (workstation.ResolvedTopology, error) {
	request := workstation.ResolveRequest{ConfigPath: strings.TrimSpace(opts.configPath), ContextName: strings.TrimSpace(opts.contextName)}
	if strings.TrimSpace(opts.inventoryPath) != "" {
		if strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.contextName) != "" {
			return workstation.ResolvedTopology{}, fmt.Errorf("--inventory cannot be combined with --config or --context")
		}
		inv, err := loadInventory(opts.inventoryPath)
		if err != nil {
			return workstation.ResolvedTopology{}, err
		}
		request.ExplicitInventory = &inv
	}
	return workstation.ResolveTopology(request)
}

func connectKubernetesUpgradeTargets(ctx context.Context, topology workstation.ResolvedTopology, targetVersion string) ([]kubernetesUpgradeTarget, error) {
	nodes := append([]workstation.TopologyNode(nil), topology.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].SystemRole != nodes[j].SystemRole {
			return nodes[i].SystemRole == inventory.RoleControlPlane
		}
		return nodes[i].Name < nodes[j].Name
	})
	controlPlanes := 0
	for _, node := range nodes {
		if node.SystemRole == inventory.RoleControlPlane {
			controlPlanes++
		}
	}
	if controlPlanes == 0 {
		return nil, fmt.Errorf("cluster has no control-plane node")
	}
	runID := fmt.Sprintf("%d", kubernetesUpgradeNow().Unix())
	inspected := make([]kubernetesUpgradeTarget, 0, len(nodes))
	closeTargets := func() {
		for _, target := range inspected {
			_ = target.conn.Close()
		}
	}
	for _, node := range nodes {
		token, err := tokenForTopologyNode(node)
		if err != nil {
			closeTargets()
			return nil, err
		}
		conn, err := dialKatlcAgent(ctx, node.ManagementEndpoint, token)
		if err != nil {
			closeTargets()
			return nil, fmt.Errorf("connect node %s: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if err != nil {
			_ = conn.Close()
			closeTargets()
			return nil, fmt.Errorf("status node %s: %w", node.Name, err)
		}
		generationID := strings.TrimSpace(status.CurrentGenerationId)
		if generationID == "" {
			_ = conn.Close()
			closeTargets()
			return nil, fmt.Errorf("node %s did not report its current generation", node.Name)
		}
		gen, err := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: generationID})
		if err != nil {
			_ = conn.Close()
			closeTargets()
			return nil, fmt.Errorf("current generation on node %s: %w", node.Name, err)
		}
		if gen.CommitState != "committed" || gen.HealthState != "healthy" {
			_ = conn.Close()
			closeTargets()
			return nil, fmt.Errorf("node %s current generation %s is not committed and healthy", node.Name, generationID)
		}
		sourceVersion := ""
		for _, ref := range gen.Sysexts {
			if ref.Name == "kubernetes" {
				sourceVersion = strings.TrimSpace(ref.PayloadVersion)
				break
			}
		}
		if sourceVersion == "" {
			_ = conn.Close()
			closeTargets()
			return nil, fmt.Errorf("node %s current generation has no Kubernetes payload", node.Name)
		}
		candidate := kubernetesUpgradeCandidate(targetVersion, node.Name, runID)
		inspected = append(inspected, kubernetesUpgradeTarget{node: node, conn: conn, token: token, machineID: status.MachineId, agentStart: status.AgentStartId, generation: generationID, source: sourceVersion, candidate: candidate})
	}
	baseVersion := ""
	controlPlaneAtTarget := false
	pendingControlPlanes := false
	workerAtTarget := false
	for _, target := range inspected {
		if target.source == targetVersion {
			if target.node.SystemRole == inventory.RoleControlPlane {
				controlPlaneAtTarget = true
			} else {
				workerAtTarget = true
			}
			continue
		}
		if baseVersion == "" {
			baseVersion = target.source
		} else if target.source != baseVersion {
			closeTargets()
			return nil, fmt.Errorf("node %s runs Kubernetes %s while the pending rollout source is %s", target.node.Name, target.source, baseVersion)
		}
		if target.node.SystemRole == inventory.RoleControlPlane {
			pendingControlPlanes = true
		}
	}
	if workerAtTarget && pendingControlPlanes {
		closeTargets()
		return nil, fmt.Errorf("a worker already runs %s while a control plane is still pending", targetVersion)
	}
	targets := make([]kubernetesUpgradeTarget, 0, len(inspected))
	applySelected := controlPlaneAtTarget
	for _, target := range inspected {
		if target.source == targetVersion {
			_ = target.conn.Close()
			continue
		}
		target.role = "worker"
		if target.node.SystemRole == inventory.RoleControlPlane {
			target.role = "control-plane"
			if !applySelected {
				target.role = "apply"
				applySelected = true
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func kubernetesUpgradeBody(target kubernetesUpgradeTarget, image kubernetesbundle.ImageReference) *agentapi.KubernetesSysextUpdateOperationRequest {
	return &agentapi.KubernetesSysextUpdateOperationRequest{
		TargetPayloadVersion: targetVersion(image), CandidateGenerationId: target.candidate,
		UpgradeRole: target.role, SourcePayloadVersion: target.source,
		KubernetesBundleSource: image.Source, KubernetesBundleRef: image.Value,
	}
}

func targetVersion(image kubernetesbundle.ImageReference) string { return image.PayloadVersion }

func kubernetesUpgradeCandidate(version, node, runID string) string {
	version = strings.NewReplacer("v", "", ".", "-").Replace(strings.TrimSpace(version))
	return "kubernetes-" + version + "-" + node + "-" + runID
}

func tokenForTopologyNode(node workstation.TopologyNode) (string, error) {
	ref := strings.TrimSpace(node.CredentialRef)
	path, ok := strings.CutPrefix(ref, "file:")
	if !ok || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("node %s credentialRef must be a file reference", node.Name)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read node %s credential: %w", node.Name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func writeKubernetesUpgradeReport(stdout io.Writer, report kubernetesUpgradeReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
