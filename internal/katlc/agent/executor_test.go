package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
)

func TestSubmitOperationExecutesThroughAgentExecutor(t *testing.T) {
	server := newTestServer(t)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		if strings.Join(argv, " ") != "/usr/bin/kubeadm init --config /etc/katl/kubeadm/bootstrap-init-01.yaml" {
			t.Fatalf("argv = %v, want operation-scoped kubeadm config", argv)
		}
		started(123)
		return ToolResult{
			Stdout:     []byte("created token Bearer abc.def\n"),
			Stderr:     []byte("warning\n"),
			ExitStatus: 0,
			PID:        123,
		}
	}
	server.Dispatcher = executor

	req := submitRequest("req-execute")
	req.OperationTimeout = "7s"
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.ExecutorPlan == nil || record.ExecutorPlan.Timeout != "7s" {
		t.Fatalf("executor plan = %+v, want operation timeout", record.ExecutorPlan)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.Phase != "kubeadm-init" {
		t.Fatalf("record = %+v, want terminal success in kubeadm-init", record)
	}
	if len(record.PreExecMutationMarkers) != 1 || record.PreExecMutationMarkers[0].MarkerID != "kubeadm-init" {
		t.Fatalf("markers = %+v", record.PreExecMutationMarkers)
	}
	if len(record.Invocations) != 1 {
		t.Fatalf("invocations = %+v", record.Invocations)
	}
	invocation := record.Invocations[0]
	if invocation.SystemdInvocationID != "" || invocation.UnitName != "" {
		t.Fatalf("invocation used systemd identity: %+v", invocation)
	}
	if invocation.AgentStartID != "agent-test" || invocation.ExecutorAttemptID == "" || invocation.PID != 123 || invocation.ExitStatus != 0 {
		t.Fatalf("invocation missing agent executor metadata: %+v", invocation)
	}
	if !contains(record.MutationScopes, "kubeadm-state") || !record.MutatingToolRan {
		t.Fatalf("mutation state = scopes %v ran %v", record.MutationScopes, record.MutatingToolRan)
	}
	if got := readFirstArtifact(t, server.Store, record); strings.Contains(got, "abc.def") || !strings.Contains(got, "Bearer [REDACTED]") {
		t.Fatalf("artifact was not redacted: %q", got)
	}
	status, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: accepted.OperationId})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Invocations) != 1 || status.Invocations[0].AgentStartId != "agent-test" || status.Invocations[0].ExitStatus != 0 {
		t.Fatalf("status invocations = %+v", status.Invocations)
	}
}

func TestExecutorDispatchSurvivesClientCancellation(t *testing.T) {
	server := newTestServer(t)
	done := make(chan struct{})
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Now = server.Now
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		started(456)
		close(done)
		return ToolResult{ExitStatus: 0, PID: 456}
	}
	server.Dispatcher = executor

	ctx, cancel := context.WithCancel(context.Background())
	accepted, err := server.SubmitOperation(ctx, submitRequest("req-disconnect"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not run after client cancellation")
	}
	waitForOperation(t, server.Store, accepted.OperationId, func(record operation.OperationRecord) bool {
		return record.Terminal && record.Result == operation.ResultSucceeded
	})
}

