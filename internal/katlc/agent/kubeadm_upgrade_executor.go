package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
)

const kubeadmUpgradeTimeout = 30 * time.Minute

func (e *Executor) executeKubeadmUpgrade(ctx context.Context, record operation.OperationRecord) error {
	request := record.KubernetesSysextUpdate
	if request == nil || request.UpgradeRole == "" {
		return fmt.Errorf("executable kubeadm upgrade request is required")
	}
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	currentSpec, _, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	currentRef, ok := kubernetesRef(currentSpec.Sysexts)
	if !ok {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("current generation has no Kubernetes sysext"), false)
	}
	if currentRef.PayloadVersion != request.SourcePayloadVersion {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("current Kubernetes payload %q does not match requested source %q", currentRef.PayloadVersion, request.SourcePayloadVersion), false)
	}
	if err := verifyFileDigest(rootedRuntimePath(e.Root, request.TargetSysextPath), request.TargetSysextSHA256, request.TargetSysextSize); err != nil {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("verify target Kubernetes sysext: %w", err), false)
	}
	if request.UpgradeRole != "worker" {
		if err := verifyFileDigest(rootedRuntimePath(e.Root, request.SnapshotStorageLocation), request.SnapshotDigest, 0); err != nil {
			return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("verify referenced etcd snapshot: %w", err), false)
		}
	}

	candidateRef, err := e.stageKubernetesCandidate(currentSpec, currentRef, *request, record.OperationID)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	toolRoot := rootedRuntimePath(e.Root, filepath.ToSlash(filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/kubernetes")))
	if err := os.MkdirAll(toolRoot, 0o700); err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	if result := e.toolRunner()(ctx, []string{"systemd-dissect", "--mount", "--read-only", rootedRuntimePath(e.Root, candidateRef.Path), toolRoot}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("mount operation-private target sysext: %s", toolFailure(result)), false)
	}
	retainToolView := false
	defer func() {
		if !retainToolView {
			_ = e.toolRunner()(context.Background(), []string{"systemd-dissect", "--umount", toolRoot}, nil)
		}
	}()
	targetKubeadm := filepath.Join(toolRoot, "usr/bin/kubeadm")
	versionResult := e.toolRunner()(ctx, []string{targetKubeadm, "version", "-o", "short"}, nil)
	if versionResult.Err != nil || versionResult.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("inspect target kubeadm: %s", toolFailure(versionResult)), false)
	}
	observedVersion := strings.TrimSpace(string(versionResult.Stdout))
	if observedVersion != request.TargetPayloadVersion {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("target kubeadm reported %q, want %q", observedVersion, request.TargetPayloadVersion), false)
	}
	gatePath := filepath.ToSlash(filepath.Join("/run/katl/operation-gates", record.OperationID, "target-kubelet-released"))
	gateUnit := "kubelet.service.d/20-katl-upgrade-gate.conf"
	if err := e.installKubeletGate(gatePath, gateUnit); err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	if result := e.toolRunner()(ctx, []string{"systemctl", "daemon-reload"}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("load target kubelet activation gate: %s", toolFailure(result)), false)
	}
	record, err = e.Store.Update(record.OperationID, "kubeadm-upgrade-staged", "staged", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "staged"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "accepted", "staged")
		current.PhaseIndex = len(current.CompletedPhases)
		current.PreviousGenerationID = currentID
		current.CandidateGenerationID = request.CandidateGenerationID
		current.KubeadmUpgradeEvidence.TargetKubeadmArtifactPath = filepath.ToSlash(filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/kubernetes"))
		current.KubeadmUpgradeEvidence.TargetKubeadmArtifactDigest = request.TargetSysextSHA256
		current.KubeadmUpgradeEvidence.TargetKubeadmObservedVersion = observedVersion
		current.KubeadmUpgradeEvidence.KubeletGateTokenPath = gatePath
		current.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit = gateUnit
		current.UpdatedAt = e.clock()
		current.NextAction = "run target kubeadm while source kubelet remains active"
		return current, nil
	})
	if err != nil {
		return err
	}

	if request.UpgradeRole == "apply" {
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-plan-running", []string{targetKubeadm, "upgrade", "plan", request.TargetPayloadVersion}, false); err != nil {
			return err
		}
		if _, err := e.Store.Update(record.OperationID, "kubeadm-plan-complete", "kubeadm-plan-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = "kubeadm-plan-complete"
			current.CompletedPhases = appendMissing(current.CompletedPhases, "kubeadm-plan-running", "kubeadm-plan-complete")
			current.PhaseIndex = len(current.CompletedPhases)
			current.UpdatedAt = e.clock()
			return current, nil
		}); err != nil {
			return err
		}
		retainToolView = true
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-apply-running", []string{targetKubeadm, "upgrade", "apply", "--yes", request.TargetPayloadVersion}, true); err != nil {
			return err
		}
	} else {
		retainToolView = true
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-node-running", []string{targetKubeadm, "upgrade", "node"}, true); err != nil {
			return err
		}
	}

	if _, err := e.Store.Update(record.OperationID, "release-target-kubelet", "kubelet-restart-running", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "kubelet-restart-running"
		current.KubeadmUpgradeEvidence.KubeletGateState = "released"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(rootedRuntimePath(e.Root, gatePath)), 0o700); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", err, true)
	}
	if err := os.WriteFile(rootedRuntimePath(e.Root, gatePath), []byte(record.OperationID+"\n"), 0o600); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", err, true)
	}
	if err := e.activateKubernetesCandidate(ctx, currentRef, candidateRef); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", err, true)
	}
	if result := e.toolRunner()(ctx, []string{"systemctl", "restart", "kubelet.service"}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", fmt.Errorf("restart target kubelet: %s", toolFailure(result)), true)
	}
	if err := e.checkKubeadmUpgradeHealth(ctx, *request); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	if err := e.completeKubeadmUpgrade(ctx, record); err != nil {
		return err
	}
	retainToolView = false
	return nil
}

