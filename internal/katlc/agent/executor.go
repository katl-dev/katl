package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/bootstrapruntime"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
)

const (
	defaultToolTimeout       = 25 * time.Minute
	maxToolTimeout           = 25 * time.Minute
	readinessTimeout         = 2 * time.Minute
	postKubeadmHealthTimeout = 2 * time.Minute
	bootRootMountTimeout     = 10 * time.Second
)

type ToolResult struct {
	Stdout     []byte
	Stderr     []byte
	ExitStatus int
	PID        int
	Err        error
}

type ToolRunner func(context.Context, []string, func(int)) ToolResult

type BootRootMounter func(context.Context, string) error
type BootEntrySetter func(context.Context, string, string) error
type HostUpgradeResolver func(context.Context, operation.HostUpgrade) (katlosimage.Payload, error)

type Executor struct {
	Root                 string
	Store                operation.Store
	AgentStartID         string
	Now                  func() time.Time
	RunTool              ToolRunner
	RunReadiness         ToolRunner
	RunPostHealth        ToolRunner
	RunEndpointLifecycle ToolRunner
	MountBootRoot        BootRootMounter
	SetBootOneshot       BootEntrySetter
	SetBootDefault       BootEntrySetter
	ConfigApplyRunner    configapply.CommandRunner
	ConfigApplyActivator configapply.ConfextActivator
	BundleClient         *http.Client
	ResolveHostUpgrade   HostUpgradeResolver
	Async                bool
	workerMu             sync.Mutex
	workerWG             sync.WaitGroup
	workerCtx            context.Context
	workerCancel         context.CancelFunc
	stopped              bool
}

type toolPlan = operation.ExecutorPlan

func NewExecutor(root string, store operation.Store, agentStartID string) *Executor {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	executor := &Executor{
		Root:                 strings.TrimSpace(root),
		Store:                store,
		AgentStartID:         strings.TrimSpace(agentStartID),
		Now:                  func() time.Time { return time.Now().UTC() },
		RunTool:              runChildProcess,
		RunReadiness:         runReadinessCommand,
		RunPostHealth:        runPostKubeadmHealthCommand,
		RunEndpointLifecycle: runChildProcess,
		MountBootRoot:        mountRuntimeBootRoot,
		BundleClient:         http.DefaultClient,
		Async:                true,
		workerCtx:            workerCtx,
		workerCancel:         workerCancel,
	}
	if runtimeRoot(executor.Root) == "/" {
		executor.SetBootOneshot = setBootOneshot
		executor.SetBootDefault = setBootDefault
	}
	return executor
}

func AuditStartup(store operation.Store, now time.Time) (operation.ReconcileReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report, err := store.ReconcileBoot(now, currentBootID(), liveAgentInvocation)
	if err != nil {
		return operation.ReconcileReport{}, err
	}
	if err := failAcceptedButNotStarted(store, now); err != nil {
		return operation.ReconcileReport{}, err
	}
	return report, nil
}

func (e *Executor) Dispatch(ctx context.Context, record operation.OperationRecord) error {
	e.workerMu.Lock()
	if e.stopped {
		e.workerMu.Unlock()
		return fmt.Errorf("agent executor is shutting down")
	}
	if e.Async {
		workerCtx := e.workerCtx
		e.workerWG.Add(1)
		e.workerMu.Unlock()
		go func() {
			defer e.workerWG.Done()
			if err := e.Execute(workerCtx, record); err != nil {
				e.recordUnhandledExecutionFailure(record.OperationID, err)
			}
		}()
		return nil
	}
	e.workerMu.Unlock()
	_ = e.Execute(ctx, record)
	return nil
}

func (e *Executor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.workerMu.Lock()
	if !e.stopped {
		e.stopped = true
		if e.workerCancel != nil {
			e.workerCancel()
		}
	}
	e.workerMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for agent executor shutdown: %w", ctx.Err())
	}
}

func (e *Executor) recordUnhandledExecutionFailure(operationID string, cause error) {
	current, err := e.Store.Read(operationID)
	if err != nil || current.Terminal {
		return
	}
	_, _ = e.failRecordPhase(operationID, "executor-unhandled-failure", "executor-unhandled-failure", "executor-unhandled-failure", "inspect the recorded executor failure, correct it, and submit a new operation", cause)
}

