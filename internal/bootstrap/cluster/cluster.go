package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/kubeconfig"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
	"gopkg.in/yaml.v3"
)

const (
	adminKubeconfigPath = "/etc/kubernetes/admin.conf"
	defaultAPIPort      = "6443"
	kubectlWaitTimeout  = "2m"
)

var (
	certificateKeyLinePattern = regexp.MustCompile(`(?i)^[a-f0-9]{64}$`)
	labelDNSPattern           = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelNamePattern          = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

type Request struct {
	Inventory            inventory.Inventory
	InitNode             string
	AddressOverrides     map[string]string
	ControlPlaneEndpoint string
	KubeconfigOut        string
	OverwriteKubeconfig  bool
	DryRun               bool
	ClusterName          string
	ContextName          string
	UserName             string
	Bootstrap            UserBootstrap
}

type Dependencies struct {
	ReadinessChecker inventory.ReadinessChecker
	NodeRunner       NodeRunner
	BootstrapRunner  BootstrapRunner
}

type NodeRunner interface {
	RunKubeadmInit(ctx context.Context, node inventory.PlannedNode, controlPlaneEndpoint string) (AdminCredentials, error)
	CreateControlPlaneJoin(ctx context.Context, initNode inventory.PlannedNode) (JoinMaterial, error)
	RunControlPlaneJoin(ctx context.Context, node inventory.PlannedNode, material JoinMaterial) error
	WaitControlPlaneJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error
	CreateWorkerJoin(ctx context.Context, initNode inventory.PlannedNode) (JoinMaterial, error)
	RunWorkerJoin(ctx context.Context, node inventory.PlannedNode, material JoinMaterial) error
	WaitWorkerJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error
	WaitAPIReady(ctx context.Context, initNode inventory.PlannedNode) error
}

type BootstrapRunner interface {
	RunUserBootstrap(ctx context.Context, request BootstrapRequest) (BootstrapResult, error)
}

type UserBootstrap struct {
	Manifests                     []BootstrapManifest
	PreWaits                      []BootstrapWait
	Waits                         []BootstrapWait
	StableEndpoint                string
	StableEndpointBeforeManifests bool
}

type BootstrapManifest struct {
	Path    string `json:"path,omitempty"`
	Content []byte `json:"-"`
	SHA256  string `json:"sha256,omitempty"`
}

type BootstrapWait struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Condition string `json:"condition,omitempty"`
	Selector  string `json:"selector,omitempty"`
}

type BootstrapRequest struct {
	Server         string
	StableEndpoint string
	Credentials    AdminCredentials
	PreWaits       []BootstrapWait
	Manifests      []BootstrapManifest
	Waits          []BootstrapWait
}

type BootstrapResult struct {
	AppliedManifests    []BootstrapManifest
	Waits               []BootstrapWait
	StableEndpointReady bool
}

type KubectlCommandRunner interface {
	Run(ctx context.Context, argv []string) (readiness.CommandResult, error)
}

type AdminCredentials struct {
	CertificateAuthorityData string
	ClientCertificateData    string
	ClientKeyData            string
}

type JoinMaterial struct {
	Argv                []string
	DiscoveryKubeconfig []byte
}

type Result struct {
	Plan       inventory.Plan
	Phases     []Phase
	Readiness  inventory.ReadinessReport
	Kubeconfig kubeconfig.Result
	Bootstrap  BootstrapResult
	NextStep   string
	DryRun     bool
}

type Phase struct {
	Name        string                    `json:"name"`
	Node        string                    `json:"node,omitempty"`
	Action      inventory.BootstrapAction `json:"action,omitempty"`
	Status      string                    `json:"status"`
	OperationID string                    `json:"operationID,omitempty"`
}

const (
	BootstrapWaitAPIReady       = "api-ready"
	BootstrapWaitResourceExists = "resource-exists"
	BootstrapWaitCondition      = "condition"
	BootstrapWaitNodesReady     = "nodes-ready"
	BootstrapWaitStableEndpoint = "stable-endpoint"
	BootstrapWaitRolloutStatus  = "rollout-status"
	BootstrapWaitPodsReady      = "pods-ready"
)