func (e *Executor) runKubeadmUpgradeCommand(ctx context.Context, record operation.OperationRecord, phase string, argv []string, mutating bool) error {
	started := e.clock()
	invocationID := strings.TrimSuffix(phase, "-running")
	if mutating {
		scopes := []string{"kubeadm-state", "kubernetes-api"}
		if record.KubernetesSysextUpdate != nil && record.KubernetesSysextUpdate.UpgradeRole != "worker" {
			scopes = append(scopes, "stacked-etcd")
		}
		marker := operation.PreExecMutationMarker{MarkerID: invocationID, InvocationID: invocationID, Phase: phase, Tool: "kubeadm", ArgvDigest: digestArgv(argv), ExpectedMutationScopes: scopes, MarkedAt: started}
		if _, err := e.Store.Update(record.OperationID, invocationID+"-start", "pre-exec-mutation", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = phase
			current.ExternalMutationStarted = true
			current.PreExecMutationMarkers = append(current.PreExecMutationMarkers, marker)
			current.MutationScopes = appendMissing(current.MutationScopes, marker.ExpectedMutationScopes...)
			current.Invocations = append(current.Invocations, operation.InvocationRecord{InvocationID: invocationID, AgentStartID: e.AgentStartID, ExecutorAttemptID: e.AgentStartID, ChildProcess: redactArgv(argv), BootID: currentBootID(), StartedAt: started, Result: "started"})
			current.UpdatedAt = started
			return current, nil
		}); err != nil {
			return err
		}
	} else if _, err := e.Store.Update(record.OperationID, invocationID+"-start", phase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = phase
		current.Invocations = append(current.Invocations, operation.InvocationRecord{InvocationID: invocationID, AgentStartID: e.AgentStartID, ExecutorAttemptID: e.AgentStartID, ChildProcess: redactArgv(argv), BootID: currentBootID(), StartedAt: started, Result: "started"})
		current.UpdatedAt = started
		return current, nil
	}); err != nil {
		return err
	}
	toolCtx, cancel := context.WithTimeout(ctx, kubeadmUpgradeTimeout)
	defer cancel()
	result := e.toolRunner()(toolCtx, argv, nil)
	completed := e.clock()
	var artifactErr error
	if len(result.Stdout) > 0 {
		_, artifactErr = e.Store.AddDiagnosticArtifact(record.OperationID, invocationID+"-stdout", []byte(inventory.Redact(string(result.Stdout))), completed)
	}
	if len(result.Stderr) > 0 {
		_, err := e.Store.AddDiagnosticArtifact(record.OperationID, invocationID+"-stderr", []byte(inventory.Redact(string(result.Stderr))), completed)
		artifactErr = errors.Join(artifactErr, err)
	}
	if result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, phase, errors.Join(fmt.Errorf("target kubeadm failed: %s", toolFailure(result)), artifactErr), mutating)
	}
	if artifactErr != nil {
		return e.failKubeadmUpgrade(record, phase, fmt.Errorf("record redacted kubeadm diagnostics: %w", artifactErr), mutating)
	}
	_, err := e.Store.Update(record.OperationID, invocationID+"-complete", phase+"-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		completeInvocation(current.Invocations, invocationID, completed, operation.ResultSucceeded, result)
		if mutating {
			current.MutatingToolRan = true
			current.MutatingToolInvocations = appendMissing(current.MutatingToolInvocations, inventory.Redact(strings.Join(argv, " ")))
		}
		current.CompletedPhases = appendMissing(current.CompletedPhases, phase)
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = completed
		return current, nil
	})
	return err
}

