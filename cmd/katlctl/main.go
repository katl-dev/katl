package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zariel/katl/internal/bootstrap/cluster"
	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/readiness"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/vmtest"
	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
	"gopkg.in/yaml.v3"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

var runBootstrap = cluster.Run
var dialVMTestAgent = vmtest.DialAgent

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katlctl: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required")
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(stdout, "katlctl version=%s commit=%s date=%s\n", version, commit, date)
		return nil
	}
	if len(args) >= 2 && args[0] == "cluster" && args[1] == "bootstrap" {
		return runClusterBootstrap(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 3 && args[0] == "config" && args[1] == "apply" && args[2] == "status" {
		return runConfigApplyStatus(args[3:], stdout, stderr)
	}
	return fmt.Errorf("unsupported command %q", strings.Join(args, " "))
}

func runConfigApplyStatus(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlctl config apply status", flag.ContinueOnError)
	flags.SetOutput(stderr)

	root := flags.String("root", "/", "runtime root to inspect")
	activeGeneration := flags.String("active-generation", "", "active generation id")
	nextBootGeneration := flags.String("next-boot-generation", "", "next boot generation id")
	output := flags.String("output", "json", "output format: json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *output != "json" {
		return fmt.Errorf("--output = %q, want json", *output)
	}
	report, err := loadConfigApplyReport(*root, *activeGeneration, *nextBootGeneration)
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

func runClusterBootstrap(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlctl cluster bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var addresses addressOverrides
	inventoryPath := flags.String("inventory", "", "path to cluster bootstrap inventory")
	initNode := flags.String("init-node", "", "first control-plane node for kubeadm init")
	controlPlaneEndpoint := flags.String("control-plane-endpoint", "", "control-plane endpoint host:port")
	kubeconfigOut := flags.String("kubeconfig-out", "", "operator kubeconfig output path")
	overwriteKubeconfig := flags.Bool("overwrite-kubeconfig", false, "overwrite different existing kubeconfig")
	dryRun := flags.Bool("dry-run", false, "validate and print the bootstrap plan without running kubeadm")
	vmtestTranscriptDir := flags.String("vmtest-transcript-dir", "", "directory for per-node vmtest agent transcript artifacts")
	var bootstrapManifestPaths stringList
	var bootstrapWaitValues stringList
	flags.Var(&addresses, "node-address", "node address override in node=address form")
	flags.Var(&bootstrapManifestPaths, "bootstrap-manifest", "ordered Kubernetes manifest file or bundle to apply after API readiness")
	flags.Var(&bootstrapWaitValues, "bootstrap-wait", "post-bootstrap wait: api-ready, nodes-ready, resource-exists[:namespace]:kind/name, condition[:namespace]:kind/name:Condition, rollout-status[:namespace]:kind/name, or pods-ready[:namespace]:selector")
	bootstrapStableEndpoint := flags.String("bootstrap-stable-endpoint", "", "stable API endpoint host:port to wait for before writing kubeconfig")
	bootstrapStableEndpointBeforeManifests := flags.Bool("bootstrap-stable-endpoint-before-manifests", false, "wait for stable API endpoint before applying bootstrap manifests")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*inventoryPath) == "" {
		return fmt.Errorf("--inventory is required")
	}
	inv, err := loadInventory(*inventoryPath)
	if err != nil {
		return err
	}
	bootstrap, err := parseUserBootstrap(bootstrapManifestPaths.values, bootstrapWaitValues.values, *bootstrapStableEndpoint, *bootstrapStableEndpointBeforeManifests)
	if err != nil {
		return err
	}
	result, err := runBootstrap(ctx, cluster.Request{
		Inventory:            inv,
		InitNode:             *initNode,
		AddressOverrides:     addresses.values,
		ControlPlaneEndpoint: *controlPlaneEndpoint,
		KubeconfigOut:        *kubeconfigOut,
		OverwriteKubeconfig:  *overwriteKubeconfig,
		DryRun:               *dryRun,
		Bootstrap:            bootstrap,
	}, bootstrapDependencies(*vmtestTranscriptDir))
	printBootstrapResult(stdout, result)
	return err
}

func bootstrapDependencies(vmtestTranscriptDir string) cluster.Dependencies {
	transport := vmtestAgentTransport{TranscriptDir: strings.TrimSpace(vmtestTranscriptDir)}
	return cluster.Dependencies{
		ReadinessChecker: readiness.Checker{Agent: transport},
		NodeRunner:       cluster.TransportRunner{Transport: transport},
		BootstrapRunner:  cluster.KubectlBootstrapRunner{},
	}
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
	return doc.inventory(), nil
}

type inventoryDocument struct {
	ControlPlaneEndpoint string               `yaml:"controlPlaneEndpoint"`
	KubernetesVersion    string               `yaml:"kubernetesVersion"`
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
	return inventory.Inventory{
		ControlPlaneEndpoint: d.ControlPlaneEndpoint,
		KubernetesVersion:    d.KubernetesVersion,
		Bootstrap:            d.Bootstrap,
		Nodes:                nodes,
	}
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

func parseUserBootstrap(manifestPaths, waitValues []string, stableEndpoint string, stableEndpointBeforeManifests bool) (cluster.UserBootstrap, error) {
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
