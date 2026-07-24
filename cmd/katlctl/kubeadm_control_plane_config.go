package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
)

type kubeadmControlPlaneConfigOptions struct {
	configPath, inventoryPath, coordinator, generationID, configName string
	rolloutID, component                                             string
	progress                                                         io.Writer
}

var kubeadmConfigNow = func() time.Time { return time.Now().UTC() }

func newClusterApplyCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := kubeadmControlPlaneConfigOptions{}
	cmd := &cobra.Command{Use: "apply", Short: "Apply the complete ClusterConfig to a running cluster", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return runClusterApply(ctx, opts, stdout, stderr) }}
	f := cmd.Flags()
	f.StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	f.StringVar(&opts.inventoryPath, "inventory", "", "advanced cluster inventory")
	f.StringVar(&opts.coordinator, "coordinator", "", "control-plane coordinator used for joins and changed last")
	f.StringVar(&opts.generationID, "generation", "", "active desired generation ID")
	f.StringVar(&opts.configName, "config-name", "", "selected KubeadmConfig name")
	f.StringVar(&opts.rolloutID, "rollout-id", "", "rollout identity")
	for _, name := range []string{"inventory", "generation", "config-name", "rollout-id"} {
		cmd.Flags().Lookup(name).Hidden = true
	}
	return cmd
}

