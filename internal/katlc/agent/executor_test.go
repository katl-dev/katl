package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestSubmitOperationExecutesThroughAgentExecutor(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "executor init kubernetes sysext")
	var bootDefaults []string
	executor.SetBootDefault = func(ctx context.Context, root string, bootEntry string) error {
		bootDefaults = append(bootDefaults, root+" "+bootEntry)
		return nil
	}
	ready := false
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		assertBootstrapRuntimePrepared(t, server.Root, "bootstrap-init-01-candidate")
		if strings.Join(argv, " ") != "bootstrap-init-01-candidate /etc/katl/kubeadm/default/config.yaml" {
			t.Fatalf("readiness argv = %v, want candidate generation and kubeadm config path", argv)
		}
		ready = true
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		if !ready {
			t.Fatal("kubeadm ran before katl-kubeadm-ready.target gate")
		}
		if strings.Join(argv, " ") != "/usr/bin/kubeadm init --config /etc/katl/kubeadm/default/config.yaml" {
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
	executor.RunPostHealth = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{
			Stdout:     []byte("readyz ok\n"),
			ExitStatus: 0,
		}
	}
	server.Dispatcher = executor

	req := submitRequest("req-execute")
	setSubmitRequestBundle(req, source, ref)
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
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.Phase != "record-operation-complete" {
		t.Fatalf("record = %+v, want terminal success after generation commit", record)
	}
	if !contains(record.CompletedPhases, "prepare-bootstrap-runtime") || !contains(record.CompletedPhases, "bootstrap-runtime-ready") || !contains(record.CompletedPhases, "kubeadm-init") || !contains(record.CompletedPhases, "post-kubeadm-health") || !contains(record.CompletedPhases, "record-operation-complete") {
		t.Fatalf("completed phases = %v", record.CompletedPhases)
	}
	if record.GenerationCommitState != operation.GenerationCommitCommitted || record.PostKubeadmHealthState != operation.PostKubeadmHealthPassed || record.BootHealthPending || record.ActivationState != operation.ActivationStateActiveLive {
		t.Fatalf("lifecycle state = commit %q health %q pending %v", record.GenerationCommitState, record.PostKubeadmHealthState, record.BootHealthPending)
	}
	if len(bootDefaults) != 1 || bootDefaults[0] != server.Root+" loader/entries/katl-bootstrap-init-01-candidate.conf" {
		t.Fatalf("boot default calls = %v", bootDefaults)
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
	if strings.Join(invocation.ChildProcess, "\x00") != strings.Join([]string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/default/config.yaml"}, "\x00") {
		t.Fatalf("invocation child process = %#v, want kubeadm argv", invocation.ChildProcess)
	}
	for _, scope := range []string{"etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects"} {
		if !contains(record.MutationScopes, scope) {
			t.Fatalf("mutation scopes = %v, missing %s", record.MutationScopes, scope)
		}
	}
	if !record.MutatingToolRan {
		t.Fatalf("mutation state = scopes %v ran %v", record.MutationScopes, record.MutatingToolRan)
	}
	assertBootstrapGenerationActive(t, server.Root, accepted.OperationId+"-candidate", accepted.OperationId)
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

func TestNewExecutorProvidesBundleHTTPClient(t *testing.T) {
	executor := NewExecutor(t.TempDir(), operation.Store{}, "agent-test")
	if executor.BundleClient == nil {
		t.Fatal("BundleClient = nil, want default HTTP client")
	}
}

func TestSubmitOperationExecutesDestructiveReset(t *testing.T) {
	server := newTestServer(t)
	writeResetGenerationZero(t, server.Root)
	writeBootSelection(t, server.Root, "1")
	writeTestFile(t, filepath.Join(server.Root, "efi/loader/entries/katl-0.conf"), "title Katl 0")
	writeTestFile(t, filepath.Join(server.Root, "efi/loader/entries/katl-1.conf"), "title Katl 1")
	writeTestFile(t, filepath.Join(server.Root, "efi/loader/entries/rescue.conf"), "title Rescue")
	writeTestFile(t, filepath.Join(server.Root, "efi/EFI/Linux/katl-0.efi"), "uki 0")
	writeTestFile(t, filepath.Join(server.Root, "efi/EFI/Linux/katl-1.EFI"), "uki 1")
	writeTestFile(t, filepath.Join(server.Root, "efi/EFI/Linux/rescue.efi"), "rescue")
	writeTestFile(t, filepath.Join(server.Root, "efi/EFI/BOOT/BOOTX64.EFI"), "katl fallback")
	writeTestFile(t, filepath.Join(server.Root, "efi/EFI/systemd/systemd-bootx64.efi"), "systemd-boot")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/katl/generations/1/sysext/kubernetes.raw"), "kubernetes")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/katl/kubernetes/etc-kubernetes/admin.conf"), "cluster-admin")
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/manifests/kube-apiserver.yaml"), "pod")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/kubelet/config.yaml"), "kubelet")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/etcd/member/snap/db"), "etcd")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/cni/networks/pod/last_reserved_ip"), "pod-ip")
	writeTestFile(t, filepath.Join(server.Root, "var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db"), "containerd")
	if err := os.MkdirAll(filepath.Join(server.Root, "run/extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/katl/generations/1/sysext/kubernetes.raw", filepath.Join(server.Root, "run/extensions/katl-kubernetes.raw")); err != nil {
		t.Fatal(err)
	}
	_, err := server.Store.Create(operation.OperationRecord{
		OperationID:             "old-bootstrap",
		OperationKind:           "bootstrap-init",
		Scope:                   "kubeadm-state",
		RequestDigest:           strings.Repeat("1", 64),
		Phase:                   "kubeadm-init",
		ExternalMutationStarted: true,
		MutatingToolRan:         true,
		MutationScopes:          []string{"etc-kubernetes", "kubelet-state", "etcd-state"},
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	server.Dispatcher = executor

	accepted, err := server.SubmitOperation(context.Background(), destructiveResetRequest("req-reset-execute"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.Phase != operation.HostBookkeepingCompletionPhase {
		t.Fatalf("record = %+v, want terminal successful reset", record)
	}
	if !reflect.DeepEqual(record.MutationScopes, destructiveResetMutationScopes) {
		t.Fatalf("mutation scopes = %v, want %v", record.MutationScopes, destructiveResetMutationScopes)
	}
	if !record.ExternalMutationStarted || !record.MutatingToolRan {
		t.Fatalf("mutation state = started %v ran %v", record.ExternalMutationStarted, record.MutatingToolRan)
	}
	for _, path := range []string{
		"efi/loader/entries/katl-0.conf",
		"efi/loader/entries/katl-1.conf",
		"efi/EFI/Linux/katl-0.efi",
		"efi/EFI/Linux/katl-1.EFI",
		"efi/EFI/BOOT/BOOTX64.EFI",
		"efi/EFI/systemd/systemd-bootx64.efi",
	} {
		if _, err := os.Lstat(filepath.Join(server.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after destructive reset: %v", path, err)
		}
	}
	for _, path := range []string{
		"efi/loader/entries/rescue.conf",
		"efi/EFI/Linux/rescue.efi",
		"var/lib/katl/generations/1",
		"var/lib/katl/kubernetes/etc-kubernetes/admin.conf",
		"etc/kubernetes/manifests/kube-apiserver.yaml",
		"var/lib/kubelet/config.yaml",
		"var/lib/etcd/member/snap/db",
		"var/lib/cni/networks/pod/last_reserved_ip",
		"var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db",
		"var/lib/katl/operations/old-bootstrap/record.json",
		"run/extensions/katl-kubernetes.raw",
	} {
		if _, err := os.Lstat(filepath.Join(server.Root, path)); err != nil {
			t.Fatalf("%s missing after destructive reset: %v", path, err)
		}
	}
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "1" || selection.BootedGenerationID != "1" {
		t.Fatalf("boot selection = %#v, want existing generation selection preserved", selection)
	}
	machineID, err := os.ReadFile(filepath.Join(server.Root, "var/lib/katl/identity/machine-id"))
	if err != nil {
		t.Fatalf("read machine id: %v", err)
	}
	if got := strings.TrimSpace(string(machineID)); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("machine id = %q, want preserved install identity", got)
	}
	if _, err := os.Stat(filepath.Join(server.Root, "var/lib/katl/operations", accepted.OperationId, "record.json")); err != nil {
		t.Fatalf("current reset operation record missing: %v", err)
	}
}

func TestRuntimeBootRootSourcesPreferActiveVMESP(t *testing.T) {
	got := runtimeBootRootSources()
	wantPrefix := []string{
		"/dev/disk/by-label/KATLEFI",
		"/dev/disk/by-id/virtio-katl-efi",
		"/dev/disk/by-id/virtio-katl-efi-part1",
	}
	if len(got) < len(wantPrefix)+1 {
		t.Fatalf("runtime boot root sources = %#v", got)
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Fatalf("runtime boot root sources = %#v, want active EFI source %q at index %d", got, want, i)
		}
	}
	if got[len(got)-1] != "/dev/disk/by-partlabel/KATL_ESP" {
		t.Fatalf("runtime boot root fallback = %q, want installed disk ESP last", got[len(got)-1])
	}
}

func TestExecutorDispatchSurvivesClientCancellation(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	done := make(chan struct{})
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "executor cancellation kubernetes sysext")
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		started(456)
		close(done)
		return ToolResult{ExitStatus: 0, PID: 456}
	}
	executor.RunPostHealth = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
	server.Dispatcher = executor

	ctx, cancel := context.WithCancel(context.Background())
	req := submitRequest("req-disconnect")
	setSubmitRequestBundle(req, source, ref)
	accepted, err := server.SubmitOperation(ctx, req)
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
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := executor.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := executor.Dispatch(context.Background(), operation.OperationRecord{OperationID: "op-after-shutdown"}); err == nil {
		t.Fatal("executor accepted dispatch after shutdown")
	}
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

func TestExecutorRejectsMissingPlanBeforeBootstrapRuntimePrep(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	record := createBootstrapOperationWithoutPlan(t, server.Store, "op-missing-bootstrap-plan")
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		t.Fatal("readiness gate ran for missing executor plan")
		return ToolResult{}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		t.Fatal("kubeadm ran for missing executor plan")
		return ToolResult{}
	}

	err := executor.Execute(context.Background(), record)
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
	if _, _, err := generation.ReadGeneration(server.Root, "candidate-missing-plan"); err == nil {
		t.Fatal("candidate generation was prepared before executor plan validation")
	}
}

func TestExecutorStopsBeforeKubeadmWhenReadinessFails(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "readiness failure kubernetes sysext")
	record := createAcceptedBootstrapOperation(t, server.Store, "op-ready-fail", "candidate-ready-fail", source, ref, &operation.ExecutorPlan{
		Phase:          "kubeadm-init",
		MarkerID:       "kubeadm-init",
		MutationScopes: []string{"kubeadm-state", "etc-kubernetes"},
		Argv:           []string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/default/config.yaml"},
	})
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{
			Stderr:     []byte("containerd.service failed\n"),
			ExitStatus: 1,
			Err:        errors.New("exit status 1"),
		}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		t.Fatal("kubeadm ran after readiness failure")
		return ToolResult{}
	}

	err := executor.Execute(context.Background(), record)
	if err == nil {
		t.Fatal("Execute succeeded, want readiness error")
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || !read.RecoveryRequired || read.Phase != "bootstrap-runtime-ready" || read.ExternalMutationStarted || len(read.PreExecMutationMarkers) != 0 || read.MutatingToolRan {
		t.Fatalf("record = %+v, want pre-kubeadm readiness failure", read)
	}
	if !strings.Contains(read.FailureReason, "bootstrap runtime readiness gate") {
		t.Fatalf("failure reason = %q, want readiness gate", read.FailureReason)
	}
	if got := readFirstArtifact(t, server.Store, read); !strings.Contains(got, "containerd.service failed") {
		t.Fatalf("readiness artifact = %q", got)
	}
}

