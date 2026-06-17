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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type dispatchFunc func(context.Context, operation.OperationRecord) error

func (f dispatchFunc) Dispatch(ctx context.Context, record operation.OperationRecord) error {
	return f(ctx, record)
}

func TestSubmitOperationCreatesRecord(t *testing.T) {
	server := newTestServer(t)
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})

	accepted, err := server.SubmitOperation(context.Background(), submitRequest("req-create"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.OperationId == "" || accepted.RequestDigest == "" {
		t.Fatalf("accepted response missing identity: %+v", accepted)
	}
	if accepted.InitialStatus.Phase != "accepted" || accepted.InitialStatus.Terminal {
		t.Fatalf("initial status = %+v, want active accepted", accepted.InitialStatus)
	}
	if dispatched.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatched.Load())
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.ClientRequestID != "req-create" || record.Actor != "test-actor" {
		t.Fatalf("record request metadata = %+v", record)
	}
	if record.BootstrapRequest == nil || record.BootstrapRequest.InventoryNodeName != "node-a" || record.BootstrapRequest.SystemRole != "control-plane" {
		t.Fatalf("bootstrap request = %+v", record.BootstrapRequest)
	}
	if record.CandidateGenerationID == "" || record.ActivationState != operation.ActivationStatePending || record.GenerationCommitState != operation.GenerationCommitCandidate || record.PostKubeadmHealthState != operation.PostKubeadmHealthNotRun || record.BootHealthPending {
		t.Fatalf("lifecycle status = candidate %q activation %q commit %q health %q pending %v", record.CandidateGenerationID, record.ActivationState, record.GenerationCommitState, record.PostKubeadmHealthState, record.BootHealthPending)
	}
	if len(record.ResourceLocks) != 2 {
		t.Fatalf("resource locks = %v, want bootstrap locks", record.ResourceLocks)
	}
}

func TestNodeStatusDoesNotAdvertiseControlPlaneJoin(t *testing.T) {
	server := newTestServer(t)
	server.SupportedOperationKinds = []string{"bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker"}

	status, err := server.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if contains(status.SupportedOperationKinds, "bootstrap-join-control-plane") {
		t.Fatalf("supported operation kinds = %#v, must not advertise control-plane join", status.SupportedOperationKinds)
	}
	if !contains(status.SupportedOperationKinds, "bootstrap-init") || !contains(status.SupportedOperationKinds, "bootstrap-join-worker") {
		t.Fatalf("supported operation kinds = %#v, want init and worker join", status.SupportedOperationKinds)
	}
}

func TestSubmitOperationRejectsControlPlaneJoinBeforeRecord(t *testing.T) {
	server := newTestServer(t)
	server.SupportedOperationKinds = []string{"bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker"}
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})
	req := submitRequest("req-control-plane-join")
	req.OperationKind = "bootstrap-join-control-plane"
	req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"

	_, err := server.SubmitOperation(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("SubmitOperation error = %v, want unsupported InvalidArgument", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatcher calls = %d, want none", dispatched.Load())
	}
	entries, err := os.ReadDir(server.Store.Root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for unsupported control-plane join", len(entries))
	}
}

func TestSubmitOperationRejectsDigestMismatch(t *testing.T) {
	server := newTestServer(t)
	req := submitRequest("req-digest")
	req.RequestDigest = strings.Repeat("1", 64)

	_, err := server.SubmitOperation(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SubmitOperation error = %v, want InvalidArgument", err)
	}
}

func TestSubmitOperationIdempotentClientRequest(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	req := submitRequest("req-idempotent")

	first, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.SubmitOperation(context.Background(), submitRequest("req-idempotent"))
	if err != nil {
		t.Fatal(err)
	}
	if first.OperationId != second.OperationId || first.RequestDigest != second.RequestDigest {
		t.Fatalf("idempotent response changed: first=%+v second=%+v", first, second)
	}

	different := submitRequest("req-idempotent")
	different.Actor = "other-actor"
	_, err = server.SubmitOperation(context.Background(), different)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("SubmitOperation with reused client request = %v, want AlreadyExists", err)
	}
}