func (e *Executor) stageKubernetesCandidate(previous generation.GenerationSpec, current generation.ExtensionRef, request operation.KubernetesSysextUpdate, operationID string) (generation.ExtensionRef, error) {
	dir, err := generation.GenerationDir(e.Root, request.CandidateGenerationID)
	if err != nil {
		return generation.ExtensionRef{}, err
	}
	targetLogical := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, request.CandidateGenerationID, "sysext", "kubernetes.raw"))
	targetHost := rootedRuntimePath(e.Root, targetLogical)
	if err := os.MkdirAll(filepath.Dir(targetHost), 0o700); err != nil {
		return generation.ExtensionRef{}, err
	}
	if err := copyVerifiedFile(rootedRuntimePath(e.Root, request.TargetSysextPath), targetHost, request.TargetSysextSHA256); err != nil {
		return generation.ExtensionRef{}, err
	}
	ref := current
	ref.Path = targetLogical
	ref.SHA256 = request.TargetSysextSHA256
	ref.PayloadVersion = request.TargetPayloadVersion
	ref.ArtifactVersion = request.TargetPayloadVersion
	spec := previous
	spec.GenerationID = request.CandidateGenerationID
	spec.PreviousGenerationID = previous.GenerationID
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + request.CandidateGenerationID + ".conf"
	spec.Sysexts = replaceKubernetesRef(spec.Sysexts, ref)
	spec.CreatedAt = e.clock()
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, e.clock())
	if err != nil {
		return generation.ExtensionRef{}, err
	}
	if err := generation.WriteGeneration(e.Root, spec, status); err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("write candidate generation %s (%s): %w", request.CandidateGenerationID, dir, err)
	}
	return ref, nil
}