func (e *Executor) Execute(ctx context.Context, record operation.OperationRecord) error {
	if record.KubeadmControlPlaneConfig != nil {
		return e.executeKubeadmControlPlaneConfig(ctx, record)
	}
	if record.ConfigApplyRequest != nil {
		return e.executeConfigApply(ctx, record)
	}
	if record.DestructiveResetRequest != nil {
		return e.executeDestructiveReset(ctx, record)
	}
	if record.HostUpgradeRequest != nil {
		return e.executeHostUpgrade(ctx, record)
	}
	if record.KubernetesSysextUpdate != nil {
		return e.executeKubeadmUpgrade(ctx, record)
	}
	plan, err := executorPlan(record)
	if err != nil {
		_, markErr := e.failRecord(record.OperationID, "executor-plan-refused", "executor-plan-refused", "agent executor could not read operation tool plan", err)
		return errors.Join(err, markErr)
	}
	if err := validateToolPlan(plan); err != nil {
		_, markErr := e.failRecord(record.OperationID, "executor-plan-invalid", "executor-plan-invalid", "agent executor rejected operation tool plan", err)
		return errors.Join(err, markErr)
	}
	timeout := defaultToolTimeout
	if strings.TrimSpace(plan.Timeout) != "" {
		parsed, err := time.ParseDuration(plan.Timeout)
		if err != nil || parsed <= 0 {
			_, markErr := e.failRecord(record.OperationID, "executor-timeout-invalid", "executor-plan-invalid", "agent executor rejected operation timeout", fmt.Errorf("timeout must be a positive Go duration"))
			return errors.Join(err, markErr)
		}
		timeout = parsed
	}
	if timeout > maxToolTimeout {
		_, markErr := e.failRecord(record.OperationID, "executor-timeout-too-large", "executor-plan-invalid", "agent executor rejected operation timeout", fmt.Errorf("timeout must not exceed %s", maxToolTimeout))
		return markErr
	}
	prepared, err := e.prepareBootstrapRuntime(ctx, record)
	if err != nil {
		return err
	}
	record = prepared
	ready, err := e.gateBootstrapReadiness(ctx, record, plan)
	if err != nil {
		return err
	}
	record = ready
	if expired := expiredJoinMaterial(record, e.clock()); expired != "" {
		_, markErr := e.failRecordPhase(record.OperationID, "join-material-expired", "bootstrap-runtime-ready", "bootstrap-runtime-ready", "submit a new worker join operation with unexpired join material", fmt.Errorf("%s", expired))
		return markErr
	}
	endpointSuspended := false
	var managedRoute *managedJoinRoute
	if record.OperationKind == bootstrapplan.OperationKindJoinControlPlane {
		lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), 30*time.Second)
		endpointSuspended, managedRoute, err = suspendManagedEndpointForJoin(lifecycleCtx, e.Root, joinDiscoveryConfigPath(record), e.endpointLifecycleRunner())
		lifecycleCancel()
		if err != nil {
			_, markErr := e.failRecordPhase(record.OperationID, "managed-endpoint-suspend-failed", "managed-endpoint-lifecycle", "suspend-managed-endpoint", "repair the managed endpoint lifecycle before retrying the control-plane join", err)
			return errors.Join(err, markErr)
		}
		if endpointSuspended {
			updatedAt := e.clock()
			record, err = e.Store.Update(record.OperationID, "managed-endpoint-suspended", "managed-endpoint-lifecycle", func(record operation.OperationRecord) (operation.OperationRecord, error) {
				record.Phase = "suspend-managed-endpoint"
				record.CompletedPhases = appendMissing(record.CompletedPhases, "suspend-managed-endpoint")
				record.PhaseIndex = len(record.CompletedPhases)
				record.NextAction = "run control-plane join while the managed endpoint is off the local path"
				record.UpdatedAt = updatedAt
				return record, nil
			})
			if err != nil {
				return err
			}
		}
	}
	markerID := strings.TrimSpace(plan.MarkerID)
	if markerID == "" {
		markerID = generatedMarkerID(plan.Phase, plan.Argv)
	}
	attemptID, err := randomID("executor")
	if err != nil {
		return err
	}
	startedAt := e.clock()
	argvDigest := digestArgv(plan.Argv)
	marker := operation.PreExecMutationMarker{
		MarkerID:               markerID,
		InvocationID:           attemptID,
		Phase:                  plan.Phase,
		Tool:                   filepath.Base(plan.Argv[0]),
		ArgvDigest:             argvDigest,
		ExpectedMutationScopes: append([]string(nil), plan.MutationScopes...),
		MarkedAt:               startedAt,
	}
	if _, err := e.Store.Update(record.OperationID, markerID+"-start", "pre-exec-mutation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		record.PreExecMutationMarkers = append(record.PreExecMutationMarkers, marker)
		record.MutationScopes = appendMissing(record.MutationScopes, plan.MutationScopes...)
		record.Invocations = append(record.Invocations, operation.InvocationRecord{
			InvocationID:      markerID,
			AgentStartID:      e.AgentStartID,
			ExecutorAttemptID: attemptID,
			ChildProcess:      redactArgv(plan.Argv),
			BootID:            currentBootID(),
			StartedAt:         startedAt,
			Result:            "started",
		})
		record.Phase = plan.Phase
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return err
	}

	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var startUpdateErr error
	var startMu sync.Mutex
	result := e.toolRunner()(toolCtx, plan.Argv, func(pid int) {
		if pid <= 0 {
			return
		}
		_, err := e.Store.Update(record.OperationID, markerID+"-child-started", "child-process-started", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			for i := range record.Invocations {
				if record.Invocations[i].InvocationID == markerID {
					record.Invocations[i].PID = pid
					break
				}
			}
			record.UpdatedAt = e.clock()
			return record, nil
		})
		if err != nil {
			startMu.Lock()
			startUpdateErr = err
			startMu.Unlock()
		}
	})
	if managedRoute != nil {
		lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), 30*time.Second)
		routeErr := removeManagedJoinRoute(lifecycleCtx, managedRoute, e.endpointLifecycleRunner())
		lifecycleCancel()
		if routeErr != nil {
			result.Err = errors.Join(result.Err, routeErr)
			if result.ExitStatus == 0 {
				result.ExitStatus = -1
			}
		}
	}
	if errors.Is(toolCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
		result.Err = toolCtx.Err()
		result.ExitStatus = -1
	}
	completedAt := e.clock()
	cleanupTemporaryJoinConfig(e.Root, record)
	timedOut := errors.Is(toolCtx.Err(), context.DeadlineExceeded) || errors.Is(result.Err, context.DeadlineExceeded)
	var artifactErr error
	if len(result.Stdout) > 0 {
		if _, err := e.Store.AddDiagnosticArtifact(record.OperationID, markerID+"-stdout", []byte(inventory.Redact(string(result.Stdout))), completedAt); err != nil {
			artifactErr = errors.Join(artifactErr, err)
		}
	}
	if len(result.Stderr) > 0 {
		if _, err := e.Store.AddDiagnosticArtifact(record.OperationID, markerID+"-stderr", []byte(inventory.Redact(string(result.Stderr))), completedAt); err != nil {
			artifactErr = errors.Join(artifactErr, err)
		}
	}
	if result.Err != nil {
		if _, err := e.Store.AddDiagnosticArtifact(record.OperationID, markerID+"-error", []byte(inventory.Redact(toolFailure(result))), completedAt); err != nil {
			artifactErr = errors.Join(artifactErr, err)
		}
	}
	startMu.Lock()
	startErr := startUpdateErr
	startMu.Unlock()
	resultText := exitResult(result)
	if timedOut {
		resultText = operation.ResultTimedOut
	}
	if result.Err != nil || result.ExitStatus != 0 {
		if alreadyJoined(record, result) && e.joinPostHealthPassed(ctx, record) {
			result.Err = nil
			result.ExitStatus = 0
			resultText = operation.ResultSucceeded
		}
	}
	_, updateErr := e.Store.Update(record.OperationID, markerID+"-complete", "child-process-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		completeInvocation(record.Invocations, markerID, completedAt, resultText, result)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, inventory.Redact(strings.Join(plan.Argv, " ")))
		record.Phase = plan.Phase
		record.UpdatedAt = completedAt
		if timedOut {
			record.CompletedAt = &completedAt
			record.Terminal = true
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.Interruption = operation.ResultTimedOut
			record.NextAction = "explicit repair required after operation timeout"
			record.FailureReason = inventory.Redact(toolFailure(result))
			return record, nil
		}
		if result.Err == nil && result.ExitStatus == 0 {
			if startErr != nil || artifactErr != nil {
				record.CompletedAt = &completedAt
				record.Terminal = true
				record.RecoveryRequired = true
				record.Result = operation.ResultFailedNeedsRepair
				record.NextAction = "explicit repair required after executor bookkeeping failure"
				record.FailureReason = inventory.Redact(errors.Join(startErr, artifactErr).Error())
				return record, nil
			}
			record.CompletedPhases = appendMissing(record.CompletedPhases, plan.Phase)
			if len(record.CompletedPhases) > record.PhaseIndex {
				record.PhaseIndex = len(record.CompletedPhases)
			}
			record.NextAction = "run bounded post-kubeadm health checks"
			return record, nil
		}
		record.CompletedAt = &completedAt
		record.Terminal = true
		record.RecoveryRequired = true
		record.Result = operation.ResultFailedNeedsRepair
		record.NextAction = "explicit repair required after child process failure"
		record.FailureReason = inventory.Redact(toolFailure(result))
		return record, nil
	})
	if updateErr != nil {
		return updateErr
	}
	if result.Err != nil {
		return fmt.Errorf("run %s: %s", plan.Argv[0], inventory.Redact(result.Err.Error()))
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("run %s: exit status %d", plan.Argv[0], result.ExitStatus)
	}
	if endpointSuspended {
		lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := resumeManagedEndpointAfterJoin(lifecycleCtx, e.Root, e.endpointLifecycleRunner())
		lifecycleCancel()
		if err != nil {
			_, markErr := e.failRecordPhase(record.OperationID, "managed-endpoint-restore-failed", "managed-endpoint-lifecycle", "restore-managed-endpoint", "repair the managed endpoint interface after kubeadm joined the control plane", err)
			return errors.Join(err, markErr)
		}
		updatedAt := e.clock()
		if _, err := e.Store.Update(record.OperationID, "managed-endpoint-restored", "managed-endpoint-lifecycle", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.Phase = "restore-managed-endpoint"
			record.CompletedPhases = appendMissing(record.CompletedPhases, "restore-managed-endpoint")
			record.PhaseIndex = len(record.CompletedPhases)
			record.NextAction = "run bounded post-kubeadm health checks"
			record.UpdatedAt = updatedAt
			return record, nil
		}); err != nil {
			return err
		}
	}
	if err := e.finalizeSuccessfulOperation(ctx, record.OperationID); err != nil {
		return err
	}
	return nil
}