func TestSubmitOperationRejectsConflictingLocks(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	if _, err := server.SubmitOperation(context.Background(), submitRequest("req-first")); err != nil {
		t.Fatal(err)
	}

	_, err := server.SubmitOperation(context.Background(), submitRequest("req-second"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SubmitOperation conflict = %v, want FailedPrecondition", err)
	}
}

func TestSubmitOperationRecordsWorkerJoinMaterializationFailureBeforeDispatch(t *testing.T) {
	server := newTestServer(t)
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})
	req := submitRequest("req-worker-material-failure")
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatcher calls = %d, want none for terminal materialization failure", dispatched.Load())
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultFailedNeedsRepair || !record.RecoveryRequired || record.ExternalMutationStarted {
		t.Fatalf("record = %+v, want terminal pre-mutation materialization failure", record)
	}
	if record.BootstrapRequest == nil || record.BootstrapRequest.JoinMaterialDigest == "" || record.BootstrapRequest.TemporaryJoinConfigPath == "" {
		t.Fatalf("bootstrap metadata = %+v, want non-secret join material metadata", record.BootstrapRequest)
	}
	if _, err := os.Lstat(filepath.Join(server.Root, "run/katl/bootstrap-join", accepted.OperationId, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("temporary join config exists after failed materialization: %v", err)
	}
}

func TestSubmitOperationDoesNotPersistRawWorkerJoinMaterial(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	req := submitRequest("req-worker-material-no-persist")
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	operationDir := filepath.Join(server.Store.Root, accepted.OperationId)
	err = filepath.WalkDir(operationDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "abcdef.0123456789abcdef") {
			return fmt.Errorf("%s contains raw bootstrap token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSubmitOperationSerializesConcurrentConflicts(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := server.SubmitOperation(context.Background(), submitRequest(fmt.Sprintf("req-race-%d", i)))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	var accepted, conflicted int
	for err := range errs {
		switch status.Code(err) {
		case codes.OK:
			accepted++
		case codes.FailedPrecondition:
			conflicted++
		default:
			t.Fatalf("unexpected SubmitOperation error: %v", err)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("accepted=%d conflicted=%d, want 1/1", accepted, conflicted)
	}
}

func TestSubmitOperationWithoutDispatcherRejectsBeforeRecord(t *testing.T) {
	server := newTestServer(t)

	_, err := server.SubmitOperation(context.Background(), submitRequest("req-no-dispatcher"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SubmitOperation error = %v, want FailedPrecondition", err)
	}
	ids, err := server.Store.OperationIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("operation ids = %v, want none", ids)
	}
}

func TestDryRunDoesNotRequireDispatcher(t *testing.T) {
	server := newTestServer(t)
	req := submitRequest("req-dry-run-no-dispatcher")
	req.DryRun = true

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.OperationId != "" || accepted.InitialStatus.Phase != "dry-run" {
		t.Fatalf("dry run response = %+v", accepted)
	}
	nodeStatus, err := server.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if nodeStatus.OperationLockHeld || len(nodeStatus.ActiveOperationIds) != 0 {
		t.Fatalf("node status = %+v, want no active lock after terminal dispatch failure", nodeStatus)
	}
}

func TestSubmitOperationValidatesRequestBodyAndUnsupportedExpectations(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})

	tests := []struct {
		name string
		edit func(*agentapi.SubmitOperationRequest)
	}{
		{name: "missing body", edit: func(req *agentapi.SubmitOperationRequest) { req.Bootstrap = nil }},
		{name: "missing inventory node", edit: func(req *agentapi.SubmitOperationRequest) { req.Bootstrap.InventoryNodeName = "" }},
		{name: "bad role", edit: func(req *agentapi.SubmitOperationRequest) { req.Bootstrap.SystemRole = "database" }},
		{name: "worker role for init", edit: func(req *agentapi.SubmitOperationRequest) { req.Bootstrap.SystemRole = "worker" }},
		{name: "bad expected generation", edit: func(req *agentapi.SubmitOperationRequest) { req.ExpectedCurrentGenerationId = "../gen-1" }},
		{name: "bad expected cluster intent", edit: func(req *agentapi.SubmitOperationRequest) { req.ExpectedClusterIntentDigest = "not-a-digest" }},
		{name: "bad timeout", edit: func(req *agentapi.SubmitOperationRequest) { req.OperationTimeout = "-1s" }},
		{name: "too large timeout", edit: func(req *agentapi.SubmitOperationRequest) { req.OperationTimeout = "26m" }},
		{name: "raw worker join material", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-worker"
			req.Bootstrap.SystemRole = "worker"
			req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
			req.Bootstrap.JoinMaterialRef = "kubeadm join api.katl.test:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		}},
		{name: "missing worker join material", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-worker"
			req.Bootstrap.SystemRole = "worker"
		}},
		{name: "expired worker join material", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-worker"
			req.Bootstrap.SystemRole = "worker"
			req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
			req.Bootstrap.WorkerJoinMaterial.ExpiresAt = "2026-06-15T11:59:59Z"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := submitRequest("req-" + strings.ReplaceAll(tt.name, " ", "-"))
			tt.edit(req)
			_, err := server.SubmitOperation(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("SubmitOperation error = %v, want InvalidArgument", err)
			}
		})
	}
}