func runClusterApply(ctx context.Context, opts kubeadmControlPlaneConfigOptions, stdout, stderr io.Writer) error {
	opts.progress = stderr
	inv, err := kubeadmConfigInventory(opts)
	if err != nil {
		return err
	}
	if err := clusterApplyProgress(opts.progress, "phase=configuration status=started nodes=%d", len(inv.Nodes)); err != nil {
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
	activated := activatedClusterConfig{}
	if strings.TrimSpace(opts.configPath) != "" {
		activated, err = activateClusterConfig(ctx, opts, inv.Nodes)
		if err != nil {
			return err
		}
		generations = activated.generations
		components = activated.components
		preBootstrap = activated.preBootstrap
		stagedNodes = activated.stagedNodes
	} else if strings.TrimSpace(opts.generationID) == "" {
		return fmt.Errorf("--generation is required with --inventory")
	}

	results := map[string]any{}
	var joined []string
	for _, node := range activated.joinNodes {
		if err := clusterApplyProgress(opts.progress, "phase=node-join node=%s coordinator=%s status=started", node, activated.joinCoordinator); err != nil {
			return err
		}
		deps := agentBootstrapDependencies()
		deps.Actor = "katlctl cluster apply"
		deps.Progress = func(progress cluster.AgentBootstrapProgress) {
			if progress.Node != node || strings.TrimSpace(progress.Phase) == "" {
				return
			}
			_ = clusterApplyProgress(opts.progress, "phase=node-join node=%s step=%s status=running", node, progress.Phase)
		}
		if _, err := runAgentNodeJoin(ctx, cluster.Request{
			Inventory: inv,
			InitNode:  activated.joinCoordinator,
		}, node, deps); err != nil {
			return fmt.Errorf("join replacement node %s: %w", node, err)
		}
		joinedGeneration, err := currentClusterApplyGeneration(ctx, inv.Nodes, node)
		if err != nil {
			return fmt.Errorf("refresh replacement node %s after join: %w", node, err)
		}
		generations[node] = joinedGeneration
		joined = append(joined, node)
		if err := clusterApplyProgress(opts.progress, "phase=node-join node=%s status=succeeded", node); err != nil {
			return err
		}
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
	report := map[string]any{
		"nodes":      len(inv.Nodes),
		"joined":     joined,
		"kubernetes": results,
		"result":     "succeeded",
	}
	if len(stagedNodes) > 0 {
		report["rebootRequired"] = true
		report["stagedNodes"] = stagedNodes
	}
	return json.NewEncoder(stdout).Encode(report)
}

func currentClusterApplyGeneration(ctx context.Context, nodes []inventory.Node, nodeName string) (string, error) {
	var selected inventory.Node
	for _, node := range nodes {
		if node.Name == nodeName {
			selected = node
			break
		}
	}
	if selected.Name == "" {
		return "", fmt.Errorf("node is not present in cluster inventory")
	}
	conn, err := dialKatlcAgent(ctx, cluster.AgentEndpoint(selected.Address, "9443"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return "", err
	}
	generationID := strings.TrimSpace(status.GetCurrentGenerationId())
	if generationID == "" {
		return "", fmt.Errorf("agent did not report a current generation")
	}
	return generationID, nil
}

func runKubeadmControlPlaneConfig(ctx context.Context, opts kubeadmControlPlaneConfigOptions, stdout io.Writer) error {
	if strings.TrimSpace(opts.component) == "" {
		opts.component = "control-plane"
	}
	if opts.component != "control-plane" && opts.component != "kubelet" && opts.component != "kube-proxy" {
		return fmt.Errorf("internal component = %q, want control-plane, kubelet, or kube-proxy", opts.component)
	}
	inv, err := kubeadmConfigInventory(opts)
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
	if strings.TrimSpace(opts.coordinator) == "" {
		sort.Slice(controlPlanes, func(i, j int) bool { return controlPlanes[i].Name < controlPlanes[j].Name })
		opts.coordinator = controlPlanes[len(controlPlanes)-1].Name
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
		conn, err := dialKatlcAgent(ctx, cluster.AgentEndpoint(node.Address, "9443"))
		if err != nil {
			return nil, fmt.Errorf("connect %s: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if err != nil {
			return nil, fmt.Errorf("status %s: %w", node.Name, err)
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
		accepted, err := t.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: opts.rolloutID + "-dry-run-" + t.node.Name, OperationKind: "kubeadm-control-plane-config", Actor: "katlctl cluster apply", ExpectedMachineId: t.machine, ExpectedCurrentGenerationId: t.generation, DryRun: true, KubeadmControlPlaneConfig: body})
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
		accepted, err := t.conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: opts.rolloutID + "-" + t.node.Name, OperationKind: "kubeadm-control-plane-config", Actor: "katlctl cluster apply", ExpectedMachineId: t.machine, ExpectedCurrentGenerationId: t.generation, KubeadmControlPlaneConfig: body})
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
	return &agentapi.KubeadmControlPlaneConfigOperationRequest{RolloutId: opts.rolloutID, NodePosition: position, NodeCount: count, NodeName: node.Name, CoordinatorNode: opts.coordinator, CoordinatorUpload: node.Name == opts.coordinator, DesiredGenerationId: generationID, ConfigName: node.KubeadmConfig.Ref, SupportedFieldDelta: []string{component}}
}

const (
	kubeadmConfigComponentControlPlane = "component/control-plane"
	kubeadmConfigComponentKubelet      = "component/kubelet"
	kubeadmConfigComponentKubeProxy    = "component/kube-proxy"
)

func kubeadmConfigInventory(opts kubeadmControlPlaneConfigOptions) (inventory.Inventory, error) {
	configPath := strings.TrimSpace(opts.configPath)
	inventoryPath := strings.TrimSpace(opts.inventoryPath)
	if (configPath == "") == (inventoryPath == "") {
		return inventory.Inventory{}, fmt.Errorf("exactly one of --config or --inventory is required")
	}
	if inventoryPath != "" {
		return loadInventory(inventoryPath)
	}
	return loadWipeInventory(configPath, "")
}

type activatedClusterConfig struct {
	generations     map[string]string
	components      map[string]bool
	preBootstrap    bool
	stagedNodes     []string
	joinNodes       []string
	joinCoordinator string
}

func activateClusterConfig(ctx context.Context, opts kubeadmControlPlaneConfigOptions, nodes []inventory.Node) (activatedClusterConfig, error) {
	loaded, err := loadKatlConfig(opts.configPath, configBundleCreator, configbundle.PlanningInputs{})
	if err != nil {
		return activatedClusterConfig{}, err
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
	}
	prepared := make([]preparedInput, 0, len(nodes))
	components := map[string]bool{}
	for _, node := range nodes {
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
		for _, document := range plan.Documents {
			switch document.Kind {
			case "ClusterConfiguration":
				components["control-plane"] = true
			case "KubeletConfiguration":
				components["kubelet"] = true
			case "KubeProxyConfiguration":
				components["kube-proxy"] = true
			}
		}
		configYAML, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{
			NodeName: selected.Node.Name, Manifest: selected.InstallManifest, KubeadmConfigs: selected.KubeadmConfigs,
			SourceID: selected.BundleManifest.ClusterName, DesiredVersion: desiredVersion, ApplyMode: generation.ApplyModeAuto,
		})
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("render cluster config for %s: %w", node.Name, err)
		}
		prepared = append(prepared, preparedInput{node: node, configYAML: configYAML})
	}
	result := make(map[string]string, len(nodes))
	for i := range prepared {
		input := &prepared[i]
		node := input.node
		conn, err := dialKatlcAgent(ctx, cluster.AgentEndpoint(node.Address, "9443"))
		if err != nil {
			return activatedClusterConfig{}, fmt.Errorf("connect %s to apply cluster config: %w", node.Name, err)
		}
		status, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("status %s before cluster config apply: %w", node.Name, err)
		}
		validation, err := conn.Client.ValidateConfig(ctx, &agentapi.ValidateConfigRequest{
			ApiVersion: operation.APIVersion, Kind: "ValidateConfigRequest", ClientRequestId: opts.rolloutID + "-stage-" + node.Name,
			Actor: "katlctl cluster apply", ExpectedMachineId: status.MachineId, ApplyMode: generation.ApplyModeAuto,
			CandidateGenerationId: generationID, NodeName: node.Name, ConfigYaml: string(input.configYAML),
		})
		if err != nil {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("validate cluster config on %s: %w", node.Name, err)
		}
		if !validation.Accepted {
			_ = conn.Close()
			return activatedClusterConfig{}, fmt.Errorf("node %s rejected cluster config: %s", node.Name, firstNonEmpty(validation.FailureReason, strings.Join(validation.Diagnostics, "; ")))
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
		input.changedDomains = append([]string(nil), validation.ChangedDomains...)
		_ = conn.Close()
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
	var kubernetesStates []string
	for _, input := range prepared {
		if input.kubernetesState == "not-configured" {
			notConfigured++
		}
		kubernetesStates = append(kubernetesStates, input.node.Name+"="+firstNonEmpty(input.kubernetesState, "unknown"))
	}
	preBootstrap := len(prepared) > 0 && notConfigured == len(prepared)
	joinCoordinator := ""
	if notConfigured > 0 && !preBootstrap {
		if notConfigured != 1 {
			return activatedClusterConfig{}, fmt.Errorf(
				"cluster apply can join one fresh replacement node at a time; found mixed Kubernetes lifecycle state (%s)",
				strings.Join(kubernetesStates, ", "),
			)
		}
		requestedCoordinator := strings.TrimSpace(opts.coordinator)
		for _, input := range prepared {
			if input.kubernetesState == "not-configured" {
				continue
			}
			if input.node.SystemRole == inventory.RoleControlPlane && input.kubernetesState == "ready" {
				if strings.TrimSpace(joinCoordinator) == "" {
					joinCoordinator = input.node.Name
				}
				if input.node.Name == requestedCoordinator {
					joinCoordinator = input.node.Name
					break
				}
			}
		}
		if requestedCoordinator != "" && joinCoordinator != requestedCoordinator {
			return activatedClusterConfig{}, fmt.Errorf(
				"coordinator %q cannot coordinate the replacement join: it must be an existing ready control-plane node (%s)",
				requestedCoordinator,
				strings.Join(kubernetesStates, ", "),
			)
		}
		if joinCoordinator == "" {
			return activatedClusterConfig{}, fmt.Errorf(
				"cluster has one fresh node but no ready control plane can coordinate its join (%s)",
				strings.Join(kubernetesStates, ", "),
			)
		}
	}

	for _, input := range prepared {
		node := input.node
		if input.noChanges {
			continue
		}
		conn, err := dialKatlcAgent(ctx, cluster.AgentEndpoint(node.Address, "9443"))
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
			OperationKind: operationKind, Actor: "katlctl cluster apply", ExpectedMachineId: input.machineID, ExpectedCurrentGenerationId: input.currentGeneration,
			ConfigApply: &agentapi.ConfigApplyOperationRequest{CandidateGenerationId: generationID, ApplyMode: generation.ApplyModeAuto, NodeName: node.Name, ConfigYaml: string(input.configYAML)},
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
	for _, input := range prepared {
		if input.acceptedApplyMode == generation.ApplyModeNextBoot {
			stagedNodes = append(stagedNodes, input.node.Name)
		}
	}
	sort.Strings(stagedNodes)
	var joinNodes []string
	if notConfigured == 1 && !preBootstrap {
		for _, input := range prepared {
			if input.kubernetesState == "not-configured" {
				joinNodes = append(joinNodes, input.node.Name)
			}
		}
	}
	return activatedClusterConfig{
		generations:     result,
		components:      components,
		preBootstrap:    preBootstrap,
		stagedNodes:     stagedNodes,
		joinNodes:       joinNodes,
		joinCoordinator: joinCoordinator,
	}, nil
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