func Run(ctx context.Context, request Request, deps Dependencies) (Result, error) {
	inv := request.Inventory
	if strings.TrimSpace(request.ControlPlaneEndpoint) != "" {
		inv.ControlPlaneEndpoint = strings.TrimSpace(request.ControlPlaneEndpoint)
	}
	plan, err := inventory.PlanInventory(inventory.PlanRequest{
		Inventory:       inv,
		InitNode:        request.InitNode,
		AddressOverride: request.AddressOverrides,
	})
	if err != nil {
		return Result{}, err
	}
	result := Result{Plan: plan, DryRun: request.DryRun}
	result.addPhase("plan", "", "", "passed")
	bootstrapInput := mergeBootstrap(planBootstrap(plan.Bootstrap), request.Bootstrap)
	nodeManifests, err := nodeLabelManifests(plan)
	if err != nil {
		return result, err
	}
	bootstrapInput.Manifests = append(nodeManifests, bootstrapInput.Manifests...)
	bootstrap, err := prepareBootstrap(bootstrapInput)
	if err != nil {
		return result, err
	}
	if deps.ReadinessChecker == nil {
		return result, errors.New("bootstrap readiness checker is required")
	}
	report, err := inventory.VerifyReadiness(ctx, plan, deps.ReadinessChecker)
	if err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, err
	}
	result.Readiness = report
	if err := inventory.Error(report); err != nil {
		result.addPhase("readiness", "", "", "failed")
		return result, err
	}
	result.addPhase("readiness", "", "", "passed")
	if request.DryRun {
		result.addPhase("dry-run", "", "", "passed")
		return result, nil
	}
	if deps.NodeRunner == nil {
		return result, errors.New("bootstrap node runner is required")
	}
	initNode, err := findInitNode(plan)
	if err != nil {
		return result, err
	}
	credentials, err := deps.NodeRunner.RunKubeadmInit(ctx, initNode, plan.ControlPlaneEndpoint)
	if err != nil {
		result.addPhase("kubeadm-init", initNode.Name, inventory.ActionInit, "failed")
		return result, fmt.Errorf("kubeadm init on %s: %s", initNode.Name, inventory.Redact(err.Error()))
	}
	result.addPhase("kubeadm-init", initNode.Name, inventory.ActionInit, "passed")
	if err := deps.NodeRunner.WaitAPIReady(ctx, initNode); err != nil {
		result.addPhase("api-ready", initNode.Name, "", "failed")
		return result, fmt.Errorf("wait for API readiness on %s: %s", initNode.Name, inventory.Redact(err.Error()))
	}
	result.addPhase("api-ready", initNode.Name, "", "passed")
	stableEndpointReady := false
	if plan.ControlPlaneEndpointManaged {
		if deps.BootstrapRunner == nil {
			return result, errors.New("managed control-plane endpoint reachability checker is required")
		}
		endpointResult, err := verifyManagedControlPlaneEndpoint(ctx, deps.BootstrapRunner, initNode, plan, credentials)
		if err != nil {
			result.addPhase("stable-endpoint", "", "", "failed")
			return result, fmt.Errorf("wait for managed control-plane endpoint: %s", inventory.Redact(err.Error()))
		}
		if !endpointResult.StableEndpointReady {
			result.addPhase("stable-endpoint", "", "", "failed")
			return result, errors.New("managed control-plane endpoint check completed without confirming readiness")
		}
		stableEndpointReady = true
		result.addPhase("stable-endpoint", "", "", "passed")
	}

	controlPlanes := controlPlaneJoinNodes(plan)
	if len(controlPlanes) > 0 {
		material, err := deps.NodeRunner.CreateControlPlaneJoin(ctx, initNode)
		if err != nil {
			result.addPhase("control-plane-join-material", initNode.Name, "", "failed")
			return result, fmt.Errorf("create control-plane join material: %s", inventory.Redact(err.Error()))
		}
		result.addPhase("control-plane-join-material", initNode.Name, "", "passed")
		for _, node := range controlPlanes {
			if err := deps.NodeRunner.RunControlPlaneJoin(ctx, node, material); err != nil {
				result.addPhase("control-plane-join", node.Name, inventory.ActionControlPlaneJoin, "failed")
				return result, fmt.Errorf("control-plane join on %s: %s", node.Name, inventory.Redact(err.Error()))
			}
			result.addPhase("control-plane-join", node.Name, inventory.ActionControlPlaneJoin, "passed")
			if err := deps.NodeRunner.WaitControlPlaneJoinReady(ctx, initNode, node); err != nil {
				result.addPhase("control-plane-ready", node.Name, "", "failed")
				return result, fmt.Errorf("wait for control-plane readiness on %s: %s", node.Name, inventory.Redact(err.Error()))
			}
			result.addPhase("control-plane-ready", node.Name, "", "passed")
		}
	}

	workers := workerNodes(plan)
	if len(workers) > 0 {
		material, err := deps.NodeRunner.CreateWorkerJoin(ctx, initNode)
		if err != nil {
			result.addPhase("join-material", initNode.Name, "", "failed")
			return result, fmt.Errorf("create worker join material: %s", inventory.Redact(err.Error()))
		}
		result.addPhase("join-material", initNode.Name, "", "passed")
		for _, node := range workers {
			if err := deps.NodeRunner.RunWorkerJoin(ctx, node, material); err != nil {
				result.addPhase("worker-join", node.Name, inventory.ActionWorkerJoin, "failed")
				return result, fmt.Errorf("worker join on %s: %s", node.Name, inventory.Redact(err.Error()))
			}
			result.addPhase("worker-join", node.Name, inventory.ActionWorkerJoin, "passed")
			if err := deps.NodeRunner.WaitWorkerJoinReady(ctx, initNode, node); err != nil {
				result.addPhase("worker-ready", node.Name, "", "failed")
				return result, fmt.Errorf("wait for worker registration on %s: %s", node.Name, inventory.Redact(err.Error()))
			}
			result.addPhase("worker-ready", node.Name, "", "passed")
		}
	}
	if err := deps.NodeRunner.WaitAPIReady(ctx, initNode); err != nil {
		result.addPhase("api-ready-after-join", initNode.Name, "", "failed")
		return result, fmt.Errorf("wait for API readiness after joins on %s: %s", initNode.Name, inventory.Redact(err.Error()))
	}
	result.addPhase("api-ready-after-join", initNode.Name, "", "passed")

	kubeconfigResult, err := writeOperatorKubeconfig(request, initNode, plan, bootstrap, credentials, stableEndpointReady, request.OverwriteKubeconfig)
	if err != nil {
		result.addPhase("kubeconfig", "", "", "failed")
		return result, err
	}
	result.Kubeconfig = kubeconfigResult
	result.NextStep = kubeconfigResult.NextStep()
	result.addPhase("kubeconfig", "", "", "passed")

	if bootstrap.enabled() {
		if deps.BootstrapRunner == nil {
			return result, errors.New("bootstrap handoff runner is required")
		}
		bootstrapResult, err := deps.BootstrapRunner.RunUserBootstrap(ctx, BootstrapRequest{
			Server:         bootstrapServer(initNode, plan),
			StableEndpoint: bootstrap.StableEndpoint,
			Credentials:    credentials,
			PreWaits:       bootstrap.preWaits(),
			Manifests:      bootstrap.Manifests,
			Waits:          bootstrap.waitsWithEndpoint(),
		})
		if err != nil {
			result.addPhase("user-bootstrap", "", "", "failed")
			return result, fmt.Errorf("user bootstrap handoff: %s", inventory.Redact(err.Error()))
		}
		result.Bootstrap = bootstrapResult
		wasStableEndpointReady := stableEndpointReady
		stableEndpointReady = stableEndpointReady || bootstrapResult.StableEndpointReady
		result.addPhase("user-bootstrap", "", "", "passed")
		if !wasStableEndpointReady && stableEndpointReady {
			refreshed, err := writeOperatorKubeconfig(request, initNode, plan, bootstrap, credentials, stableEndpointReady, true)
			if err != nil {
				return result, fmt.Errorf("refresh kubeconfig for stable endpoint: %w", err)
			}
			result.Kubeconfig = refreshed
			result.NextStep = refreshed.NextStep()
		}
	}
	return result, nil
}

func writeOperatorKubeconfig(request Request, initNode inventory.PlannedNode, plan inventory.Plan, bootstrap UserBootstrap, credentials AdminCredentials, stableEndpointReady, overwrite bool) (kubeconfig.Result, error) {
	return kubeconfig.Write(kubeconfig.Request{
		Path:      request.KubeconfigOut,
		Overwrite: overwrite,
		Endpoint: kubeconfig.EndpointSelection{
			InitialEndpoint:      endpointForNode(initNode),
			ControlPlaneEndpoint: plan.ControlPlaneEndpoint,
			StableEndpoint:       bootstrap.StableEndpoint,
			StableEndpointReady:  stableEndpointReady,
		},
		ClusterName:              valueOrDefault(request.ClusterName, "katl"),
		ContextName:              valueOrDefault(request.ContextName, "katl"),
		UserName:                 valueOrDefault(request.UserName, "katl-admin"),
		CertificateAuthorityData: credentials.CertificateAuthorityData,
		ClientCertificateData:    credentials.ClientCertificateData,
		ClientKeyData:            credentials.ClientKeyData,
	})
}

func verifyManagedControlPlaneEndpoint(ctx context.Context, runner BootstrapRunner, initNode inventory.PlannedNode, plan inventory.Plan, credentials AdminCredentials) (BootstrapResult, error) {
	endpoint := strings.TrimSpace(plan.ControlPlaneEndpoint)
	if endpoint == "" {
		return BootstrapResult{}, errors.New("managed control-plane endpoint is empty")
	}
	return runner.RunUserBootstrap(ctx, BootstrapRequest{
		Server:         endpointForNode(initNode),
		StableEndpoint: endpoint,
		Credentials:    credentials,
		PreWaits:       []BootstrapWait{{Kind: BootstrapWaitStableEndpoint, Name: endpoint}},
	})
}

