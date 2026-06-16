package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/bootstrapplan"
	"github.com/zariel/katl/internal/installer/bootstrapruntime"
	"github.com/zariel/katl/internal/installer/operation"
)

const (
	defaultToolTimeout = 25 * time.Minute
	maxToolTimeout     = 25 * time.Minute
	readinessTimeout   = 2 * time.Minute
)

type ToolResult struct {
	Stdout     []byte
	Stderr     []byte
	ExitStatus int
	PID        int
	Err        error
}

type ToolRunner func(context.Context, []string, func(int)) ToolResult

type Executor struct {
	Root         string
	Store        operation.Store
	AgentStartID string
	Now          func() time.Time
	RunTool      ToolRunner
	RunReadiness ToolRunner
	Async        bool
}

type toolPlan = operation.ExecutorPlan

func NewExecutor(root string, store operation.Store, agentStartID string) *Executor {
	return &Executor{
		Root:         strings.TrimSpace(root),
		Store:        store,
		AgentStartID: strings.TrimSpace(agentStartID),
		Now:          func() time.Time { return time.Now().UTC() },
		RunTool:      runChildProcess,
		RunReadiness: runReadinessCommand,
		Async:        true,
	}
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
	if e.Async {
		go func() {
			_ = e.Execute(context.Background(), record)
		}()
		return nil
	}
	_ = e.Execute(ctx, record)
	return nil
}

func (e *Executor) Execute(ctx context.Context, record operation.OperationRecord) error {
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
	ready, err := e.gateBootstrapReadiness(ctx, record)
	if err != nil {
		return err
	}
	record = ready
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
	if errors.Is(toolCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
		result.Err = toolCtx.Err()
		result.ExitStatus = -1
	}
	completedAt := e.clock()
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
	startMu.Lock()
	startErr := startUpdateErr
	startMu.Unlock()
	resultText := exitResult(result)
	if timedOut {
		resultText = operation.ResultTimedOut
	}
	_, updateErr := e.Store.Update(record.OperationID, markerID+"-complete", "child-process-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		completeInvocation(record.Invocations, markerID, completedAt, resultText, result)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, inventory.Redact(strings.Join(plan.Argv, " ")))
		record.Phase = plan.Phase
		record.UpdatedAt = completedAt
		record.CompletedAt = &completedAt
		record.Terminal = true
		if timedOut {
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.Interruption = operation.ResultTimedOut
			record.NextAction = "explicit repair required after operation timeout"
			record.FailureReason = inventory.Redact(toolFailure(result))
			return record, nil
		}
		if result.Err == nil && result.ExitStatus == 0 {
			if startErr != nil || artifactErr != nil {
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
			record.Result = operation.ResultSucceeded
			record.NextAction = "operation completed by katlc agent executor"
			return record, nil
		}
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
	return nil
}

func (e *Executor) prepareBootstrapRuntime(ctx context.Context, record operation.OperationRecord) (operation.OperationRecord, error) {
	if ctx.Err() != nil {
		return operation.OperationRecord{}, ctx.Err()
	}
	if !requiresBootstrapRuntime(record) {
		return record, nil
	}
	plan, err := bootstrapplan.FromOperation(e.Root, record)
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
		record.NextAction = "wait for katl-kubeadm-ready.target before kubeadm"
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

func (e *Executor) gateBootstrapReadiness(ctx context.Context, record operation.OperationRecord) (operation.OperationRecord, error) {
	if ctx.Err() != nil {
		return operation.OperationRecord{}, ctx.Err()
	}
	if !requiresBootstrapRuntime(record) {
		return record, nil
	}
	startedAt := e.clock()
	if _, err := e.Store.Update(record.OperationID, "bootstrap-runtime-ready-start", "bootstrap-runtime-ready", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "bootstrap-runtime-ready"
		record.NextAction = "start katl-kubeadm-ready.target before kubeadm"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return operation.OperationRecord{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	result := e.readinessRunner()(readyCtx, nil, func(int) {})
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
		cause := fmt.Errorf("katl-kubeadm-ready.target gate failed: %s", toolFailure(result))
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

func requiresBootstrapRuntime(record operation.OperationRecord) bool {
	if record.BootstrapRequest == nil {
		return false
	}
	switch record.OperationKind {
	case bootstrapplan.OperationKindInit, bootstrapplan.OperationKindJoinWorker:
		return true
	default:
		return false
	}
}

func runReadinessCommand(ctx context.Context, _ []string, started func(int)) ToolResult {
	commands := [][]string{
		{"/usr/bin/systemctl", "daemon-reload"},
		{"/usr/bin/systemctl", "restart", "systemd-sysext.service"},
		{"/usr/bin/systemctl", "restart", "systemd-confext.service"},
		{"/usr/bin/systemctl", "start", "katl-kubeadm-ready.target"},
		{"/usr/bin/systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"},
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
	return []string{inventory.Redact(strings.Join(argv, " "))}
}