func (e *Executor) installKubeletGate(gatePath, unit string) error {
	path := rootedRuntimePath(e.Root, "/etc/systemd/system/"+unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := "[Unit]\nConditionPathExists=" + gatePath + "\n"
	return os.WriteFile(path, []byte(content), 0o644)
}

func (e *Executor) activateKubernetesCandidate(ctx context.Context, current, candidate generation.ExtensionRef) error {
	activation := rootedRuntimePath(e.Root, candidate.ActivationPath)
	if err := os.MkdirAll(filepath.Dir(activation), 0o755); err != nil {
		return err
	}
	_ = os.Remove(activation)
	if err := os.Symlink(candidate.Path, activation); err != nil {
		return err
	}
	for _, argv := range [][]string{{"systemd-sysext", "refresh"}, {"systemctl", "daemon-reload"}} {
		result := e.toolRunner()(ctx, argv, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			_ = os.Remove(activation)
			_ = os.Symlink(current.Path, activation)
			_ = e.toolRunner()(ctx, []string{"systemd-sysext", "refresh"}, nil)
			return fmt.Errorf("%s: %s", argv[0], toolFailure(result))
		}
	}
	return nil
}

func (e *Executor) checkKubeadmUpgradeHealth(ctx context.Context, request operation.KubernetesSysextUpdate) error {
	commands := [][]string{{"systemctl", "is-active", "--quiet", "containerd.service"}, {"systemctl", "is-active", "--quiet", "kubelet.service"}, {"kubelet", "--version"}}
	if request.UpgradeRole != "worker" {
		commands = append(commands, []string{"kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"})
	}
	for _, argv := range commands {
		result := e.toolRunner()(ctx, argv, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			return fmt.Errorf("health command %s: %s", argv[0], toolFailure(result))
		}
		if argv[0] == "kubelet" && !strings.Contains(string(result.Stdout), strings.TrimPrefix(request.TargetPayloadVersion, "v")) {
			return fmt.Errorf("target kubelet version not observed: %q", strings.TrimSpace(string(result.Stdout)))
		}
	}
	return nil
}

func (e *Executor) completeKubeadmUpgrade(ctx context.Context, record operation.OperationRecord) error {
	now := e.clock()
	if err := e.removeKubeletGate(ctx, record); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	if err := e.commitCandidateGeneration(ctx, record, now, "Kubernetes upgrade passed local health checks"); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	_, err := e.Store.Update(record.OperationID, "kubeadm-upgrade-healthy", "healthy", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "healthy"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "kubelet-restart-running", "health-check-running", "healthy")
		current.PhaseIndex = len(current.CompletedPhases)
		current.KubeadmUpgradeEvidence.KubeletGateState = "target-observed"
		current.ActivationState = operation.ActivationStateActiveLive
		current.GenerationCommitState = operation.GenerationCommitCommitted
		current.PostKubeadmHealthState = operation.PostKubeadmHealthPassed
		current.BootHealthPending = true
		current.Terminal = true
		current.Result = operation.ResultSucceeded
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.NextAction = "reboot into the committed candidate for boot health validation before continuing the serialized rollout"
		return current, nil
	})
	return err
}

func (e *Executor) removeKubeletGate(ctx context.Context, record operation.OperationRecord) error {
	if record.KubeadmUpgradeEvidence == nil || strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit) == "" {
		return fmt.Errorf("kubelet activation gate enforcement unit is not recorded")
	}
	path := rootedRuntimePath(e.Root, "/etc/systemd/system/"+record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result := e.toolRunner()(ctx, []string{"systemctl", "daemon-reload"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("remove completed kubelet activation gate: %s", toolFailure(result))
	}
	return nil
}

func (e *Executor) failKubeadmUpgrade(record operation.OperationRecord, phase string, cause error, postMutation bool) error {
	now := e.clock()
	latest, readErr := e.Store.Read(record.OperationID)
	mutated := postMutation || (readErr == nil && latest.ExternalMutationStarted)
	cleanupErr := e.restoreSourceKubernetesAfterFailure(latest)
	cause = errors.Join(cause, cleanupErr)
	var abandonErr error
	if !mutated {
		abandonErr = e.abandonKubeadmCandidate(record.CandidateGenerationID, record.OperationID, now)
	}
	_, err := e.Store.Update(record.OperationID, "kubeadm-upgrade-failed-"+strings.ReplaceAll(phase, "-", "_"), "kubeadm-upgrade-failed", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = phase
		current.Terminal = true
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.FailureReason = inventory.Redact(cause.Error())
		current.Result = "failed"
		current.GenerationCommitState = operation.GenerationCommitAbandoned
		current.NextAction = "fix the pre-mutation failure and submit a new explicit upgrade operation"
		if mutated {
			current.RecoveryRequired = true
			current.Result = operation.ResultFailedNeedsRepair
			current.GenerationCommitState = operation.GenerationCommitCandidate
			current.ActivationState = operation.ActivationStateFailed
			current.NextAction = "inspect kubeadm diagnostics and use explicit kubeadm-aware repair; host rollback does not repair Kubernetes or etcd state"
			current.PostMutationRollbackAllowed = true
			current.HostRollback = current.PreviousGenerationID
		}
		return current, nil
	})
	return errors.Join(cause, readErr, abandonErr, err)
}

