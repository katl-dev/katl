package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

const OperationKindKubeadmControlPlaneConfig = "kubeadm-control-plane-config"

const (
	kubeadmConfigComponentControlPlane = "component/control-plane"
	kubeadmConfigComponentKubelet      = "component/kubelet"
	kubeadmConfigComponentKubeProxy    = "component/kube-proxy"
)

var supportedControlPlaneConfigFields = map[string]bool{
	"ClusterConfiguration.apiServer.extraArgs.profiling=false":         true,
	"ClusterConfiguration.controllerManager.extraArgs.profiling=false": true,
	"ClusterConfiguration.scheduler.extraArgs.profiling=false":         true,
	kubeadmConfigComponentControlPlane:                                 true,
	kubeadmConfigComponentKubelet:                                      true,
	kubeadmConfigComponentKubeProxy:                                    true,
}

func validateKubeadmControlPlaneConfigRequest(kind string, req *agentapi.KubeadmControlPlaneConfigOperationRequest) error {
	if kind != OperationKindKubeadmControlPlaneConfig {
		return fmt.Errorf("operation kind must be %q", OperationKindKubeadmControlPlaneConfig)
	}
	if strings.TrimSpace(req.GetRolloutId()) == "" {
		return fmt.Errorf("rolloutID is required")
	}
	if req.GetNodeCount() < 1 || req.GetNodePosition() < 1 || req.GetNodePosition() > req.GetNodeCount() {
		return fmt.Errorf("node position must identify one node in the rollout")
	}
	if strings.TrimSpace(req.GetCoordinatorNode()) == "" || strings.TrimSpace(req.GetDesiredGenerationId()) == "" {
		return fmt.Errorf("coordinatorNode and desiredGenerationID are required")
	}
	if strings.TrimSpace(req.GetNodeName()) == "" {
		return fmt.Errorf("nodeName is required")
	}
	name := strings.TrimSpace(req.GetConfigName())
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("configName is invalid")
	}
	for field, value := range map[string]string{"desiredConfigSHA256": req.GetDesiredConfigSha256(), "expectedLiveConfigSHA256": req.GetExpectedLiveConfigSha256(), "kubernetesPayloadSHA256": req.GetKubernetesPayloadSha256()} {
		if strings.TrimSpace(value) != "" {
			if err := validateDigestValue(field, value); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	component := ""
	for _, field := range req.GetSupportedFieldDelta() {
		if !supportedControlPlaneConfigFields[field] || seen[field] {
			return fmt.Errorf("unsupported or repeated field delta %q", field)
		}
		if field == kubeadmConfigComponentControlPlane || field == kubeadmConfigComponentKubelet || field == kubeadmConfigComponentKubeProxy {
			if component != "" {
				return fmt.Errorf("exactly one kubeadm component may be selected")
			}
			component = field
		}
		seen[field] = true
	}
	if (component == kubeadmConfigComponentKubelet || component == kubeadmConfigComponentKubeProxy) && len(seen) != 1 {
		return fmt.Errorf("%s configuration does not accept control-plane field deltas", strings.TrimPrefix(component, "component/"))
	}
	return nil
}

func kubeadmConfigComponentFromFields(fields []string) string {
	for _, field := range fields {
		if field == kubeadmConfigComponentKubeProxy {
			return "kube-proxy"
		}
		if field == kubeadmConfigComponentKubelet {
			return "kubelet"
		}
	}
	return "control-plane"
}

func controlPlaneConfigFromProto(req *agentapi.KubeadmControlPlaneConfigOperationRequest) operation.KubeadmControlPlaneConfig {
	component := kubeadmConfigComponentFromFields(req.GetSupportedFieldDelta())
	var fields []string
	for _, field := range req.GetSupportedFieldDelta() {
		if field != kubeadmConfigComponentControlPlane && field != kubeadmConfigComponentKubelet && field != kubeadmConfigComponentKubeProxy {
			fields = append(fields, field)
		}
	}
	return operation.KubeadmControlPlaneConfig{
		Component: component, RolloutID: req.GetRolloutId(), NodePosition: req.GetNodePosition(), NodeCount: req.GetNodeCount(), CoordinatorNode: req.GetCoordinatorNode(), NodeName: req.GetNodeName(), CoordinatorUpload: req.GetCoordinatorUpload(), DesiredGenerationID: req.GetDesiredGenerationId(), ConfigName: req.GetConfigName(), ConfigPath: "/etc/katl/kubeadm/" + req.GetConfigName() + "/config.yaml", DesiredConfigSHA256: req.GetDesiredConfigSha256(), ExpectedLiveConfigSHA256: req.GetExpectedLiveConfigSha256(), KubernetesPayloadVersion: req.GetKubernetesPayloadVersion(), KubernetesPayloadSHA256: req.GetKubernetesPayloadSha256(), SupportedFieldDelta: fields, SnapshotRef: req.GetSnapshotRef(), SnapshotDigest: req.GetSnapshotDigest(), SnapshotRevision: req.GetSnapshotRevision(), CapturedMemberListDigest: req.GetCapturedMemberListDigest(), SourceEtcdVersion: req.GetSourceEtcdVersion(), SnapshotCreatedAt: req.GetSnapshotCreatedAt(), SnapshotStorageLocation: req.GetSnapshotStorageLocation(), SnapshotOperatorIdentity: req.GetSnapshotOperatorIdentity(),
	}
}

func (s *Server) acceptKubeadmControlPlaneConfigOperation(req *agentapi.SubmitOperationRequest, digest, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	body, err := s.validateKubeadmControlPlaneConfigState(req)
	if err != nil {
		return operation.OperationRecord{}, nil, err
	}
	var phases []string
	if body.Component == "kube-proxy" {
		phases = []string{"accepted", "preflight-complete", "kube-proxy-config-running", "kube-proxy-rollout-complete", operation.HostBookkeepingCompletionPhase}
	} else if body.Component == "kubelet" {
		phases = []string{"accepted", "preflight-complete", "kubelet-config-backup-complete"}
		if body.CoordinatorUpload {
			phases = append(phases, "kubelet-config-upload-running", "kubelet-config-upload-complete")
		}
		phases = append(phases, "kubelet-config-running", "kubelet-config-complete", "kubelet-restart-running", "post-kubelet-health-complete", operation.HostBookkeepingCompletionPhase)
	} else {
		phases = []string{"accepted", "preflight-complete", "cordon-complete", "manifest-backup-complete", "control-plane-manifests-running", "control-plane-manifests-complete", "post-manifest-health-complete"}
		if body.CoordinatorUpload {
			phases = append(phases, "kubeadm-config-upload-running", "kubeadm-config-upload-complete", "post-upload-health-complete")
		}
		phases = append(phases, "uncordon-complete", operation.HostBookkeepingCompletionPhase)
	}
	record := operation.OperationRecord{
		OperationID: id, OperationKind: req.OperationKind, Scope: "kubeadm-state", ClientRequestID: req.ClientRequestId, Actor: req.Actor, ExpectedMachineID: req.ExpectedMachineId, ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId, ExpectedClusterIntentDigest: req.ExpectedClusterIntentDigest, RequestDigest: digest, Phase: "accepted", PhasePlan: phases, PreviousGenerationID: body.DesiredGenerationID, KubeadmControlPlaneConfig: &body, ActivationMode: operation.ActivationModeLive, ActivationState: operation.ActivationStatePending, GenerationCommitState: operation.GenerationCommitCommitted, ResourceLocks: locks, NextAction: "run bounded kubeadm configuration phases",
	}
	created, err := s.Store.Create(record, "accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, err
	}
	return created, nil, nil
}

func (s *Server) validateKubeadmControlPlaneConfigState(req *agentapi.SubmitOperationRequest) (operation.KubeadmControlPlaneConfig, error) {
	body := controlPlaneConfigFromProto(req.GetKubeadmControlPlaneConfig())
	if strings.TrimSpace(req.ExpectedCurrentGenerationId) != body.DesiredGenerationID {
		return body, fmt.Errorf("desired generation must equal expected active generation")
	}
	data, err := os.ReadFile(rootedRuntimePath(s.Root, body.ConfigPath))
	if err != nil {
		return body, fmt.Errorf("read selected kubeadm config: %w", err)
	}
	var desiredDigest string
	if body.Component == "kube-proxy" {
		desiredDigest, err = kubeadmplan.CanonicalKubeProxyConfigurationSHA256(data)
	} else if body.Component == "kubelet" {
		desiredDigest, err = kubeadmplan.CanonicalKubeletConfigurationSHA256(data)
	} else {
		desiredDigest, err = kubeadmplan.CanonicalClusterConfigurationSHA256(data)
	}
	if err != nil {
		return body, fmt.Errorf("read selected kubeadm config identity: %w", err)
	}
	if body.DesiredConfigSHA256 != "" && desiredDigest != body.DesiredConfigSHA256 {
		return body, fmt.Errorf("selected kubeadm config digest changed")
	}
	body.DesiredConfigSHA256 = desiredDigest
	spec, _, err := generation.ReadGeneration(s.Root, body.DesiredGenerationID)
	if err != nil {
		return body, fmt.Errorf("read desired generation: %w", err)
	}
	metadata, err := os.ReadFile(rootedRuntimePath(s.Root, "/etc/katl/node.json"))
	if err != nil {
		return body, fmt.Errorf("read active node metadata: %w", err)
	}
	var node struct {
		Kubeadm struct {
			ConfigRef string `json:"configRef"`
		} `json:"kubeadm"`
	}
	if err := json.Unmarshal(metadata, &node); err != nil {
		return body, fmt.Errorf("decode active node metadata: %w", err)
	}
	if strings.TrimSpace(node.Kubeadm.ConfigRef) != body.ConfigName {
		return body, fmt.Errorf("active generation does not select kubeadm config %q", body.ConfigName)
	}
	applyStatusPath, err := generation.ConfigApplyStatusPath(s.Root, body.DesiredGenerationID)
	if err != nil {
		return body, err
	}
	if applyStatus, readErr := generation.ReadConfigApplyStatus(applyStatusPath); readErr == nil && strings.TrimSpace(applyStatus.Kubeadm.SelectedConfigName) != "" && applyStatus.Kubeadm.SelectedConfigName != body.ConfigName {
		return body, fmt.Errorf("generation status selects kubeadm config %q instead of %q", applyStatus.Kubeadm.SelectedConfigName, body.ConfigName)
	}
	var matchedPayload *generation.ExtensionRef
	for _, ref := range spec.Sysexts {
		if ref.Name == "kubernetes" {
			copy := ref
			matchedPayload = &copy
			break
		}
	}
	if matchedPayload == nil || matchedPayload.PayloadVersion == "" || matchedPayload.SHA256 == "" {
		return body, fmt.Errorf("active generation has no identified Kubernetes payload")
	}
	if body.KubernetesPayloadVersion != "" && body.KubernetesPayloadVersion != matchedPayload.PayloadVersion {
		return body, fmt.Errorf("active Kubernetes payload version does not match request")
	}
	if body.KubernetesPayloadSHA256 != "" && body.KubernetesPayloadSHA256 != matchedPayload.SHA256 {
		return body, fmt.Errorf("active Kubernetes payload digest does not match request")
	}
	body.KubernetesPayloadVersion = matchedPayload.PayloadVersion
	body.KubernetesPayloadSHA256 = matchedPayload.SHA256
	return body, nil
}

func (e *Executor) executeKubeadmControlPlaneConfig(ctx context.Context, record operation.OperationRecord) error {
	request := record.KubeadmControlPlaneConfig
	if request.Component == "kube-proxy" {
		return e.executeKubeProxyConfig(ctx, record)
	}
	if request.Component == "kubelet" {
		return e.executeKubeletConfig(ctx, record)
	}
	desired, err := os.ReadFile(rootedRuntimePath(e.Root, request.ConfigPath))
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	desiredDigest, err := kubeadmplan.CanonicalClusterConfigurationSHA256(desired)
	if err != nil || desiredDigest != request.DesiredConfigSHA256 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("selected kubeadm config changed after operation acceptance"))
	}
	liveResult := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}"}, nil)
	if liveResult.Err != nil || liveResult.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("collect live kubeadm config: %s", toolFailure(liveResult)))
	}
	effective, delta, err := kubeadmplan.EffectiveControlPlaneConfiguration(desired, liveResult.Stdout)
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	requestedDelta := make([]string, 0, len(request.SupportedFieldDelta))
	for _, field := range request.SupportedFieldDelta {
		if field != kubeadmConfigComponentControlPlane {
			requestedDelta = append(requestedDelta, field)
		}
	}
	sort.Strings(requestedDelta)
	if len(requestedDelta) > 0 && !reflect.DeepEqual(delta, requestedDelta) {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("observed supported delta %v does not match request %v", delta, request.SupportedFieldDelta))
	}
	liveDigest, err := kubeadmplan.CanonicalClusterConfigurationSHA256(liveResult.Stdout)
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("identify live kubeadm config: %w", err))
	}
	if request.ExpectedLiveConfigSHA256 != "" && liveDigest != request.ExpectedLiveConfigSHA256 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("live kubeadm config digest is stale"))
	}
	if len(delta) == 0 {
		manifests, err := e.digestControlPlaneManifests()
		if err != nil {
			return e.failControlPlaneConfig(record, "preflight", err)
		}
		completedAt := e.clock()
		if _, err := e.Store.Update(record.OperationID, "no-change", operation.HostBookkeepingCompletionPhase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.KubeadmControlPlaneConfig.ExpectedLiveConfigSHA256 = liveDigest
			current.KubeadmControlPlaneConfig.SupportedFieldDelta = []string{}
			current.KubeadmControlPlaneConfig.BeforeManifestSHA256 = manifests
			current.KubeadmControlPlaneConfig.AfterManifestSHA256 = maps.Clone(manifests)
			current.Phase = operation.HostBookkeepingCompletionPhase
			current.CompletedPhases = appendMissing(current.CompletedPhases, "preflight-complete", operation.HostBookkeepingCompletionPhase)
			current.CompletedAt = &completedAt
			current.Terminal = true
			current.Result = operation.ResultSucceeded
			current.NextAction = "desired control-plane configuration already matches live state"
			current.UpdatedAt = completedAt
			return current, nil
		}); err != nil {
			return err
		}
		return nil
	}
	effectivePath, err := e.writeEffectiveControlPlaneConfig(record.OperationID, effective)
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	component := controlPlanePhaseTarget(delta)
	if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-dry-run", []string{"/usr/bin/kubeadm", "init", "phase", "control-plane", component, "--config", effectivePath, "--dry-run"}, false); err != nil {
		return err
	}
	schedResult := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "node", request.NodeName, "-o", "jsonpath={.spec.unschedulable}"}, nil)
	if schedResult.Err != nil || schedResult.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("read node schedulability: %s", toolFailure(schedResult)))
	}
	originalUnschedulable := strings.TrimSpace(string(schedResult.Stdout)) == "true"
	if _, err := e.Store.Update(record.OperationID, "preflight-complete", "preflight-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmControlPlaneConfig.ExpectedLiveConfigSHA256 = liveDigest
		current.KubeadmControlPlaneConfig.SupportedFieldDelta = append([]string(nil), delta...)
		current.KubeadmControlPlaneConfig.OriginalNodeUnschedulable = originalUnschedulable
		current.Phase = "preflight-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if err := e.runControlPlaneConfigCommand(ctx, record, "cordon-running", []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "cordon", request.NodeName}, true); err != nil {
		return err
	}
	if _, err := e.Store.Update(record.OperationID, "cordon-complete", "cordon-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "cordon-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "cordon-complete")
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	before, err := e.backupControlPlaneManifests(record.OperationID)
	if err != nil {
		return e.failControlPlaneConfig(record, "manifest-backup", err)
	}
	if _, err := e.Store.Update(record.OperationID, "manifest-backup-complete", "manifest-backup-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmControlPlaneConfig.BeforeManifestSHA256 = before
		current.Phase = "manifest-backup-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	argv := []string{"/usr/bin/kubeadm", "init", "phase", "control-plane", component, "--config", effectivePath}
	if err := e.runControlPlaneConfigCommand(ctx, record, "control-plane-manifests-running", argv, true); err != nil {
		return err
	}
	after, err := e.digestControlPlaneManifests()
	if err != nil {
		return e.failControlPlaneConfig(record, "post-manifest-digest", err)
	}
	if _, err := e.Store.Update(record.OperationID, "control-plane-manifests-complete", "control-plane-manifests-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmControlPlaneConfig.AfterManifestSHA256 = after
		current.Phase = "control-plane-manifests-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if result := e.runControlPlaneConfigHealth(ctx, request.NodeName); result.Err != nil || result.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "post-manifest-health", fmt.Errorf("post-manifest health failed: %s", toolFailure(result)))
	}
	if _, err := e.Store.Update(record.OperationID, "post-manifest-health-complete", "post-manifest-health-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "post-manifest-health-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "post-manifest-health-complete")
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if request.CoordinatorUpload {
		upload := []string{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubeadm", "--config", effectivePath}
		if err := e.runControlPlaneConfigCommand(ctx, record, "kubeadm-config-upload-running", upload, true); err != nil {
			return err
		}
		if _, err := e.Store.Update(record.OperationID, "kubeadm-config-upload-complete", "kubeadm-config-upload-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.KubeadmControlPlaneConfig.ConfigUploadRan = true
			current.UpdatedAt = e.clock()
			return current, nil
		}); err != nil {
			return err
		}
		if result := e.runControlPlaneConfigHealth(ctx, request.NodeName); result.Err != nil || result.ExitStatus != 0 {
			return e.failControlPlaneConfig(record, "post-upload-health", fmt.Errorf("post-upload health failed: %s", toolFailure(result)))
		}
		if _, err := e.Store.Update(record.OperationID, "post-upload-health-complete", "post-upload-health-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = "post-upload-health-complete"
			current.CompletedPhases = appendMissing(current.CompletedPhases, "post-upload-health-complete")
			current.UpdatedAt = e.clock()
			return current, nil
		}); err != nil {
			return err
		}
	}
	if !originalUnschedulable {
		if err := e.runControlPlaneConfigCommand(ctx, record, "uncordon-running", []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "uncordon", request.NodeName}, true); err != nil {
			return err
		}
	}
	if _, err := e.Store.Update(record.OperationID, "uncordon-complete", "uncordon-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "uncordon-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "uncordon-complete")
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if _, err = e.Store.Update(record.OperationID, "record-operation-complete", "record-operation-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = operation.HostBookkeepingCompletionPhase
		current.CompletedPhases = appendMissing(current.CompletedPhases, operation.HostBookkeepingCompletionPhase)
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	return e.finalizeSuccessfulOperation(ctx, record.OperationID)
}