func prepareBootstrap(bootstrap UserBootstrap) (UserBootstrap, error) {
	bootstrap.StableEndpoint = strings.TrimSpace(bootstrap.StableEndpoint)
	if bootstrap.StableEndpoint != "" {
		if err := validateEndpointLike(bootstrap.StableEndpoint); err != nil {
			return UserBootstrap{}, fmt.Errorf("bootstrap stable endpoint: %w", err)
		}
	} else if bootstrap.StableEndpointBeforeManifests {
		return UserBootstrap{}, fmt.Errorf("bootstrap stable endpoint before manifests requires stable endpoint")
	}
	manifests := make([]BootstrapManifest, 0, len(bootstrap.Manifests))
	for _, manifest := range bootstrap.Manifests {
		loaded, err := loadBootstrapManifest(manifest)
		if err != nil {
			return UserBootstrap{}, err
		}
		manifests = append(manifests, loaded)
	}
	preWaits := make([]BootstrapWait, 0, len(bootstrap.PreWaits))
	for _, wait := range bootstrap.PreWaits {
		normalized, err := normalizeBootstrapWait(wait)
		if err != nil {
			return UserBootstrap{}, err
		}
		preWaits = append(preWaits, normalized)
	}
	waits := make([]BootstrapWait, 0, len(bootstrap.Waits))
	for _, wait := range bootstrap.Waits {
		normalized, err := normalizeBootstrapWait(wait)
		if err != nil {
			return UserBootstrap{}, err
		}
		waits = append(waits, normalized)
	}
	bootstrap.Manifests = manifests
	bootstrap.PreWaits = preWaits
	bootstrap.Waits = waits
	return bootstrap, nil
}

func planBootstrap(bootstrap *inventory.Bootstrap) UserBootstrap {
	if bootstrap == nil {
		return UserBootstrap{}
	}
	result := UserBootstrap{
		StableEndpoint:                bootstrap.StableEndpoint,
		StableEndpointBeforeManifests: bootstrap.StableEndpointBeforeManifests,
	}
	for _, manifest := range bootstrap.Manifests {
		result.Manifests = append(result.Manifests, BootstrapManifest{Path: manifest.Path})
	}
	for _, wait := range bootstrap.Waits {
		result.Waits = append(result.Waits, BootstrapWait{
			Kind:      wait.Kind,
			Namespace: wait.Namespace,
			Name:      wait.Name,
			Condition: wait.Condition,
			Selector:  wait.Selector,
		})
	}
	return result
}

func mergeBootstrap(plan, request UserBootstrap) UserBootstrap {
	result := UserBootstrap{
		Manifests:                     append([]BootstrapManifest(nil), plan.Manifests...),
		PreWaits:                      append([]BootstrapWait(nil), plan.PreWaits...),
		Waits:                         append([]BootstrapWait(nil), plan.Waits...),
		StableEndpointBeforeManifests: plan.StableEndpointBeforeManifests || request.StableEndpointBeforeManifests,
	}
	if strings.TrimSpace(request.StableEndpoint) != "" {
		result.StableEndpoint = request.StableEndpoint
	} else {
		result.StableEndpoint = plan.StableEndpoint
	}
	result.Manifests = append(result.Manifests, request.Manifests...)
	result.PreWaits = append(result.PreWaits, request.PreWaits...)
	result.Waits = append(result.Waits, request.Waits...)
	return result
}

func nodeLabelManifests(plan inventory.Plan) ([]BootstrapManifest, error) {
	manifests := make([]BootstrapManifest, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if len(node.Labels) == 0 {
			continue
		}
		content, err := yaml.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Node",
			"metadata": map[string]any{
				"name":   node.Name,
				"labels": node.Labels,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("render labels for node %s: %w", node.Name, err)
		}
		manifests = append(manifests, BootstrapManifest{Content: content})
	}
	return manifests, nil
}

func loadBootstrapManifest(manifest BootstrapManifest) (BootstrapManifest, error) {
	manifest.Path = strings.TrimSpace(manifest.Path)
	if len(manifest.Content) == 0 {
		if manifest.Path == "" {
			return BootstrapManifest{}, fmt.Errorf("bootstrap manifest path or content is required")
		}
		data, err := os.ReadFile(manifest.Path)
		if err != nil {
			return BootstrapManifest{}, fmt.Errorf("read bootstrap manifest %s: %w", manifest.Path, err)
		}
		manifest.Content = data
	}
	if err := validateBootstrapYAML(manifest.Content); err != nil {
		return BootstrapManifest{}, fmt.Errorf("bootstrap manifest %s: %w", valueOrDefault(manifest.Path, "<inline>"), err)
	}
	sum := sha256.Sum256(manifest.Content)
	manifest.SHA256 = hex.EncodeToString(sum[:])
	return manifest, nil
}

func validateBootstrapYAML(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	seen := false
	documentIndex := 0
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode YAML: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		documentIndex++
		apiVersion, apiVersionOK := doc["apiVersion"].(string)
		if !apiVersionOK || strings.TrimSpace(apiVersion) == "" {
			return fmt.Errorf("Kubernetes YAML document %d must have a non-empty apiVersion", documentIndex)
		}
		kind, kindOK := doc["kind"].(string)
		if !kindOK || strings.TrimSpace(kind) == "" {
			return fmt.Errorf("Kubernetes YAML document %d must have a non-empty kind", documentIndex)
		}
		seen = true
	}
	if !seen {
		return fmt.Errorf("contains no Kubernetes YAML documents")
	}
	return nil
}

func (b UserBootstrap) enabled() bool {
	return len(b.Manifests) > 0 || len(b.PreWaits) > 0 || len(b.Waits) > 0 || strings.TrimSpace(b.StableEndpoint) != ""
}

func (b UserBootstrap) preWaits() []BootstrapWait {
	waits := append([]BootstrapWait(nil), b.PreWaits...)
	if !b.StableEndpointBeforeManifests || strings.TrimSpace(b.StableEndpoint) == "" {
		return waits
	}
	return append(waits, b.stableEndpointWait())
}

func (b UserBootstrap) waitsWithEndpoint() []BootstrapWait {
	waits := append([]BootstrapWait(nil), b.Waits...)
	if strings.TrimSpace(b.StableEndpoint) != "" && !b.StableEndpointBeforeManifests {
		waits = append(waits, b.stableEndpointWait())
	}
	return waits
}

func (b UserBootstrap) stableEndpointWait() BootstrapWait {
	return BootstrapWait{Kind: BootstrapWaitStableEndpoint, Name: b.StableEndpoint}
}

func normalizeBootstrapWait(wait BootstrapWait) (BootstrapWait, error) {
	wait.Kind = strings.TrimSpace(wait.Kind)
	wait.Namespace = strings.TrimSpace(wait.Namespace)
	wait.Name = strings.TrimSpace(wait.Name)
	wait.Condition = strings.TrimSpace(wait.Condition)
	wait.Selector = strings.TrimSpace(wait.Selector)
	switch wait.Kind {
	case BootstrapWaitAPIReady, BootstrapWaitNodesReady:
		return wait, nil
	case BootstrapWaitResourceExists:
		if wait.Name == "" {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait resource-exists requires name")
		}
		if err := validateResourceTarget(wait.Name); err != nil {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait resource-exists: %w", err)
		}
		return wait, nil
	case BootstrapWaitCondition:
		if wait.Name == "" || wait.Condition == "" {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait condition requires name and condition")
		}
		if err := validateResourceTarget(wait.Name); err != nil {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait condition: %w", err)
		}
		return wait, nil
	case BootstrapWaitRolloutStatus:
		if wait.Name == "" {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait rollout-status requires name")
		}
		if err := validateResourceTarget(wait.Name); err != nil {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait rollout-status: %w", err)
		}
		return wait, nil
	case BootstrapWaitPodsReady:
		if wait.Selector == "" {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait pods-ready requires selector")
		}
		if err := validateLabelSelector(wait.Selector); err != nil {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait pods-ready: %w", err)
		}
		return wait, nil
	case BootstrapWaitStableEndpoint:
		if wait.Name == "" {
			return BootstrapWait{}, fmt.Errorf("bootstrap wait stable-endpoint requires endpoint name")
		}
		if err := validateEndpointLike(wait.Name); err != nil {
			return BootstrapWait{}, err
		}
		return wait, nil
	case "":
		return BootstrapWait{}, fmt.Errorf("bootstrap wait kind is required")
	default:
		return BootstrapWait{}, fmt.Errorf("bootstrap wait kind %q is unsupported", wait.Kind)
	}
}