func validWorkerJoinMaterial() *agentapi.WorkerJoinMaterial {
	return &agentapi.WorkerJoinMaterial{
		JoinArgv: []string{
			"kubeadm",
			"join",
			"api.katl.test:6443",
			"--token",
			"abcdef.0123456789abcdef",
			"--discovery-token-ca-cert-hash",
			"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		ExpiresAt: "2026-06-15T13:00:00Z",
	}
}

func TestCreateWorkerJoinMaterialRunsKubeadmTokenCreate(t *testing.T) {
	server := newTestServer(t)
	var calls [][]string
	server.RunJoinMaterial = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		return ToolResult{
			Stdout: []byte("kubeadm join api.katl.test:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
		}
	}

	response, err := server.CreateWorkerJoinMaterial(context.Background(), createWorkerJoinMaterialRequest())
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"/usr/bin/kubeadm", "token", "create", "--print-join-command", "--ttl", "30m0s", "--kubeconfig", "/etc/kubernetes/admin.conf"}
	if !reflect.DeepEqual(calls, [][]string{wantArgv}) {
		t.Fatalf("RunJoinMaterial calls = %#v, want %#v", calls, [][]string{wantArgv})
	}
	if response.MaterialRef != "operation:bootstrap-init-1/worker:worker-1" || response.CreatedAt != "2026-06-15T12:00:00Z" {
		t.Fatalf("response metadata = %+v", response)
	}
	material := response.GetWorkerJoinMaterial()
	if material.GetExpiresAt() != "2026-06-15T12:30:00Z" {
		t.Fatalf("expiresAt = %q, want default ttl expiry", material.GetExpiresAt())
	}
	if !reflect.DeepEqual(material.GetJoinArgv(), []string{
		"kubeadm",
		"join",
		"api.katl.test:6443",
		"--token",
		"abcdef.0123456789abcdef",
		"--discovery-token-ca-cert-hash",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}) {
		t.Fatalf("join argv = %#v", material.GetJoinArgv())
	}
}

func TestCreateWorkerJoinMaterialRejectsActiveOperationLock(t *testing.T) {
	server := newTestServer(t)
	createAgentOperation(t, server.Store, "bootstrap-init-active")
	server.RunJoinMaterial = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		t.Fatal("RunJoinMaterial called despite active operation lock")
		return ToolResult{}
	}

	_, err := server.CreateWorkerJoinMaterial(context.Background(), createWorkerJoinMaterialRequest())
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "active operation bootstrap-init-active") {
		t.Fatalf("CreateWorkerJoinMaterial error = %v, want active lock failed precondition", err)
	}
}