func (e *Executor) prepareBootstrapRuntime(ctx context.Context, record operation.OperationRecord) (operation.OperationRecord, error) {
	if ctx.Err() != nil {
		return operation.OperationRecord{}, ctx.Err()
	}
	if !requiresBootstrapRuntime(record) {
		return record, nil
	}
	plan, err := bootstrapplan.FromOperationWithBundleClient(e.Root, record, e.BundleClient)
	if err != nil {
		updated, markErr := e.failRecordPhase(record.OperationID, "bootstrap-runtime-plan-refused", "prepare-bootstrap-runtime", "prepare-bootstrap-runtime", "bootstrap runtime planning failed before kubeadm mutation", err)
		return updated, errors.Join(err, markErr)
	}
	startedAt := e.clock()
	if _, err := e.Store.Update(record.OperationID, "prepare-bootstrap-runtime-start", "prepare-bootstrap-runtime", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "prepare-bootstrap-runtime"
		record.NextAction = "prepare operation-scoped Kubernetes runtime before kubeadm"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return operation.OperationRecord{}, err
	}
	result, err := bootstrapruntime.Prepare(e.Root, plan, startedAt)
	if err != nil {
		failedAt := e.clock()
		_, artifactErr := e.Store.AddDiagnosticArtifact(record.OperationID, "prepare-bootstrap-runtime-error", []byte(inventory.Redact(err.Error())), failedAt)
		updated, markErr := e.failRecordPhase(record.OperationID, "prepare-bootstrap-runtime-failed", "prepare-bootstrap-runtime", "prepare-bootstrap-runtime", "bootstrap runtime preparation failed before kubeadm mutation", errors.Join(err, artifactErr))
		return updated, errors.Join(err, artifactErr, markErr)
	}
	updatedAt := e.clock()
	updated, err := e.Store.Update(record.OperationID, "prepare-bootstrap-runtime-complete", "prepare-bootstrap-runtime", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "prepare-bootstrap-runtime"
		record.CompletedPhases = appendMissing(record.CompletedPhases, "accepted", "prepare-bootstrap-runtime")
		record.PhaseIndex = len(record.CompletedPhases)
		record.ActivationState = operation.ActivationStateActiveLive
		record.GenerationCommitState = operation.GenerationCommitCandidate
		record.CandidateGenerationID = result.Record.GenerationID
		if record.BootstrapRequest != nil {
			record.BootstrapRequest.KubernetesBundleManifestDigest = plan.RuntimeInputs.SelectedKubernetesSysext.BundleManifestDigest
			record.BootstrapRequest.KubernetesSysextPayloadDigest = plan.RuntimeInputs.SelectedKubernetesSysext.SysextPayloadDigest
		}
		record.NextAction = "run bootstrap runtime readiness checks before kubeadm"
		record.UpdatedAt = updatedAt
		return record, nil
	})
	if err != nil {
		return operation.OperationRecord{}, err
	}
	return updated, nil
}

func (e *Executor) failRecord(operationID string, eventID string, eventType string, nextAction string, cause error) (operation.OperationRecord, error) {
	return e.failRecordPhase(operationID, eventID, eventType, "dispatch-failed", nextAction, cause)
}

func (e *Executor) failRecordPhase(operationID string, eventID string, eventType string, phase string, nextAction string, cause error) (operation.OperationRecord, error) {
	now := e.clock()
	return e.Store.Update(operationID, eventID, eventType, func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = phase
		record.Result = operation.ResultFailedNeedsRepair
		record.RecoveryRequired = true
		record.NextAction = nextAction
		record.FailureReason = inventory.Redact(cause.Error())
		record.Terminal = true
		record.UpdatedAt = now
		record.CompletedAt = &now
		return record, nil
	})
}