func (e *Executor) restoreSourceKubernetesAfterFailure(record operation.OperationRecord) error {
	var restoreErr error
	previous := strings.TrimSpace(record.PreviousGenerationID)
	if previous != "" {
		spec, _, err := generation.ReadGeneration(e.Root, previous)
		if err == nil {
			if ref, ok := kubernetesRef(spec.Sysexts); ok {
				activation := rootedRuntimePath(e.Root, ref.ActivationPath)
				_ = os.Remove(activation)
				if err := os.MkdirAll(filepath.Dir(activation), 0o755); err != nil {
					restoreErr = errors.Join(restoreErr, err)
				} else if err := os.Symlink(ref.Path, activation); err != nil {
					restoreErr = errors.Join(restoreErr, err)
				}
				result := e.toolRunner()(context.Background(), []string{"systemd-sysext", "refresh"}, nil)
				if result.Err != nil || result.ExitStatus != 0 {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("restore source Kubernetes sysext: %s", toolFailure(result)))
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	unit := "kubelet.service.d/20-katl-upgrade-gate.conf"
	if record.KubeadmUpgradeEvidence != nil && strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit) != "" {
		unit = record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit
	}
	gatePath := rootedRuntimePath(e.Root, "/etc/systemd/system/"+unit)
	removed := false
	if err := os.Remove(gatePath); err == nil {
		removed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		restoreErr = errors.Join(restoreErr, err)
	}
	if removed {
		result := e.toolRunner()(context.Background(), []string{"systemctl", "daemon-reload"}, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("remove failed kubelet activation gate: %s", toolFailure(result)))
		}
	}
	if record.KubeadmUpgradeEvidence != nil && strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateTokenPath) != "" {
		_ = os.Remove(rootedRuntimePath(e.Root, record.KubeadmUpgradeEvidence.KubeletGateTokenPath))
	}
	return restoreErr
}

func (e *Executor) abandonKubeadmCandidate(candidate, operationID string, now time.Time) error {
	if strings.TrimSpace(candidate) == "" {
		return nil
	}
	spec, status, err := generation.ReadGeneration(e.Root, candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if status.CommitState != generation.CommitStateCandidate {
		return nil
	}
	status.CommitState = generation.CommitStateAbandoned
	status.BootState = generation.BootStateFailed
	status.UpdatedAt = now
	status.StatusTransitions = append(status.StatusTransitions, generation.StatusTransition{At: now, OperationID: operationID, Reason: "Kubernetes upgrade failed before kubeadm mutation", CommitState: status.CommitState, BootState: status.BootState, HealthState: status.HealthState})
	return generation.WriteGenerationStatus(e.Root, spec, status)
}

func kubernetesRef(refs []generation.ExtensionRef) (generation.ExtensionRef, bool) {
	for _, ref := range refs {
		if ref.Name == "kubernetes" {
			return ref, true
		}
	}
	return generation.ExtensionRef{}, false
}
func replaceKubernetesRef(refs []generation.ExtensionRef, next generation.ExtensionRef) []generation.ExtensionRef {
	out := append([]generation.ExtensionRef(nil), refs...)
	for i := range out {
		if out[i].Name == "kubernetes" {
			out[i] = next
			return out
		}
	}
	return append(out, next)
}
func rootedRuntimePath(root, path string) string {
	return filepath.Join(runtimeRoot(root), strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
}
func verifyFileDigest(path, want string, size uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	if size > 0 && uint64(n) != size {
		return fmt.Errorf("size %d, want %d", n, size)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 %s, want %s", got, want)
	}
	return nil
}
func copyVerifiedFile(source, target, want string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return errors.Join(copyErr, closeErr)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		_ = os.Remove(target)
		return fmt.Errorf("copied sysext sha256 %s, want %s", got, want)
	}
	return nil
}