func TestCreateWorkerJoinMaterialSerializesWithSubmitOperation(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	started := make(chan struct{})
	release := make(chan struct{})
	server.RunJoinMaterial = func(ctx context.Context, argv []string, startedPID func(int)) ToolResult {
		close(started)
		<-release
		return ToolResult{
			Stdout: []byte("kubeadm join api.katl.test:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
		}
	}

	createDone := make(chan error, 1)
	go func() {
		_, err := server.CreateWorkerJoinMaterial(context.Background(), createWorkerJoinMaterialRequest())
		createDone <- err
	}()
	select {
	case <-started:
	case err := <-createDone:
		t.Fatalf("CreateWorkerJoinMaterial returned before runner started: %v", err)
	case <-time.After(time.Second):
		t.Fatal("CreateWorkerJoinMaterial did not start runner")
	}

	submitDone := make(chan error, 1)
	go func() {
		_, err := server.SubmitOperation(context.Background(), submitRequest("req-submit-during-material"))
		submitDone <- err
	}()
	select {
	case err := <-submitDone:
		t.Fatalf("SubmitOperation completed while worker join material was minting: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-createDone; err != nil {
		t.Fatalf("CreateWorkerJoinMaterial error = %v", err)
	}
	if err := <-submitDone; err != nil {
		t.Fatalf("SubmitOperation error after material minting finished = %v", err)
	}
}

func TestCreateWorkerJoinMaterialRedactsKubeadmFailure(t *testing.T) {
	server := newTestServer(t)
	secret := "abcdef.0123456789abcdef"
	server.RunJoinMaterial = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		return ToolResult{
			Stderr:     []byte("failed to create token " + secret),
			ExitStatus: 1,
		}
	}

	_, err := server.CreateWorkerJoinMaterial(context.Background(), createWorkerJoinMaterialRequest())
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("CreateWorkerJoinMaterial error = %v, want redacted failed precondition", err)
	}
}

func createWorkerJoinMaterialRequest() *agentapi.CreateWorkerJoinMaterialRequest {
	return &agentapi.CreateWorkerJoinMaterialRequest{
		ApiVersion:        APIVersion,
		Kind:              WorkerJoinMaterialRequestKind,
		Actor:             "test-actor",
		ExpectedMachineId: "0123456789abcdef0123456789abcdef",
		RequestRef:        "operation:bootstrap-init-1/worker:worker-1",
	}
}

func TestSubmitOperationAcceptsExpectedGenerationAndIntentConstraints(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	writeBootSelection(t, server.Root, "generation-0")
	intentDigest := writeClusterIntent(t, server.Root, []byte("{\"profile\":\"default\",\"role\":\"control-plane\"}\n"))
	req := submitRequest("req-constraints")
	req.ExpectedCurrentGenerationId = "generation-0"
	req.ExpectedClusterIntentDigest = intentDigest
	req.Bootstrap.CandidateGenerationId = "generation-1"

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpectedCurrentGenerationID != "generation-0" || record.ExpectedClusterIntentDigest != req.ExpectedClusterIntentDigest || record.CandidateGenerationID != "generation-1" {
		t.Fatalf("record constraints = %+v", record)
	}

	staleGeneration := submitRequest("req-stale-generation")
	staleGeneration.ExpectedCurrentGenerationId = "generation-stale"
	_, err = server.SubmitOperation(context.Background(), staleGeneration)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale expected generation error = %v, want FailedPrecondition", err)
	}

	staleIntent := submitRequest("req-stale-intent")
	staleIntent.ExpectedClusterIntentDigest = "sha256:" + strings.Repeat("0", 64)
	_, err = server.SubmitOperation(context.Background(), staleIntent)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale expected cluster intent error = %v, want FailedPrecondition", err)
	}
}

func TestSubmitOperationDispatchFailureIsRedactedAndTerminal(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return errors.New("dispatch failed")
	})

	accepted, err := server.SubmitOperation(context.Background(), submitRequest("req-dispatch-fail"))
	if err != nil {
		t.Fatal(err)
	}
	status := accepted.InitialStatus
	if !status.Terminal || status.Phase != "dispatch-failed" || !status.RecoveryRequired {
		t.Fatalf("status = %+v, want terminal recovery-required failure", status)
	}
	if status.FailureReason != "dispatch failed" {
		t.Fatalf("failure reason = %q, want dispatcher error", status.FailureReason)
	}
}

func TestDryRunDoesNotCreateRecord(t *testing.T) {
	server := newTestServer(t)
	req := submitRequest("req-dry-run")
	req.DryRun = true

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.OperationId != "" || accepted.InitialStatus.Phase != "dry-run" {
		t.Fatalf("dry run response = %+v", accepted)
	}
	ids, err := server.Store.OperationIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("operation ids = %v, want none", ids)
	}
}