func ValidateBootstrapWait(wait BootstrapWait) (BootstrapWait, error) {
	return normalizeBootstrapWait(wait)
}

func validateResourceTarget(name string) error {
	kind, resource, ok := strings.Cut(strings.TrimSpace(name), "/")
	if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(resource) == "" || strings.Contains(resource, "/") {
		return fmt.Errorf("target must be kind/name")
	}
	return nil
}

func validateLabelSelector(selector string) error {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return fmt.Errorf("selector is required")
	}
	for _, requirement := range strings.Split(selector, ",") {
		requirement = strings.TrimSpace(requirement)
		if requirement == "" {
			return fmt.Errorf("selector has an empty requirement")
		}
		if strings.ContainsAny(requirement, " \t\r\n") {
			return fmt.Errorf("selector requirement %q contains unsupported whitespace", requirement)
		}
		key := requirement
		for _, op := range []string{"!=", "==", "="} {
			if before, after, ok := strings.Cut(requirement, op); ok {
				key = before
				if !validLabelValue(after) {
					return fmt.Errorf("selector requirement %q has invalid value", requirement)
				}
				break
			}
		}
		if !validLabelKey(key) {
			return fmt.Errorf("selector requirement %q has invalid key", requirement)
		}
	}
	return nil
}

func validLabelKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "!") {
		return false
	}
	prefix, name, hasPrefix := strings.Cut(key, "/")
	if hasPrefix {
		if !validDNSSubdomain(prefix) {
			return false
		}
		key = name
	}
	return validLabelName(key)
}

func validLabelValue(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && validLabelName(value)
}

func validDNSSubdomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !validDNSLabel(part) {
			return false
		}
	}
	return true
}

func validDNSLabel(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	return labelDNSPattern.MatchString(value)
}

func validLabelName(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	return labelNamePattern.MatchString(value)
}

func bootstrapServer(initNode inventory.PlannedNode, plan inventory.Plan) string {
	if plan.ControlPlaneEndpoint != "" {
		return plan.ControlPlaneEndpoint
	}
	return endpointForNode(initNode)
}

func validateEndpointLike(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if strings.HasPrefix(endpoint, "https://") {
		endpoint = strings.TrimPrefix(endpoint, "https://")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("endpoint must be host:port or https://host:port")
	}
	return nil
}

func (r *Result) addPhase(name, node string, action inventory.BootstrapAction, status string) {
	r.Phases = append(r.Phases, Phase{Name: name, Node: node, Action: action, Status: status})
}

func (r *Result) addOperationPhase(name, node string, action inventory.BootstrapAction, status string, operation operationReference) {
	r.Phases = append(r.Phases, Phase{
		Name:        name,
		Node:        node,
		Action:      action,
		Status:      status,
		OperationID: operation.ID,
	})
}