func TestExecutorPostKubeadmHealthFailureRequiresRepair(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "post health failure kubernetes sysext")
	record := createAcceptedBootstrapOperation(t, server.Store, "op-health-fail", "candidate-health-fail", source, ref, &operation.ExecutorPlan{
		Phase:          "kubeadm-init",
		MarkerID:       "kubeadm-init",
		MutationScopes: []string{"etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects"},
		Argv:           []string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/default/config.yaml"},
	})
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		started(321)
		return ToolResult{ExitStatus: 0, PID: 321}
	}
	executor.RunPostHealth = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{
			Stderr:     []byte("readyz failed certificate-key=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
			ExitStatus: 1,
			Err:        errors.New("exit status 1"),
		}
	}

	err := executor.Execute(context.Background(), record)
	if err == nil {
		t.Fatal("Execute succeeded, want post-kubeadm health error")
	}
	read, err := server.Store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || !read.RecoveryRequired || read.Result != operation.ResultFailedNeedsRepair || read.Phase != "post-kubeadm-health" {
		t.Fatalf("record = %+v, want terminal post-health repair state", read)
	}
	if !read.ExternalMutationStarted || !read.MutatingToolRan || len(read.PreExecMutationMarkers) != 1 {
		t.Fatalf("mutation markers = started %v ran %v markers %+v", read.ExternalMutationStarted, read.MutatingToolRan, read.PreExecMutationMarkers)
	}
	if read.GenerationCommitState != operation.GenerationCommitCandidate || read.PostKubeadmHealthState != operation.PostKubeadmHealthFailed {
		t.Fatalf("lifecycle state = commit %q health %q", read.GenerationCommitState, read.PostKubeadmHealthState)
	}
	if read.BootHealthPending {
		t.Fatalf("bootHealthPending = true after post-health failure")
	}
	if strings.Contains(read.FailureReason, "0123456789abcdef") || !strings.Contains(read.FailureReason, "[REDACTED]") {
		t.Fatalf("failure reason was not redacted: %q", read.FailureReason)
	}
	spec, status, err := generation.ReadGeneration(server.Root, "candidate-health-fail")
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != generation.CommitStateCandidate || status.BootState != generation.BootStatePending {
		t.Fatalf("candidate status = %#v", status)
	}
	if spec.Boot.LoaderEntryPath != "loader/entries/katl-candidate-health-fail.conf" {
		t.Fatalf("loader entry path = %q", spec.Boot.LoaderEntryPath)
	}
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" || selection.PendingHealthValidation {
		t.Fatalf("boot selection = %#v, want generation 0 fallback inspectable", selection)
	}
	if got := readArtifact(t, server.Store, read, "post-kubeadm-health-stderr"); strings.Contains(got, "0123456789abcdef") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("health artifact was not redacted: %q", got)
	}
	if got := readArtifact(t, server.Store, read, "post-kubeadm-health-evidence"); strings.Contains(got, "certificate-key") || !strings.Contains(got, "staticPodManifestEvidence") {
		t.Fatalf("evidence artifact = %q", got)
	}
}

