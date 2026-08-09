package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/kubernetescompat"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type kubernetesUpgradeOptions struct {
	clusterConfig string
	configPath    string
	contextName   string
	bundle        string
	artifact      string
	cordon        bool
	kubeconfig    string
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
	Bundle        string                        `json:"bundle,omitempty"`
	Artifact      string                        `json:"artifact,omitempty"`
	Plan          bool                          `json:"plan"`
	Nodes         []kubernetesUpgradeNodeReport `json:"nodes"`
	NextAction    string                        `json:"nextAction,omitempty"`
}

type kubernetesUpgradeArtifact struct {
	Path           string
	PayloadVersion string
	Architecture   string
	SHA256         string
	SizeBytes      uint64
}

type kubernetesUpgradeTarget struct {
	node         workstation.TopologyNode
	upgradeRole  string
	conn         katlcAgentConnection
	machineID    string
	generation   string
	source       string
	candidate    string
	architecture string
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
	opts := kubernetesUpgradeOptions{timeout: 25 * time.Minute, output: "text"}
	cmd := &cobra.Command{
		Use:   "upgrade --config CLUSTER_CONFIG",
		Short: "Upgrade Kubernetes control planes and workers online",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if strings.TrimSpace(opts.clusterConfig) == "" && command.Flags().NFlag() == 0 {
				return command.Help()
			}
			return runKubernetesUpgrade(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.clusterConfig, "config", "", "ClusterConfig YAML or Katl config bundle containing the desired Kubernetes version")
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().StringVar(&opts.bundle, "bundle", "", "Kubernetes bundle image, for example ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1")
	cmd.Flags().Lookup("bundle").Hidden = true
	cmd.Flags().StringVar(&opts.artifact, "artifact", "", "locally built Kubernetes upgrade image (uses PATH.json metadata)")
	cmd.Flags().BoolVar(&opts.cordon, "cordon", false, "temporarily cordon each node during its online upgrade")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "operator kubeconfig used with --cordon")
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "validate the complete rollout without accepting operations")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "per-node operation timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	_ = stderr
	return cmd
}

