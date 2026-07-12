package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

var supportedControlPlaneConfigFields = map[string]bool{
	"ClusterConfiguration.apiServer.extraArgs.profiling=false":         true,
	"ClusterConfiguration.controllerManager.extraArgs.profiling=false": true,
	"ClusterConfiguration.scheduler.extraArgs.profiling=false":         true,
}

func validateKubeadmControlPlaneConfigRequest(kind string, req *agentapi.KubeadmControlPlaneConfigOperationRequest) error {
	if kind != OperationKindKubeadmControlPlaneConfig {
		return fmt.Errorf("operation kind must be %q", OperationKindKubeadmControlPlaneConfig)
	}
	if strings.TrimSpace(req.GetRolloutId()) == "" {
		return fmt.Errorf("rolloutID is required")
	}
	if req.GetNodeCount() != 3 || req.GetNodePosition() < 1 || req.GetNodePosition() > 3 {
		return fmt.Errorf("node position must identify one of exactly three control-plane nodes")
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
	for _, field := range req.GetSupportedFieldDelta() {
		if !supportedControlPlaneConfigFields[field] || seen[field] {
			return fmt.Errorf("unsupported or repeated field delta %q", field)
		}
		seen[field] = true
	}
	return nil
}

func controlPlaneConfigFromProto(req *agentapi.KubeadmControlPlaneConfigOperationRequest) operation.KubeadmControlPlaneConfig {
	return operation.KubeadmControlPlaneConfig{
		RolloutID: req.GetRolloutId(), NodePosition: req.GetNodePosition(), NodeCount: req.GetNodeCount(), CoordinatorNode: req.GetCoordinatorNode(), NodeName: req.GetNodeName(), CoordinatorUpload: req.GetCoordinatorUpload(), DesiredGenerationID: req.GetDesiredGenerationId(), ConfigName: req.GetConfigName(), ConfigPath: "/etc/katl/kubeadm/" + req.GetConfigName() + "/config.yaml", DesiredConfigSHA256: req.GetDesiredConfigSha256(), ExpectedLiveConfigSHA256: req.GetExpectedLiveConfigSha256(), KubernetesPayloadVersion: req.GetKubernetesPayloadVersion(), KubernetesPayloadSHA256: req.GetKubernetesPayloadSha256(), SupportedFieldDelta: append([]string(nil), req.GetSupportedFieldDelta()...), SnapshotRef: req.GetSnapshotRef(), SnapshotDigest: req.GetSnapshotDigest(), SnapshotRevision: req.GetSnapshotRevision(), CapturedMemberListDigest: req.GetCapturedMemberListDigest(), SourceEtcdVersion: req.GetSourceEtcdVersion(), SnapshotCreatedAt: req.GetSnapshotCreatedAt(), SnapshotStorageLocation: req.GetSnapshotStorageLocation(), SnapshotOperatorIdentity: req.GetSnapshotOperatorIdentity(),
	}
}

func (s *Server) acceptKubeadmControlPlaneConfigOperation(req *agentapi.SubmitOperationRequest, digest, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	body, err := s.validateKubeadmControlPlaneConfigState(req)
	if err != nil {
		return operation.OperationRecord{}, nil, err
	}
	phases := []string{"accepted", "preflight-complete", "cordon-complete", "manifest-backup-complete", "control-plane-manifests-running", "control-plane-manifests-complete", "post-manifest-health-complete"}
	if body.CoordinatorUpload {
		phases = append(phases, "kubeadm-config-upload-running", "kubeadm-config-upload-complete", "post-upload-health-complete")
	}
	phases = append(phases, "uncordon-complete", operation.HostBookkeepingCompletionPhase)
	record := operation.OperationRecord{
		OperationID: id, OperationKind: req.OperationKind, Scope: "kubeadm-state", ClientRequestID: req.ClientRequestId, Actor: req.Actor, ExpectedMachineID: req.ExpectedMachineId, ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId, ExpectedClusterIntentDigest: req.ExpectedClusterIntentDigest, RequestDigest: digest, Phase: "accepted", PhasePlan: phases, PreviousGenerationID: body.DesiredGenerationID, KubeadmControlPlaneConfig: &body, ActivationMode: operation.ActivationModeLive, ActivationState: operation.ActivationStatePending, GenerationCommitState: operation.GenerationCommitCommitted, ResourceLocks: locks, NextAction: "run bounded kubeadm control-plane configuration phases",
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
	desiredDigest, err := kubeadmplan.CanonicalClusterConfigurationSHA256(data)
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
	applyStatusPath, err := generation.ConfigApplyStatusPath(s.Root, body.DesiredGenerationID)
	if err != nil {
		return body, err
	}
	applyStatus, err := generation.ReadConfigApplyStatus(applyStatusPath)
	if err != nil || applyStatus.Kubeadm.SelectedConfigName != body.ConfigName || !applyStatus.Kubeadm.Required {
		return body, fmt.Errorf("active generation does not select kubeadm config %q as action-required", body.ConfigName)
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
	delta, err := kubeadmplan.SupportedControlPlaneProfilingDelta(desired, liveResult.Stdout)
	if err != nil {
		return e.failControlPlaneConfig(record, "preflight", err)
	}
	requestedDelta := append([]string(nil), request.SupportedFieldDelta...)
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
	if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-dry-run", []string{"/usr/bin/kubeadm", "init", "phase", "control-plane", "all", "--config", request.ConfigPath, "--dry-run"}, false); err != nil {
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
	argv := []string{"/usr/bin/kubeadm", "init", "phase", "control-plane", "all", "--config", request.ConfigPath}
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
		upload := []string{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubeadm", "--config", request.ConfigPath}
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