func TestSubmitOperationCommitsWorkerGenerationAfterJoinHealth(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRootForRole(t, server.Root, "worker")
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "worker join kubernetes sysext")
	ready := false
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		assertBootstrapRuntimePreparedForRole(t, server.Root, "bootstrap-join-worker-01-candidate", "worker")
		ready = true
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		if !ready {
			t.Fatal("kubeadm join ran before bootstrap runtime readiness")
		}
		wantConfig := "/run/katl/bootstrap-join/bootstrap-join-worker-01/config.yaml"
		if strings.Join(argv, " ") != "/usr/bin/kubeadm join --config "+wantConfig {
			t.Fatalf("argv = %v, want worker join through operation config", argv)
		}
		configPath := filepath.Join(server.Root, strings.TrimPrefix(wantConfig, "/"))
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("temporary join config was not materialized: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("temporary join config mode = %#o, want 0600", info.Mode().Perm())
		}
		assertFileContains(t, configPath, "abcdef.0123456789abcdef")
		discoveryPath := filepath.Join(filepath.Dir(configPath), "discovery.conf")
		discoveryInfo, err := os.Stat(discoveryPath)
		if err != nil {
			t.Fatalf("temporary discovery kubeconfig was not materialized: %v", err)
		}
		if discoveryInfo.Mode().Perm() != 0o600 {
			t.Fatalf("temporary discovery kubeconfig mode = %#o, want 0600", discoveryInfo.Mode().Perm())
		}
		assertFileContains(t, configPath, "kubeConfigPath: /run/katl/bootstrap-join/bootstrap-join-worker-01/discovery.conf")
		assertFileContains(t, discoveryPath, "ephemeral discovery credentials")
		started(456)
		return ToolResult{
			Stdout:     []byte("joined node using token abcdef.0123456789abcdef\n"),
			ExitStatus: 0,
			PID:        456,
		}
	}
	executor.RunPostHealth = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		if len(argv) != 1 || argv[0] != "bootstrap-join-worker" {
			t.Fatalf("post health argv = %v, want worker join kind", argv)
		}
		return ToolResult{Stdout: []byte("worker kubelet healthy\n"), ExitStatus: 0}
	}
	server.Dispatcher = executor

	req := submitRequest("req-worker-join")
	setSubmitRequestBundle(req, source, ref)
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
	req.Bootstrap.WorkerJoinMaterial.DiscoveryKubeconfig = []byte("ephemeral discovery credentials\n")
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	read, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || read.Result != operation.ResultSucceeded || read.PostKubeadmHealthState != operation.PostKubeadmHealthPassed || read.BootHealthPending || read.ActivationState != operation.ActivationStateActiveLive {
		t.Fatalf("record = %+v, want worker join success with active healthy generation", read)
	}
	if !contains(read.CompletedPhases, "post-kubeadm-health") || read.GenerationCommitState != operation.GenerationCommitCommitted {
		t.Fatalf("completed phases = %v commit = %q", read.CompletedPhases, read.GenerationCommitState)
	}
	assertBootstrapGenerationActive(t, server.Root, accepted.OperationId+"-candidate", accepted.OperationId)
	if got := readArtifact(t, server.Store, read, "kubeadm-join-worker-stdout"); strings.Contains(got, "abcdef.0123456789abcdef") || !strings.Contains(got, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("join stdout artifact was not redacted: %q", got)
	}
	if got := readArtifact(t, server.Store, read, "post-kubeadm-health-evidence"); strings.Contains(got, "bootstrap-init-01") || !strings.Contains(got, "joinMaterialEvidence") {
		t.Fatalf("worker health evidence = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(server.Root, "run/katl/bootstrap-join/bootstrap-join-worker-01/config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("temporary join config was not deleted after kubeadm: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(server.Root, "run/katl/bootstrap-join/bootstrap-join-worker-01/discovery.conf")); !os.IsNotExist(err) {
		t.Fatalf("temporary discovery kubeconfig was not deleted after kubeadm: %v", err)
	}
}

func TestControlPlaneJoinKeepsManagedEndpointOffLocalPathUntilKubeadmCompletes(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRootForRole(t, server.Root, "control-plane")
	writeManagedEndpointTestConfig(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "control-plane join Kubernetes sysext")
	var sequence []string
	executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult {
		sequence = append(sequence, "readiness")
		return ToolResult{}
	}
	executor.RunEndpointLifecycle = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		command := strings.Join(argv, " ")
		if len(argv) > 0 && argv[0] == managedEndpointKubectl {
			command = managedEndpointKubectl + " probe-stable-endpoint"
		}
		sequence = append(sequence, command)
		if reflect.DeepEqual(argv, []string{managedEndpointIP, "-json", "route", "get", "10.0.0.11"}) {
			return ToolResult{Stdout: []byte(`[{"dst":"10.0.0.11","dev":"enp1s0"}]`)}
		}
		return ToolResult{}
	}
	executor.RunTool = func(_ context.Context, _ []string, started func(int)) ToolResult {
		wantPrefix := []string{
			"readiness",
			"systemctl stop " + endpointAdvertiserUnit,
			endpointAdvertiserCommand + " withdraw",
			managedEndpointInterface + " down katl-api0",
			managedEndpointIP + " address flush dev katl-api0 to 10.40.0.10/32",
			managedEndpointIP + " -json route get 10.0.0.11",
			managedEndpointIP + " route add 10.40.0.10/32 via 10.0.0.11 dev enp1s0",
			managedEndpointKubectl + " probe-stable-endpoint",
		}
		if !reflect.DeepEqual(sequence, wantPrefix) {
			t.Fatalf("sequence before kubeadm = %#v, want %#v", sequence, wantPrefix)
		}
		sequence = append(sequence, "kubeadm")
		started(456)
		return ToolResult{ExitStatus: 0, PID: 456}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult {
		sequence = append(sequence, "post-health")
		return ToolResult{}
	}
	server.Dispatcher = executor

	req := submitRequest("req-control-plane-managed-endpoint")
	setSubmitRequestBundle(req, source, ref)
	req.OperationKind = "bootstrap-join-control-plane"
	req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
	req.Bootstrap.WorkerJoinMaterial.DiscoveryKubeconfig = []byte("apiVersion: v1\nkind: Config\nclusters:\n  - name: katl-discovery\n    cluster:\n      server: https://10.0.0.11:6443\nusers:\n  - name: katl-bootstrap\n    user:\n      token: ephemeral\n")
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"readiness",
		"systemctl stop " + endpointAdvertiserUnit,
		endpointAdvertiserCommand + " withdraw",
		managedEndpointInterface + " down katl-api0",
		managedEndpointIP + " address flush dev katl-api0 to 10.40.0.10/32",
		managedEndpointIP + " -json route get 10.0.0.11",
		managedEndpointIP + " route add 10.40.0.10/32 via 10.0.0.11 dev enp1s0",
		managedEndpointKubectl + " probe-stable-endpoint",
		"kubeadm",
		managedEndpointIP + " route del 10.40.0.10/32 via 10.0.0.11 dev enp1s0",
		managedEndpointInterface + " up katl-api0",
		managedEndpointIP + " address replace 10.40.0.10/32 dev katl-api0",
		"systemctl start " + endpointAdvertiserUnit,
		"post-health",
	}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("control-plane join sequence = %#v, want %#v", sequence, want)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded {
		t.Fatalf("record = %+v, want successful managed endpoint join", record)
	}
	for _, phase := range []string{"suspend-managed-endpoint", "kubeadm-join-control-plane", "restore-managed-endpoint", "post-kubeadm-health"} {
		if !contains(record.CompletedPhases, phase) {
			t.Fatalf("completed phases = %v, missing %s", record.CompletedPhases, phase)
		}
	}
}