func runKubernetesUpgrade(ctx context.Context, opts kubernetesUpgradeOptions, stdout, stderr io.Writer) error {
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if opts.timeout > 25*time.Minute {
		return fmt.Errorf("--timeout must not exceed 25m")
	}
	if opts.cordon && strings.TrimSpace(opts.kubeconfig) == "" {
		return fmt.Errorf("--kubeconfig is required with --cordon")
	}
	if !opts.cordon && strings.TrimSpace(opts.kubeconfig) != "" {
		return fmt.Errorf("--kubeconfig is only used with --cordon")
	}
	topology, desiredVersion, err := resolveKubernetesUpgradeTopology(opts)
	if err != nil {
		return err
	}
	var image kubernetesbundle.ImageReference
	var localArtifact *kubernetesUpgradeArtifact
	if strings.TrimSpace(opts.artifact) != "" {
		if strings.TrimSpace(opts.bundle) != "" {
			return fmt.Errorf("--artifact cannot be combined with --bundle")
		}
		artifact, err := readKubernetesUpgradeArtifact(opts.artifact)
		if err != nil {
			return fmt.Errorf("--artifact: %w", err)
		}
		if desiredVersion != artifact.PayloadVersion {
			return fmt.Errorf("local Kubernetes artifact payload %s does not match spec.kubernetes.version %s", artifact.PayloadVersion, desiredVersion)
		}
		localArtifact = &artifact
		image = kubernetesbundle.ImageReference{Value: artifact.Path, PayloadVersion: artifact.PayloadVersion}
	} else {
		bundle, err := kubernetesUpgradeBundle(desiredVersion, opts.bundle)
		if err != nil {
			return err
		}
		image, err = kubernetesbundle.ParseImageReference(bundle)
		if err != nil {
			return fmt.Errorf("--bundle: %w", err)
		}
		if image.PayloadVersion != desiredVersion {
			return fmt.Errorf("--bundle payload version %s does not match spec.kubernetes.version %s", image.PayloadVersion, desiredVersion)
		}
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
	if localArtifact != nil {
		for _, target := range targets {
			if target.architecture == "" {
				return fmt.Errorf("node %s current generation does not report a supported artifact architecture", target.node.Name)
			}
			if target.architecture != localArtifact.Architecture {
				return fmt.Errorf("local Kubernetes image architecture %q does not match node %s architecture %q", localArtifact.Architecture, target.node.Name, target.architecture)
			}
		}
	}

	report := kubernetesUpgradeReport{Cluster: topology.ClusterName, TargetVersion: image.PayloadVersion, Plan: opts.plan}
	if localArtifact != nil {
		report.Artifact = localArtifact.Path
	} else {
		report.Bundle = image.Value
	}
	if len(targets) > 0 {
		report.SourceVersion = targets[0].source
	} else {
		report.SourceVersion = image.PayloadVersion
		report.NextAction = "every node already runs the selected Kubernetes version"
		return writeKubernetesUpgradeReport(stdout, opts.output, report)
	}
	for _, target := range targets {
		body := kubernetesUpgradeBody(target, image, localArtifact)
		accepted, err := target.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest",
			ClientRequestId: "katlctl-plan-" + target.candidate, OperationKind: "kubeadm-upgrade",
			Actor: "katlctl kubernetes upgrade", ExpectedMachineId: target.machineID,
			ExpectedEnrollmentId: target.node.EnrollmentID, ExpectedInventoryNodeName: target.node.Name,
			ExpectedCurrentGenerationId: target.generation, DryRun: true, KubernetesSysextUpdate: body,
		})
		if err != nil {
			return fmt.Errorf("plan node %s: %w", target.node.Name, err)
		}
		if accepted.InitialStatus == nil || accepted.InitialStatus.Phase != "accepted" && accepted.InitialStatus.Phase != "dry-run" {
			return fmt.Errorf("node %s did not accept the Kubernetes upgrade plan", target.node.Name)
		}
		report.Nodes = append(report.Nodes, kubernetesUpgradeNodeReport{Name: target.node.Name, Role: string(target.node.SystemRole), SourceVersion: target.source, TargetVersion: image.PayloadVersion, Result: "planned"})
	}
	if opts.plan {
		return writeKubernetesUpgradeReport(stdout, opts.output, report)
	}

	report.Nodes = nil
	for i := range targets {
		target := &targets[i]
		nodeReport, err := runKubernetesUpgradeTarget(ctx, topology, opts, *target, image, localArtifact, stderr)
		report.Nodes = append(report.Nodes, nodeReport)
		if err != nil {
			_ = writeKubernetesUpgradeReport(stdout, opts.output, report)
			return err
		}
	}
	report.NextAction = "rollout complete; every upgraded node is healthy on the target Kubernetes version without reboot"
	return writeKubernetesUpgradeReport(stdout, opts.output, report)
}