func TestExecutorRecordsFailedChildProcess(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-fail")
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		started(789)
		return ToolResult{
			Stderr:     []byte("certificate-key=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
			ExitStatus: 42,
			PID:        789,
			Err:        errors.New("exit status 42"),
		}
	}

	err := executor.Execute(context.Background(), record)
	if err == nil {
		t.Fatal("Execute succeeded, want child process error")
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair {
		t.Fatalf("record = %+v, want terminal failed-needs-repair", read)
	}
	if strings.Contains(read.FailureReason, "0123456789abcdef") || !strings.Contains(read.FailureReason, "[REDACTED]") {
		t.Fatalf("failure reason was not redacted: %q", read.FailureReason)
	}
}

func TestExecutorMarksMissingJournalPlanTerminal(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-missing-plan")
	if _, err := server.Store.Update(record.OperationID, "clear-plan", "test-clear-plan", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExecutorPlan = nil
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now

	err = executor.Execute(context.Background(), record)
	if err == nil {
		t.Fatal("Execute succeeded, want missing plan error")
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || !read.RecoveryRequired || read.Phase != "dispatch-failed" {
		t.Fatalf("record = %+v, want terminal dispatch failure", read)
	}
}

func TestAuditStartupClassifiesInterruptedOperation(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-interrupted")
	startedAt := server.Now()
	if _, err := server.Store.Update(record.OperationID, "marker-start", "pre-exec-mutation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		record.PreExecMutationMarkers = append(record.PreExecMutationMarkers, operation.PreExecMutationMarker{
			MarkerID:   "marker-start",
			Phase:      "kubeadm-init",
			Tool:       "kubeadm",
			ArgvDigest: strings.Repeat("1", 64),
			MarkedAt:   startedAt,
		})
		record.Phase = "kubeadm-init"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := AuditStartup(server.Store, server.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != operation.StalePostMutation {
		t.Fatalf("report = %+v, want stale-post-mutation", report)
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair {
		t.Fatalf("record = %+v, want recovery-required failed-needs-repair", read)
	}
}

func TestAuditStartupFailsAcceptedButNotStartedOperation(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-not-started")

	report, err := AuditStartup(server.Store, server.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != operation.StaleAmbiguous {
		t.Fatalf("report = %+v, want stale-ambiguous before terminal not-started classification", report)
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || !read.RecoveryRequired || read.NextAction != "resubmit operation request; previous accepted attempt did not start" {
		t.Fatalf("record = %+v, want terminal not-started classification", read)
	}
}

func TestAuditStartupPreservesLiveAgentChild(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-live-child")
	startedAt := server.Now()
	if _, err := server.Store.Update(record.OperationID, "child-start", "child-process-started", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		record.PreExecMutationMarkers = append(record.PreExecMutationMarkers, operation.PreExecMutationMarker{
			MarkerID:   "marker-live",
			Phase:      "kubeadm-init",
			Tool:       "kubeadm",
			ArgvDigest: strings.Repeat("1", 64),
			MarkedAt:   startedAt,
		})
		record.Invocations = append(record.Invocations, operation.InvocationRecord{
			InvocationID:      "marker-live",
			AgentStartID:      "previous-agent",
			ExecutorAttemptID: "executor-live",
			BootID:            currentBootID(),
			PID:               os.Getpid(),
			StartedAt:         startedAt,
			Result:            "started",
		})
		record.Phase = "kubeadm-init"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := AuditStartup(server.Store, server.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != operation.StaleNotStale {
		t.Fatalf("report = %+v, want not-stale", report)
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if read.RecoveryRequired {
		t.Fatalf("record = %+v, want live child left unclassified", read)
	}
}

func createAgentOperation(t *testing.T, store operation.Store, id string) operation.OperationRecord {
	t.Helper()
	record, err := store.Create(operation.OperationRecord{
		OperationID:   id,
		OperationKind: "bootstrap-init",
		Scope:         "kubeadm-state",
		Actor:         "test",
		RequestDigest: strings.Repeat("1", 64),
		Phase:         "accepted",
		PhasePlan:     []string{"accepted", "kubeadm-init"},
		ResourceLocks: []string{"generation-state.lock", "kubeadm-state.lock"},
		ExecutorPlan: &operation.ExecutorPlan{
			Phase:          "kubeadm-init",
			MarkerID:       "kubeadm-init",
			MutationScopes: []string{"kubeadm-state", "etc-kubernetes"},
			Argv:           []string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/init.yaml"},
		},
		ClientRequestID: "client-" + id,
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func readFirstArtifact(t *testing.T, store operation.Store, record operation.OperationRecord) string {
	t.Helper()
	if len(record.DiagnosticArtifacts) == 0 {
		t.Fatal("record has no diagnostic artifacts")
	}
	path := filepath.Join(store.Root, record.OperationID, record.DiagnosticArtifacts[0].Path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func waitForOperation(t *testing.T, store operation.Store, operationID string, done func(operation.OperationRecord) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err := store.Read(operationID)
		if err == nil && done(record) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, _ := store.Read(operationID)
	t.Fatalf("operation %s did not reach expected state: %+v", operationID, record)
}