func TestSubmitOperationRejectsExpiredWorkerJoinMaterialBeforeMutation(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRootForRole(t, server.Root, "worker")
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "expired worker join kubernetes sysext")
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		t.Fatal("kubeadm join ran with expired material")
		return ToolResult{}
	}
	server.Dispatcher = executor

	req := submitRequest("req-worker-join-expired-after-ready")
	setSubmitRequestBundle(req, source, ref)
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
	req.Bootstrap.WorkerJoinMaterial.ExpiresAt = "2026-06-15T12:00:01Z"
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	read, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || read.Result != operation.ResultFailedNeedsRepair || read.ExternalMutationStarted || read.MutatingToolRan {
		t.Fatalf("record = %+v, want terminal pre-mutation expiry failure", read)
	}
	if read.Phase != "bootstrap-runtime-ready" || !strings.Contains(read.FailureReason, "expired") {
		t.Fatalf("phase = %q failure = %q", read.Phase, read.FailureReason)
	}
}

func TestSubmitOperationAcceptsAlreadyJoinedWorkerWhenHealthPasses(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRootForRole(t, server.Root, "worker")
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "already joined worker kubernetes sysext")
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		started(456)
		return ToolResult{
			Stderr:     []byte("this node has already joined the cluster\n"),
			ExitStatus: 1,
			Err:        errors.New("exit status 1"),
			PID:        456,
		}
	}
	var healthChecks int
	executor.RunPostHealth = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		healthChecks++
		return ToolResult{Stdout: []byte("worker kubelet healthy\n"), ExitStatus: 0}
	}
	server.Dispatcher = executor

	req := submitRequest("req-worker-already-joined")
	setSubmitRequestBundle(req, source, ref)
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	read, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Terminal || read.Result != operation.ResultSucceeded || read.RecoveryRequired {
		t.Fatalf("record = %+v, want already-joined worker accepted after health", read)
	}
	if healthChecks < 2 {
		t.Fatalf("post-health checks = %d, want already-joined probe plus final evidence check", healthChecks)
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

func createBootstrapOperationWithoutPlan(t *testing.T, store operation.Store, id string) operation.OperationRecord {
	t.Helper()
	return createAcceptedBootstrapOperation(t, store, id, "candidate-missing-plan", "", "", nil)
}

func createAcceptedBootstrapOperation(t *testing.T, store operation.Store, id string, candidate string, bundleSource string, bundleRef string, plan *operation.ExecutorPlan) operation.OperationRecord {
	t.Helper()
	record, err := store.Create(operation.OperationRecord{
		OperationID:                 id,
		OperationKind:               "bootstrap-init",
		Scope:                       "kubeadm-state",
		Actor:                       "test",
		RequestDigest:               strings.Repeat("1", 64),
		Phase:                       "accepted",
		PhasePlan:                   []string{"accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", "kubeadm-init"},
		PreviousGenerationID:        "0",
		CandidateGenerationID:       candidate,
		ExpectedCurrentGenerationID: "0",
		ResourceLocks:               []string{"generation:0", "kubeadm-state"},
		ExecutorPlan:                plan,
		BootstrapRequest: &operation.BootstrapRequest{
			InventoryNodeName:        "node-a",
			SystemRole:               "control-plane",
			KubernetesPayloadVersion: "v1.35.0",
			KubernetesBundleSource:   bundleSource,
			KubernetesBundleRef:      bundleRef,
			BootstrapProfileRef:      "default",
			CandidateGenerationID:    candidate,
		},
		ClientRequestID: "client-" + id,
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func configureExecutorBundle(t *testing.T, executor *Executor, payloadVersion string, payload string) (string, string) {
	t.Helper()
	executor.SetBootDefault = func(context.Context, string, string) error { return nil }
	fixture := writeExecutorKubernetesBundleFixture(t, payloadVersion, payload)
	server := httptest.NewTLSServer(http.FileServer(http.Dir(fixture.root)))
	t.Cleanup(server.Close)
	executor.BundleClient = server.Client()
	return server.URL, fixture.ref
}

func setSubmitRequestBundle(req *agentapi.SubmitOperationRequest, source string, ref string) {
	req.Bootstrap.KubernetesBundleSource = source
	req.Bootstrap.KubernetesBundleRef = ref
}

type executorKubernetesBundleFixture struct {
	root string
	ref  string
}

func writeExecutorKubernetesBundleFixture(t *testing.T, payloadVersion string, payload string) executorKubernetesBundleFixture {
	t.Helper()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath := filepath.Join(sourceDir, "katl-kubernetes-"+payloadVersion+".raw")
	if err := os.WriteFile(rawPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := artifact.LocalMeta{
		Name:           sysextcatalog.KubernetesName,
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         digestBytes([]byte(payload)),
		Version:        payloadVersion + "-build.1",
		PayloadVersion: payloadVersion,
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.35/rpm/",
			Minor:   "v1.35",
		},
		PackageVersions: map[string]string{
			"cri-tools": "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubeadm":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubectl":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubelet":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:    "katl-runtime-1",
			ArtifactPath: filepath.Join(sourceDir, "katl-runtime-root.squashfs"),
		},
		Created: "2026-06-15T12:00:00Z",
	}
	metadataPath := rawPath + ".json"
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := sysextcatalog.StageKubernetesSysext(sysextcatalog.StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}
	return executorKubernetesBundleFixture{
		root: outputDir,
		ref:  payloadVersion + "@" + staged.BundleManifestDigest,
	}
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

func readArtifact(t *testing.T, store operation.Store, record operation.OperationRecord, artifactID string) string {
	t.Helper()
	for _, artifact := range record.DiagnosticArtifacts {
		if artifact.ArtifactID != artifactID {
			continue
		}
		data, err := os.ReadFile(filepath.Join(store.Root, record.OperationID, artifact.Path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	t.Fatalf("record has no artifact %s: %+v", artifactID, record.DiagnosticArtifacts)
	return ""
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

func seedBootstrapRuntimeRoot(t *testing.T, root string) {
	t.Helper()
	seedBootstrapRuntimeRootForRole(t, root, "control-plane")
}

func writeResetGenerationZero(t *testing.T, root string) {
	t.Helper()
	spec := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           "GenerationSpec",
		GenerationID:   "0",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl-0.efi",
			LoaderEntryPath: "loader/entries/katl-0.conf",
		},
		KernelCommandLine: []string{"katl.generation=0"},
		CreatedAt:         time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC),
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
}

func seedBootstrapRuntimeRootForRole(t *testing.T, root string, role string) {
	t.Helper()
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "0",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-0.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/0/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katlos",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := generation.SpecFromRecord(record)
	digest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, generation.StatusFromRecord(record, digest)); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "var/lib/katl/generations/0/confext/etc/systemd/network/80-test-dhcp.network"), "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n")
	writeTestFile(t, filepath.Join(root, "var/lib/katl/generations/0/confext/etc/extension-release.d/extension-release.katl-node"), "ID=katlos\n")
	writeTestFile(t, filepath.Join(root, "var/lib/katl/identity/machine-id"), "0123456789abcdef0123456789abcdef\n")
	writeBootSelection(t, root, "0")
	if _, err := installer.WriteClusterIntent(installer.ClusterIntentRequest{
		TargetRoot:         root,
		Manifest:           bootstrapRuntimeManifest(role),
		KubeadmConfigs:     bootstrapRuntimeKubeadmConfigs(role),
		KubernetesVersion:  "v1.35.0",
		GenerationID:       "0",
		RequestDigest:      strings.Repeat("c", 64),
		InstalledAt:        time.Date(2026, 6, 15, 11, 5, 0, 0, time.UTC),
		TargetDiskStableID: "/dev/disk/by-id/test-root",
	}); err != nil {
		t.Fatal(err)
	}
}

func bootstrapRuntimeManifest(role string) manifest.Manifest {
	return manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "node-a"},
			SystemRole: role,
			Kubernetes: manifest.KubernetesConfig{
				Kubeadm: manifest.KubeadmReference{ConfigRef: "default"},
			},
			Bootstrap: &manifest.BootstrapIntent{
				ClusterName:          "lab",
				InventoryNodeName:    "node-a",
				ControlPlaneEndpoint: "node-a.example.test:6443",
				BootstrapProfileRef:  "default",
				ProfileResolvedID:    "kubeadm:default",
			},
		},
		KatlosImage: manifest.KatlosImage{
			SHA256:           strings.Repeat("d", 64),
			Version:          "0.1.0",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
			Role:             "install",
		},
	}
}

func bootstrapRuntimeKubeadmConfigs(role string) map[string]kubeadmconfig.Plan {
	kind := "InitConfiguration"
	content := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"
	documents := []kubeadmconfig.Document{
		{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"},
		{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "ClusterConfiguration"},
	}
	if role == "worker" {
		kind = "JoinConfiguration"
		content = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"
		documents = []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: kind}}
	}
	return map[string]kubeadmconfig.Plan{
		"default": {
			Name: "default",
			Config: kubeadmconfig.File{
				RenderPath: "/etc/katl/kubeadm/default/config.yaml",
				Content:    []byte(content),
				Mode:       0o644,
			},
			Documents: documents,
		},
	}
}

