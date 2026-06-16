package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
)

func TestSubmitOperationExecutesThroughAgentExecutor(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	ready := false
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		assertBootstrapRuntimePrepared(t, server.Root, "bootstrap-init-01-candidate")
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
	if !contains(record.CompletedPhases, "prepare-bootstrap-runtime") || !contains(record.CompletedPhases, "bootstrap-runtime-ready") || !contains(record.CompletedPhases, "kubeadm-init") {
		t.Fatalf("completed phases = %v", record.CompletedPhases)
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
	assertBootstrapRuntimePrepared(t, server.Root, accepted.OperationId+"-candidate")
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
	seedBootstrapRuntimeRoot(t, server.Root)
	done := make(chan struct{})
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Now = server.Now
	executor.RunReadiness = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{ExitStatus: 0}
	}
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
	record := createAcceptedBootstrapOperation(t, server.Store, "op-ready-fail", "candidate-ready-fail", &operation.ExecutorPlan{
		Phase:          "kubeadm-init",
		MarkerID:       "kubeadm-init",
		MutationScopes: []string{"kubeadm-state", "etc-kubernetes"},
		Argv:           []string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/default/config.yaml"},
	})
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
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
	if !strings.Contains(read.FailureReason, "katl-kubeadm-ready.target") {
		t.Fatalf("failure reason = %q, want readiness target", read.FailureReason)
	}
	if got := readFirstArtifact(t, server.Store, read); !strings.Contains(got, "containerd.service failed") {
		t.Fatalf("readiness artifact = %q", got)
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
	return createAcceptedBootstrapOperation(t, store, id, "candidate-missing-plan", nil)
}

func createAcceptedBootstrapOperation(t *testing.T, store operation.Store, id string, candidate string, plan *operation.ExecutorPlan) operation.OperationRecord {
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

func seedBootstrapRuntimeRoot(t *testing.T, root string) {
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
				ID:           "katl",
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
	writeBootSelection(t, root, "0")
	sysext := []byte("kubernetes-sysext-payload")
	writeTestFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), string(sysext))
	if _, err := installer.WriteClusterIntent(installer.ClusterIntentRequest{
		TargetRoot:        root,
		Manifest:          bootstrapRuntimeManifest(),
		KubeadmConfigs:    bootstrapRuntimeKubeadmConfigs(),
		KubernetesVersion: "v1.35.0",
		KubernetesSysext: &installer.ClusterIntentKubernetesSysext{
			Path:      "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
			SHA256:    digestBytes(sysext),
			SizeBytes: uint64(len(sysext)),
		},
		GenerationID:       "0",
		RequestDigest:      strings.Repeat("c", 64),
		InstalledAt:        time.Date(2026, 6, 15, 11, 5, 0, 0, time.UTC),
		TargetDiskStableID: "/dev/disk/by-id/test-root",
	}); err != nil {
		t.Fatal(err)
	}
}

func bootstrapRuntimeManifest() manifest.Manifest {
	return manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "node-a"},
			SystemRole: "control-plane",
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

func bootstrapRuntimeKubeadmConfigs() map[string]kubeadmconfig.Plan {
	return map[string]kubeadmconfig.Plan{
		"default": {
			Name: "default",
			Config: kubeadmconfig.File{
				RenderPath: "/etc/katl/kubeadm/default/config.yaml",
				Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"),
				Mode:       0o644,
			},
			Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"}},
		},
	}
}

func assertBootstrapRuntimePrepared(t *testing.T, root string, candidate string) {
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
	assertFileContains(t, filepath.Join(root, "var/lib/katl/generations", candidate, "confext/etc/katl/kubeadm/default/config.yaml"), "InitConfiguration")
	assertFileContains(t, filepath.Join(root, "var/lib/katl/generations", candidate, "confext/etc/katl/bootstrap-runtime.json"), `"systemRole": "control-plane"`)
	assertSymlinkTarget(t, filepath.Join(root, "run/extensions/kubernetes.raw"), "/var/lib/katl/generations/"+candidate+"/sysext/kubernetes.raw")
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
