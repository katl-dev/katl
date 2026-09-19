package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/protobuf/proto"
)

func TestBootstrapContinuation(t *testing.T) {
	for _, scenario := range []struct{ kind, failure string }{
		{"bootstrap-init", "local-api"},
		{"bootstrap-init", "post-health"},
		{"bootstrap-join-worker", "post-health"},
		{"bootstrap-init", "intervening"},
	} {
		t.Run(scenario.kind+"/"+scenario.failure, func(t *testing.T) {
			failure := scenario.failure
			server := newTestServer(t)
			role := "control-plane"
			if scenario.kind == "bootstrap-join-worker" {
				role = "worker"
			}
			seedBootstrapRuntimeRootForRole(t, server.Root, role)
			executor := NewExecutor(server.Root, server.Store, "agent-test")
			executor.Async = false
			executor.Now = server.Now
			source, ref := configureExecutorBundle(t, executor, "v1.35.0", "continuation runtime")
			server.Dispatcher = executor
			kubeadmRuns := 0
			executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
			executor.RunTool = func(_ context.Context, _ []string, started func(int)) ToolResult {
				kubeadmRuns++
				started(123)
				writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/kubelet.conf"), "original kubelet identity")
				return ToolResult{}
			}
			broken := true
			executor.ConfigureLocalAPI = func(context.Context, string, operation.BootstrapRequest, ToolRunner) error {
				if broken && (failure == "local-api" || failure == "intervening") {
					return errors.New("API configuration unavailable")
				}
				return nil
			}
			executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult {
				if broken && failure == "post-health" {
					return ToolResult{ExitStatus: 1}
				}
				return ToolResult{}
			}
			req := submitRequest("bootstrap-first")
			req.OperationKind = scenario.kind
			req.Bootstrap.SystemRole = role
			if role == "worker" {
				req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
			}
			setSubmitRequestBundle(req, source, ref)
			accepted, err := server.SubmitOperation(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			original, err := server.Store.Read(accepted.OperationId)
			if err != nil {
				t.Fatal(err)
			}
			if !bootstrapContinuationEligible(original) {
				t.Fatalf("not resumable: %+v", original)
			}

			continuation := proto.Clone(req).(*agentapi.SubmitOperationRequest)
			continuation.RequestDigest = ""
			continuation.ClientRequestId = "bootstrap-retry"
			continuation.Bootstrap.ResumeOperationId = original.OperationID
			continuation.Bootstrap.WorkerJoinMaterial = nil
			if failure == "intervening" {
				other := original
				other.OperationID = "another-mutation"
				other.ClientRequestID = "another-request"
				if _, err := server.Store.Create(other, "accepted", original.CreatedAt); err != nil {
					t.Fatal(err)
				}
				if _, err := server.SubmitOperation(context.Background(), continuation); err == nil || !strings.Contains(err.Error(), "changed node state") {
					t.Fatalf("intervening mutation accepted: %v", err)
				}
				if kubeadmRuns != 1 {
					t.Fatalf("kubeadm ran %d times", kubeadmRuns)
				}
				return
			}
			for _, scenario := range []struct {
				name   string
				change func(*operation.OperationRecord)
			}{
				{"no invocation", func(record *operation.OperationRecord) { record.Invocations = nil }},
				{"failed kubeadm", func(record *operation.OperationRecord) { record.Invocations[0].Result = "exit-1" }},
				{"previous boot", func(record *operation.OperationRecord) { record.Invocations[0].BootID = "previous-boot" }},
				{"missing candidate", func(record *operation.OperationRecord) { record.CandidateGenerationID = "missing-candidate" }},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					unproven, err := server.Store.Read(original.OperationID)
					if err != nil {
						t.Fatal(err)
					}
					scenario.change(&unproven)
					if err := server.validateBootstrapContinuation(unproven, continuation); err == nil {
						t.Fatal("accepted unsafe continuation")
					}
				})
			}
			changed := proto.Clone(continuation).(*agentapi.SubmitOperationRequest)
			changed.Bootstrap.ControlPlaneEndpoint = "another.example:6443"
			if _, err := server.SubmitOperation(context.Background(), changed); err == nil {
				t.Fatal("accepted changed bootstrap configuration")
			}
			if kubeadmRuns != 1 {
				t.Fatal("ran kubeadm while rejecting changed intent")
			}

			// A failed continuation must also remain resumable without replacing
			// the original operation or preparing a new Kubernetes identity.
			again, err := server.SubmitOperation(context.Background(), continuation)
			if err != nil {
				t.Fatal(err)
			}
			retried, err := server.Store.Read(again.OperationId)
			if err != nil || retried.Result != operation.ResultFailedNeedsRepair {
				t.Fatalf("second attempt = %+v, %v", retried, err)
			}
			broken = false
			continuation.ClientRequestId = "bootstrap-retry-2"
			continuation.RequestDigest = ""
			continuation.Bootstrap.ResumeOperationId = again.OperationId
			finished, err := server.SubmitOperation(context.Background(), continuation)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := server.Store.Read(finished.OperationId)
			if err != nil || completed.Result != operation.ResultSucceeded || completed.CandidateGenerationID != original.CandidateGenerationID {
				t.Fatalf("completion = %+v, %v", completed, err)
			}
			if kubeadmRuns != 1 {
				t.Fatalf("kubeadm ran %d times", kubeadmRuns)
			}
			unchanged, err := server.Store.Read(original.OperationID)
			if err != nil || !reflect.DeepEqual(original, unchanged) {
				t.Fatalf("original failure changed: %v", err)
			}
			identity, err := os.ReadFile(filepath.Join(server.Root, "etc/kubernetes/kubelet.conf"))
			if err != nil || string(identity) != "original kubelet identity" {
				t.Fatalf("identity changed: %q %v", identity, err)
			}
		})
	}
}