func assertBootstrapRuntimePrepared(t *testing.T, root string, candidate string) {
	t.Helper()
	assertBootstrapRuntimePreparedForRole(t, root, candidate, "control-plane")
}

func assertBootstrapRuntimePreparedForRole(t *testing.T, root string, candidate string, role string) {
	t.Helper()
	spec, status, err := generation.ReadGeneration(root, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != generation.CommitStateCandidate || status.BootState != generation.BootStatePending {
		t.Fatalf("candidate status = %#v", status)
	}
	if len(spec.Sysexts) != 1 || spec.Sysexts[0].PayloadVersion != "v1.35.0" {
		t.Fatalf("candidate sysexts = %#v", spec.Sysexts)
	}
	kind := "InitConfiguration"
	if role == "worker" {
		kind = "JoinConfiguration"
	}
	assertFileContains(t, filepath.Join(root, "var/lib/katl/generations", candidate, "confext/etc/katl/kubeadm/default/config.yaml"), kind)
	assertFileContains(t, filepath.Join(root, "var/lib/katl/generations", candidate, "confext/etc/katl/bootstrap-runtime.json"), `"systemRole": "`+role+`"`)
	assertFileContains(t, filepath.Join(root, "run/systemd/system/katl-generation-activate.service.d/10-katl-live-generation.conf"), "--generation "+candidate)
	assertSymlinkTargetPrefix(t, filepath.Join(root, "run/extensions/katl-kubernetes.raw"), "/var/lib/katl/generations/"+candidate+"/sysext/katl-kubernetes-")
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" {
		t.Fatalf("boot selection changed = %#v", selection)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target")); !os.IsNotExist(err) {
		t.Fatalf("katl-kubeadm-ready.target was enabled: %v", err)
	}
}