func (e *Executor) gateBootstrapReadiness(ctx context.Context, record operation.OperationRecord, plan toolPlan) (operation.OperationRecord, error) {
	if ctx.Err() != nil {
		return operation.OperationRecord{}, ctx.Err()
	}
	if !requiresBootstrapRuntime(record) {
		return record, nil
	}
	startedAt := e.clock()
	if _, err := e.Store.Update(record.OperationID, "bootstrap-runtime-ready-start", "bootstrap-runtime-ready", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "bootstrap-runtime-ready"
		record.NextAction = "run bootstrap runtime readiness checks before kubeadm"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return operation.OperationRecord{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	result := e.readinessRunner()(readyCtx, bootstrapReadinessArgs(record, plan), func(int) {})
	if errors.Is(readyCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
		result.Err = readyCtx.Err()
		result.ExitStatus = -1
	}
	completedAt := e.clock()
	if result.Err != nil || result.ExitStatus != 0 {
		var artifactErr error
		if len(result.Stdout) > 0 {
			if _, err := e.Store.AddDiagnosticArtifact(record.OperationID, "bootstrap-runtime-ready-stdout", []byte(inventory.Redact(string(result.Stdout))), completedAt); err != nil {
				artifactErr = errors.Join(artifactErr, err)
			}
		}
		if len(result.Stderr) > 0 {
			if _, err := e.Store.AddDiagnosticArtifact(record.OperationID, "bootstrap-runtime-ready-stderr", []byte(inventory.Redact(string(result.Stderr))), completedAt); err != nil {
				artifactErr = errors.Join(artifactErr, err)
			}
		}
		cause := fmt.Errorf("bootstrap runtime readiness gate failed: %s", toolFailure(result))
		updated, markErr := e.failRecordPhase(record.OperationID, "bootstrap-runtime-ready-failed", "bootstrap-runtime-ready", "bootstrap-runtime-ready", "bootstrap runtime readiness failed before kubeadm mutation", errors.Join(cause, artifactErr))
		return updated, errors.Join(cause, artifactErr, markErr)
	}
	updated, err := e.Store.Update(record.OperationID, "bootstrap-runtime-ready-complete", "bootstrap-runtime-ready", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "bootstrap-runtime-ready"
		record.CompletedPhases = appendMissing(record.CompletedPhases, "bootstrap-runtime-ready")
		record.PhaseIndex = len(record.CompletedPhases)
		record.NextAction = "run kubeadm through katlc agent executor"
		record.UpdatedAt = completedAt
		return record, nil
	})
	if err != nil {
		return operation.OperationRecord{}, err
	}
	return updated, nil
}

func (e *Executor) finalizeSuccessfulOperation(ctx context.Context, operationID string) error {
	record, err := e.Store.Read(operationID)
	if err != nil {
		return err
	}
	if !requiresBootstrapGenerationCommit(record) {
		now := e.clock()
		_, err := e.Store.Update(operationID, "operation-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.CompletedAt = &now
			record.Terminal = true
			record.Result = operation.ResultSucceeded
			record.NextAction = "operation completed by katlc agent executor"
			record.UpdatedAt = now
			return record, nil
		})
		return err
	}
	startedAt := e.clock()
	if _, err := e.Store.Update(operationID, "post-kubeadm-health-start", "post-kubeadm-health", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "post-kubeadm-health"
		record.PostKubeadmHealthState = operation.PostKubeadmHealthRunning
		record.NextAction = "validate local kubeadm health before committing generation"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return err
	}
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	result := e.postHealthRunner()(healthCtx, postKubeadmHealthArgs(record), func(int) {})
	if errors.Is(healthCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
		result.Err = healthCtx.Err()
		result.ExitStatus = -1
	}
	completedAt := e.clock()
	var artifactErr error
	if len(result.Stdout) > 0 {
		if _, err := e.Store.AddDiagnosticArtifact(operationID, "post-kubeadm-health-stdout", []byte(inventory.Redact(string(result.Stdout))), completedAt); err != nil {
			artifactErr = errors.Join(artifactErr, err)
		}
	}
	if len(result.Stderr) > 0 {
		if _, err := e.Store.AddDiagnosticArtifact(operationID, "post-kubeadm-health-stderr", []byte(inventory.Redact(string(result.Stderr))), completedAt); err != nil {
			artifactErr = errors.Join(artifactErr, err)
		}
	}
	evidence := postKubeadmEvidence(e.Root, record, result, completedAt)
	evidenceData, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		artifactErr = errors.Join(artifactErr, err)
	} else if _, err := e.Store.AddDiagnosticArtifact(operationID, "post-kubeadm-health-evidence", append(evidenceData, '\n'), completedAt); err != nil {
		artifactErr = errors.Join(artifactErr, err)
	}
	if result.Err != nil || result.ExitStatus != 0 {
		cause := fmt.Errorf("post-kubeadm health checks failed: %s", toolFailure(result))
		_, markErr := e.Store.Update(operationID, "post-kubeadm-health-failed", "post-kubeadm-health", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.Phase = "post-kubeadm-health"
			record.PostKubeadmHealthState = operation.PostKubeadmHealthFailed
			record.CompletedAt = &completedAt
			record.Terminal = true
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.BootHealthPending = false
			record.NextAction = "operator inspection required after kubeadm mutated Kubernetes state"
			record.FailureReason = inventory.Redact(errors.Join(cause, artifactErr).Error())
			record.UpdatedAt = completedAt
			return record, nil
		})
		return errors.Join(cause, artifactErr, markErr)
	}
	if artifactErr != nil {
		_, markErr := e.Store.Update(operationID, "post-kubeadm-health-bookkeeping-failed", "post-kubeadm-health", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.Phase = "post-kubeadm-health"
			record.PostKubeadmHealthState = operation.PostKubeadmHealthPassed
			record.CompletedAt = &completedAt
			record.Terminal = true
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.BootHealthPending = false
			record.NextAction = "explicit repair required after post-kubeadm health evidence bookkeeping failure"
			record.FailureReason = inventory.Redact(artifactErr.Error())
			record.UpdatedAt = completedAt
			return record, nil
		})
		return errors.Join(artifactErr, markErr)
	}
	record, err = e.Store.Read(operationID)
	if err != nil {
		return err
	}
	if err := e.promoteCandidateGenerationLive(ctx, record, completedAt, "kubeadm completed and post-kubeadm health checks passed"); err != nil {
		_, markErr := e.Store.Update(operationID, "bootstrap-generation-commit-failed", "bootstrap-generation-commit", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.Phase = "post-kubeadm-health"
			record.PostKubeadmHealthState = operation.PostKubeadmHealthPassed
			record.CompletedAt = &completedAt
			record.Terminal = true
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.BootHealthPending = false
			record.NextAction = "explicit repair required after generation commit bookkeeping failure"
			record.FailureReason = inventory.Redact(errors.Join(err, artifactErr).Error())
			record.UpdatedAt = completedAt
			return record, nil
		})
		return errors.Join(err, artifactErr, markErr)
	}
	_, err = e.Store.Update(operationID, "operation-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "record-operation-complete"
		record.CompletedPhases = appendMissing(record.CompletedPhases, "post-kubeadm-health", "record-operation-complete")
		record.PhaseIndex = len(record.CompletedPhases)
		record.PostKubeadmHealthState = operation.PostKubeadmHealthPassed
		record.GenerationCommitState = operation.GenerationCommitCommitted
		record.ActivationState = operation.ActivationStateActiveLive
		record.BootHealthPending = false
		record.CompletedAt = &completedAt
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.NextAction = "continue managing the node through its active generation"
		record.UpdatedAt = completedAt
		return record, nil
	})
	return errors.Join(err, artifactErr)
}

func (e *Executor) promoteCandidateGenerationLive(ctx context.Context, record operation.OperationRecord, now time.Time, reason string) error {
	candidate := strings.TrimSpace(record.CandidateGenerationID)
	if candidate == "" {
		return fmt.Errorf("candidate generation id is required")
	}
	_, _, _, err := e.writeCandidateLoaderEntry(ctx, candidate)
	if err != nil {
		return err
	}
	if e.SetBootDefault == nil {
		return fmt.Errorf("persistent boot default updater is not configured")
	}
	_, err = generation.PromoteLiveGeneration(generation.LivePromotionRequest{
		Root:         e.Root,
		GenerationID: candidate,
		OperationID:  record.OperationID,
		Reason:       reason,
		Now:          now,
		SetBootDefault: func(root, entry string) error {
			return e.SetBootDefault(ctx, root, entry)
		},
	})
	return err
}

