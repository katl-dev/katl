package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/operation"
)

func TestOperationReconcileBootRebuildsSnapshotAndMarksAmbiguous(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-reconcile", "bootstrap-init", "kubeadm-state")
	if err := os.Remove(filepath.Join(root, "var/lib/katl/operations", created.OperationID, "record.json")); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	restore := setTestClock(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC))
	defer restore()
	restoreBoot := setBootID("boot-reconcile")
	defer restoreBoot()
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"operation", "reconcile", "--boot", "--root", root}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, stderr.String())
	}

	var report operation.ReconcileReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Operations) != 1 || report.Operations[0].OperationID != created.OperationID {
		t.Fatalf("report operations = %#v", report.Operations)
	}
	if report.Operations[0].StaleClass != operation.StaleAmbiguous || !report.Operations[0].RecoveryRequired {
		t.Fatalf("reconciled operation = %#v", report.Operations[0])
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair || read.Interruption != operation.StaleAmbiguous {
		t.Fatalf("reconciled record = %#v", read)
	}
	assertExists(t, filepath.Join(root, "var/lib/katl/operations", created.OperationID, "record.json"))
}

func TestOperationRunToolRecordsInvocationMarkerArtifactsAndPhase(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-tool", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 5, 0, 0, time.UTC))
	defer restoreClock()
	restoreBoot := setBootID("boot-tool")
	defer restoreBoot()
	restoreRunner := setToolRunner(func(_ []string) toolResult {
		return toolResult{
			Stdout:     []byte("created token abcdef.0123456789abcdef\n"),
			Stderr:     []byte("warning https://user:secret@example.invalid/path?token=secret\n"),
			ExitStatus: 0,
		}
	})
	defer restoreRunner()
	t.Setenv("INVOCATION_ID", "systemd-invocation-1")
	t.Setenv("KATL_OPERATION_UNIT", "katl-operation@op-tool.service")

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{
		"operation", "run-tool",
		"--root", root,
		"--operation-id", created.OperationID,
		"--phase", "kubeadm-init",
		"--marker-id", "marker-1",
		"--mutation-scope", "etc-kubernetes",
		"--mutation-scope", "cluster-objects",
		"--",
		"kubeadm", "init", "--token", "abcdef.0123456789abcdef",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=exit-0") {
		t.Fatalf("stdout = %q, want exit result", stdout.String())
	}

	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !read.ExternalMutationStarted || !read.MutatingToolRan {
		t.Fatalf("mutation flags were not recorded: %#v", read)
	}
	if len(read.PreExecMutationMarkers) != 1 {
		t.Fatalf("markers = %#v", read.PreExecMutationMarkers)
	}
	marker := read.PreExecMutationMarkers[0]
	if marker.InvocationID != "systemd-invocation-1" || marker.Tool != "kubeadm" || marker.Phase != "kubeadm-init" {
		t.Fatalf("marker = %#v", marker)
	}
	if !contains(read.MutationScopes, "etc-kubernetes") || !contains(read.MutationScopes, "cluster-objects") {
		t.Fatalf("mutation scopes = %#v", read.MutationScopes)
	}
	if len(read.Invocations) != 1 || read.Invocations[0].SystemdInvocationID != "systemd-invocation-1" || read.Invocations[0].UnitName != "katl-operation@op-tool.service" || read.Invocations[0].BootID != "boot-tool" || read.Invocations[0].Result != "exit-0" || read.Invocations[0].CompletedAt == nil {
		t.Fatalf("invocations = %#v", read.Invocations)
	}
	if read.Phase != "kubeadm-init" || !contains(read.CompletedPhases, "kubeadm-init") {
		t.Fatalf("phase state = phase %q completed %#v", read.Phase, read.CompletedPhases)
	}
	stdoutArtifact := readArtifact(t, root, created.OperationID, "marker-1-stdout")
	if strings.Contains(stdoutArtifact, "abcdef.0123456789abcdef") || !strings.Contains(stdoutArtifact, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("stdout artifact was not redacted: %q", stdoutArtifact)
	}
	stderrArtifact := readArtifact(t, root, created.OperationID, "marker-1-stderr")
	if strings.Contains(stderrArtifact, "user:secret") || strings.Contains(stderrArtifact, "token=secret") || !strings.Contains(stderrArtifact, "https://example.invalid/path") {
		t.Fatalf("stderr artifact was not redacted: %q", stderrArtifact)
	}
}