func assertBootstrapGenerationActive(t *testing.T, root string, candidate string, operationID string) {
	t.Helper()
	spec, status, err := generation.ReadGeneration(root, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != generation.CommitStateCommitted || status.BootState != generation.BootStateGood || status.HealthState != generation.HealthStateHealthy || status.CommittedAt == nil || status.CommittedByOperation != operationID {
		t.Fatalf("candidate status = %#v, want committed healthy by %s", status, operationID)
	}
	if spec.Boot.LoaderEntryPath != "loader/entries/katl-"+candidate+".conf" {
		t.Fatalf("loader entry path = %q", spec.Boot.LoaderEntryPath)
	}
	assertFileContains(t, filepath.Join(root, "efi", spec.Boot.LoaderEntryPath), "katl.generation="+candidate)
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != candidate ||
		selection.TargetBootGenerationID != "" ||
		selection.TrialGenerationID != "" ||
		selection.PreviousKnownGoodGenerationID != "0" ||
		selection.BootedGenerationID != candidate ||
		selection.PendingHealthValidation ||
		selection.PersistentDefaultPromotion != generation.DefaultPromotionDone ||
		selection.PendingTransactionID != "" {
		t.Fatalf("boot selection = %#v, want generation %s active and persistent", selection, candidate)
	}
	if selection.DefaultBootEntry != spec.Boot.LoaderEntryPath || selection.BootedBootEntry != spec.Boot.LoaderEntryPath {
		t.Fatalf("boot entries = default %q booted %q want %q", selection.DefaultBootEntry, selection.BootedBootEntry, spec.Boot.LoaderEntryPath)
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertSymlinkTarget(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("symlink %s -> %s, want %s", path, got, want)
	}
}

func assertSymlinkTargetPrefix(t *testing.T, path string, wantPrefix string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("symlink %s -> %s, want prefix %s", path, got, wantPrefix)
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