func (e *Executor) commitCandidateGeneration(ctx context.Context, record operation.OperationRecord, now time.Time, reason string) error {
	candidate := strings.TrimSpace(record.CandidateGenerationID)
	if candidate == "" {
		return fmt.Errorf("candidate generation id is required")
	}
	spec, status, entry, err := e.writeCandidateLoaderEntry(ctx, candidate)
	if err != nil {
		return err
	}
	committedAt := now.UTC()
	selection, err := generation.ReadBootSelection(e.Root)
	if err != nil {
		return fmt.Errorf("read boot selection: %w", err)
	}
	previousSelection := selection
	fallback := strings.TrimSpace(selection.Generation0FallbackID)
	if fallback == "" {
		fallback = selection.DefaultGenerationID
	}
	selection.TargetBootGenerationID = candidate
	selection.TrialGenerationID = candidate
	selection.PreviousKnownGoodGenerationID = selection.DefaultGenerationID
	selection.Generation0FallbackID = fallback
	selection.TargetBootEntry = entry
	selection.TrialBootEntry = entry
	selection.PreviousKnownGoodBootEntry = selection.DefaultBootEntry
	selection.PendingTransactionID = record.OperationID
	selection.PendingHealthValidation = true
	selection.PersistentDefaultPromotion = generation.DefaultPromotionPending
	selection.UpdatedAt = committedAt
	if err := generation.WriteBootSelection(e.Root, selection); err != nil {
		return fmt.Errorf("arm boot health validation: %w", err)
	}
	if e.SetBootOneshot != nil {
		if err := e.SetBootOneshot(ctx, e.Root, selection.TrialBootEntry); err != nil {
			if restoreErr := generation.WriteBootSelection(e.Root, previousSelection); restoreErr != nil {
				return fmt.Errorf("arm boot health validation: %w; restore boot selection: %w", err, restoreErr)
			}
			return fmt.Errorf("arm boot health validation: %w", err)
		}
	}
	status.CommitState = generation.CommitStateCommitted
	status.BootState = generation.BootStateTrying
	status.HealthState = generation.HealthStateUnknown
	status.UpdatedAt = committedAt
	status.CommittedAt = &committedAt
	status.CommittedByOperation = record.OperationID
	status.StatusTransitions = append(status.StatusTransitions, generation.StatusTransition{
		At:          committedAt,
		OperationID: record.OperationID,
		Reason:      reason,
		CommitState: status.CommitState,
		BootState:   status.BootState,
		HealthState: status.HealthState,
	})
	if err := generation.WriteGenerationStatus(e.Root, spec, status); err != nil {
		if restoreErr := generation.WriteBootSelection(e.Root, previousSelection); restoreErr != nil {
			return fmt.Errorf("commit candidate generation: %w; restore boot selection: %w", err, restoreErr)
		}
		return fmt.Errorf("commit candidate generation: %w", err)
	}
	return nil
}

func (e *Executor) writeCandidateLoaderEntry(ctx context.Context, candidate string) (generation.GenerationSpec, generation.GenerationStatus, string, error) {
	spec, status, err := generation.ReadGeneration(e.Root, candidate)
	if err != nil {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("read candidate generation: %w", err)
	}
	if status.CommitState != generation.CommitStateCandidate {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("candidate generation %s commitState is %s, want candidate", candidate, status.CommitState)
	}
	entry := strings.TrimSpace(spec.Boot.LoaderEntryPath)
	if entry == "" {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("candidate generation %s loader entry path is required", candidate)
	}
	machineID, err := runtimeMachineID(e.Root)
	if err != nil {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", err
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	entryPath, err := generation.WriteEntry(bootRoot, generation.LoaderRequest{Record: generation.RecordFromSplit(spec, status), MachineID: machineID})
	if err != nil && errors.Is(err, syscall.EROFS) {
		if e.MountBootRoot == nil {
			return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("write candidate loader entry: %w", err)
		}
		if mountErr := e.MountBootRoot(ctx, bootRoot); mountErr != nil {
			return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("write candidate loader entry: %w; mount boot root: %v", err, mountErr)
		}
		entryPath, err = generation.WriteEntry(bootRoot, generation.LoaderRequest{Record: generation.RecordFromSplit(spec, status), MachineID: machineID})
	}
	if err != nil {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("write candidate loader entry: %w", err)
	}
	relativeEntry, err := bootRelativePath(bootRoot, entryPath)
	if err != nil {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", err
	}
	if relativeEntry != entry {
		return generation.GenerationSpec{}, generation.GenerationStatus{}, "", fmt.Errorf("candidate loader entry %s does not match generation metadata %s", relativeEntry, entry)
	}
	return spec, status, entry, nil
}

func (e *Executor) operationStoreRoot() string {
	if strings.TrimSpace(e.Store.Root) != "" {
		return e.Store.Root
	}
	root := strings.TrimSpace(e.Root)
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, "var/lib/katl/operations")
}

func (e *Executor) clock() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Executor) toolRunner() ToolRunner {
	if e.RunTool != nil {
		return e.RunTool
	}
	return runChildProcess
}

func (e *Executor) readinessRunner() ToolRunner {
	if e.RunReadiness != nil {
		return e.RunReadiness
	}
	return runReadinessCommand
}

func (e *Executor) postHealthRunner() ToolRunner {
	if e.RunPostHealth != nil {
		return e.RunPostHealth
	}
	return runPostKubeadmHealthCommand
}

func (e *Executor) endpointLifecycleRunner() ToolRunner {
	if e.RunEndpointLifecycle != nil {
		return e.RunEndpointLifecycle
	}
	return runChildProcess
}

func requiresBootstrapRuntime(record operation.OperationRecord) bool {
	if record.BootstrapRequest == nil {
		return false
	}
	switch record.OperationKind {
	case bootstrapplan.OperationKindInit, bootstrapplan.OperationKindJoinWorker, bootstrapplan.OperationKindJoinControlPlane:
		return true
	default:
		return false
	}
}

func requiresBootstrapGenerationCommit(record operation.OperationRecord) bool {
	if record.BootstrapRequest == nil {
		return false
	}
	switch record.OperationKind {
	case bootstrapplan.OperationKindInit, bootstrapplan.OperationKindJoinWorker, bootstrapplan.OperationKindJoinControlPlane:
		return true
	default:
		return false
	}
}

func postKubeadmHealthArgs(record operation.OperationRecord) []string {
	return []string{record.OperationKind}
}

func bootstrapReadinessArgs(record operation.OperationRecord, plan toolPlan) []string {
	return []string{record.CandidateGenerationID, kubeadmConfigPath(plan.Argv)}
}

func kubeadmConfigPath(argv []string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--config" {
			return argv[i+1]
		}
	}
	return ""
}