func (e *Executor) writeEffectiveControlPlaneConfig(operationID string, data []byte) (string, error) {
	const name = "effective-kubeadm.yaml"
	path := filepath.Join(e.Store.Root, operationID, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("/var/lib/katl/operations", operationID, name)), nil
}

func controlPlanePhaseTarget(delta []string) string {
	components := map[string]string{}
	for _, field := range delta {
		parts := strings.Split(field, ".")
		if len(parts) < 3 {
			return "all"
		}
		switch parts[1] {
		case "apiServer":
			components[parts[1]] = "apiserver"
		case "controllerManager":
			components[parts[1]] = "controller-manager"
		case "scheduler":
			components[parts[1]] = "scheduler"
		default:
			return "all"
		}
	}
	if len(components) != 1 {
		return "all"
	}
	for _, component := range components {
		return component
	}
	return "all"
}

func (e *Executor) executeKubeProxyConfig(ctx context.Context, record operation.OperationRecord) error {
	request := record.KubeadmControlPlaneConfig
	desired, err := os.ReadFile(rootedRuntimePath(e.Root, request.ConfigPath))
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	desiredDigest, err := kubeadmplan.CanonicalKubeProxyConfigurationSHA256(desired)
	if err != nil || desiredDigest != request.DesiredConfigSHA256 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("selected kube-proxy configuration changed after operation acceptance"))
	}
	collect := func() ToolResult {
		return e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "jsonpath={.data.config\\.conf}"}, nil)
	}
	liveBefore := collect()
	if liveBefore.Err != nil || liveBefore.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("collect live kube-proxy config: %s", toolFailure(liveBefore)))
	}
	if kubeadmplan.KubeProxyConfigurationContains(liveBefore.Stdout, desired) == nil {
		completedAt := e.clock()
		if _, err := e.Store.Update(record.OperationID, "no-change", operation.HostBookkeepingCompletionPhase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = operation.HostBookkeepingCompletionPhase
			current.CompletedPhases = appendMissing(current.CompletedPhases, "preflight-complete", operation.HostBookkeepingCompletionPhase)
			current.CompletedAt = &completedAt
			current.Terminal = true
			current.Result = operation.ResultSucceeded
			current.NextAction = "desired kube-proxy configuration already matches live state"
			current.UpdatedAt = completedAt
			return current, nil
		}); err != nil {
			return err
		}
		return nil
	}
	argv := []string{"/usr/bin/kubeadm", "init", "phase", "addon", "kube-proxy", "--config", request.ConfigPath}
	if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-kube-proxy-config-validate", []string{"/usr/bin/kubeadm", "config", "validate", "--config", request.ConfigPath}, false); err != nil {
		return err
	}
	if _, err := e.Store.Update(record.OperationID, "preflight-complete", "preflight-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "preflight-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if err := e.runControlPlaneConfigCommand(ctx, record, "kube-proxy-config-running", argv, true); err != nil {
		return err
	}
	liveAfter := collect()
	if liveAfter.Err != nil || liveAfter.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "kube-proxy-config-verify", fmt.Errorf("collect updated kube-proxy config: %s", toolFailure(liveAfter)))
	}
	if err := kubeadmplan.KubeProxyConfigurationContains(liveAfter.Stdout, desired); err != nil {
		return e.failControlPlaneConfig(record, "kube-proxy-config-verify", err)
	}
	rollout := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "rollout", "status", "daemonset/kube-proxy", "--timeout=5m"}, nil)
	if rollout.Err != nil || rollout.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "kube-proxy-rollout", fmt.Errorf("kube-proxy rollout failed: %s", toolFailure(rollout)))
	}
	if _, err := e.Store.Update(record.OperationID, "kube-proxy-rollout-complete", "kube-proxy-rollout-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "kube-proxy-rollout-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "kube-proxy-rollout-complete")
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if _, err := e.Store.Update(record.OperationID, "record-operation-complete", operation.HostBookkeepingCompletionPhase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = operation.HostBookkeepingCompletionPhase
		current.CompletedPhases = appendMissing(current.CompletedPhases, operation.HostBookkeepingCompletionPhase)
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	return e.finalizeSuccessfulOperation(ctx, record.OperationID)
}