func TestGetOperationChecksDigest(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	accepted, err := server.SubmitOperation(context.Background(), submitRequest("req-get"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = server.GetOperation(context.Background(), &agentapi.GetOperationRequest{
		OperationId:           accepted.OperationId,
		ExpectedRequestDigest: strings.Repeat("2", 64),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("GetOperation error = %v, want FailedPrecondition", err)
	}
	got, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{
		OperationId:           accepted.OperationId,
		ExpectedRequestDigest: accepted.RequestDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationId != accepted.OperationId {
		t.Fatalf("operation id = %q, want %q", got.OperationId, accepted.OperationId)
	}
}

func TestGetOperationHonorsDiagnosticsMode(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-diagnostics")
	if _, err := server.Store.AddDiagnosticArtifact(record.OperationID, "stderr", []byte("redacted output\n"), server.Now()); err != nil {
		t.Fatal(err)
	}

	normal, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: record.OperationID, IncludeDiagnostics: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(normal.Diagnostics) != 0 {
		t.Fatalf("normal diagnostics = %+v, want none", normal.Diagnostics)
	}
	verbose, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: record.OperationID, IncludeDiagnostics: "verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if len(verbose.Diagnostics) != 1 || !verbose.Diagnostics[0].Redacted {
		t.Fatalf("verbose diagnostics = %+v", verbose.Diagnostics)
	}
	_, err = server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: record.OperationID, IncludeDiagnostics: "everything"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GetOperation invalid diagnostics = %v, want InvalidArgument", err)
	}
}

func TestGetOperationCanReturnBootstrapKubeconfigOutput(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-kubeconfig")
	completedAt := server.Now()
	record, err := server.Store.Update(record.OperationID, "complete", "terminal", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.CompletedAt = &completedAt
		return record, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ca-data
  name: kubernetes
users:
- name: kubernetes-admin
  user:
    client-certificate-data: cert-data
    client-key-data: key-data
`
	if err := os.MkdirAll(filepath.Join(server.Root, "etc/kubernetes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server.Root, "etc/kubernetes/admin.conf"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	normal, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: record.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	if normal.AdminKubeconfig != "" {
		t.Fatalf("normal admin kubeconfig = %q, want empty", normal.AdminKubeconfig)
	}
	output, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{
		OperationId:        record.OperationID,
		IncludeDiagnostics: "bootstrap-output",
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.AdminKubeconfig != kubeconfig {
		t.Fatalf("admin kubeconfig = %q, want fixture", output.AdminKubeconfig)
	}
}

func TestWatchOperationWaitsForJournalUpdate(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-watch")
	stream := newWatchStream(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.WatchOperation(&agentapi.WatchOperationRequest{
			OperationId:     record.OperationID,
			AfterJournalSeq: int32(record.LatestJournalSeq),
			WatchTimeout:    "2s",
		}, stream)
	}()

	time.Sleep(50 * time.Millisecond)
	_, err := server.Store.Update(record.OperationID, "advance", "phase", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		completedAt := server.Now()
		record.Phase = "post-kubeadm-health"
		record.PostKubeadmHealthState = operation.PostKubeadmHealthRunning
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.CompletedAt = &completedAt
		return record, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchOperation did not return after journal update")
	}
	if len(stream.events) != 1 || stream.events[0].JournalSeq <= int32(record.LatestJournalSeq) || stream.events[0].Status.PostKubeadmHealthState != operation.PostKubeadmHealthRunning {
		t.Fatalf("events = %+v", stream.events)
	}
	if stream.events[0].EventType != "phase" {
		t.Fatalf("event type = %q, want journal event type", stream.events[0].EventType)
	}
}

func TestWatchOperationTimesOutWithoutNewEvent(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-watch-timeout")
	stream := newWatchStream(context.Background())

	err := server.WatchOperation(&agentapi.WatchOperationRequest{
		OperationId:     record.OperationID,
		AfterJournalSeq: int32(record.LatestJournalSeq),
		WatchTimeout:    "25ms",
	}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream.events) != 0 {
		t.Fatalf("events = %+v, want none", stream.events)
	}
}

func TestWatchOperationHonorsDiagnosticsModeAndTerminalAtCurrentSeq(t *testing.T) {
	server := newTestServer(t)
	record := createAgentOperation(t, server.Store, "op-watch-diagnostics")
	if _, err := server.Store.AddDiagnosticArtifact(record.OperationID, "stderr", []byte("redacted output\n"), server.Now()); err != nil {
		t.Fatal(err)
	}
	completed, err := server.Store.Update(record.OperationID, "complete", "terminal", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		completedAt := server.Now()
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.CompletedAt = &completedAt
		return record, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	normal := newWatchStream(context.Background())
	if err := server.WatchOperation(&agentapi.WatchOperationRequest{
		OperationId:     record.OperationID,
		AfterJournalSeq: int32(record.LatestJournalSeq),
		WatchTimeout:    "1s",
	}, normal); err != nil {
		t.Fatal(err)
	}
	if len(normal.events) != 2 || hasDiagnostics(normal.events) {
		t.Fatalf("normal events = %+v", normal.events)
	}

	verbose := newWatchStream(context.Background())
	if err := server.WatchOperation(&agentapi.WatchOperationRequest{
		OperationId:        record.OperationID,
		AfterJournalSeq:    int32(record.LatestJournalSeq),
		WatchTimeout:       "1s",
		IncludeDiagnostics: "verbose",
	}, verbose); err != nil {
		t.Fatal(err)
	}
	if len(verbose.events) != 2 || !hasDiagnostics(verbose.events) {
		t.Fatalf("verbose events = %+v", verbose.events)
	}

	current := newWatchStream(context.Background())
	start := time.Now()
	if err := server.WatchOperation(&agentapi.WatchOperationRequest{
		OperationId:     record.OperationID,
		AfterJournalSeq: int32(completed.LatestJournalSeq),
	}, current); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("terminal current watch took %s, want immediate return", elapsed)
	}
	if len(current.events) != 0 {
		t.Fatalf("current events = %+v, want none", current.events)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib/katl/identity"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var/lib/katl/identity/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(root, store)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var seq atomic.Int64
	server.Now = func() time.Time {
		return now.Add(time.Duration(seq.Load()) * time.Second)
	}
	server.OperationID = func(kind string, t time.Time) (string, error) {
		next := seq.Add(1)
		return fmt.Sprintf("%s-%02d", kind, next), nil
	}
	return server
}

func submitRequest(clientRequestID string) *agentapi.SubmitOperationRequest {
	return &agentapi.SubmitOperationRequest{
		ApiVersion:        APIVersion,
		Kind:              RequestKind,
		ClientRequestId:   clientRequestID,
		OperationKind:     "bootstrap-init",
		Actor:             "test-actor",
		ExpectedMachineId: "0123456789abcdef0123456789abcdef",
		Bootstrap: &agentapi.BootstrapOperationRequest{
			InventoryNodeName:        "node-a",
			SystemRole:               "control-plane",
			KubernetesPayloadVersion: "v1.35.0",
			BootstrapProfileRef:      "default",
			ControlPlaneEndpoint:     "node-a.example.test:6443",
		},
	}
}

func writeBootSelection(t *testing.T, root string, generationID string) {
	t.Helper()
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:            generation.APIVersion,
		Kind:                  generation.BootSelectionKind,
		DefaultGenerationID:   generationID,
		BootedGenerationID:    generationID,
		Generation0FallbackID: generationID,
		DefaultBootEntry:      "loader/entries/katl-" + generationID + ".conf",
		BootedBootEntry:       "loader/entries/katl-" + generationID + ".conf",
		UpdatedAt:             time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

func writeClusterIntent(t *testing.T, root string, content []byte) string {
	t.Helper()
	dir := filepath.Join(root, "var/lib/katl/cluster")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hasDiagnostics(events []*agentapi.OperationEvent) bool {
	for _, event := range events {
		if len(event.Diagnostics) > 0 {
			return true
		}
	}
	return false
}

type watchStream struct {
	agentapi.KatlcAgent_WatchOperationServer
	ctx    context.Context
	events []*agentapi.OperationEvent
}

func newWatchStream(ctx context.Context) *watchStream {
	return &watchStream{ctx: ctx}
}

func (s *watchStream) Send(event *agentapi.OperationEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *watchStream) Context() context.Context {
	return s.ctx
}

func (s *watchStream) SetHeader(metadata.MD) error {
	return nil
}

func (s *watchStream) SendHeader(metadata.MD) error {
	return nil
}

func (s *watchStream) SetTrailer(metadata.MD) {}

func (s *watchStream) SendMsg(any) error {
	return nil
}

func (s *watchStream) RecvMsg(any) error {
	return nil
}
