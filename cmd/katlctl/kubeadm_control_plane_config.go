package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
)

type kubeadmControlPlaneConfigOptions struct {
	plan                                                             bool
	output, mode                                                     string
	timeout                                                          time.Duration
	configPath, inventoryPath, coordinator, generationID, configName string
	rolloutID, component                                             string
	selectedNodes                                                    []string
	progress                                                         io.Writer
	destructiveStorageAcknowledgements                               []string
	volumeRebinds                                                    []string
}

var kubeadmConfigNow = func() time.Time { return time.Now().UTC() }

func newClusterApplyCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := kubeadmControlPlaneConfigOptions{output: "text", mode: generation.ApplyModeAuto, timeout: 30 * time.Minute}
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply configuration to running cluster nodes",
		Long: `Apply the desired ClusterConfig to running nodes. By default, apply to every
node in the config; use --node NAME to select a node, repeating it for more nodes.
Keep the complete ClusterConfig when selecting nodes.

Validates selected nodes before applying supported host and Kubernetes changes.
Unchanged configuration is a no-op. Changes that require a reboot are staged for
the next boot and reported. Use 'katlctl node join NODE --config cluster.yaml'
to join an installed node to an existing cluster. Apply never joins nodes.
Kubernetes settings shared by the cluster can still affect the whole cluster.

Use --plan to validate the selected nodes and preview host changes without
accepting operations. Kubernetes component readiness is checked during apply.
--mode live refuses changes that need reboot; --mode next-boot stages host changes.

Apply does not remove nodes omitted from the config or change an enrolled node's
name or role. See docs/operations/configure-nodes.md for configuration workflows.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runClusterApply(ctx, opts, stdout, stderr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	f.StringArrayVar(&opts.selectedNodes, "node", nil, "apply to this config node only (repeatable; default: all nodes)")
	f.StringVar(&opts.inventoryPath, "inventory", "", "advanced cluster inventory")
	f.StringVar(&opts.coordinator, "coordinator", "", "selected control-plane coordinator changed last")
	f.BoolVar(&opts.plan, "plan", false, "validate and preview changes without accepting operations")
	f.StringVar(&opts.mode, "mode", opts.mode, "apply mode: auto, live, or next-boot")
	f.DurationVar(&opts.timeout, "timeout", opts.timeout, "overall validation and apply timeout")
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
	f.StringVar(&opts.generationID, "generation", "", "active desired generation ID")
	f.StringVar(&opts.configName, "config-name", "", "selected KubeadmConfig name")
	f.StringVar(&opts.rolloutID, "rollout-id", "", "rollout identity")
	f.StringArrayVar(&opts.destructiveStorageAcknowledgements, "acknowledge-storage-wipe", nil, "deprecated: wipe intent is configured by wipe: true")
	_ = f.MarkHidden("acknowledge-storage-wipe")
	f.StringArrayVar(&opts.volumeRebinds, "rebind-volume", nil, "authorize replacing one generation-bound volume identity as NODE/VOLUME (repeatable)")
	for _, name := range []string{"inventory", "generation", "config-name", "rollout-id"} {
		cmd.Flags().Lookup(name).Hidden = true
	}
	return cmd
}

func runClusterApply(ctx context.Context, opts kubeadmControlPlaneConfigOptions, stdout, stderr io.Writer) error {
	if opts.mode == "" {
		opts.mode = generation.ApplyModeAuto
	}
	if opts.mode != generation.ApplyModeAuto && opts.mode != generation.ApplyModeLive && opts.mode != generation.ApplyModeNextBoot {
		return fmt.Errorf("--mode must be auto, live, or next-boot")
	}
	if opts.output == "" {
		opts.output = "text"
	}
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	if opts.timeout == 0 {
		opts.timeout = 30 * time.Minute
	}
	if opts.timeout < 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	if opts.plan && strings.TrimSpace(opts.configPath) == "" {
		return fmt.Errorf("--config is required for --plan")
	}
	opts.coordinator = strings.TrimSpace(opts.coordinator)
	if len(opts.selectedNodes) > 0 && opts.coordinator != "" && !slices.Contains(opts.selectedNodes, opts.coordinator) {
		return fmt.Errorf("coordinator %q is not selected; include it with --node or omit --coordinator", opts.coordinator)
	}
	refreshed, err := refreshConfiguredManagement(ctx, opts.configPath, "", "", stderr, opts.selectedNodes...)
	if err != nil {
		return err
	}
	ctx = refreshed
	opts.progress = stderr
	acknowledgements, err := normalizeDestructiveStorageAcknowledgements(opts.destructiveStorageAcknowledgements)
	if err != nil {
		return err
	}
	opts.destructiveStorageAcknowledgements = acknowledgements
	rebinds, err := normalizeVolumeRebinds(opts.volumeRebinds)
	if err != nil {
		return err
	}
	opts.volumeRebinds = rebinds
	inv, err := kubeadmConfigInventory(ctx, opts)
	if err != nil {
		return err
	}
	selected, err := selectConfigNodes(inv.Nodes, opts.selectedNodes)
	if err != nil {
		return err
	}
	if err := clusterApplyProgress(opts.progress, "phase=configuration status=started nodes=%d", len(selected)); err != nil {
		return err
	}
	if strings.TrimSpace(opts.rolloutID) == "" {
		opts.rolloutID = "cluster-config-" + strconv.FormatInt(kubeadmConfigNow().UnixNano(), 10)
	}
	generations := map[string]string{}
	components := map[string]bool{
		"control-plane": true,
		"kubelet":       true,
	}
	preBootstrap := false
	var stagedNodes []string
	var nodePlans []clusterConfigNodePlan
	if strings.TrimSpace(opts.configPath) != "" {
		activated, err := activateClusterConfig(ctx, opts, inv.Nodes)
		if err != nil {
			return err
		}
		generations = activated.generations
		components = activated.components
		preBootstrap = activated.preBootstrap
		stagedNodes = activated.stagedNodes
		nodePlans = activated.nodePlans
	} else if strings.TrimSpace(opts.generationID) == "" {
		return fmt.Errorf("--generation is required with --inventory")
	}

	results := map[string]any{}
	if opts.plan {
		return writeClusterApplyReport(stdout, opts.output, clusterApplyReport{Nodes: len(selected), NodePlans: nodePlans, Result: "planned", RebootRequired: len(stagedNodes) > 0, StagedNodes: stagedNodes})
	}
	for _, component := range []string{"control-plane", "kubelet", "kube-proxy"} {
		if !components[component] {
			continue
		}
		if len(stagedNodes) > 0 {
			if err := clusterApplyProgress(opts.progress, "component=%s status=skipped reason=node-config-reboot-required", component); err != nil {
				return err
			}
			results[component] = map[string]string{
				"component": component,
				"reason":    "node-config-reboot-required",
				"result":    "skipped",
			}
			continue
		}
		if preBootstrap {
			if err := clusterApplyProgress(opts.progress, "component=%s status=skipped reason=kubernetes-not-configured", component); err != nil {
				return err
			}
			results[component] = map[string]string{
				"component": component,
				"reason":    "kubernetes-not-configured",
				"result":    "skipped",
			}
			continue
		}
		componentOpts := opts
		componentOpts.component = component
		componentOpts.rolloutID = opts.rolloutID + "-" + component
		if err := clusterApplyProgress(opts.progress, "component=%s status=started", component); err != nil {
			return err
		}
		summary, err := runKubeadmConfigComponent(ctx, componentOpts, inv, generations)
		if err != nil {
			return err
		}
		results[component] = summary
		if err := clusterApplyProgress(opts.progress, "component=%s status=succeeded", component); err != nil {
			return err
		}
	}
	return writeClusterApplyReport(stdout, opts.output, clusterApplyReport{Nodes: len(selected), NodePlans: nodePlans, Kubernetes: results, Result: "succeeded", RebootRequired: len(stagedNodes) > 0, StagedNodes: stagedNodes})
}

type clusterConfigNodePlan struct {
	Node           string   `json:"node"`
	ApplyMode      string   `json:"applyMode"`
	NoChanges      bool     `json:"noChanges"`
	ChangedDomains []string `json:"changedDomains,omitempty"`
}

type clusterApplyReport struct {
	Nodes          int                     `json:"nodes"`
	NodePlans      []clusterConfigNodePlan `json:"nodePlans"`
	Kubernetes     map[string]any          `json:"kubernetes,omitempty"`
	Result         string                  `json:"result"`
	RebootRequired bool                    `json:"rebootRequired,omitempty"`
	StagedNodes    []string                `json:"stagedNodes,omitempty"`
}

func writeClusterApplyReport(stdout io.Writer, format string, report clusterApplyReport) error {
	if format == "json" {
		return json.NewEncoder(stdout).Encode(report)
	}
	w := newTable(stdout)
	w.row("NODE", "CHANGE", "DOMAINS")
	for _, node := range report.NodePlans {
		change := node.ApplyMode
		if node.NoChanges {
			change = "unchanged"
		}
		if node.ApplyMode == generation.ApplyModeNextBoot {
			change = "next boot (reboot required)"
		}
		w.row(node.Node, change, strings.Join(node.ChangedDomains, ", "))
	}
	if err := w.flush(); err != nil {
		return err
	}
	if report.Result == "planned" {
		_, err := fmt.Fprintln(stdout, "Plan complete; no operations accepted. Run without --plan to apply.")
		return err
	}
	if report.RebootRequired {
		_, err := fmt.Fprintf(stdout, "Configuration staged. Reboot %s with 'katlctl node reboot NODE --config CONFIG', then rerun cluster apply.\n", strings.Join(report.StagedNodes, ", "))
		return err
	}
	_, err := fmt.Fprintf(stdout, "Configuration applied to %d node(s).\n", report.Nodes)
	return err
}

func runKubeadmControlPlaneConfig(ctx context.Context, opts kubeadmControlPlaneConfigOptions, stdout io.Writer) error {
	if strings.TrimSpace(opts.component) == "" {
		opts.component = "control-plane"
	}
	if opts.component != "control-plane" && opts.component != "kubelet" && opts.component != "kube-proxy" {
		return fmt.Errorf("internal component = %q, want control-plane, kubelet, or kube-proxy", opts.component)
	}
	inv, err := kubeadmConfigInventory(ctx, opts)
	if err != nil {
		return err
	}
	generations := map[string]string{}
	if strings.TrimSpace(opts.configPath) != "" {
		activated, err := activateClusterConfig(ctx, opts, inv.Nodes)
		if err != nil {
			return err
		}
		generations = activated.generations
		if len(activated.stagedNodes) > 0 {
			return fmt.Errorf("node configuration staged for next boot on %s; reboot and wait for healthy promotion before applying Kubernetes configuration", strings.Join(activated.stagedNodes, ", "))
		}
	}
	summary, err := runKubeadmConfigComponent(ctx, opts, inv, generations)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(summary)
}

func runKubeadmConfigComponent(ctx context.Context, opts kubeadmControlPlaneConfigOptions, inv inventory.Inventory, generations map[string]string) (map[string]any, error) {
	var controlPlanes []inventory.Node
	for _, node := range inv.Nodes {
		if node.SystemRole == inventory.RoleControlPlane {
			controlPlanes = append(controlPlanes, node)
		}
	}
	if len(controlPlanes) == 0 {
		return nil, fmt.Errorf("at least one control-plane node is required")
	}
	if opts.coordinator != "" && !slices.ContainsFunc(controlPlanes, func(node inventory.Node) bool { return node.Name == opts.coordinator }) {
		return nil, fmt.Errorf("coordinator %q is not a control-plane node", opts.coordinator)
	}
	if strings.TrimSpace(opts.coordinator) == "" {
		sort.Slice(controlPlanes, func(i, j int) bool { return controlPlanes[i].Name < controlPlanes[j].Name })
		opts.coordinator = controlPlanes[len(controlPlanes)-1].Name
		for _, node := range controlPlanes {
			if len(opts.selectedNodes) == 0 || slices.Contains(opts.selectedNodes, node.Name) {
				opts.coordinator = node.Name
			}
		}
	}
	if strings.TrimSpace(opts.rolloutID) == "" {
		opts.rolloutID = "kubeadm-config-" + strconv.FormatInt(kubeadmConfigNow().UnixNano(), 10)
	}
	var nodes []inventory.Node
	var err error
	if opts.component == "kube-proxy" {
		ordered, orderErr := orderControlPlanes(controlPlanes, opts.coordinator)
		if orderErr != nil {
			return nil, orderErr
		}
		nodes = []inventory.Node{ordered[len(ordered)-1]}
	} else if opts.component == "kubelet" {
		nodes, err = orderKubeletNodes(inv.Nodes, controlPlanes, opts.coordinator)
	} else {
		nodes, err = orderControlPlanes(controlPlanes, opts.coordinator)
	}
	if err != nil {
		return nil, err
	}
	if len(opts.selectedNodes) > 0 {
		nodes = slices.DeleteFunc(nodes, func(node inventory.Node) bool {
			return !slices.Contains(opts.selectedNodes, node.Name)
		})
	}
	type target struct {
		node           inventory.Node
		conn           katlcAgentConnection
		machine        string
		payloadVersion string
		generation     string
	}
	targets := make([]target, 0, len(nodes))
	defer func() {
		for _, t := range targets {
			_ = t.conn.Close()
		}
	}()
	for _, node := range nodes {
		nodeCtx, err := managementContextForNode(ctx, opts.configPath, node.Name)
		if err != nil {
			return nil, err
		}
		conn, err := dialKatlcAgent(nodeCtx, cluster.AgentEndpoint(node.Address, "9443"))
		if err != nil {
			return nil, fmt.Errorf("connect %s: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if err != nil {
			return nil, fmt.Errorf("status %s: %w", node.Name, err)
		}
		if err := verifyPlannedStatus(managementTarget{nodeName: node.Name, endpoint: cluster.AgentEndpoint(node.Address, "9443"), enrollmentID: node.EnrollmentID, machineID: node.MachineID}, status); err != nil {
			return nil, err
		}
		generationID := strings.TrimSpace(opts.generationID)
		if value := strings.TrimSpace(generations[node.Name]); value != "" {
			generationID = value
		}
		gen, err := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: generationID, IncludeConfigApply: true})
		if err != nil {
			return nil, fmt.Errorf("generation %s on %s: %w", generationID, node.Name, err)
		}
		if gen.CommitState != "committed" || gen.HealthState != "healthy" {
			return nil, fmt.Errorf("node %s generation %s is not committed and healthy", node.Name, generationID)
		}
		configName := strings.TrimSpace(opts.configName)
		if configName == "" {
			configName = strings.TrimSpace(node.KubeadmConfig.Ref)
		}
		if gen.ConfigApply != nil && strings.TrimSpace(gen.ConfigApply.SelectedKubeadmConfigName) != "" && gen.ConfigApply.SelectedKubeadmConfigName != configName {
			return nil, fmt.Errorf("node %s generation %s selects kubeadm config %q instead of %q", node.Name, generationID, gen.ConfigApply.SelectedKubeadmConfigName, configName)
		}
		node.KubeadmConfig.Ref = configName
		payloadVersion := ""
		for _, ref := range gen.Sysexts {
			if ref.Name == "kubernetes" && ref.PayloadVersion != "" && ref.Sha256 != "" {
				payloadVersion = ref.PayloadVersion
				break
			}
		}
		if payloadVersion == "" {
			return nil, fmt.Errorf("node %s generation %s has no active Kubernetes payload", node.Name, generationID)
		}
		if len(targets) > 0 && payloadVersion != targets[0].payloadVersion {
			return nil, fmt.Errorf("node %s active Kubernetes payload version does not match %s", node.Name, targets[0].node.Name)
		}
		targets = append(targets, target{node: node, conn: conn, machine: status.MachineId, payloadVersion: payloadVersion, generation: generationID})
	}
	var summary []map[string]string
	for i, t := range targets {
		body := kubeadmControlPlaneConfigBody(opts, t.node, t.generation, uint32(i+1), uint32(len(targets)))
		accepted, err := t.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: opts.rolloutID + "-dry-run-" + t.node.Name, OperationKind: "kubeadm-control-plane-config", Actor: "katlctl cluster apply", ExpectedEnrollmentId: t.node.EnrollmentID, ExpectedInventoryNodeName: t.node.Name, ExpectedMachineId: t.machine, ExpectedCurrentGenerationId: t.generation, DryRun: true, KubeadmControlPlaneConfig: body})
		if err != nil {
			return nil, fmt.Errorf("dry-run %s: %w", t.node.Name, err)
		}
		if accepted.InitialStatus == nil || accepted.InitialStatus.Phase != "dry-run" {
			return nil, fmt.Errorf("node %s did not confirm kubeadm rollout dry-run", t.node.Name)
		}
	}
	if err := clusterApplyProgress(opts.progress, "component=%s phase=preflight status=succeeded nodes=%d", opts.component, len(targets)); err != nil {
		return nil, err
	}
	for i, t := range targets {
		body := kubeadmControlPlaneConfigBody(opts, t.node, t.generation, uint32(i+1), uint32(len(targets)))
		if err := clusterApplyProgress(opts.progress, "component=%s node=%s phase=apply status=started", opts.component, t.node.Name); err != nil {
			return nil, err
		}
		accepted, err := t.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: opts.rolloutID + "-" + t.node.Name, OperationKind: "kubeadm-control-plane-config", Actor: "katlctl cluster apply", ExpectedEnrollmentId: t.node.EnrollmentID, ExpectedInventoryNodeName: t.node.Name, ExpectedMachineId: t.machine, ExpectedCurrentGenerationId: t.generation, KubeadmControlPlaneConfig: body})
		if err != nil {
			return nil, fmt.Errorf("submit %s: %w", t.node.Name, err)
		}
		terminal, err := waitKubeadmControlPlaneConfig(ctx, t.conn.Client, accepted.OperationId, opts.component, t.node.Name, opts.progress)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", t.node.Name, err)
		}
		if terminal.Result != operation.ResultSucceeded {
			return nil, fmt.Errorf("node %s stopped rollout: %s: %s", t.node.Name, terminal.Phase, terminal.FailureReason)
		}
		if err := clusterApplyProgress(opts.progress, "component=%s node=%s phase=%s status=succeeded", opts.component, t.node.Name, firstNonEmpty(terminal.Phase, "complete")); err != nil {
			return nil, err
		}
		summary = append(summary, map[string]string{"node": t.node.Name, "result": terminal.Result})
	}
	return map[string]any{"component": opts.component, "coordinator": opts.coordinator, "nodes": summary, "automaticRollback": false}, nil
}

func kubeadmControlPlaneConfigBody(opts kubeadmControlPlaneConfigOptions, node inventory.Node, generationID string, position, count uint32) *agentapi.KubeadmControlPlaneConfigOperationRequest {
	component := kubeadmConfigComponentControlPlane
	if opts.component == "kube-proxy" {
		component = kubeadmConfigComponentKubeProxy
	} else if opts.component == "kubelet" {
		component = kubeadmConfigComponentKubelet
	}
	localKubelet := component == kubeadmConfigComponentKubelet && node.KubeadmConfig.NodeLocalKubelet
	return &agentapi.KubeadmControlPlaneConfigOperationRequest{RolloutId: opts.rolloutID, NodePosition: position, NodeCount: count, NodeName: node.Name, CoordinatorNode: opts.coordinator, CoordinatorUpload: node.Name == opts.coordinator && !localKubelet, NodeLocalKubelet: localKubelet, DesiredGenerationId: generationID, ConfigName: node.KubeadmConfig.Ref, SupportedFieldDelta: []string{component}}
}

const (
	kubeadmConfigComponentControlPlane = "component/control-plane"
	kubeadmConfigComponentKubelet      = "component/kubelet"
	kubeadmConfigComponentKubeProxy    = "component/kube-proxy"
)

func kubeadmConfigInventory(ctx context.Context, opts kubeadmControlPlaneConfigOptions) (inventory.Inventory, error) {
	configPath := strings.TrimSpace(opts.configPath)
	inventoryPath := strings.TrimSpace(opts.inventoryPath)
	if (configPath == "") == (inventoryPath == "") {
		if configPath == "" {
			return inventory.Inventory{}, fmt.Errorf("--config is required; use --config ./cluster.yaml")
		}
		return inventory.Inventory{}, fmt.Errorf("exactly one of --config or --inventory is required")
	}
	if inventoryPath != "" {
		inv, err := loadInventory(inventoryPath)
		if err != nil {
			return inventory.Inventory{}, err
		}
		return overlayWipeContext(ctx, inv, "", "", configPath)
	}
	inv, err := loadWipeInventory(ctx, configPath, "", opts.progress)
	if err != nil {
		return inventory.Inventory{}, err
	}
	return overlayWipeContext(ctx, inv, "", "", configPath)
}

func selectConfigNodes(nodes []inventory.Node, names []string) ([]inventory.Node, error) {
	if len(names) == 0 {
		return nodes, nil
	}
	for _, name := range names {
		if !slices.ContainsFunc(nodes, func(node inventory.Node) bool { return node.Name == name }) {
			return nil, fmt.Errorf("node %q is not in the config; use a node name from the complete ClusterConfig", name)
		}
	}
	var selected []inventory.Node
	for _, node := range nodes {
		if slices.Contains(names, node.Name) {
			selected = append(selected, node)
		}
	}
	return selected, nil
}

type activatedClusterConfig struct {
	nodePlans    []clusterConfigNodePlan
	generations  map[string]string
	components   map[string]bool
	preBootstrap bool
	stagedNodes  []string
}

func activateClusterConfig(ctx context.Context, opts kubeadmControlPlaneConfigOptions, nodes []inventory.Node) (activatedClusterConfig, error) {
	if opts.mode == "" {
		opts.mode = generation.ApplyModeAuto
	}
	if opts.mode != generation.ApplyModeAuto && opts.mode != generation.ApplyModeLive && opts.mode != generation.ApplyModeNextBoot {
		return activatedClusterConfig{}, fmt.Errorf("--mode = %q, want auto, live, or next-boot", opts.mode)
	}
	loaded, err := loadKatlConfig(opts.configPath, configBundleCreator, configbundle.PlanningInputs{}, nil)
	if err != nil {
		return activatedClusterConfig{}, err
	}
	for index := range nodes {
		target, ok := enrolledTarget(ctx, "", "", loaded.Bundle.Manifest.ClusterName, nodes[index].Name)
		if !ok {
			continue
		}
		host, _, splitErr := net.SplitHostPort(target.endpoint)
		if splitErr != nil {
			return activatedClusterConfig{}, fmt.Errorf("node %q enrolled management endpoint: %w", nodes[index].Name, splitErr)
		}
		nodes[index].Address = host
		nodes[index].EnrollmentID = target.enrollmentID
		nodes[index].MachineID = target.machineID
	}
	now := kubeadmConfigNow()
	generationID := strings.TrimSpace(opts.generationID)
	if generationID == "" {
		generationID = "cluster-config-" + strconv.FormatInt(now.UnixNano(), 10)
	}
	desiredVersion := strconv.FormatInt(now.UnixNano(), 10)
	type preparedInput struct {
		node              inventory.Node
		configYAML        []byte
		machineID         string
		currentGeneration string
		kubernetesState   string
		noChanges         bool
		acceptedApplyMode string
		changedDomains    []string
		components        []string
	}
	selected, err := selectConfigNodes(nodes, opts.selectedNodes)
	if err != nil {
		return activatedClusterConfig{}, err
	}
	prepared := make([]preparedInput, 0, len(selected))
	components := map[string]bool{}
	for _, node := range selected {
		if err := clusterApplyProgress(opts.progress, "phase=config-validation node=%s status=started", node.Name); err != nil {
			return activatedClusterConfig{}, err
		}
		selected, err := configbundle.ReadSelectedNode(bytes.NewReader(loaded.Archive), configbundle.ReadOptions{NodeName: node.Name, AllowMissingKatlosImage: true})
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("select cluster config for %s: %w", node.Name, err)
		}
		plan, ok := selected.KubeadmConfigs[node.KubeadmConfig.Ref]
		if !ok {
			return activatedClusterConfig{}, fmt.Errorf("selected kubeadm input %q for %s is missing", node.KubeadmConfig.Ref, node.Name)
		}
		var nodeComponents []string
		for _, document := range plan.Documents {
			switch document.Kind {
			case "ClusterConfiguration":
				nodeComponents = append(nodeComponents, "control-plane")
			case "KubeletConfiguration":
				nodeComponents = append(nodeComponents, "kubelet")
			case "KubeProxyConfiguration":
				nodeComponents = append(nodeComponents, "kube-proxy")
			}
		}
		configYAML, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{
			NodeName: selected.Node.Name, Manifest: selected.InstallManifest, KubeadmConfigs: selected.KubeadmConfigs,
			SourceID: selected.BundleManifest.ClusterName, DesiredVersion: desiredVersion, ApplyMode: opts.mode,
			SystemExtensionPayloads: configApplySystemExtensionPayloads(selected.SystemExtensionPayloads),
			APIProxy:                selected.NodeMaterial.APIProxy,
		})
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("render cluster config for %s: %w", node.Name, err)
		}
		prepared = append(prepared, preparedInput{node: node, configYAML: configYAML, components: nodeComponents})
	}
	result := make(map[string]string, len(nodes))
	for i := range prepared {
		input := &prepared[i]
		node := input.node
		nodeCtx, err := managementContextForNode(ctx, opts.configPath, node.Name)
		if err != nil {
			return activatedClusterConfig{}, err
		}
		conn, err := dialKatlcAgent(nodeCtx, cluster.AgentEndpoint(node.Address, "9443"))
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("connect %s to apply cluster config: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("status %s before cluster config apply: %w", node.Name, err)
		}
		if err := verifyPlannedStatus(managementTarget{nodeName: node.Name, endpoint: cluster.AgentEndpoint(node.Address, "9443"), enrollmentID: node.EnrollmentID, machineID: node.MachineID}, status); err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, err
		}
		if err := validateClusterNodeLifecycle(node, status); err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, err
		}
		if isClusterJoinCandidate(status.GetCurrentGenerationId()) {
			candidate, generationErr := conn.Client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: status.GetCurrentGenerationId()})
			if generationErr != nil {
				_ = conn.Close()
				return activatedClusterConfig{}, fmt.Errorf("inspect replacement generation on %s: %w", node.Name, generationErr)
			}
			if candidate.GetCommitState() != generation.CommitStateCommitted || candidate.GetHealthState() != generation.HealthStateHealthy {
				_ = conn.Close()
				return activatedClusterConfig{}, fmt.Errorf("node %s has an unfinished join; run 'katlctl node join %s --config %s' before applying configuration", node.Name, node.Name, opts.configPath)
			}
		}
		validation, err := conn.Client.ValidateConfig(ctx, &agentapi.ValidateConfigRequest{
			ApiVersion: operation.APIVersion, Kind: "ValidateConfigRequest", ClientRequestId: opts.rolloutID + "-stage-" + node.Name,
			Actor: "katlctl cluster apply", ExpectedEnrollmentId: status.EnrollmentId, ExpectedInventoryNodeName: status.InventoryNodeName,
			ExpectedMachineId: status.MachineId, ExpectedCurrentGenerationId: status.CurrentGenerationId, ApplyMode: opts.mode,
			CandidateGenerationId: generationID, NodeName: node.Name, ConfigYaml: string(input.configYAML),
			DestructiveStorageAcknowledgements: slices.Clone(opts.destructiveStorageAcknowledgements),
			VolumeRebinds:                      slices.Clone(opts.volumeRebinds),
		})
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("validate cluster config on %s: %w", node.Name, err)
		}
		if !validation.Accepted {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("node %s rejected cluster config: %s", node.Name, clusterConfigRejection(node.Name, validation))
		}
		if err := clusterApplyProgress(opts.progress, "phase=config-validation node=%s status=succeeded", node.Name); err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, err
		}
		input.machineID = status.MachineId
		input.currentGeneration = status.CurrentGenerationId
		input.kubernetesState = strings.TrimSpace(status.GetKubernetes().GetState())
		input.noChanges = validation.NoChanges
		input.acceptedApplyMode = validation.AcceptedApplyMode
		input.changedDomains = slices.Clone(validation.ChangedDomains)
		_ = conn.Close()
		if containsKubernetesConfigDomain(validation.ChangedDomains) {
			for _, component := range input.components {
				components[component] = true
			}
		}
		if validation.NoChanges {
			if len(validation.Diagnostics) > 0 {
				return activatedClusterConfig{}, fmt.Errorf("node %s configuration files match, but runtime state is not current: %s", node.Name, strings.Join(validation.Diagnostics, "; "))
			}
			result[node.Name] = status.CurrentGenerationId
			if err := clusterApplyProgress(opts.progress, "phase=node-config node=%s status=unchanged", node.Name); err != nil {
				return activatedClusterConfig{}, err
			}
			continue
		}
		if validation.AcceptedApplyMode == generation.ApplyModeNextBoot && containsKubernetesConfigDomain(validation.ChangedDomains) {
			return activatedClusterConfig{}, fmt.Errorf("node %s requires next-boot mode for a change that also includes Kubernetes configuration; stage host configuration separately before applying Kubernetes configuration online", node.Name)
		}
		if validation.AcceptedApplyMode != generation.ApplyModeLive && validation.AcceptedApplyMode != generation.ApplyModeNextBoot {
			return activatedClusterConfig{}, fmt.Errorf("node %s returned unsupported apply mode %s", node.Name, validation.AcceptedApplyMode)
		}
	}

	notConfigured := 0
	for _, input := range prepared {
		if input.kubernetesState == "not-configured" {
			notConfigured++
		}
	}
	preBootstrap := len(prepared) > 0 && notConfigured == len(prepared)
	if notConfigured > 0 && !preBootstrap {
		for _, input := range prepared {
			if input.kubernetesState == "not-configured" {
				return activatedClusterConfig{}, fmt.Errorf("node %s has not joined Kubernetes; run 'katlctl node join %s --config %s', or use --node to configure nodes separately", input.node.Name, input.node.Name, opts.configPath)
			}
		}
	}
	if preBootstrap {
		for _, input := range prepared {
			for _, component := range input.components {
				components[component] = true
			}
		}
	}

	for _, input := range prepared {
		node := input.node
		if input.noChanges || opts.plan {
			continue
		}
		nodeCtx, err := managementContextForNode(ctx, opts.configPath, node.Name)
		if err != nil {
			return activatedClusterConfig{}, err
		}
		conn, err := dialKatlcAgent(nodeCtx, cluster.AgentEndpoint(node.Address, "9443"))
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("connect %s to apply cluster config: %w", node.Name, err)
		}
		if err := clusterApplyProgress(opts.progress, "phase=node-config node=%s status=started", node.Name); err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, err
		}
		operationKind, err := configApplyOperationKind(input.acceptedApplyMode)
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("apply cluster config on %s: %w", node.Name, err)
		}
		accepted, err := conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
			ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: opts.rolloutID + "-stage-" + node.Name,
			OperationKind: operationKind, Actor: "katlctl cluster apply", ExpectedEnrollmentId: node.EnrollmentID, ExpectedInventoryNodeName: node.Name,
			ExpectedMachineId: input.machineID, ExpectedCurrentGenerationId: input.currentGeneration,
			ConfigApply: &agentapi.ConfigApplyOperationRequest{CandidateGenerationId: generationID, ApplyMode: opts.mode, NodeName: node.Name, ConfigYaml: string(input.configYAML), DestructiveStorageAcknowledgements: slices.Clone(opts.destructiveStorageAcknowledgements), VolumeRebinds: slices.Clone(opts.volumeRebinds)},
		})
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("apply cluster config on %s: %w", node.Name, err)
		}
		terminal, err := waitClusterApplyNodeConfig(ctx, conn.Client, accepted, node.Name, opts.progress)
		if err == nil {
			err = operationResultError(terminal)
		}
		_ = conn.Close()
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("apply cluster config on %s: %w", node.Name, err)
		}
		nodeStatus := "succeeded"
		if input.acceptedApplyMode == generation.ApplyModeNextBoot {
			nodeStatus = "staged-next-boot reboot-required"
		}
		if err := clusterApplyProgress(opts.progress, "phase=node-config node=%s status=%s", node.Name, nodeStatus); err != nil {
			return activatedClusterConfig{}, err
		}
		activatedGeneration := strings.TrimSpace(terminal.GetCandidateGenerationId())
		if activatedGeneration == "" {
			activatedGeneration = generationID
			if terminal.GetGenerationCommitState() == operation.GenerationCommitAbandoned {
				activatedGeneration = input.currentGeneration
			}
		}
		result[node.Name] = activatedGeneration
	}
	var stagedNodes []string
	var nodePlans []clusterConfigNodePlan
	for _, input := range prepared {
		nodePlans = append(nodePlans, clusterConfigNodePlan{Node: input.node.Name, ApplyMode: input.acceptedApplyMode, NoChanges: input.noChanges, ChangedDomains: input.changedDomains})
		if input.acceptedApplyMode == generation.ApplyModeNextBoot {
			stagedNodes = append(stagedNodes, input.node.Name)
		}
	}
	sort.Strings(stagedNodes)
	return activatedClusterConfig{
		nodePlans:    nodePlans,
		generations:  result,
		components:   components,
		preBootstrap: preBootstrap,
		stagedNodes:  stagedNodes,
	}, nil
}

func validateClusterNodeLifecycle(node inventory.Node, status *agentapi.NodeStatus) error {
	kubernetes := status.GetKubernetes()
	if kubernetes == nil || strings.TrimSpace(kubernetes.GetState()) == "" || strings.TrimSpace(kubernetes.GetState()) == "not-configured" {
		return nil
	}
	observedName := strings.TrimSpace(kubernetes.GetNodeName())
	if observedName != "" && observedName != node.Name {
		return fmt.Errorf(
			"refusing operation for node %q at %s: the host is enrolled in Kubernetes as %q; enrolled node rename is unsupported; restore spec.nodes[].name to %q, or plan 'katlctl node wipe %s --config <previous-config> --plan' before reinstalling it with the new name; no node was wiped",
			node.Name, node.Address, observedName, observedName, observedName,
		)
	}
	observedRole := strings.TrimSpace(kubernetes.GetRole())
	if observedRole != "" && observedRole != string(node.SystemRole) {
		return fmt.Errorf(
			"refusing operation for node %q: desired role is %q but the enrolled Kubernetes role is %q; role changes require 'katlctl node wipe %s --config <current-config> --plan', reinstall with the desired role, then node join; no node was wiped",
			node.Name, node.SystemRole, observedRole, node.Name,
		)
	}
	return nil
}

func clusterConfigRejection(node string, validation *agentapi.ConfigValidationResult) string {
	if validation == nil {
		return "node returned an empty validation result"
	}
	diagnostics := strings.TrimSpace(strings.Join(validation.Diagnostics, "; "))
	if strings.Contains(diagnostics, configapply.DomainSystemRole+":") {
		diagnostics += fmt.Sprintf(
			"; keep the node in its current config and plan 'katlctl node wipe %s --config <current-config> --plan', then reinstall it with the desired role and retry; no node was wiped",
			node,
		)
	}
	failure := strings.TrimSpace(validation.FailureReason)
	if diagnostics == "" {
		return firstNonEmpty(failure, "configuration was rejected without a diagnostic")
	}
	if failure == "" || (strings.HasPrefix(failure, "config apply ") && strings.Contains(failure, " request rejected for ")) {
		return diagnostics
	}
	return failure + "; " + diagnostics
}

func containsKubernetesConfigDomain(domains []string) bool {
	for _, domain := range domains {
		switch domain {
		case configapply.DomainKubeadmConfig, configapply.DomainSelectedKubeadmConfig:
			return true
		}
	}
	return false
}

func clusterApplyProgress(w io.Writer, format string, args ...any) error {
	if w == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "cluster apply "+format+"\n", args...)
	return err
}

func waitClusterApplyNodeConfig(ctx context.Context, client agentapi.KatlcAgentClient, accepted *agentapi.OperationAccepted, node string, progress io.Writer) (*agentapi.OperationStatus, error) {
	if accepted == nil || strings.TrimSpace(accepted.OperationId) == "" {
		return nil, fmt.Errorf("agent returned an empty operation acceptance")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	status := accepted.InitialStatus
	lastPhase := ""
	for {
		if status == nil {
			var err error
			status, err = client.GetOperation(waitCtx, &agentapi.GetOperationRequest{OperationId: accepted.OperationId, IncludeDiagnostics: "normal"})
			if err != nil {
				return nil, err
			}
		}
		phase := firstNonEmpty(status.Phase, "pending")
		if phase != lastPhase {
			if err := clusterApplyProgress(progress, "phase=node-config node=%s step=%s status=running", node, phase); err != nil {
				return nil, err
			}
			lastPhase = phase
		}
		if status.Terminal {
			return status, nil
		}
		status = nil
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func orderControlPlanes(nodes []inventory.Node, coordinator string) ([]inventory.Node, error) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	ordered := make([]inventory.Node, 0, len(nodes))
	var coordinatorNode *inventory.Node
	for i := range nodes {
		if nodes[i].Name == coordinator {
			copy := nodes[i]
			coordinatorNode = &copy
		} else {
			ordered = append(ordered, nodes[i])
		}
	}
	if coordinatorNode == nil {
		return nil, fmt.Errorf("coordinator %q is not a control-plane node", coordinator)
	}
	return append(ordered, *coordinatorNode), nil
}

func orderKubeletNodes(nodes, controlPlanes []inventory.Node, coordinator string) ([]inventory.Node, error) {
	orderedControlPlanes, err := orderControlPlanes(controlPlanes, coordinator)
	if err != nil {
		return nil, err
	}
	coordinatorNode := orderedControlPlanes[len(orderedControlPlanes)-1]
	remaining := make([]inventory.Node, 0, len(nodes)-1)
	for _, node := range nodes {
		if node.Name != coordinatorNode.Name {
			remaining = append(remaining, node)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].Name < remaining[j].Name })
	return append([]inventory.Node{coordinatorNode}, remaining...), nil
}

func waitKubeadmControlPlaneConfig(ctx context.Context, client agentapi.KatlcAgentClient, id, component, node string, progress io.Writer) (*agentapi.OperationStatus, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastPhase := ""
	for {
		status, err := client.GetOperation(ctx, &agentapi.GetOperationRequest{OperationId: id, IncludeDiagnostics: "normal"})
		if err != nil {
			return nil, err
		}
		phase := firstNonEmpty(status.Phase, "pending")
		if phase != lastPhase {
			if err := clusterApplyProgress(progress, "component=%s node=%s phase=%s status=running", component, node, phase); err != nil {
				return nil, err
			}
			lastPhase = phase
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