func runReadinessCommand(ctx context.Context, argv []string, started func(int)) ToolResult {
	candidate := ""
	configPath := ""
	if len(argv) > 0 {
		candidate = strings.TrimSpace(argv[0])
	}
	if len(argv) > 1 {
		configPath = strings.TrimSpace(argv[1])
	}
	commands := [][]string{
		{"/usr/bin/systemctl", "daemon-reload"},
	}
	if candidate != "" {
		commands = append(commands,
			[]string{"/usr/lib/katl/runtime/katl-generation-activate", "--root=/", "--generation", candidate},
			[]string{"/usr/bin/systemd-sysext", "refresh"},
			[]string{"/usr/bin/systemd-confext", "refresh"},
		)
	} else {
		commands = append(commands,
			[]string{"/usr/bin/systemctl", "restart", "systemd-sysext.service"},
			[]string{"/usr/bin/systemctl", "restart", "systemd-confext.service"},
		)
	}
	commands = append(commands,
		[]string{"/usr/bin/test", "-x", "/usr/bin/kubelet"},
		[]string{"/usr/bin/systemctl", "start", "etc-kubernetes.mount"},
		[]string{"/usr/bin/systemctl", "start", "containerd.service"},
		[]string{"/usr/bin/systemctl", "start", "katl-state-projection-check.service"},
		[]string{"/usr/bin/systemctl", "start", "katl-kubeadm-ready.target"},
		[]string{"/usr/bin/systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"},
		[]string{"/usr/bin/systemctl", "is-active", "--quiet", "containerd.service"},
		[]string{"/usr/bin/test", "-x", "/usr/bin/kubeadm"},
		[]string{"/usr/bin/test", "-x", "/usr/bin/crictl"},
	)
	if configPath != "" {
		commands = append(commands, []string{"/usr/bin/test", "-s", configPath})
	}
	var stdout, stderr bytes.Buffer
	for _, argv := range commands {
		result := runChildProcess(ctx, argv, started)
		stdout.Write(result.Stdout)
		stderr.Write(result.Stderr)
		if result.Err != nil || result.ExitStatus != 0 {
			result.Stdout = stdout.Bytes()
			result.Stderr = stderr.Bytes()
			return result
		}
	}
	return ToolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitStatus: 0}
}

func runPostKubeadmHealthCommand(ctx context.Context, argv []string, started func(int)) ToolResult {
	commands := postKubeadmHealthCommands(argv...)
	retryKubectl := len(argv) > 0 && argv[0] == OperationKindKubeadmControlPlaneConfig
	var stdout, stderr bytes.Buffer
	for _, argv := range commands {
		result := runChildProcess(ctx, argv, started)
		for retryKubectl && len(argv) > 0 && argv[0] == "/usr/bin/kubectl" && (result.Err != nil || result.ExitStatus != 0) {
			select {
			case <-ctx.Done():
				result.Err = errors.Join(result.Err, ctx.Err())
			case <-time.After(time.Second):
				result = runChildProcess(ctx, argv, started)
				continue
			}
			break
		}
		stdout.Write(result.Stdout)
		stderr.Write(result.Stderr)
		if result.Err != nil || result.ExitStatus != 0 {
			result.Stdout = stdout.Bytes()
			result.Stderr = stderr.Bytes()
			return result
		}
	}
	return ToolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitStatus: 0}
}

func postKubeadmHealthCommands(args ...string) [][]string {
	kind := ""
	if len(args) > 0 {
		kind = args[0]
	}
	if kind == OperationKindKubeadmControlPlaneConfig {
		node := ""
		if len(args) > 1 {
			node = args[1]
		}
		return [][]string{
			{"/usr/bin/test", "-s", "/etc/kubernetes/manifests/kube-apiserver.yaml"},
			{"/usr/bin/test", "-s", "/etc/kubernetes/manifests/kube-controller-manager.yaml"},
			{"/usr/bin/test", "-s", "/etc/kubernetes/manifests/kube-scheduler.yaml"},
			{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "wait", "--for=condition=Ready", "node/" + node, "--timeout=2m"},
			{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"},
			{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "wait", "--for=condition=Ready", "pod/etcd-" + node, "--timeout=2m"},
			{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "exec", "etcd-" + node, "--", "etcdctl", "--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt", "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt", "--key=/etc/kubernetes/pki/etcd/healthcheck-client.key", "endpoint", "health", "--cluster"},
		}
	}
	if kind == bootstrapplan.OperationKindJoinWorker {
		return [][]string{
			{"/usr/bin/test", "-s", "/etc/kubernetes/kubelet.conf"},
			{"/usr/bin/test", "-s", "/var/lib/kubelet/config.yaml"},
			{"/usr/bin/test", "!", "-e", "/etc/kubernetes/admin.conf"},
			{"/usr/bin/test", "!", "-e", "/etc/kubernetes/manifests/kube-apiserver.yaml"},
			{"/usr/bin/systemctl", "is-active", "--quiet", "kubelet.service"},
		}
	}
	return [][]string{
		{"/usr/bin/test", "-s", "/etc/kubernetes/admin.conf"},
		{"/usr/bin/test", "-s", "/etc/kubernetes/manifests/kube-apiserver.yaml"},
		{"/usr/bin/test", "-s", "/etc/kubernetes/manifests/etcd.yaml"},
		{"/usr/bin/systemctl", "is-active", "--quiet", "kubelet.service"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"},
	}
}

func cleanupTemporaryJoinConfig(root string, record operation.OperationRecord) {
	if record.BootstrapRequest == nil {
		return
	}
	path := strings.TrimSpace(record.BootstrapRequest.TemporaryJoinConfigPath)
	if path == "" {
		return
	}
	if !strings.HasPrefix(path, "/run/katl/bootstrap-join/") || strings.Contains(path, "\x00") {
		return
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = "/"
	}
	target := filepath.Join(filepath.Clean(root), strings.TrimPrefix(path, "/"))
	_ = os.Remove(filepath.Join(filepath.Dir(target), "discovery.conf"))
	_ = os.Remove(target)
	_ = os.Remove(filepath.Dir(target))
}

func joinDiscoveryConfigPath(record operation.OperationRecord) string {
	if record.BootstrapRequest == nil {
		return ""
	}
	path := strings.TrimSpace(record.BootstrapRequest.TemporaryJoinConfigPath)
	if !strings.HasPrefix(path, "/run/katl/bootstrap-join/") || strings.Contains(path, "\x00") {
		return ""
	}
	return filepath.Join(filepath.Dir(path), "discovery.conf")
}

func expiredJoinMaterial(record operation.OperationRecord, now time.Time) string {
	if !bootstrapJoinOperation(record.OperationKind) || record.BootstrapRequest == nil {
		return ""
	}
	expiresAt := strings.TrimSpace(record.BootstrapRequest.JoinMaterialExpiresAt)
	if expiresAt == "" {
		return "join material expiry is not recorded"
	}
	parsed, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return "join material expiry is invalid"
	}
	if !parsed.After(now.UTC()) {
		return "join material is expired"
	}
	return ""
}

func alreadyJoined(record operation.OperationRecord, result ToolResult) bool {
	if !bootstrapJoinOperation(record.OperationKind) {
		return false
	}
	text := strings.ToLower(string(result.Stdout) + "\n" + string(result.Stderr) + "\n" + toolFailure(result))
	return strings.Contains(text, "already joined")
}

func (e *Executor) joinPostHealthPassed(ctx context.Context, record operation.OperationRecord) bool {
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	result := e.postHealthRunner()(healthCtx, postKubeadmHealthArgs(record), func(int) {})
	return healthCtx.Err() == nil && result.Err == nil && result.ExitStatus == 0
}

func bootstrapJoinOperation(kind string) bool {
	return kind == bootstrapplan.OperationKindJoinWorker || kind == bootstrapplan.OperationKindJoinControlPlane
}

func mountRuntimeBootRoot(ctx context.Context, bootRoot string) error {
	bootRoot = strings.TrimSpace(bootRoot)
	if bootRoot == "" {
		return fmt.Errorf("boot root is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if mounted, err := bootRootIsMountpoint(ctx, bootRoot); err != nil {
		return err
	} else if mounted {
		return nil
	}
	var errs []error
	for _, source := range runtimeBootRootSources() {
		mountCtx, cancel := context.WithTimeout(ctx, bootRootMountTimeout)
		result := runChildProcess(mountCtx, []string{"/usr/bin/mount", source, bootRoot}, nil)
		cancel()
		if result.Err == nil && result.ExitStatus == 0 {
			return nil
		}
		errs = append(errs, fmt.Errorf("mount %s on %s: %s", source, bootRoot, toolFailure(result)))
	}
	return errors.Join(errs...)
}

func runtimeBootRootSources() []string {
	return []string{
		"/dev/disk/by-label/KATLEFI",
		"/dev/disk/by-id/virtio-katl-efi",
		"/dev/disk/by-id/virtio-katl-efi-part1",
		"/dev/disk/by-partlabel/" + disk.GPTLabelESP,
	}
}

func bootRootIsMountpoint(ctx context.Context, bootRoot string) (bool, error) {
	mountCtx, cancel := context.WithTimeout(ctx, bootRootMountTimeout)
	result := runChildProcess(mountCtx, []string{"/usr/bin/mountpoint", "-q", bootRoot}, nil)
	cancel()
	if result.Err == nil && result.ExitStatus == 0 {
		return true, nil
	}
	if errors.Is(result.Err, os.ErrNotExist) {
		return false, nil
	}
	if result.ExitStatus == 1 || result.ExitStatus == 32 {
		return false, nil
	}
	return false, fmt.Errorf("check boot root mountpoint %s: %s", bootRoot, toolFailure(result))
}

func setBootOneshot(ctx context.Context, root string, bootEntry string) error {
	return setBootEntry(ctx, root, "set-oneshot", bootEntry)
}

func setBootDefault(ctx context.Context, root string, bootEntry string) error {
	return setBootEntry(ctx, root, "set-default", bootEntry)
}

func setBootEntry(ctx context.Context, root, verb, bootEntry string) error {
	bootEntry = filepath.Base(strings.TrimSpace(bootEntry))
	if bootEntry == "." || bootEntry == "" {
		return fmt.Errorf("boot entry is required")
	}
	args := []string{"bootctl"}
	root = runtimeRoot(root)
	if root != "/" {
		args = append(args, "--esp-path="+filepath.Join(root, "efi"))
	}
	args = append(args, verb, bootEntry)
	bootCtx, cancel := context.WithTimeout(ctx, bootRootMountTimeout)
	defer cancel()
	result := runChildProcess(bootCtx, args, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), toolFailure(result))
	}
	return nil
}