func runKubernetesUpgradeTarget(ctx context.Context, topology workstation.ResolvedTopology, opts kubernetesUpgradeOptions, target kubernetesUpgradeTarget, image kubernetesbundle.ImageReference, localArtifact *kubernetesUpgradeArtifact, stderr io.Writer) (nodeReport kubernetesUpgradeNodeReport, resultErr error) {
	nodeReport = kubernetesUpgradeNodeReport{Name: target.node.Name, Role: string(target.node.SystemRole), SourceVersion: target.source, TargetVersion: image.PayloadVersion}
	cordoned := false
	if opts.cordon {
		if err := setKubernetesNodeCordon(ctx, opts.kubeconfig, target.node.Name, true); err != nil {
			nodeReport.Result = "cordon-failed"
			return nodeReport, fmt.Errorf("Kubernetes upgrade stopped while cordoning node %s: %w", target.node.Name, err)
		}
		cordoned = true
		defer func() {
			if !cordoned {
				return
			}
			if err := setKubernetesNodeCordon(ctx, opts.kubeconfig, target.node.Name, false); err != nil {
				if resultErr == nil {
					nodeReport.Result = "uncordon-failed"
				}
				nodeReport.NextAction = "uncordon the node after confirming it is healthy"
				resultErr = errors.Join(resultErr, fmt.Errorf("uncordon node %s: %w", target.node.Name, err))
			}
		}()
	}
	body := kubernetesUpgradeBody(target, image, localArtifact)
	if localArtifact != nil {
		localRef, err := stageKubernetesUpgradeArtifact(ctx, target.conn.Client, target, *localArtifact, target.node.Name, stderr)
		if err != nil {
			nodeReport.Result = "upload-failed"
			return nodeReport, err
		}
		body.TargetSysextPath = kubernetesUpgradeArtifactLogicalPath(localRef)
	}
	accepted, err := target.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest",
		ClientRequestId: "katlctl-" + target.candidate, OperationKind: "kubeadm-upgrade",
		Actor: "katlctl kubernetes upgrade", ExpectedMachineId: target.machineID,
		ExpectedEnrollmentId: target.node.EnrollmentID, ExpectedInventoryNodeName: target.node.Name,
		ExpectedCurrentGenerationId: target.generation, OperationTimeout: opts.timeout.String(),
		KubernetesSysextUpdate: body,
	})
	if err != nil {
		nodeReport.Result = "submit-failed"
		return nodeReport, fmt.Errorf("submit node %s: %w", target.node.Name, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	terminal, err := waitKubernetesUpgrade(waitCtx, target.conn.Client, accepted.OperationId, target.node.Name, stderr)
	cancel()
	if err != nil {
		nodeReport.Result = "wait-failed"
		return nodeReport, fmt.Errorf("wait for node %s: %w", target.node.Name, err)
	}
	nodeReport.Result = terminal.Result
	nodeReport.Phase = terminal.Phase
	nodeReport.RecoveryRequired = terminal.RecoveryRequired
	nodeReport.NextAction = terminal.NextAction
	if terminal.Result != operation.ResultSucceeded {
		return nodeReport, fmt.Errorf("Kubernetes upgrade stopped at node %s: %s", target.node.Name, terminal.FailureReason)
	}
	if target.node.SystemRole == inventory.RoleControlPlane {
		readyCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		err := waitKubernetesEndpoint(readyCtx, topology.ControlPlaneEndpoint, target.node.Name, stderr)
		cancel()
		if err != nil {
			nodeReport.Result = "cluster-health-failed"
			return nodeReport, fmt.Errorf("Kubernetes upgrade stopped after node %s: %w", target.node.Name, err)
		}
	}
	nodeReport.Phase = "healthy"
	nodeReport.NextAction = ""
	return nodeReport, nil
}

func setKubernetesNodeCordon(ctx context.Context, kubeconfig, node string, cordon bool) error {
	verb := "uncordon"
	if cordon {
		verb = "cordon"
	}
	result, err := operatorKubectlRunner.Run(ctx, []string{"kubectl", "--kubeconfig", filepath.Clean(kubeconfig), verb, node})
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("kubectl %s failed: %s", verb, inventory.Redact(strings.TrimSpace(result.Stderr)))
	}
	return nil
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
	if explicit != "" {
		return explicit, nil
	}
	if version == "" {
		return "", fmt.Errorf("spec.kubernetes.version is required")
	}
	selection, err := kubernetescompat.Resolve(kubernetescompat.Request{KubernetesVersion: version})
	if err != nil {
		return "", fmt.Errorf("resolve Kubernetes upgrade version: %w", err)
	}
	return selection.Bundle, nil
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

func resolveKubernetesUpgradeTopology(opts kubernetesUpgradeOptions) (workstation.ResolvedTopology, string, error) {
	path := strings.TrimSpace(opts.clusterConfig)
	if path == "" {
		return workstation.ResolvedTopology{}, "", fmt.Errorf("--config is required; use --config cluster.yaml after setting spec.kubernetes.version in the Git-managed ClusterConfig")
	}
	resolved, err := resolveClusterConfigTopology(path)
	if err != nil {
		return workstation.ResolvedTopology{}, "", err
	}
	mergeEnrolledTopology(&resolved, strings.TrimSpace(opts.configPath), strings.TrimSpace(opts.contextName))
	version, err := kubernetesUpgradeConfigVersion(path)
	if err != nil {
		return workstation.ResolvedTopology{}, "", err
	}
	return resolved, version, nil
}

func kubernetesUpgradeConfigVersion(path string) (string, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("read --config %s: %w", path, err)
	}
	if source, sourceErr := configbundle.DecodeSource(bytes.NewReader(data)); sourceErr == nil {
		version := strings.TrimSpace(source.Spec.Kubernetes.Version)
		if version == "" {
			return "", fmt.Errorf("read --config %s: spec.kubernetes.version is required", path)
		}
		return version, nil
	}
	bundle, err := configbundle.ReadBundle(bytes.NewReader(data), "")
	if err != nil {
		return "", fmt.Errorf("read --config %s as ClusterConfig YAML or Katl config bundle: %w", path, err)
	}
	version := ""
	for _, payload := range bundle.Manifest.Cluster.KubernetesPayloads {
		candidate := strings.TrimSpace(payload.RequestedVersion)
		if candidate == "" {
			candidate = strings.TrimSpace(payload.ResolvedPayloadVersion)
		}
		if candidate == "" {
			continue
		}
		if version != "" && candidate != version {
			return "", fmt.Errorf("read --config %s: compiled bundle contains multiple desired Kubernetes versions", path)
		}
		version = candidate
	}
	if version == "" {
		return "", fmt.Errorf("read --config %s: compiled bundle has no spec.kubernetes.version", path)
	}
	return version, nil
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
		conn, err := dialKatlcAgent(ctx, node.ManagementEndpoint)
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
		if err := verifyEnrolledStatus(managementTarget{nodeName: node.Name, endpoint: node.ManagementEndpoint, enrollmentID: node.EnrollmentID, machineID: node.MachineID}, status); err != nil {
			_ = conn.Close()
			closeTargets()
			return nil, err
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
		architecture, _ := nodeArtifactArchitecture(gen)
		candidate := kubernetesUpgradeCandidate(targetVersion, node.Name, runID)
		inspected = append(inspected, kubernetesUpgradeTarget{node: node, conn: conn, machineID: status.MachineId, generation: generationID, source: sourceVersion, candidate: candidate, architecture: architecture})
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
		target.upgradeRole = "worker"
		if target.node.SystemRole == inventory.RoleControlPlane {
			target.upgradeRole = "control-plane"
			if !applySelected {
				target.upgradeRole = "apply"
				applySelected = true
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func kubernetesUpgradeBody(target kubernetesUpgradeTarget, image kubernetesbundle.ImageReference, localArtifact *kubernetesUpgradeArtifact) *agentapi.KubernetesSysextUpdateOperationRequest {
	body := &agentapi.KubernetesSysextUpdateOperationRequest{
		TargetPayloadVersion: targetVersion(image), CandidateGenerationId: target.candidate,
		UpgradeRole: target.upgradeRole, SourcePayloadVersion: target.source,
		KubernetesBundleSource: image.Source, KubernetesBundleRef: image.Value,
	}
	if localArtifact != nil {
		body.KubernetesBundleSource = ""
		body.KubernetesBundleRef = ""
		body.TargetSysextPath = kubernetesUpgradeArtifactLogicalPath(kubernetesUpgradeArtifactLocalRef(localArtifact.SHA256))
		body.TargetSysextSha256 = localArtifact.SHA256
		body.TargetSysextSizeBytes = localArtifact.SizeBytes
	}
	return body
}

func targetVersion(image kubernetesbundle.ImageReference) string { return image.PayloadVersion }

func readKubernetesUpgradeArtifact(path string) (kubernetesUpgradeArtifact, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return kubernetesUpgradeArtifact{}, fmt.Errorf("resolve path: %w", err)
	}
	meta, err := artifact.ReadLocal(absolute + ".json")
	if err != nil {
		return kubernetesUpgradeArtifact{}, err
	}
	if meta.Name != sysextcatalog.KubernetesName || meta.Kind != artifact.ArtifactSysext || meta.Format != "sysext" {
		return kubernetesUpgradeArtifact{}, fmt.Errorf("metadata does not describe a Kubernetes sysext")
	}
	if err := meta.VerifyFile(absolute); err != nil {
		return kubernetesUpgradeArtifact{}, err
	}
	entry, err := sysextcatalog.EntryFromLocalMeta(meta)
	if err != nil {
		return kubernetesUpgradeArtifact{}, err
	}
	if err := sysextcatalog.ValidateForRuntime(entry, sysextcatalog.Runtime{Interface: "katl-runtime-1", Architecture: meta.Architecture}); err != nil {
		return kubernetesUpgradeArtifact{}, err
	}
	return kubernetesUpgradeArtifact{
		Path: absolute, PayloadVersion: meta.PayloadVersion, Architecture: meta.Architecture,
		SHA256: meta.SHA256, SizeBytes: uint64(meta.SizeBytes),
	}, nil
}

func kubernetesUpgradeArtifactLocalRef(digest string) string {
	return "kubernetes-upgrade/uploads/" + digest + ".raw"
}

func kubernetesUpgradeArtifactLogicalPath(localRef string) string {
	return filepath.ToSlash(filepath.Join("/var/lib/katl/artifacts", localRef))
}

func stageKubernetesUpgradeArtifact(ctx context.Context, client agentapi.KatlcAgentClient, target kubernetesUpgradeTarget, local kubernetesUpgradeArtifact, node string, stderr io.Writer) (string, error) {
	return stageLocalUpgradeArtifact(ctx, client, localArtifactUpload{
		Path: local.Path, Label: "Kubernetes", Version: local.PayloadVersion,
		Kind: "StageKubernetesUpgradeArtifactRequest", Actor: "katlctl kubernetes upgrade",
		MachineID: target.machineID, EnrollmentID: target.node.EnrollmentID, InventoryNodeName: target.node.Name, CurrentGenerationID: target.generation,
		SHA256: local.SHA256, SizeBytes: local.SizeBytes,
		LocalRef: kubernetesUpgradeArtifactLocalRef(local.SHA256), Node: node,
	}, stderr)
}

func kubernetesUpgradeCandidate(version, node, runID string) string {
	version = strings.NewReplacer("v", "", ".", "-").Replace(strings.TrimSpace(version))
	return "kubernetes-" + version + "-" + node + "-" + runID
}

func writeKubernetesUpgradeReport(stdout io.Writer, output string, report kubernetesUpgradeReport) error {
	if output == "text" {
		action := "Kubernetes upgrade"
		if report.Plan {
			action = "Kubernetes upgrade plan"
		}
		fmt.Fprintf(stdout, "%s: %s -> %s\n", action, report.SourceVersion, report.TargetVersion)
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tROLE\tRESULT\tPHASE")
		for _, node := range report.Nodes {
			phase := node.Phase
			if phase == "" {
				phase = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", node.Name, node.Role, node.Result, phase)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if report.NextAction != "" {
			_, err := fmt.Fprintf(stdout, "Next action: %s\n", report.NextAction)
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