func TestOperationRunToolManualDoesNotClaimSystemdOwnership(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-tool-manual", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 7, 0, 0, time.UTC))
	defer restoreClock()
	restoreBoot := setBootID("boot-manual")
	defer restoreBoot()
	restoreRunner := setToolRunner(func(_ []string) toolResult {
		return toolResult{ExitStatus: 0}
	})
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{
		"operation", "run-tool",
		"--root", root,
		"--operation-id", created.OperationID,
		"--phase", "kubeadm-init",
		"--marker-id", "marker-manual",
		"--",
		"kubeadm", "init",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, stderr.String())
	}

	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(read.PreExecMutationMarkers) != 1 || read.PreExecMutationMarkers[0].InvocationID != "marker-manual" {
		t.Fatalf("markers = %#v", read.PreExecMutationMarkers)
	}
	if len(read.Invocations) != 1 || read.Invocations[0].SystemdInvocationID != "" || read.Invocations[0].UnitName != "" || read.Invocations[0].BootID != "" {
		t.Fatalf("manual invocation claimed systemd ownership: %#v", read.Invocations)
	}
}

func TestOperationRunToolRecordsFailure(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-tool-failed", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 10, 0, 0, time.UTC))
	defer restoreClock()
	restoreBoot := setBootID("boot-failed-tool")
	defer restoreBoot()
	restoreRunner := setToolRunner(func(_ []string) toolResult {
		return toolResult{
			Stderr:     []byte("failed token abcdef.0123456789abcdef\n"),
			ExitStatus: 2,
			Err:        errors.New("exit status 2"),
		}
	})
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{
		"operation", "run-tool",
		"--root", root,
		"--operation-id", created.OperationID,
		"--phase", "kubeadm-init",
		"--marker-id", "marker-fail",
		"--",
		"kubeadm", "init",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() error = nil, want tool failure")
	}
	read, readErr := store.Read(created.OperationID)
	if readErr != nil {
		t.Fatalf("Read() error = %v", readErr)
	}
	if !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair {
		t.Fatalf("failed record = %#v", read)
	}
	if strings.Contains(read.FailureReason, "abcdef.0123456789abcdef") {
		t.Fatalf("failure reason was not redacted: %q", read.FailureReason)
	}
}