func runChildProcess(ctx context.Context, argv []string, started func(int)) ToolResult {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return ToolResult{Err: err, ExitStatus: -1}
	}
	pid := cmd.Process.Pid
	if started != nil {
		started(pid)
	}
	err := cmd.Wait()
	status := 0
	if err != nil {
		status = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = exitErr.ExitCode()
		}
	}
	return ToolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitStatus: status, PID: pid, Err: err}
}

func liveAgentInvocation(invocation operation.InvocationRecord) bool {
	if invocation.PID <= 0 || invocation.CompletedAt != nil {
		return false
	}
	process, err := os.FindProcess(invocation.PID)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func failAcceptedButNotStarted(store operation.Store, now time.Time) error {
	ids, err := store.OperationIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, err := store.Read(id)
		if err != nil {
			return err
		}
		if record.Terminal || record.ExecutorPlan == nil || record.ExternalMutationStarted || record.MutatingToolRan || len(record.PreExecMutationMarkers) > 0 || len(record.Invocations) > 0 {
			continue
		}
		_, err = store.Update(record.OperationID, "startup-audit-not-started", "startup-audit-not-started", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			record.Phase = "startup-audit-not-started"
			record.Terminal = true
			record.CompletedAt = &now
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.NextAction = "resubmit operation request; previous accepted attempt did not start"
			record.FailureReason = "agent stopped before executor start"
			record.UpdatedAt = now
			return record, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func currentBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func executorPlan(record operation.OperationRecord) (toolPlan, error) {
	if record.ExecutorPlan == nil {
		return toolPlan{}, fmt.Errorf("operation executor plan is required")
	}
	plan := *record.ExecutorPlan
	plan.MutationScopes = append([]string(nil), plan.MutationScopes...)
	plan.Argv = append([]string(nil), plan.Argv...)
	return plan, validateToolPlan(plan)
}

func validateToolPlan(plan toolPlan) error {
	if strings.TrimSpace(plan.Phase) == "" {
		return fmt.Errorf("operation tool plan phase is required")
	}
	if len(plan.Argv) == 0 {
		return fmt.Errorf("operation tool plan argv is required")
	}
	if strings.TrimSpace(plan.Argv[0]) == "" {
		return fmt.Errorf("operation tool plan argv[0] is required")
	}
	return nil
}

func digestArgv(argv []string) string {
	data, _ := json.Marshal(argv)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func generatedMarkerID(phase string, argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{phase}, argv...), "\x00")))
	return "pre-exec-" + cleanID(phase) + "-" + hex.EncodeToString(sum[:8])
}

func cleanID(value string) string {
	value = strings.Trim(strings.ToLower(value), " \t\n\r")
	value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "phase"
	}
	return b.String()
}

func completeInvocation(invocations []operation.InvocationRecord, id string, completedAt time.Time, resultText string, result ToolResult) {
	for i := range invocations {
		if invocations[i].InvocationID == id {
			invocations[i].CompletedAt = &completedAt
			invocations[i].Result = resultText
			invocations[i].PID = result.PID
			invocations[i].ExitStatus = result.ExitStatus
			return
		}
	}
}

func exitResult(result ToolResult) string {
	return fmt.Sprintf("exit-%d", result.ExitStatus)
}