func controlPlaneJoinNodes(plan inventory.Plan) []inventory.PlannedNode {
	var nodes []inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.Action == inventory.ActionControlPlaneJoin {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func findInitNode(plan inventory.Plan) (inventory.PlannedNode, error) {
	for _, node := range plan.Nodes {
		if node.Action == inventory.ActionInit {
			return node, nil
		}
	}
	return inventory.PlannedNode{}, fmt.Errorf("plan has no init node")
}

func workerNodes(plan inventory.Plan) []inventory.PlannedNode {
	var nodes []inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.Action == inventory.ActionWorkerJoin {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func endpointForNode(node inventory.PlannedNode) string {
	if hasPort(node.Address) {
		return node.Address
	}
	return net.JoinHostPort(node.Address, defaultAPIPort)
}

func hasPort(value string) bool {
	_, _, err := net.SplitHostPort(value)
	return err == nil
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

type KubectlBootstrapRunner struct {
	CommandRunner KubectlCommandRunner
	TempDir       string
	Timeout       time.Duration
	PollInterval  time.Duration
	ProbeTimeout  time.Duration
}

func (r KubectlBootstrapRunner) RunUserBootstrap(ctx context.Context, request BootstrapRequest) (BootstrapResult, error) {
	if strings.TrimSpace(request.Server) == "" {
		return BootstrapResult{}, fmt.Errorf("bootstrap API server endpoint is required")
	}
	dir, cleanup, err := r.workDir()
	if err != nil {
		return BootstrapResult{}, err
	}
	defer cleanup()

	kubeconfigPath, err := r.writeKubeconfig(dir, request.Server, request.Credentials)
	if err != nil {
		return BootstrapResult{}, err
	}
	result := BootstrapResult{}
	for _, wait := range request.PreWaits {
		stableReady, err := r.runBootstrapWait(ctx, dir, kubeconfigPath, &request, wait)
		if err != nil {
			return result, err
		}
		result.StableEndpointReady = result.StableEndpointReady || stableReady
		result.Waits = append(result.Waits, wait)
	}
	for index, manifest := range request.Manifests {
		path, err := r.writeManifest(dir, index, manifest)
		if err != nil {
			return result, err
		}
		if err := r.runKubectl(ctx, []string{"--kubeconfig", kubeconfigPath, "--context", "katl-bootstrap", "apply", "-f", path}); err != nil {
			return result, fmt.Errorf("apply bootstrap manifest %s: %w", valueOrDefault(manifest.Path, "<inline>"), err)
		}
		result.AppliedManifests = append(result.AppliedManifests, manifest)
	}
	for _, wait := range request.Waits {
		stableReady, err := r.runBootstrapWait(ctx, dir, kubeconfigPath, &request, wait)
		if err != nil {
			return result, err
		}
		result.StableEndpointReady = result.StableEndpointReady || stableReady
		result.Waits = append(result.Waits, wait)
	}
	return result, nil
}

func (r KubectlBootstrapRunner) runBootstrapWait(ctx context.Context, dir, kubeconfigPath string, request *BootstrapRequest, wait BootstrapWait) (bool, error) {
	if wait.Kind == BootstrapWaitStableEndpoint {
		if strings.TrimSpace(request.StableEndpoint) == "" {
			request.StableEndpoint = wait.Name
		}
		stableKubeconfig, err := r.writeKubeconfig(dir, request.StableEndpoint, request.Credentials)
		if err != nil {
			return false, err
		}
		if err := r.pollKubectl(ctx, []string{"--kubeconfig", stableKubeconfig, "--context", "katl-bootstrap", "get", "--raw=/readyz"}); err != nil {
			return false, err
		}
		return true, nil
	}
	args, err := waitKubectlArgs(kubeconfigPath, wait)
	if err != nil {
		return false, err
	}
	if wait.Kind == BootstrapWaitAPIReady || wait.Kind == BootstrapWaitResourceExists {
		err = r.pollKubectl(ctx, args)
	} else {
		err = r.runKubectl(ctx, args)
	}
	return false, err
}

func (r KubectlBootstrapRunner) workDir() (string, func(), error) {
	if strings.TrimSpace(r.TempDir) != "" {
		if err := os.MkdirAll(r.TempDir, 0o700); err != nil {
			return "", func() {}, fmt.Errorf("create bootstrap temp dir: %w", err)
		}
		return r.TempDir, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "katl-bootstrap-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create bootstrap temp dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (r KubectlBootstrapRunner) writeKubeconfig(dir, endpoint string, credentials AdminCredentials) (string, error) {
	server, err := kubeconfig.SelectServer(kubeconfig.EndpointSelection{InitialEndpoint: endpoint})
	if err != nil {
		return "", err
	}
	data, err := kubeconfig.Render(kubeconfig.RenderRequest{
		Server:                   server,
		ClusterName:              "katl-bootstrap",
		ContextName:              "katl-bootstrap",
		UserName:                 "katl-bootstrap-admin",
		CertificateAuthorityData: credentials.CertificateAuthorityData,
		ClientCertificateData:    credentials.ClientCertificateData,
		ClientKeyData:            credentials.ClientKeyData,
	})
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kubeconfig")
	if strings.Contains(endpoint, "api") {
		sum := sha256.Sum256([]byte(endpoint))
		path = filepath.Join(dir, "kubeconfig-"+hex.EncodeToString(sum[:])[:8])
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write bootstrap kubeconfig: %w", err)
	}
	return path, nil
}

func (r KubectlBootstrapRunner) writeManifest(dir string, index int, manifest BootstrapManifest) (string, error) {
	name := fmt.Sprintf("%04d.yaml", index)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, manifest.Content, 0o600); err != nil {
		return "", fmt.Errorf("write bootstrap manifest: %w", err)
	}
	return path, nil
}

func (r KubectlBootstrapRunner) runKubectl(ctx context.Context, args []string) error {
	return r.runKubectlWithTimeout(ctx, args, r.timeout())
}

func (r KubectlBootstrapRunner) runKubectlProbe(ctx context.Context, args []string) error {
	return r.runKubectlWithTimeout(ctx, args, r.probeTimeout())
}

func (r KubectlBootstrapRunner) pollKubectl(ctx context.Context, args []string) error {
	timeout := r.timeout()
	interval := r.pollInterval()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.runKubectlProbe(ctx, args); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bounded kubectl wait timed out after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r KubectlBootstrapRunner) runKubectlWithTimeout(ctx context.Context, args []string, timeout time.Duration) error {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{"kubectl"}, args...)
	runner := r.CommandRunner
	if runner == nil {
		runner = execKubectlCommandRunner{}
	}
	result, err := runner.Run(ctx, argv)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return commandError(argv, result)
	}
	return nil
}

func (r KubectlBootstrapRunner) timeout() time.Duration {
	if r.Timeout != 0 {
		return r.Timeout
	}
	return 5 * time.Minute
}

func (r KubectlBootstrapRunner) pollInterval() time.Duration {
	if r.PollInterval != 0 {
		return r.PollInterval
	}
	return 2 * time.Second
}

func (r KubectlBootstrapRunner) probeTimeout() time.Duration {
	if r.ProbeTimeout != 0 {
		return r.ProbeTimeout
	}
	return 10 * time.Second
}

type execKubectlCommandRunner struct{}

func (execKubectlCommandRunner) Run(ctx context.Context, argv []string) (readiness.CommandResult, error) {
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

func waitKubectlArgs(kubeconfigPath string, wait BootstrapWait) ([]string, error) {
	base := []string{"--kubeconfig", kubeconfigPath, "--context", "katl-bootstrap"}
	if wait.Namespace != "" {
		base = append(base, "-n", wait.Namespace)
	}
	switch wait.Kind {
	case BootstrapWaitAPIReady:
		return append(base, "get", "--raw=/readyz"), nil
	case BootstrapWaitResourceExists:
		return append(base, "get", wait.Name), nil
	case BootstrapWaitCondition:
		return append(base, "wait", "--for=condition="+wait.Condition, wait.Name, "--timeout="+kubectlWaitTimeout), nil
	case BootstrapWaitNodesReady:
		return append(base, "wait", "--for=condition=Ready", "nodes", "--all", "--timeout="+kubectlWaitTimeout), nil
	case BootstrapWaitRolloutStatus:
		return append(base, "rollout", "status", wait.Name, "--timeout="+kubectlWaitTimeout), nil
	case BootstrapWaitPodsReady:
		return append(base, "wait", "--for=condition=Ready", "pod", "-l", wait.Selector, "--timeout="+kubectlWaitTimeout), nil
	default:
		return nil, fmt.Errorf("bootstrap wait kind %q is unsupported", wait.Kind)
	}
}

type TransportRunner struct {
	Transport       readiness.CommandTransport
	Timeout         time.Duration
	APITimeout      time.Duration
	APIPollInterval time.Duration
	OutputLimit     uint32
	FileLimit       uint32
}

func (r TransportRunner) RunKubeadmInit(ctx context.Context, node inventory.PlannedNode, controlPlaneEndpoint string) (AdminCredentials, error) {
	configPath, err := r.initConfigPath(ctx, node, controlPlaneEndpoint)
	if err != nil {
		return AdminCredentials{}, err
	}
	result, err := r.run(ctx, node, []string{"kubeadm", "init", "--config", configPath}, true)
	if err != nil {
		if result.ExitStatus == 0 || !alreadyInitialized(result) {
			return AdminCredentials{}, err
		}
	}
	transport := r.transport()
	if transport == nil {
		return AdminCredentials{}, errors.New("bootstrap command transport is required")
	}
	file, err := transport.ReadFile(ctx, node, readiness.FileRequest{
		Path:      adminKubeconfigPath,
		Timeout:   r.timeout(),
		MaxBytes:  r.fileLimit(),
		Sensitive: true,
	})
	if err != nil {
		return AdminCredentials{}, err
	}
	return parseAdminCredentials(file.Content)
}

func (r TransportRunner) initConfigPath(ctx context.Context, node inventory.PlannedNode, controlPlaneEndpoint string) (string, error) {
	source := kubeadmConfigPath(node)
	endpoint := strings.TrimSpace(controlPlaneEndpoint)
	if endpoint == "" {
		return source, nil
	}
	transport := r.transport()
	if transport == nil {
		return "", errors.New("bootstrap command transport is required")
	}
	file, err := transport.ReadFile(ctx, node, readiness.FileRequest{
		Path:      source,
		Timeout:   r.timeout(),
		MaxBytes:  r.fileLimit(),
		Sensitive: true,
	})
	if err != nil {
		return "", fmt.Errorf("read kubeadm init config %s: %w", source, err)
	}
	content, err := renderInitConfig(file.Content, endpoint)
	if err != nil {
		return "", err
	}
	target := generatedInitConfigPath(node)
	if _, err := transport.WriteFile(ctx, node, readiness.WriteFileRequest{
		Path:      target,
		Content:   content,
		Mode:      0o600,
		Timeout:   r.timeout(),
		Sensitive: true,
	}); err != nil {
		return "", fmt.Errorf("write kubeadm init config %s: %w", target, err)
	}
	return target, nil
}

func (r TransportRunner) CreateWorkerJoin(ctx context.Context, initNode inventory.PlannedNode) (JoinMaterial, error) {
	result, err := r.run(ctx, initNode, []string{"kubeadm", "token", "create", "--print-join-command", "--kubeconfig", adminKubeconfigPath}, true)
	if err != nil {
		return JoinMaterial{}, err
	}
	return parseJoinMaterial(result.Stdout)
}

func (r TransportRunner) CreateControlPlaneJoin(ctx context.Context, initNode inventory.PlannedNode) (JoinMaterial, error) {
	keyResult, err := r.run(ctx, initNode, []string{"kubeadm", "init", "phase", "upload-certs", "--upload-certs", "--kubeconfig", adminKubeconfigPath}, true)
	if err != nil {
		return JoinMaterial{}, err
	}
	certificateKey := certificateKey(keyResult.Stdout + "\n" + keyResult.Stderr)
	if certificateKey == "" {
		return JoinMaterial{}, errors.New("kubeadm did not print a certificate key")
	}
	result, err := r.run(ctx, initNode, []string{"kubeadm", "token", "create", "--print-join-command", "--certificate-key", certificateKey, "--kubeconfig", adminKubeconfigPath}, true)
	if err != nil {
		return JoinMaterial{}, err
	}
	return ControlPlaneJoinMaterial(result.Stdout, certificateKey)
}

func (r TransportRunner) RunControlPlaneJoin(ctx context.Context, node inventory.PlannedNode, material JoinMaterial) error {
	if node.SystemRole != inventory.RoleControlPlane {
		return fmt.Errorf("control-plane join requires control-plane node, got %s", node.SystemRole)
	}
	if len(material.Argv) == 0 {
		return errors.New("control-plane join material is required")
	}
	if !hasFlag(material.Argv, "--control-plane") {
		return errors.New("control-plane join material must include --control-plane")
	}
	if flagValue(material.Argv, "--certificate-key") == "" {
		return errors.New("control-plane join material must include --certificate-key")
	}
	configPath, err := r.writeJoinConfig(ctx, node, material, true)
	if err != nil {
		return err
	}
	argv := []string{"kubeadm", "join", "--config", configPath}
	result, err := r.run(ctx, node, argv, true)
	if err != nil && (!alreadyJoined(result) || !r.controlPlaneJoinComplete(ctx, node)) {
		return err
	}
	return nil
}

func (r TransportRunner) WaitControlPlaneJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error {
	timeout := r.apiTimeout()
	interval := r.apiPollInterval()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.controlPlaneJoinReady(ctx, initNode, node); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control-plane %s health: %w", node.Name, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r TransportRunner) WaitWorkerJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error {
	timeout := r.apiTimeout()
	interval := r.apiPollInterval()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.workerJoinReady(ctx, initNode, node); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("worker %s registration: %w", node.Name, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func parseJoinMaterial(output string) (JoinMaterial, error) {
	argv := strings.Fields(strings.TrimSpace(output))
	if len(argv) < 2 || argv[0] != "kubeadm" || argv[1] != "join" {
		return JoinMaterial{}, errors.New("kubeadm did not print a join command")
	}
	return JoinMaterial{Argv: argv}, nil
}

func ParseJoinMaterial(output string) (JoinMaterial, error) {
	return parseJoinMaterial(output)
}

func (r TransportRunner) RunWorkerJoin(ctx context.Context, node inventory.PlannedNode, material JoinMaterial) error {
	if node.SystemRole != inventory.RoleWorker {
		return fmt.Errorf("worker join requires worker node, got %s", node.SystemRole)
	}
	if len(material.Argv) == 0 {
		return errors.New("worker join material is required")
	}
	if hasFlag(material.Argv, "--control-plane") {
		return errors.New("worker join material must not include --control-plane")
	}
	if flagValue(material.Argv, "--certificate-key") != "" {
		return errors.New("worker join material must not include --certificate-key")
	}
	configPath, err := r.writeJoinConfig(ctx, node, material, false)
	if err != nil {
		return err
	}
	argv := []string{"kubeadm", "join", "--config", configPath}
	result, err := r.run(ctx, node, argv, true)
	if err != nil && (!alreadyJoined(result) || !r.workerJoinComplete(ctx, node)) {
		return err
	}
	return nil
}

func (r TransportRunner) writeJoinConfig(ctx context.Context, node inventory.PlannedNode, material JoinMaterial, controlPlane bool) (string, error) {
	transport := r.transport()
	if transport == nil {
		return "", errors.New("bootstrap command transport is required")
	}
	source := kubeadmConfigPath(node)
	file, err := transport.ReadFile(ctx, node, readiness.FileRequest{
		Path:      source,
		Timeout:   r.timeout(),
		MaxBytes:  r.fileLimit(),
		Sensitive: true,
	})
	if err != nil {
		return "", fmt.Errorf("read kubeadm join config %s: %w", source, err)
	}
	content, err := renderJoinConfig(file.Content, material, controlPlane)
	if err != nil {
		return "", err
	}
	target := generatedJoinConfigPath(node)
	if _, err := transport.WriteFile(ctx, node, readiness.WriteFileRequest{
		Path:      target,
		Content:   content,
		Mode:      0o600,
		Timeout:   r.timeout(),
		Sensitive: true,
	}); err != nil {
		return "", fmt.Errorf("write kubeadm join config %s: %w", target, err)
	}
	return target, nil
}

func RenderInitConfig(base []byte, controlPlaneEndpoint string, additionalSANs ...string) ([]byte, error) {
	endpoint := strings.TrimSpace(controlPlaneEndpoint)
	if endpoint == "" {
		return base, nil
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("control-plane endpoint must be host:port")
	}
	docs, err := decodeYAMLDocuments(base)
	if err != nil {
		return nil, fmt.Errorf("decode kubeadm init config: %w", err)
	}
	found := false
	for _, doc := range docs {
		if docKind(doc) != "ClusterConfiguration" {
			continue
		}
		found = true
		if configured := strings.TrimSpace(fmt.Sprint(doc["controlPlaneEndpoint"])); configured != "" && configured != "<nil>" && configured != endpoint {
			return nil, fmt.Errorf("kubeadm ClusterConfiguration controlPlaneEndpoint %q conflicts with cluster endpoint %q", configured, endpoint)
		}
		doc["controlPlaneEndpoint"] = endpoint
		apiServer, _ := doc["apiServer"].(map[string]any)
		if apiServer == nil {
			apiServer = map[string]any{}
		}
		apiServer["certSANs"] = appendUniqueString(apiServer["certSANs"], host)
		for _, san := range additionalSANs {
			if san = strings.TrimSpace(san); san != "" {
				apiServer["certSANs"] = appendUniqueString(apiServer["certSANs"], san)
			}
		}
		doc["apiServer"] = apiServer
	}
	if !found {
		return nil, errors.New("kubeadm init config did not contain ClusterConfiguration")
	}
	return encodeYAMLDocuments(docs)
}

func renderInitConfig(base []byte, controlPlaneEndpoint string) ([]byte, error) {
	return RenderInitConfig(base, controlPlaneEndpoint)
}

func renderJoinConfig(base []byte, material JoinMaterial, controlPlane bool, discoveryKubeconfigPath ...string) ([]byte, error) {
	endpoint, token, hashes, err := joinDiscovery(material)
	if err != nil {
		return nil, err
	}
	docs, err := decodeYAMLDocuments(base)
	if err != nil {
		return nil, fmt.Errorf("decode kubeadm join config: %w", err)
	}
	var joinDocs []map[string]any
	var initDoc map[string]any
	for _, doc := range docs {
		switch docKind(doc) {
		case "JoinConfiguration":
			joinDocs = append(joinDocs, doc)
		case "InitConfiguration":
			initDoc = doc
		}
	}
	if controlPlane && len(joinDocs) == 0 && initDoc != nil {
		joinDocs = append(joinDocs, synthesizeJoinConfig(initDoc))
	}
	if len(joinDocs) == 0 {
		return nil, errors.New("kubeadm join config did not contain JoinConfiguration")
	}
	discoveryPath := ""
	if len(material.DiscoveryKubeconfig) > 0 {
		if len(discoveryKubeconfigPath) != 1 || strings.TrimSpace(discoveryKubeconfigPath[0]) == "" {
			return nil, errors.New("join discovery kubeconfig path is required")
		}
		discoveryPath = strings.TrimSpace(discoveryKubeconfigPath[0])
	}
	for _, doc := range joinDocs {
		if discoveryPath != "" {
			doc["discovery"] = map[string]any{
				"file": map[string]any{
					"kubeConfigPath": discoveryPath,
				},
				"tlsBootstrapToken": token,
			}
		} else {
			doc["discovery"] = map[string]any{
				"bootstrapToken": map[string]any{
					"apiServerEndpoint": endpoint,
					"token":             token,
					"caCertHashes":      hashes,
				},
			}
		}
		if controlPlane {
			cp, _ := doc["controlPlane"].(map[string]any)
			if cp == nil {
				cp = map[string]any{}
			}
			cp["certificateKey"] = flagValue(material.Argv, "--certificate-key")
			doc["controlPlane"] = cp
		}
	}
	return encodeYAMLDocuments(joinDocs)
}

func RenderWorkerJoinConfig(base []byte, material JoinMaterial, discoveryKubeconfigPath ...string) ([]byte, error) {
	if len(material.Argv) == 0 {
		return nil, errors.New("worker join material is required")
	}
	if hasFlag(material.Argv, "--control-plane") {
		return nil, errors.New("worker join material must not include --control-plane")
	}
	if flagValue(material.Argv, "--certificate-key") != "" {
		return nil, errors.New("worker join material must not include --certificate-key")
	}
	return renderJoinConfig(base, material, false, discoveryKubeconfigPath...)
}

func RenderControlPlaneJoinConfig(base []byte, material JoinMaterial, discoveryKubeconfigPath ...string) ([]byte, error) {
	if len(material.Argv) == 0 {
		return nil, errors.New("control-plane join material is required")
	}
	if !hasFlag(material.Argv, "--control-plane") {
		return nil, errors.New("control-plane join material must include --control-plane")
	}
	if flagValue(material.Argv, "--certificate-key") == "" {
		return nil, errors.New("control-plane join material must include --certificate-key")
	}
	return renderJoinConfig(base, material, true, discoveryKubeconfigPath...)
}

func ControlPlaneJoinMaterial(output string, certificateKey string) (JoinMaterial, error) {
	certificateKey = strings.TrimSpace(certificateKey)
	if certificateKey == "" {
		return JoinMaterial{}, errors.New("control-plane join material must include --certificate-key")
	}
	material, err := parseJoinMaterial(output)
	if err != nil {
		return JoinMaterial{}, err
	}
	material.Argv = ensureFlag(material.Argv, "--control-plane")
	material.Argv = ensureFlagValue(material.Argv, "--certificate-key", certificateKey)
	return material, nil
}

func CertificateKey(output string) string {
	return certificateKey(output)
}

func appendUniqueString(value any, item string) []any {
	item = strings.TrimSpace(item)
	var items []any
	switch typed := value.(type) {
	case []any:
		items = append(items, typed...)
	case []string:
		for _, existing := range typed {
			items = append(items, existing)
		}
	}
	for _, existing := range items {
		if text, ok := existing.(string); ok && strings.TrimSpace(text) == item {
			return items
		}
	}
	return append(items, item)
}

func synthesizeJoinConfig(initDoc map[string]any) map[string]any {
	doc := map[string]any{
		"apiVersion": "kubeadm.k8s.io/v1beta4",
		"kind":       "JoinConfiguration",
	}
	if nodeRegistration, ok := initDoc["nodeRegistration"].(map[string]any); ok {
		doc["nodeRegistration"] = nodeRegistration
	}
	if patches, ok := initDoc["patches"].(map[string]any); ok {
		doc["patches"] = patches
	}
	return doc
}

func joinDiscovery(material JoinMaterial) (string, string, []string, error) {
	if len(material.Argv) < 3 || material.Argv[0] != "kubeadm" || material.Argv[1] != "join" {
		return "", "", nil, errors.New("join material must start with kubeadm join")
	}
	endpoint := strings.TrimSpace(material.Argv[2])
	token := strings.TrimSpace(flagValue(material.Argv, "--token"))
	hash := strings.TrimSpace(flagValue(material.Argv, "--discovery-token-ca-cert-hash"))
	if endpoint == "" {
		return "", "", nil, errors.New("join material is missing API server endpoint")
	}
	if token == "" {
		return "", "", nil, errors.New("join material is missing bootstrap token")
	}
	if hash == "" {
		return "", "", nil, errors.New("join material is missing discovery token CA cert hash")
	}
	return endpoint, token, []string{hash}, nil
}

func decodeYAMLDocuments(data []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]any
	for {
		doc := map[string]any{}
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, errors.New("kubeadm join config is empty")
	}
	return docs, nil
}

func encodeYAMLDocuments(docs []map[string]any) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, doc := range docs {
		if err := encoder.Encode(doc); err != nil {
			_ = encoder.Close()
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func docKind(doc map[string]any) string {
	kind, _ := doc["kind"].(string)
	return kind
}

func (r TransportRunner) WaitAPIReady(ctx context.Context, initNode inventory.PlannedNode) error {
	timeout := r.apiTimeout()
	interval := r.apiPollInterval()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.runSensitive(ctx, initNode, []string{"kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz"}); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for API readyz: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r TransportRunner) workerJoinComplete(ctx context.Context, node inventory.PlannedNode) bool {
	for _, argv := range [][]string{
		{"test", "-f", "/etc/kubernetes/kubelet.conf"},
		{"systemctl", "is-active", "--quiet", "kubelet.service"},
	} {
		if _, err := r.run(ctx, node, argv, false); err != nil {
			return false
		}
	}
	return true
}

func (r TransportRunner) controlPlaneJoinComplete(ctx context.Context, node inventory.PlannedNode) bool {
	for _, argv := range [][]string{
		{"test", "-f", "/etc/kubernetes/kubelet.conf"},
		{"test", "-f", "/etc/kubernetes/manifests/kube-apiserver.yaml"},
		{"test", "-f", "/etc/kubernetes/manifests/kube-controller-manager.yaml"},
		{"test", "-f", "/etc/kubernetes/manifests/kube-scheduler.yaml"},
		{"test", "-f", "/etc/kubernetes/manifests/etcd.yaml"},
		{"systemctl", "is-active", "--quiet", "kubelet.service"},
	} {
		if _, err := r.run(ctx, node, argv, false); err != nil {
			return false
		}
	}
	return true
}

func (r TransportRunner) controlPlaneJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error {
	if err := r.runSensitive(ctx, initNode, []string{"kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz"}); err != nil {
		return fmt.Errorf("api readyz: %w", err)
	}
	if err := r.runSensitive(ctx, initNode, []string{"kubectl", "--kubeconfig", adminKubeconfigPath, "get", "node", node.Name}); err != nil {
		return fmt.Errorf("node object: %w", err)
	}
	for _, name := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
		if err := r.runningContainer(ctx, node, name); err != nil {
			return err
		}
	}
	etcdReport, err := (EtcdChecker{Transport: r.transport(), Timeout: r.timeout(), OutputLimit: r.outputLimit()}).Check(ctx, node)
	if err != nil {
		return err
	}
	if !etcdReport.Healthy {
		return fmt.Errorf("etcd health: %s", diagnosticsSummary(etcdReport.Diagnostics))
	}
	if !etcdReport.HasMember(node.Name) {
		return fmt.Errorf("etcd health: member %s is missing from etcd member list", node.Name)
	}
	return nil
}

func (r TransportRunner) workerJoinReady(ctx context.Context, initNode, node inventory.PlannedNode) error {
	if err := r.runSensitive(ctx, initNode, []string{"kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz"}); err != nil {
		return fmt.Errorf("api readyz: %w", err)
	}
	if err := r.runSensitive(ctx, initNode, []string{"kubectl", "--kubeconfig", adminKubeconfigPath, "get", "node", node.Name}); err != nil {
		return fmt.Errorf("node object: %w", err)
	}
	return nil
}

func (r TransportRunner) runningContainer(ctx context.Context, node inventory.PlannedNode, name string) error {
	result, err := r.run(ctx, node, []string{"crictl", "ps", "--name", name, "--state", "Running", "--quiet"}, false)
	if err != nil {
		return fmt.Errorf("%s static pod: %w", name, err)
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return fmt.Errorf("%s static pod is not running", name)
	}
	return nil
}

func (r TransportRunner) runSensitive(ctx context.Context, node inventory.PlannedNode, argv []string) error {
	_, err := r.run(ctx, node, argv, true)
	return err
}

func (r TransportRunner) run(ctx context.Context, node inventory.PlannedNode, argv []string, sensitive bool) (readiness.CommandResult, error) {
	transport := r.transport()
	if transport == nil {
		return readiness.CommandResult{}, errors.New("bootstrap command transport is required")
	}
	result, err := transport.RunCommand(ctx, node, readiness.CommandRequest{
		Argv:            argv,
		Timeout:         r.timeout(),
		StdoutLimit:     r.outputLimit(),
		StderrLimit:     r.outputLimit(),
		SensitiveOutput: sensitive,
	})
	if err != nil {
		return result, err
	}
	if result.ExitStatus != 0 {
		return result, commandError(argv, result)
	}
	return result, nil
}

func (r TransportRunner) transport() readiness.CommandTransport {
	return r.Transport
}

func (r TransportRunner) timeout() time.Duration {
	if r.Timeout != 0 {
		return r.Timeout
	}
	return 5 * time.Minute
}

func (r TransportRunner) apiTimeout() time.Duration {
	if r.APITimeout != 0 {
		return r.APITimeout
	}
	return 3 * time.Minute
}

func (r TransportRunner) apiPollInterval() time.Duration {
	if r.APIPollInterval != 0 {
		return r.APIPollInterval
	}
	return 2 * time.Second
}

func (r TransportRunner) outputLimit() uint32 {
	if r.OutputLimit != 0 {
		return r.OutputLimit
	}
	return 256 << 10
}

func (r TransportRunner) fileLimit() uint32 {
	if r.FileLimit != 0 {
		return r.FileLimit
	}
	return 512 << 10
}

func commandError(argv []string, result readiness.CommandResult) error {
	parts := []string{fmt.Sprintf("%q exited %d", inventory.Redact(strings.Join(argv, " ")), result.ExitStatus)}
	if strings.TrimSpace(result.Stdout) != "" {
		parts = append(parts, "stdout: "+inventory.Redact(strings.TrimSpace(result.Stdout)))
	}
	if strings.TrimSpace(result.Stderr) != "" {
		parts = append(parts, "stderr: "+inventory.Redact(strings.TrimSpace(result.Stderr)))
	}
	return errors.New(strings.Join(parts, "; "))
}

func certificateKey(output string) string {
	previousLineMentionedKey := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if certificateKeyLinePattern.MatchString(line) {
			if previousLineMentionedKey {
				return line
			}
			continue
		}
		previousLineMentionedKey = strings.Contains(strings.ToLower(line), "certificate key")
	}
	return ""
}

func ensureFlag(argv []string, flag string) []string {
	for _, arg := range argv {
		if arg == flag {
			return argv
		}
	}
	return append(argv, flag)
}

func hasFlag(argv []string, flag string) bool {
	for _, arg := range argv {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func flagValue(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag {
			if i+1 >= len(argv) {
				return ""
			}
			return argv[i+1]
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			return value
		}
	}
	return ""
}

func ensureFlagValue(argv []string, flag, value string) []string {
	for i, arg := range argv {
		if arg == flag {
			if i+1 < len(argv) {
				argv[i+1] = value
				return argv
			}
			return append(argv, value)
		}
		if strings.HasPrefix(arg, flag+"=") {
			argv[i] = flag + "=" + value
			return argv
		}
	}
	return append(argv, flag, value)
}

func diagnosticsSummary(diagnostics []inventory.Diagnostic) string {
	if len(diagnostics) == 0 {
		return "not healthy"
	}
	parts := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		parts = append(parts, diagnostic.Field+": "+inventory.Redact(diagnostic.Message))
	}
	return strings.Join(parts, "; ")
}

func alreadyInitialized(result readiness.CommandResult) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "already initialized")
}

func alreadyJoined(result readiness.CommandResult) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "already joined")
}

func kubeadmConfigPath(node inventory.PlannedNode) string {
	if strings.TrimSpace(node.KubeadmConfig.Path) != "" {
		return node.KubeadmConfig.Path
	}
	return "/etc/katl/kubeadm/" + node.KubeadmConfig.Ref + "/config.yaml"
}

func generatedInitConfigPath(node inventory.PlannedNode) string {
	return generatedKubeadmConfigPath(node, "init")
}

func generatedJoinConfigPath(node inventory.PlannedNode) string {
	return generatedKubeadmConfigPath(node, "join")
}

func generatedKubeadmConfigPath(node inventory.PlannedNode, action string) string {
	name := strings.TrimSpace(node.Name)
	if name == "" {
		name = "node"
	}
	var clean strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			clean.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			clean.WriteRune(r)
		case r >= '0' && r <= '9':
			clean.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			clean.WriteRune(r)
		default:
			clean.WriteByte('-')
		}
	}
	name = clean.String()
	name = strings.Trim(name, "-")
	if name == "" {
		name = "node"
	}
	return "/var/lib/katl/test-artifacts/kubeadm-" + action + "-" + name + ".yaml"
}

func parseAdminCredentials(data []byte) (AdminCredentials, error) {
	var parsed struct {
		Clusters []struct {
			Cluster struct {
				CertificateAuthorityData string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				ClientCertificateData string `yaml:"client-certificate-data"`
				ClientKeyData         string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return AdminCredentials{}, fmt.Errorf("parse admin kubeconfig: %w", err)
	}
	if len(parsed.Clusters) == 0 || len(parsed.Users) == 0 {
		return AdminCredentials{}, errors.New("admin kubeconfig is missing cluster or user data")
	}
	credentials := AdminCredentials{
		CertificateAuthorityData: strings.TrimSpace(parsed.Clusters[0].Cluster.CertificateAuthorityData),
		ClientCertificateData:    strings.TrimSpace(parsed.Users[0].User.ClientCertificateData),
		ClientKeyData:            strings.TrimSpace(parsed.Users[0].User.ClientKeyData),
	}
	if credentials.CertificateAuthorityData == "" || credentials.ClientCertificateData == "" || credentials.ClientKeyData == "" {
		return AdminCredentials{}, errors.New("admin kubeconfig is missing embedded credential data")
	}
	return credentials, nil
}