func (e *Executor) executeKubeletConfig(ctx context.Context, record operation.OperationRecord) error {
	request := record.KubeadmControlPlaneConfig
	desired, err := os.ReadFile(rootedRuntimePath(e.Root, request.ConfigPath))
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	desiredDigest, err := kubeadmplan.CanonicalKubeletConfigurationSHA256(desired)
	if err != nil || desiredDigest != request.DesiredConfigSHA256 {
		return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("selected kubelet configuration changed after operation acceptance"))
	}
	localBefore, err := os.ReadFile(rootedRuntimePath(e.Root, "/var/lib/kubelet/config.yaml"))
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	localMatches := kubeadmplan.KubeletConfigurationContains(localBefore, desired) == nil
	uploadNeeded := request.CoordinatorUpload
	if request.CoordinatorUpload {
		liveResult := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}"}, nil)
		if liveResult.Err != nil || liveResult.ExitStatus != 0 {
			return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("collect live kubelet config: %s", toolFailure(liveResult)))
		}
		liveDigest, err := kubeadmplan.CanonicalKubeletConfigurationSHA256(liveResult.Stdout)
		if err != nil {
			return e.failControlPlaneConfig(record, "preflight", fmt.Errorf("identify live kubelet config: %w", err))
		}
		uploadNeeded = liveDigest != desiredDigest
	}
	before := sha256Bytes(localBefore)
	if !uploadNeeded && localMatches {
		completedAt := e.clock()
		if _, err := e.Store.Update(record.OperationID, "no-change", operation.HostBookkeepingCompletionPhase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.KubeadmControlPlaneConfig.BeforeKubeletConfigSHA256 = before
			current.KubeadmControlPlaneConfig.AfterKubeletConfigSHA256 = before
			current.Phase = operation.HostBookkeepingCompletionPhase
			current.CompletedPhases = appendMissing(current.CompletedPhases, "preflight-complete", operation.HostBookkeepingCompletionPhase)
			current.CompletedAt = &completedAt
			current.Terminal = true
			current.Result = operation.ResultSucceeded
			current.NextAction = "desired kubelet configuration already matches live state"
			current.UpdatedAt = completedAt
			return current, nil
		}); err != nil {
			return err
		}
		return nil
	}
	if uploadNeeded {
		argv := []string{"/usr/bin/kubeadm", "config", "validate", "--config", request.ConfigPath}
		if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-kubelet-config-validate", argv, false); err != nil {
			return err
		}
	}
	if !localMatches {
		if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-kubelet-config-dry-run", []string{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--dry-run"}, false); err != nil {
			return err
		}
	}
	if _, err := e.Store.Update(record.OperationID, "preflight-complete", "preflight-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "preflight-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if !localMatches {
		before, err = e.backupKubeletConfig(record.OperationID)
		if err != nil {
			return e.failControlPlaneConfig(record, "kubelet-config-backup", err)
		}
	}
	if _, err := e.Store.Update(record.OperationID, "kubelet-config-backup-complete", "kubelet-config-backup-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmControlPlaneConfig.BeforeKubeletConfigSHA256 = before
		current.Phase = "kubelet-config-backup-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if uploadNeeded {
		argv := []string{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubelet", "--config", request.ConfigPath}
		if err := e.runControlPlaneConfigCommand(ctx, record, "kubelet-config-upload-running", argv, true); err != nil {
			return err
		}
		if _, err := e.Store.Update(record.OperationID, "kubelet-config-upload-complete", "kubelet-config-upload-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.KubeadmControlPlaneConfig.ConfigUploadRan = true
			current.Phase = "kubelet-config-upload-complete"
			current.UpdatedAt = e.clock()
			return current, nil
		}); err != nil {
			return err
		}
	}
	afterData := localBefore
	if !localMatches {
		if err := e.runControlPlaneConfigCommand(ctx, record, "kubelet-config-running", []string{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config"}, true); err != nil {
			return err
		}
		afterData, err = os.ReadFile(rootedRuntimePath(e.Root, "/var/lib/kubelet/config.yaml"))
		if err != nil {
			return e.failControlPlaneConfig(record, "kubelet-config-verify", err)
		}
		if err := kubeadmplan.KubeletConfigurationContains(afterData, desired); err != nil {
			return e.failControlPlaneConfig(record, "kubelet-config-verify", err)
		}
	}
	after := sha256Bytes(afterData)
	if _, err := e.Store.Update(record.OperationID, "kubelet-config-complete", "kubelet-config-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmControlPlaneConfig.AfterKubeletConfigSHA256 = after
		current.Phase = "kubelet-config-complete"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if !localMatches {
		if err := e.runControlPlaneConfigCommand(ctx, record, "kubelet-restart-running", []string{"/usr/bin/systemctl", "restart", "kubelet.service"}, true); err != nil {
			return err
		}
	}
	if result := e.runControlPlaneConfigHealth(ctx, request.NodeName); result.Err != nil || result.ExitStatus != 0 {
		return e.failControlPlaneConfig(record, "post-kubelet-health", fmt.Errorf("post-kubelet health failed: %s", toolFailure(result)))
	}
	if _, err := e.Store.Update(record.OperationID, "post-kubelet-health-complete", "post-kubelet-health-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "post-kubelet-health-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "post-kubelet-health-complete")
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if _, err := e.Store.Update(record.OperationID, "record-operation-complete", "record-operation-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = operation.HostBookkeepingCompletionPhase
		current.CompletedPhases = appendMissing(current.CompletedPhases, operation.HostBookkeepingCompletionPhase)
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	return e.finalizeSuccessfulOperation(ctx, record.OperationID)
}

func (e *Executor) backupKubeletConfig(operationID string) (string, error) {
	data, err := os.ReadFile(rootedRuntimePath(e.Root, "/var/lib/kubelet/config.yaml"))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(e.Store.Root, operationID, "kubelet-config-backup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o600); err != nil {
		return "", err
	}
	return sha256Bytes(data), nil
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e *Executor) runControlPlaneConfigHealth(ctx context.Context, nodeName string) ToolResult {
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	return e.postHealthRunner()(healthCtx, []string{OperationKindKubeadmControlPlaneConfig, nodeName}, nil)
}

var controlPlaneManifestNames = []string{"kube-apiserver.yaml", "kube-controller-manager.yaml", "kube-scheduler.yaml"}

func (e *Executor) backupControlPlaneManifests(operationID string) (map[string]string, error) {
	digests := map[string]string{}
	dir := filepath.Join(e.Store.Root, operationID, "manifest-backup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, name := range controlPlaneManifestNames {
		data, err := os.ReadFile(rootedRuntimePath(e.Root, "/etc/kubernetes/manifests/"+name))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		digests[name] = hex.EncodeToString(sum[:])
	}
	return digests, nil
}

func (e *Executor) digestControlPlaneManifests() (map[string]string, error) {
	digests := map[string]string{}
	for _, name := range controlPlaneManifestNames {
		data, err := os.ReadFile(rootedRuntimePath(e.Root, "/etc/kubernetes/manifests/"+name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		digests[name] = hex.EncodeToString(sum[:])
	}
	return digests, nil
}

func (e *Executor) runControlPlaneConfigCommand(ctx context.Context, record operation.OperationRecord, phase string, argv []string, mutating bool) error {
	return e.runKubeadmUpgradeCommand(ctx, record, phase, argv, mutating)
}

func (e *Executor) failControlPlaneConfig(record operation.OperationRecord, phase string, cause error) error {
	now := e.clock()
	latest, _ := e.Store.Read(record.OperationID)
	mutated := latest.ExternalMutationStarted
	_, updateErr := e.Store.Update(record.OperationID, "control-plane-config-failed", "control-plane-config-failed", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = phase
		current.Terminal = true
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.FailureReason = inventory.Redact(cause.Error())
		current.Result = "failed"
		current.PostMutationRollbackAllowed = false
		current.HostRollback = ""
		if mutated {
			current.RecoveryRequired = true
			current.Result = operation.ResultFailedNeedsRepair
			current.NextAction = "stop rollout; inspect manifest backups and kubeadm diagnostics, then submit an explicit repair or reverse operation"
		} else {
			current.NextAction = "fix the refusal and submit a new rollout"
		}
		return current, nil
	})
	return errors.Join(cause, updateErr)
}