func toolFailure(result ToolResult) string {
	parts := []string{exitResult(result)}
	if len(result.Stderr) > 0 {
		parts = append(parts, strings.TrimSpace(string(result.Stderr)))
	}
	if result.Err != nil {
		parts = append(parts, result.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func appendMissing(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, addition := range additions {
		if strings.TrimSpace(addition) == "" {
			continue
		}
		if _, ok := seen[addition]; ok {
			continue
		}
		values = append(values, addition)
		seen[addition] = struct{}{}
	}
	return values
}

func redactArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		out = append(out, inventory.Redact(arg))
	}
	return out
}

type postHealthEvidence struct {
	CollectedAt        time.Time       `json:"collectedAt"`
	HealthExitStatus   int             `json:"healthExitStatus"`
	HealthStdoutBytes  int             `json:"healthStdoutBytes,omitempty"`
	HealthStdoutSHA256 string          `json:"healthStdoutSHA256,omitempty"`
	HealthStderrBytes  int             `json:"healthStderrBytes,omitempty"`
	HealthStderrSHA256 string          `json:"healthStderrSHA256,omitempty"`
	NodeIdentity       nodeEvidence    `json:"nodeIdentityEvidence"`
	Kubeadm            kubeadmEvidence `json:"kubeadmEvidence"`
	JoinMaterial       joinEvidence    `json:"joinMaterialEvidence,omitempty"`
	APIEvidence        []fileEvidence  `json:"apiEvidence,omitempty"`
	StaticPods         []fileEvidence  `json:"staticPodManifestEvidence,omitempty"`
	EtcdEvidence       []fileEvidence  `json:"etcdMemberEvidence,omitempty"`
	BootstrapMaterial  []fileEvidence  `json:"bootstrapMaterialEvidence,omitempty"`
}

type nodeEvidence struct {
	InventoryNodeName   string `json:"inventoryNodeName,omitempty"`
	SystemRole          string `json:"systemRole,omitempty"`
	CandidateGeneration string `json:"candidateGenerationID,omitempty"`
	ExpectedMachineID   string `json:"expectedMachineID,omitempty"`
}

type kubeadmEvidence struct {
	OperationKind      string `json:"operationKind"`
	Phase              string `json:"phase"`
	RequestDigest      string `json:"requestDigest,omitempty"`
	KubeadmInputDigest string `json:"kubeadmInputDigest,omitempty"`
	ArgvDigest         string `json:"argvDigest,omitempty"`
	ExitStatus         int    `json:"exitStatus"`
}

type joinEvidence struct {
	Present        bool   `json:"present,omitempty"`
	RefDigest      string `json:"refDigest,omitempty"`
	MaterialDigest string `json:"materialDigest,omitempty"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	ConfigPath     string `json:"configPath,omitempty"`
}

type fileEvidence struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	IsDir     bool   `json:"isDir,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Error     string `json:"error,omitempty"`
}

func postKubeadmEvidence(root string, record operation.OperationRecord, result ToolResult, collectedAt time.Time) postHealthEvidence {
	var node nodeEvidence
	var join joinEvidence
	inputDigest := ""
	if record.BootstrapRequest != nil {
		node.InventoryNodeName = record.BootstrapRequest.InventoryNodeName
		node.SystemRole = record.BootstrapRequest.SystemRole
		node.CandidateGeneration = record.CandidateGenerationID
		if strings.TrimSpace(record.BootstrapRequest.JoinMaterialRef) != "" {
			join.Present = true
			join.RefDigest = digestEvidenceBytes([]byte(record.BootstrapRequest.JoinMaterialRef))
		}
		if strings.TrimSpace(record.BootstrapRequest.JoinMaterialDigest) != "" {
			join.Present = true
			join.MaterialDigest = record.BootstrapRequest.JoinMaterialDigest
		}
		join.ExpiresAt = strings.TrimSpace(record.BootstrapRequest.JoinMaterialExpiresAt)
		join.ConfigPath = strings.TrimSpace(record.BootstrapRequest.TemporaryJoinConfigPath)
		inputDigest = record.BootstrapRequest.KubeadmInputDigest
	}
	node.ExpectedMachineID = record.ExpectedMachineID
	argvDigest := ""
	if record.ExecutorPlan != nil {
		argvDigest = digestArgv(record.ExecutorPlan.Argv)
	}
	evidence := postHealthEvidence{
		CollectedAt:      collectedAt.UTC(),
		HealthExitStatus: result.ExitStatus,
		NodeIdentity:     node,
		Kubeadm: kubeadmEvidence{
			OperationKind:      record.OperationKind,
			Phase:              record.Phase,
			RequestDigest:      record.RequestDigest,
			KubeadmInputDigest: inputDigest,
			ArgvDigest:         argvDigest,
			ExitStatus:         result.ExitStatus,
		},
		JoinMaterial: join,
		APIEvidence: []fileEvidence{
			evidenceForPath(root, "/etc/kubernetes/admin.conf"),
		},
		StaticPods: []fileEvidence{
			evidenceForPath(root, "/etc/kubernetes/manifests/kube-apiserver.yaml"),
			evidenceForPath(root, "/etc/kubernetes/manifests/kube-controller-manager.yaml"),
			evidenceForPath(root, "/etc/kubernetes/manifests/kube-scheduler.yaml"),
			evidenceForPath(root, "/etc/kubernetes/manifests/etcd.yaml"),
		},
		EtcdEvidence: []fileEvidence{
			evidenceForPath(root, "/var/lib/etcd/member"),
		},
		BootstrapMaterial: []fileEvidence{
			evidenceForPath(root, "/etc/kubernetes/kubelet.conf"),
			evidenceForPath(root, "/etc/kubernetes/pki/ca.crt"),
			evidenceForPath(root, "/etc/kubernetes/admin.conf"),
		},
	}
	if len(result.Stdout) > 0 {
		evidence.HealthStdoutBytes = len(result.Stdout)
		evidence.HealthStdoutSHA256 = digestEvidenceBytes(result.Stdout)
	}
	if len(result.Stderr) > 0 {
		evidence.HealthStderrBytes = len(result.Stderr)
		evidence.HealthStderrSHA256 = digestEvidenceBytes(result.Stderr)
	}
	return evidence
}

func evidenceForPath(root string, runtimePath string) fileEvidence {
	path := filepath.ToSlash(filepath.Clean(runtimePath))
	evidence := fileEvidence{Path: path}
	hostPath := filepath.Join(runtimeRoot(root), strings.TrimPrefix(path, "/"))
	info, err := os.Stat(hostPath)
	if errors.Is(err, os.ErrNotExist) {
		return evidence
	}
	if err != nil {
		evidence.Error = inventory.Redact(err.Error())
		return evidence
	}
	evidence.Exists = true
	evidence.IsDir = info.IsDir()
	evidence.SizeBytes = info.Size()
	if info.Mode().IsRegular() {
		data, err := os.ReadFile(hostPath)
		if err != nil {
			evidence.Error = inventory.Redact(err.Error())
		} else {
			evidence.SHA256 = digestEvidenceBytes(data)
		}
	}
	return evidence
}

func digestEvidenceBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func runtimeMachineID(root string) (string, error) {
	for _, path := range []string{
		filepath.Join(runtimeRoot(root), "var/lib/katl/identity/machine-id"),
		filepath.Join(runtimeRoot(root), "etc/machine-id"),
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if value != "" {
				return value, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("machine identity is not initialized")
}

func bootRelativePath(bootRoot string, entryPath string) (string, error) {
	rel, err := filepath.Rel(bootRoot, entryPath)
	if err != nil {
		return "", fmt.Errorf("make loader entry path boot-relative: %w", err)
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("loader entry %s is outside boot root %s", entryPath, bootRoot)
	}
	return filepath.ToSlash(rel), nil
}

func runtimeRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return string(filepath.Separator)
	}
	return filepath.Clean(root)
}