func TestOperationRunToolTimeoutRecordsTerminalRepair(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-tool-timeout", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 12, 0, 0, time.UTC))
	defer restoreClock()
	restoreRunner := setToolRunnerContext(func(ctx context.Context, _ []string) toolResult {
		<-ctx.Done()
		return toolResult{ExitStatus: -1, Err: ctx.Err()}
	})
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{
		"operation", "run-tool",
		"--root", root,
		"--operation-id", created.OperationID,
		"--phase", "kubeadm-init",
		"--marker-id", "marker-timeout",
		"--timeout", "1ns",
		"--",
		"kubeadm", "init",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() error = nil, want timeout")
	}
	read, readErr := store.Read(created.OperationID)
	if readErr != nil {
		t.Fatalf("Read() error = %v", readErr)
	}
	if !read.Terminal || read.CompletedAt == nil || !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair || read.Interruption != operation.ResultTimedOut {
		t.Fatalf("timeout record = %#v", read)
	}
	if read.NextAction != "explicit repair required after operation timeout" {
		t.Fatalf("timeout next action = %q", read.NextAction)
	}
	if len(read.Invocations) != 1 || read.Invocations[0].Result != operation.ResultTimedOut || read.Invocations[0].CompletedAt == nil {
		t.Fatalf("timeout invocations = %#v", read.Invocations)
	}
	if !strings.Contains(stdout.String(), "result=timed-out") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestOperationRunToolRejectsDisabledTimeout(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-tool-zero-timeout", "bootstrap-init", "kubeadm-state")

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{
		"operation", "run-tool",
		"--root", root,
		"--operation-id", created.OperationID,
		"--phase", "kubeadm-init",
		"--timeout", "0",
		"--",
		"kubeadm", "init",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--timeout must be positive") {
		t.Fatalf("run() error = %v, want positive timeout", err)
	}
	read, readErr := store.Read(created.OperationID)
	if readErr != nil {
		t.Fatalf("Read() error = %v", readErr)
	}
	if len(read.Invocations) != 0 || len(read.PreExecMutationMarkers) != 0 {
		t.Fatalf("timeout validation mutated record: %#v", read)
	}
}

func TestOperationExecuteRunsToolPlan(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-execute", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 15, 0, 0, time.UTC))
	defer restoreClock()
	restoreBoot := setBootID("boot-execute")
	defer restoreBoot()
	restoreRunner := setToolRunner(func(argv []string) toolResult {
		if strings.Join(argv, " ") != "kubeadm init" {
			t.Fatalf("argv = %#v, want kubeadm init", argv)
		}
		return toolResult{Stdout: []byte("ok\n"), ExitStatus: 0}
	})
	defer restoreRunner()
	t.Setenv("INVOCATION_ID", "systemd-execute-1")
	writeToolPlan(t, root, created.OperationID, toolPlan{
		Phase:          "kubeadm-init",
		MarkerID:       "execute-marker",
		Timeout:        "2m",
		MutationScopes: []string{"etc-kubernetes"},
		Argv:           []string{"kubeadm", "init"},
	})

	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"operation", "execute", "--root", root, "--operation-id", created.OperationID}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, stderr.String())
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(read.Invocations) != 1 || read.Invocations[0].SystemdInvocationID != "systemd-execute-1" || read.Invocations[0].BootID != "boot-execute" || !read.MutatingToolRan {
		t.Fatalf("execute record = %#v", read)
	}
	if !strings.Contains(stdout.String(), "result=exit-0") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestOperationExecuteRefusesMissingToolPlan(t *testing.T) {
	root := t.TempDir()
	store := testOperationStore(t, root)
	created := createOperation(t, store, "op-execute-missing-plan", "bootstrap-init", "kubeadm-state")
	restoreClock := setTestClock(time.Date(2026, 6, 15, 13, 20, 0, 0, time.UTC))
	defer restoreClock()

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"operation", "execute", "--root", root, "--operation-id", created.OperationID}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "run-tool.json") {
		t.Fatalf("run() error = %v, want missing plan", err)
	}
	read, readErr := store.Read(created.OperationID)
	if readErr != nil {
		t.Fatalf("Read() error = %v", readErr)
	}
	if read.NextAction != "write explicit operation run-tool plan" || read.MutatingToolRan {
		t.Fatalf("refused execute record = %#v", read)
	}
}

func testOperationStore(t *testing.T, root string) operation.Store {
	t.Helper()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

func createOperation(t *testing.T, store operation.Store, id, kind, scope string) operation.OperationRecord {
	t.Helper()
	record, err := store.Create(operation.OperationRecord{
		OperationID:           id,
		OperationKind:         kind,
		Scope:                 scope,
		Actor:                 "test",
		RequestDigest:         strings.Repeat("1", 64),
		PreviousGenerationID:  "0",
		CandidateGenerationID: "1",
		Phase:                 "accepted",
		PhasePlan:             []string{"accepted", "kubeadm-init", "commit"},
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return record
}

func setTestClock(value time.Time) func() {
	previous := now
	now = func() time.Time { return value }
	return func() { now = previous }
}

func setToolRunner(fn func([]string) toolResult) func() {
	return setToolRunnerContext(func(_ context.Context, argv []string) toolResult {
		return fn(argv)
	})
}

func setToolRunnerContext(fn func(context.Context, []string) toolResult) func() {
	previous := runToolCommand
	runToolCommand = fn
	return func() { runToolCommand = previous }
}

func setBootID(value string) func() {
	previous := currentBootID
	currentBootID = func() string { return value }
	return func() { currentBootID = previous }
}

func writeToolPlan(t *testing.T, root, operationID string, plan toolPlan) {
	t.Helper()
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	path := filepath.Join(root, "var/lib/katl/operations", operationID, "run-tool.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
}

func readArtifact(t *testing.T, root, operationID, artifactID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/operations", operationID, "attachments", artifactID+".log"))
	if err != nil {
		t.Fatalf("read artifact %s: %v", artifactID, err)
	}
	return string(data)
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
