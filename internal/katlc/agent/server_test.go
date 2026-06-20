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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/configapply"
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

func TestSubmitOperationRecordsKubernetesBundleRequest(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		return nil
	})
	req := submitRequest("req-bundle")
	req.Bootstrap.KubernetesBundleSource = "https://artifacts.example.test/kubernetes"
	req.Bootstrap.KubernetesBundleRef = req.Bootstrap.KubernetesPayloadVersion + "@sha256:" + strings.Repeat("a", 64)

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.BootstrapRequest == nil || record.BootstrapRequest.KubernetesBundleSource != req.Bootstrap.KubernetesBundleSource || record.BootstrapRequest.KubernetesBundleRef != req.Bootstrap.KubernetesBundleRef {
		t.Fatalf("bootstrap request = %+v", record.BootstrapRequest)
	}
}

func TestKubernetesSysextUpdateRefusesBootstrappedNode(t *testing.T) {
	tests := []struct {
		name          string
		operationKind string
		wantEvidence  string
	}{
		{
			name:          "control plane",
			operationKind: "bootstrap-init",
			wantEvidence:  "etc-kubernetes",
		},
		{
			name:          "worker",
			operationKind: "bootstrap-join-worker",
			wantEvidence:  "kubelet-state",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t)
			writeConfigApplyBaseState(t, server.Root)
			writeKubeadmMutationEvidence(t, server, "bootstrap-evidence", tt.operationKind, tt.wantEvidence)
			var dispatched atomic.Int32
			server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
				dispatched.Add(1)
				return nil
			})

			accepted, err := server.SubmitOperation(context.Background(), kubernetesSysextUpdateRequest("req-kube-upgrade-"+tt.name, "v1.36.0", strings.Repeat("e", 64)))
			if err != nil {
				t.Fatal(err)
			}
			status := accepted.InitialStatus
			if status.Phase != kubeadmUpgradeRefusedPhase || !status.Terminal || status.Result != kubeadmUpgradeRefusedPhase {
				t.Fatalf("initial status = %+v, want terminal refused plan-only operation", status)
			}
			if status.CandidateGenerationId != "" || status.ExternalMutationStarted || status.RecoveryRequired {
				t.Fatalf("status mutated or selected candidate: %+v", status)
			}
			if !strings.Contains(status.FailureReason, "target kubeadm access mode") || !strings.Contains(status.FailureReason, "kubelet activation gate") {
				t.Fatalf("failure reason = %q, want missing upgrade gates", status.FailureReason)
			}
			if !strings.Contains(status.NextAction, "target kubeadm access mode") || !strings.Contains(status.NextAction, "kubelet activation gate") {
				t.Fatalf("next action = %q, want missing upgrade gates", status.NextAction)
			}
			if dispatched.Load() != 0 {
				t.Fatalf("dispatcher calls = %d, want none for refused Kubernetes sysext update", dispatched.Load())
			}
			record, err := server.Store.Read(accepted.OperationId)
			if err != nil {
				t.Fatal(err)
			}
			if record.KubernetesSysextUpdate == nil || record.KubernetesSysextUpdate.TargetPayloadVersion != "v1.36.0" {
				t.Fatalf("record Kubernetes sysext update = %+v", record.KubernetesSysextUpdate)
			}
			if record.CandidateGenerationID != "" || record.ExternalMutationStarted || record.MutatingToolRan || len(record.MutationScopes) != 0 {
				t.Fatalf("record mutated or selected candidate: %+v", record)
			}
		})
	}
}

func TestKubernetesSysextUpdateNoopsForCurrentSysext(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	current := currentKubernetesExtensionRef(t, server.Root)
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})

	accepted, err := server.SubmitOperation(context.Background(), kubernetesSysextUpdateRequest("req-kube-current", current.PayloadVersion, current.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	status := accepted.InitialStatus
	if status.Phase != kubeadmUpgradeNoopPhase || !status.Terminal || status.Result != operation.ResultSucceeded {
		t.Fatalf("initial status = %+v, want terminal no-op success", status)
	}
	if status.CandidateGenerationId != "" || status.ExternalMutationStarted {
		t.Fatalf("status selected candidate or mutation: %+v", status)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatcher calls = %d, want none for current Kubernetes sysext", dispatched.Load())
	}
}

func TestKubernetesSysextUpdateRejectsRawActivationPath(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		t.Fatalf("dispatcher called for invalid raw activation path")
		return nil
	})
	req := kubernetesSysextUpdateRequest("req-kube-raw", "v1.36.0", strings.Repeat("e", 64))
	req.KubernetesSysextUpdate.TargetActivationPath = "/run/extensions/kubernetes.raw"

	_, err := server.SubmitOperation(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "kubelet activation gate") {
		t.Fatalf("SubmitOperation() error = %v, want raw activation InvalidArgument", err)
	}
	ids, err := server.Store.OperationIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("operation ids = %v, want no record for invalid raw activation path", ids)
	}
}

func TestKubernetesSysextUpdateDryRunUsesRefusalPlan(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	writeKubeadmMutationEvidence(t, server, "bootstrap-evidence", "bootstrap-init", "etc-kubernetes")
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		t.Fatalf("dispatcher called for dry-run Kubernetes sysext update")
		return nil
	})
	req := kubernetesSysextUpdateRequest("req-kube-dry-run", "v1.36.0", strings.Repeat("e", 64))
	req.DryRun = true

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := accepted.InitialStatus
	if status.Phase != kubeadmUpgradeRefusedPhase || !status.Terminal || status.Result != kubeadmUpgradeRefusedPhase {
		t.Fatalf("dry-run status = %+v, want terminal refusal plan", status)
	}
	if status.CandidateGenerationId != "" || status.ExternalMutationStarted || status.RecoveryRequired {
		t.Fatalf("dry-run status selected candidate or mutation: %+v", status)
	}
	if !strings.Contains(status.FailureReason, "target kubeadm access mode") || !strings.Contains(status.FailureReason, "kubelet activation gate") {
		t.Fatalf("failure reason = %q, want missing upgrade gates", status.FailureReason)
	}
	ids, err := server.Store.OperationIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "bootstrap-evidence" {
		t.Fatalf("operation ids = %v, want only existing evidence record", ids)
	}
}

func TestKubernetesSysextUpdateRejectsInvalidDigest(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		t.Fatalf("dispatcher called for invalid Kubernetes sysext digest")
		return nil
	})
	req := kubernetesSysextUpdateRequest("req-kube-bad-digest", "v1.36.0", "BAD")

	_, err := server.SubmitOperation(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "targetSysextSHA256") {
		t.Fatalf("SubmitOperation() error = %v, want digest InvalidArgument", err)
	}
}

func TestKubernetesSysextUpdateCleanGenerationZeroUsesBootstrapPath(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})

	_, err := server.SubmitOperation(context.Background(), kubernetesSysextUpdateRequest("req-kube-clean-gen0", "v1.35.0", strings.Repeat("e", 64)))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "bootstrap operation path") {
		t.Fatalf("SubmitOperation() error = %v, want bootstrap-path FailedPrecondition", err)
	}
	accepted, err := server.SubmitOperation(context.Background(), submitRequest("req-bootstrap-clean-gen0"))
	if err != nil {
		t.Fatalf("bootstrap SubmitOperation() error = %v", err)
	}
	if accepted.InitialStatus.Phase != "accepted" || accepted.InitialStatus.Terminal {
		t.Fatalf("bootstrap status = %+v, want accepted active operation", accepted.InitialStatus)
	}
	if dispatched.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want one bootstrap dispatch", dispatched.Load())
	}
}

func TestKubernetesSysextUpdateBootstrappedGenerationZeroUsesClusterIntent(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeInstalledClusterIntent(t, server.Root, "v1.35.0", "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw")
	writeKubeadmMutationEvidence(t, server, "bootstrap-evidence", "bootstrap-init", "etc-kubernetes")
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})

	accepted, err := server.SubmitOperation(context.Background(), kubernetesSysextUpdateRequest("req-kube-gen0-intent", "v1.36.0", strings.Repeat("e", 64)))
	if err != nil {
		t.Fatal(err)
	}
	status := accepted.InitialStatus
	if status.Phase != kubeadmUpgradeRefusedPhase || !status.Terminal || status.Result != kubeadmUpgradeRefusedPhase {
		t.Fatalf("initial status = %+v, want terminal refused operation", status)
	}
	if !strings.Contains(status.FailureReason, "v1.35.0") || !strings.Contains(status.FailureReason, "target kubeadm access mode") {
		t.Fatalf("failure reason = %q, want installed intent current version and missing gates", status.FailureReason)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatcher calls = %d, want none for refused Kubernetes sysext update", dispatched.Load())
	}
}

func TestKubernetesSysextUpdateGenerationZeroIntentMatchStillRefuses(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeInstalledClusterIntent(t, server.Root, "v1.35.0", "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw")
	writeKubeadmMutationEvidence(t, server, "bootstrap-evidence", "bootstrap-init", "etc-kubernetes")
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		t.Fatalf("dispatcher called for refused Kubernetes sysext update")
		return nil
	})

	accepted, err := server.SubmitOperation(context.Background(), kubernetesSysextUpdateRequest("req-kube-gen0-intent-match", "v1.35.0", strings.Repeat("d", 64)))
	if err != nil {
		t.Fatal(err)
	}
	status := accepted.InitialStatus
	if status.Phase != kubeadmUpgradeRefusedPhase || status.Result != kubeadmUpgradeRefusedPhase {
		t.Fatalf("initial status = %+v, want refused instead of no-op", status)
	}
	if !strings.Contains(status.FailureReason, "target kubeadm access mode") {
		t.Fatalf("failure reason = %q, want missing upgrade gate", status.FailureReason)
	}
}

func TestStageGenerationCreatesOperationAndGenerationReadModel(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	writeProcCmdline(t, server.Root, "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro systemd.machine_id=0123456789abcdef0123456789abcdef katl.generation=generation-0 katl.root-slot=root-a console=ttyS0,115200n8 systemd.log_target=console loglevel=6 katl.vmtest_agent=1 katl.vmtest_debug_shell=1")
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	server.Dispatcher = executor

	accepted, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-stage-generation",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		CandidateGenerationId: "generation-1",
		ConfigYaml:            configApplyYAML("next-boot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.OperationKind != OperationKindGenerationStage || accepted.OperationId == "" {
		t.Fatalf("accepted = %+v", accepted)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.ConfigApplyPhase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("record = %+v, want successful staged config apply", record)
	}
	if record.GenerationCommitState != operation.GenerationCommitCommitted || !record.BootHealthPending || record.ActivationState != operation.ActivationStatePending {
		t.Fatalf("record lifecycle = commit %q bootPending %v activation %q, want committed pending boot", record.GenerationCommitState, record.BootHealthPending, record.ActivationState)
	}
	if record.ConfigApplyRequest == nil || record.ConfigApplyRequest.ApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("config apply request = %+v", record.ConfigApplyRequest)
	}
	gen, err := server.GetGeneration(context.Background(), &agentapi.GetGenerationRequest{
		GenerationId:       "generation-1",
		IncludeConfigApply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gen.GenerationId != "generation-1" || gen.PreviousGenerationId != "generation-0" || gen.CommitState != generation.CommitStateCommitted || gen.ConfigApply == nil || gen.ConfigApply.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("generation read model = %+v", gen)
	}
	assertConfigApplyGenerationCommitted(t, server.Root, "generation-1", accepted.OperationId)
	assertConfigApplyGenerationKernelOptions(t, server.Root, "generation-1",
		"console=ttyS0,115200n8",
		"systemd.log_target=console",
		"loglevel=6",
		"katl.vmtest_agent=1",
		"katl.vmtest_debug_shell=1",
	)
	list, err := server.ListGenerations(context.Background(), &agentapi.ListGenerationsRequest{IncludeConfigApply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Generations) != 2 || list.Generations[1].GenerationId != "generation-1" {
		t.Fatalf("generation list = %+v", list.Generations)
	}
}

func TestMergeKernelCommandLinePreservesOnlyUncontrolledCurrentOptions(t *testing.T) {
	base := []string{
		"root=PARTUUID=22222222-2222-2222-2222-222222222222",
		"katl.generation=generation-1",
		"console=ttyS0,115200n8",
	}
	current := []string{
		"root=PARTUUID=11111111-1111-1111-1111-111111111111",
		"rootfstype=squashfs",
		"ro",
		"rw",
		"systemd.machine_id=0123456789abcdef0123456789abcdef",
		"katl.generation=generation-0",
		"katl.root-slot=root-a",
		"console=ttyS0,115200n8",
		"systemd.log_target=console",
		"loglevel=6",
		"katl.vmtest_agent=1",
		"katl.vmtest_agent=1",
		"katl.vmtest_debug_shell=1",
	}
	want := []string{
		"root=PARTUUID=22222222-2222-2222-2222-222222222222",
		"katl.generation=generation-1",
		"console=ttyS0,115200n8",
		"systemd.log_target=console",
		"loglevel=6",
		"katl.vmtest_agent=1",
		"katl.vmtest_debug_shell=1",
	}
	if got := mergeKernelCommandLine(base, current); !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeKernelCommandLine() = %#v, want %#v", got, want)
	}
}

func TestValidateConfigRejectsInvalidDocumentWithoutRecord(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)

	result, err := server.ValidateConfig(context.Background(), &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-invalid-config",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		ApplyMode:             generation.ApplyModeNextBoot,
		CandidateGenerationId: "generation-1",
		ConfigYaml:            "apiVersion: katl.dev/v1alpha1\nkind: Wrong\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || !strings.Contains(result.FailureReason, "kind: must be NodeConfigurationChange") || !contains(result.Diagnostics, "invalid-envelope: kind: must be NodeConfigurationChange") {
		t.Fatalf("validation result = %+v, want rejected wrong kind", result)
	}
	if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for validation", len(entries))
	}
}

func TestValidateConfigReturnsDeterministicPlanDiagnostics(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	req := &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-plan-diagnostics",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		ApplyMode:             generation.ApplyModeLive,
		CandidateGenerationId: "generation-live-plan",
		ConfigYaml: strings.Join([]string{
			"apiVersion: katl.dev/v1alpha1",
			"kind: NodeConfigurationChange",
			"metadata:",
			"  sourceID: operator",
			"  desiredVersion: \"4\"",
			"apply:",
			"  mode: live",
			"spec:",
			"  clusterDefaults:",
			"    identity:",
			"      hostname: cp-2",
			"    networkd:",
			"      files:",
			"        - name: 20-uplink.network",
			"          content: |",
			"            [Match]",
			"            Name=ens3",
			"            [Network]",
			"            DHCP=yes",
			"",
		}, "\n"),
	}

	first, err := server.ValidateConfig(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.ValidateConfig(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantDiagnostics := []string{
		"node-identity: staged-required: domain is staged-only for normal runtime configuration apply",
		"bootstrap-node-metadata: staged-required: domain is staged-only for normal runtime configuration apply",
		"networkd: staged-required: domain is staged-only for normal runtime configuration apply",
	}
	if first.Accepted || second.Accepted {
		t.Fatalf("accepted = %v/%v, want rejected", first.Accepted, second.Accepted)
	}
	if first.RequestDigest == "" || first.RequestDigest != second.RequestDigest {
		t.Fatalf("request digests = %q/%q, want stable non-empty", first.RequestDigest, second.RequestDigest)
	}
	if !reflect.DeepEqual(first.Diagnostics, wantDiagnostics) || !reflect.DeepEqual(second.Diagnostics, wantDiagnostics) {
		t.Fatalf("diagnostics = %#v/%#v, want %#v", first.Diagnostics, second.Diagnostics, wantDiagnostics)
	}
	if first.FailureReason != second.FailureReason || !strings.Contains(first.FailureReason, "config apply live request rejected for 3 domain(s)") {
		t.Fatalf("failure reasons = %q/%q, want stable plan rejection", first.FailureReason, second.FailureReason)
	}
	if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for validation", len(entries))
	}
}

func TestValidateConfigRejectsMissingKubeadmConfigRefWithoutRecord(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)

	result, err := server.ValidateConfig(context.Background(), &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-missing-kubeadm-ref",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		ApplyMode:             generation.ApplyModeNextBoot,
		CandidateGenerationId: "generation-kubeadm-ref",
		ConfigYaml: strings.Join([]string{
			"apiVersion: katl.dev/v1alpha1",
			"kind: NodeConfigurationChange",
			"metadata:",
			"  sourceID: operator",
			"  desiredVersion: \"5\"",
			"apply:",
			"  mode: next-boot",
			"spec:",
			"  clusterDefaults:",
			"    kubernetes:",
			"      kubeadm:",
			"        configRef: missing",
			"",
		}, "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `invalid-kubeadm-ref: spec.clusterDefaults.kubernetes.kubeadm.configRef: KubeadmConfig "missing" was not resolved`
	if result.Accepted || !contains(result.Diagnostics, want) || !strings.Contains(result.FailureReason, want) {
		t.Fatalf("validation result = %+v, want missing kubeadm ref diagnostic", result)
	}
	if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for validation", len(entries))
	}
}

func TestStageGenerationRejectsInvalidConfigBeforeRecord(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		want       string
	}{
		{
			name: "invalid ssh key",
			configYAML: strings.Join([]string{
				"apiVersion: katl.dev/v1alpha1",
				"kind: NodeConfigurationChange",
				"metadata:",
				"  sourceID: operator",
				"  desiredVersion: \"6\"",
				"apply:",
				"  mode: next-boot",
				"spec:",
				"  clusterDefaults:",
				"    identity:",
				"      authorizedKeys:",
				"        - ssh-ed25519 not-a-real-public-key",
				"",
			}, "\n"),
			want: "invalid-ssh-key",
		},
		{
			name: "unknown apply child field",
			configYAML: strings.Join([]string{
				"apiVersion: katl.dev/v1alpha1",
				"kind: NodeConfigurationChange",
				"metadata:",
				"  sourceID: operator",
				"  desiredVersion: \"7\"",
				"apply:",
				"  mode: next-boot",
				"  unexpected: true",
				"spec:",
				"  clusterDefaults:",
				"    networkd:",
				"      files:",
				"        - name: 20-uplink.network",
				"          content: ok",
				"",
			}, "\n"),
			want: "field unexpected not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t)
			writeConfigApplyBaseState(t, server.Root)
			var dispatched atomic.Int32
			server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
				dispatched.Add(1)
				return nil
			})

			_, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
				ApiVersion:            APIVersion,
				Kind:                  "GenerationApplyRequest",
				ClientRequestId:       "req-invalid-config-apply-" + strings.ReplaceAll(tt.name, " ", "-"),
				Actor:                 "test-actor",
				ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
				CandidateGenerationId: "generation-invalid-config-" + strings.ReplaceAll(tt.name, " ", "-"),
				ConfigYaml:            tt.configYAML,
			})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("StageGeneration error = %v, want InvalidArgument containing %q", err, tt.want)
			}
			if dispatched.Load() != 0 {
				t.Fatalf("dispatcher calls = %d, want none", dispatched.Load())
			}
			if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Fatalf("operation store entries = %d, want no record for invalid config", len(entries))
			}
		})
	}
}

func TestValidateConfigPlansAndDigestStagesGeneration(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	server.Dispatcher = executor

	result, err := server.ValidateConfig(context.Background(), &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-plan-stage",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		ApplyMode:             generation.ApplyModeNextBoot,
		CandidateGenerationId: "generation-2",
		ConfigYaml:            configApplyYAML(generation.ApplyModeNextBoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.RequestDigest == "" || result.AcceptedApplyMode != generation.ApplyModeNextBoot || !contains(result.ChangedDomains, "networkd") {
		t.Fatalf("validation result = %+v, want accepted staged plan with networkd domain", result)
	}

	accepted, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-plan-stage",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		RequestDigest:         result.RequestDigest,
		CandidateGenerationId: "generation-2",
		ConfigYaml:            configApplyYAML(generation.ApplyModeNextBoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.RequestDigest != result.RequestDigest {
		t.Fatalf("accepted digest = %q, want validation digest %q", accepted.RequestDigest, result.RequestDigest)
	}
}

func TestValidateConfigAutoLiveDigestMatchesConcreteSubmit(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.ConfigApplyRunner = &fakeConfigApplyRunner{}
	executor.ConfigApplyActivator = &fakeConfigApplyActivator{}
	server.Dispatcher = executor

	result, err := server.ValidateConfig(context.Background(), &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-auto-live-digest",
		Actor:                 "test-actor",
		ApplyMode:             generation.ApplyModeAuto,
		CandidateGenerationId: "generation-auto-live-digest",
		ConfigYaml:            configApplyLiveYAML(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.RequestDigest == "" || result.RequestedApplyMode != generation.ApplyModeAuto || result.AcceptedApplyMode != generation.ApplyModeLive {
		t.Fatalf("validation result = %+v, want accepted auto->live with digest", result)
	}

	accepted, err := server.SubmitOperation(context.Background(), &agentapi.SubmitOperationRequest{
		ApiVersion:      APIVersion,
		Kind:            RequestKind,
		ClientRequestId: "req-auto-live-digest",
		OperationKind:   OperationKindGenerationApply,
		Actor:           "test-actor",
		RequestDigest:   result.RequestDigest,
		ConfigApply: &agentapi.ConfigApplyOperationRequest{
			CandidateGenerationId: "generation-auto-live-digest",
			ApplyMode:             generation.ApplyModeAuto,
			ConfigYaml:            configApplyLiveYAML(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.RequestDigest != result.RequestDigest {
		t.Fatalf("accepted digest = %q, want validation digest %q", accepted.RequestDigest, result.RequestDigest)
	}
}

func TestValidateConfigRejectsPlanPolicyWithoutRecord(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)

	result, err := server.ValidateConfig(context.Background(), &agentapi.ValidateConfigRequest{
		ApiVersion:            APIVersion,
		Kind:                  "ValidateConfigRequest",
		ClientRequestId:       "req-live-networkd",
		Actor:                 "test-actor",
		ExpectedMachineId:     "0123456789abcdef0123456789abcdef",
		ApplyMode:             generation.ApplyModeLive,
		CandidateGenerationId: "generation-live",
		ConfigYaml:            configApplyYAML(generation.ApplyModeLive),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || !strings.Contains(result.FailureReason, "rejected") || len(result.Diagnostics) == 0 {
		t.Fatalf("validation result = %+v, want rejected live policy plan", result)
	}
	if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for validation", len(entries))
	}
}

func TestApplyGenerationLiveRejectedRecordsPlanDiagnostics(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	server.Dispatcher = executor

	accepted, err := server.ApplyGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-live-rejected",
		Actor:                 "test-actor",
		CandidateGenerationId: "generation-live-rejected",
		ConfigYaml:            configApplyYAML(generation.ApplyModeLive),
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultFailedNeedsRepair || record.ExternalMutationStarted {
		t.Fatalf("record = %+v, want terminal failed before external mutation", record)
	}
	if !strings.Contains(record.FailureReason, "staged-only") {
		t.Fatalf("failure reason = %q, want staged-only diagnostic", record.FailureReason)
	}
	if len(record.DiagnosticArtifacts) != 1 || record.DiagnosticArtifacts[0].ArtifactID != "config-apply-plan-diagnostics" {
		t.Fatalf("diagnostic artifacts = %+v", record.DiagnosticArtifacts)
	}
	attachment, err := os.ReadFile(filepath.Join(server.Store.Root, accepted.OperationId, record.DiagnosticArtifacts[0].Path))
	if err != nil {
		t.Fatalf("read diagnostic attachment: %v", err)
	}
	if !strings.Contains(string(attachment), "staged-only") {
		t.Fatalf("diagnostic attachment = %q, want staged-only diagnostic", attachment)
	}
	status, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{
		OperationId:        accepted.OperationId,
		IncludeDiagnostics: "verbose",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Diagnostics) != 1 || status.Diagnostics[0].ArtifactId != "config-apply-plan-diagnostics" {
		t.Fatalf("operation status diagnostics = %+v", status.Diagnostics)
	}
}

func TestApplyGenerationLiveMarksMutationAndActivationState(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	runner := &fakeConfigApplyRunner{}
	activator := &fakeConfigApplyActivator{}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.ConfigApplyRunner = runner
	executor.ConfigApplyActivator = activator
	server.Dispatcher = executor

	accepted, err := server.ApplyGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-live-generation",
		Actor:                 "test-actor",
		CandidateGenerationId: "generation-live",
		ConfigYaml:            configApplyLiveYAML(),
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || !record.ExternalMutationStarted {
		t.Fatalf("record = %+v, want terminal success with mutation started", record)
	}
	if record.ActivationState != operation.ActivationStateActiveLive || record.ConfigApplyPhase != generation.ConfigApplyPhaseActive {
		t.Fatalf("activation/config phase = %q/%q, want active-live/active", record.ActivationState, record.ConfigApplyPhase)
	}
	if !contains(record.MutationScopes, "confext-activation") || !contains(record.MutationScopes, "config-domain:sysctl") {
		t.Fatalf("mutation scopes = %v, want confext activation and sysctl domain", record.MutationScopes)
	}
	if len(record.Invocations) != 1 || record.Invocations[0].CompletedAt == nil || record.Invocations[0].Result != operation.ResultSucceeded {
		t.Fatalf("invocations = %+v, want completed live config apply invocation", record.Invocations)
	}
	if activator.activated == "" || runner.calls == 0 {
		t.Fatalf("live dependencies activated=%q runner calls=%d, want both used", activator.activated, runner.calls)
	}
}

func TestSubmitOperationAutoConfigApplyRejectsOperationKindMismatch(t *testing.T) {
	for _, tt := range []struct {
		name          string
		operationKind string
		configYAML    string
		wantMode      string
	}{
		{
			name:          "stage request cannot live apply",
			operationKind: OperationKindGenerationStage,
			configYAML:    configApplyLiveYAML(),
			wantMode:      generation.ApplyModeLive,
		},
		{
			name:          "apply request cannot stage",
			operationKind: OperationKindGenerationApply,
			configYAML:    configApplyYAML(generation.ApplyModeNextBoot),
			wantMode:      generation.ApplyModeNextBoot,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t)
			writeConfigApplyBaseState(t, server.Root)
			server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
				t.Fatal("dispatcher should not run for operation kind mismatch")
				return nil
			})

			_, err := server.SubmitOperation(context.Background(), &agentapi.SubmitOperationRequest{
				ApiVersion:      APIVersion,
				Kind:            RequestKind,
				ClientRequestId: "req-auto-mismatch-" + strings.ReplaceAll(tt.name, " ", "-"),
				OperationKind:   tt.operationKind,
				Actor:           "test-actor",
				ConfigApply: &agentapi.ConfigApplyOperationRequest{
					CandidateGenerationId: "generation-auto-mismatch",
					ApplyMode:             generation.ApplyModeAuto,
					ConfigYaml:            tt.configYAML,
				},
			})
			if err == nil || !strings.Contains(err.Error(), "does not match accepted applyMode "+strconv.Quote(tt.wantMode)) {
				t.Fatalf("SubmitOperation() error = %v, want operation kind mismatch for %s", err, tt.wantMode)
			}
			if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Fatalf("operation store entries = %d, want no record for operation kind mismatch", len(entries))
			}
		})
	}
}

func TestSubmitOperationAutoConfigApplyRunsAcceptedLivePath(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	runner := &fakeConfigApplyRunner{}
	activator := &fakeConfigApplyActivator{}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.ConfigApplyRunner = runner
	executor.ConfigApplyActivator = activator
	server.Dispatcher = executor

	accepted, err := server.SubmitOperation(context.Background(), &agentapi.SubmitOperationRequest{
		ApiVersion:      APIVersion,
		Kind:            RequestKind,
		ClientRequestId: "req-auto-live-generation",
		OperationKind:   OperationKindGenerationApply,
		Actor:           "test-actor",
		ConfigApply: &agentapi.ConfigApplyOperationRequest{
			CandidateGenerationId: "generation-auto-live",
			ApplyMode:             generation.ApplyModeAuto,
			ConfigYaml:            configApplyLiveYAML(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.ConfigApplyPhase != generation.ConfigApplyPhaseActive {
		t.Fatalf("record = %+v, want successful auto->live config apply", record)
	}
	if record.ConfigApplyRequest == nil || record.ConfigApplyRequest.ApplyMode != generation.ApplyModeAuto {
		t.Fatalf("config apply request = %+v", record.ConfigApplyRequest)
	}
	status, err := generation.ReadConfigApplyStatus(filepath.Join(server.Root, "var/lib/katl/generations/generation-auto-live/config-apply-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if status.RequestedApplyMode != generation.ApplyModeAuto || status.AcceptedApplyMode != generation.ApplyModeLive {
		t.Fatalf("config apply status = %#v, want requested auto accepted live", status)
	}
	if activator.activated == "" || runner.calls == 0 {
		t.Fatalf("live dependencies activated=%q runner calls=%d, want both used", activator.activated, runner.calls)
	}
}

func TestApplyGenerationLiveLoadsInstalledKubeadmInputs(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeConfigApplyManifestWithKubeadmRef(t, server.Root, "control-plane")
	writeStoredKubeadmConfig(t, server.Root, "control-plane")
	writeInstalledClusterIntent(t, server.Root, "v1.35.0", "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw")
	runner := &fakeConfigApplyRunner{}
	activator := &fakeConfigApplyActivator{}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.ConfigApplyRunner = runner
	executor.ConfigApplyActivator = activator
	server.Dispatcher = executor

	accepted, err := server.ApplyGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-live-generation-kubeadm",
		Actor:                 "test-actor",
		CandidateGenerationId: "generation-live-kubeadm",
		ConfigYaml:            configApplyLiveYAML(),
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.ConfigApplyPhase != generation.ConfigApplyPhaseActive {
		t.Fatalf("record = %+v, want successful live config apply", record)
	}
	if runner.calls == 0 || activator.activated == "" {
		t.Fatalf("live dependencies activated=%q runner calls=%d, want both used", activator.activated, runner.calls)
	}
	nodeMetadata, err := os.ReadFile(filepath.Join(server.Root, "var/lib/katl/generations/generation-live-kubeadm/confext/etc/katl/node.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodeMetadata), `"payloadVersion": "v1.35.0"`) || !strings.Contains(string(nodeMetadata), `"activationPath": "/run/extensions/katl-kubernetes.raw"`) {
		t.Fatalf("node metadata = %s, want installed Kubernetes selection", nodeMetadata)
	}
}

func TestApplyGenerationLiveFailureRecordsRollbackState(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	var acceptedRecord operation.OperationRecord
	server.Dispatcher = dispatchFunc(func(ctx context.Context, record operation.OperationRecord) error {
		acceptedRecord = record
		return nil
	})
	accepted, err := server.ApplyGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-live-generation-fail",
		Actor:                 "test-actor",
		CandidateGenerationId: "generation-live-fail",
		ConfigYaml:            configApplyLiveYAML(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeConfigApplyRunner{exitStatus: 1, stderr: "sysctl apply failed"}
	activator := &fakeConfigApplyActivator{}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.ConfigApplyRunner = runner
	executor.ConfigApplyActivator = activator
	if err := executor.Execute(context.Background(), acceptedRecord); err == nil {
		t.Fatal("Execute() error = nil, want live action failure")
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultFailedNeedsRepair || !record.ExternalMutationStarted {
		t.Fatalf("record = %+v, want terminal failed with mutation started", record)
	}
	if record.ActivationState != operation.ActivationStateRolledBack || record.ConfigApplyPhase != generation.ConfigApplyPhaseRolledBack {
		t.Fatalf("activation/config phase = %q/%q, want rolled-back/rolled-back", record.ActivationState, record.ConfigApplyPhase)
	}
	if len(record.Invocations) != 1 || record.Invocations[0].CompletedAt == nil || record.Invocations[0].Result != operation.ResultFailedNeedsRepair {
		t.Fatalf("invocations = %+v, want completed failed live config apply invocation", record.Invocations)
	}
	if activator.rollbackTarget != "generation-0" {
		t.Fatalf("rollback target = %q, want generation-0", activator.rollbackTarget)
	}
}

func TestStageGenerationRejectsExistingCandidateBeforeRecord(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})

	_, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion:            APIVersion,
		Kind:                  "GenerationApplyRequest",
		ClientRequestId:       "req-duplicate-generation",
		Actor:                 "test-actor",
		CandidateGenerationId: "generation-0",
		ConfigYaml:            configApplyYAML(generation.ApplyModeNextBoot),
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("StageGeneration error = %v, want existing candidate FailedPrecondition", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatcher calls = %d, want none", dispatched.Load())
	}
	if entries, err := os.ReadDir(server.Store.Root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("operation store entries = %d, want no record for duplicate candidate", len(entries))
	}
}

func TestNodeStatusAdvertisesControlPlaneJoin(t *testing.T) {
	server := newTestServer(t)
	server.SupportedOperationKinds = []string{"bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker"}

	status, err := server.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker"} {
		if !contains(status.SupportedOperationKinds, want) {
			t.Fatalf("supported operation kinds = %#v, missing %s", status.SupportedOperationKinds, want)
		}
	}
}

func TestSubmitOperationAcceptsControlPlaneJoin(t *testing.T) {
	server := newTestServer(t)
	writeStoredKubeadmConfig(t, server.Root, "default")
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})
	req := submitRequest("req-control-plane-join")
	req.OperationKind = "bootstrap-join-control-plane"
	req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
	req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatched.Load())
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.OperationKind != "bootstrap-join-control-plane" || record.Scope != "kubeadm-state" {
		t.Fatalf("record = %+v", record)
	}
	if record.ExecutorPlan == nil || record.ExecutorPlan.Phase != "kubeadm-join-control-plane" {
		t.Fatalf("executor plan = %+v", record.ExecutorPlan)
	}
	if record.BootstrapRequest == nil || record.BootstrapRequest.JoinMaterialDigest == "" || record.BootstrapRequest.TemporaryJoinConfigPath == "" {
		t.Fatalf("bootstrap metadata = %+v", record.BootstrapRequest)
	}
	if _, err := os.Stat(filepath.Join(server.Root, "run/katl/bootstrap-join", accepted.OperationId, "config.yaml")); err != nil {
		t.Fatalf("temporary join config: %v", err)
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
		{name: "partial bundle ref", edit: func(req *agentapi.SubmitOperationRequest) {
			req.Bootstrap.KubernetesBundleSource = "https://artifacts.example.test/kubernetes"
		}},
		{name: "non https bundle source", edit: func(req *agentapi.SubmitOperationRequest) {
			req.Bootstrap.KubernetesBundleSource = "http://artifacts.example.test/kubernetes"
			req.Bootstrap.KubernetesBundleRef = req.Bootstrap.KubernetesPayloadVersion + "@sha256:" + strings.Repeat("a", 64)
		}},
		{name: "bundle version mismatch", edit: func(req *agentapi.SubmitOperationRequest) {
			req.Bootstrap.KubernetesBundleSource = "https://artifacts.example.test/kubernetes"
			req.Bootstrap.KubernetesBundleRef = "v1.36.1@sha256:" + strings.Repeat("a", 64)
		}},
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
		{name: "bare control-plane certificate key", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-control-plane"
			req.Bootstrap.SystemRole = "control-plane"
			req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
			req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
			req.Bootstrap.WorkerJoinMaterial.JoinArgv = append(validWorkerJoinMaterial().JoinArgv, "--control-plane", "--certificate-key")
		}},
		{name: "empty control-plane certificate key", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-control-plane"
			req.Bootstrap.SystemRole = "control-plane"
			req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
			req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
			req.Bootstrap.WorkerJoinMaterial.JoinArgv = append(validWorkerJoinMaterial().JoinArgv, "--control-plane", "--certificate-key=")
		}},
		{name: "flag control-plane certificate key", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-control-plane"
			req.Bootstrap.SystemRole = "control-plane"
			req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
			req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
			req.Bootstrap.WorkerJoinMaterial.JoinArgv = append(validWorkerJoinMaterial().JoinArgv, "--control-plane", "--certificate-key", "--skip-certificate-key-print")
		}},
		{name: "malformed control-plane certificate key", edit: func(req *agentapi.SubmitOperationRequest) {
			req.OperationKind = "bootstrap-join-control-plane"
			req.Bootstrap.SystemRole = "control-plane"
			req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
			req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
			req.Bootstrap.WorkerJoinMaterial.JoinArgv = append(validWorkerJoinMaterial().JoinArgv, "--control-plane", "--certificate-key", strings.Repeat("x", 64))
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

func TestSubmitOperationRecordsControlPlaneJoinMaterializationNextAction(t *testing.T) {
	server := newTestServer(t)
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
		t.Fatal("dispatcher called after materialization failure")
		return nil
	})
	req := submitRequest("req-control-plane-materialization-fails")
	req.OperationKind = "bootstrap-join-control-plane"
	req.Bootstrap.SystemRole = "control-plane"
	req.Bootstrap.JoinMaterialRef = "opaque-control-plane-join-ref"
	req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()

	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitOperation error = %v", err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result != operation.ResultFailedNeedsRepair || !record.RecoveryRequired || !record.Terminal {
		t.Fatalf("record = %+v, want terminal repair-required failure", record)
	}
	if !strings.Contains(record.NextAction, "control-plane join operation") {
		t.Fatalf("NextAction = %q, want control-plane join guidance", record.NextAction)
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

func validControlPlaneJoinMaterial() *agentapi.WorkerJoinMaterial {
	material := validWorkerJoinMaterial()
	material.JoinArgv = append(material.JoinArgv, "--control-plane", "--certificate-key", strings.Repeat("a", 64))
	return material
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

func kubernetesSysextUpdateRequest(clientRequestID string, payloadVersion string, sha256Hex string) *agentapi.SubmitOperationRequest {
	return &agentapi.SubmitOperationRequest{
		ApiVersion:        APIVersion,
		Kind:              RequestKind,
		ClientRequestId:   clientRequestID,
		OperationKind:     OperationKindKubeadmUpgrade,
		Actor:             "test-actor",
		ExpectedMachineId: "0123456789abcdef0123456789abcdef",
		KubernetesSysextUpdate: &agentapi.KubernetesSysextUpdateOperationRequest{
			TargetPayloadVersion: payloadVersion,
			TargetSysextPath:     "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
			TargetSysextSha256:   sha256Hex,
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

func assertConfigApplyGenerationCommitted(t *testing.T, root string, candidate string, operationID string) {
	t.Helper()
	spec, status, err := generation.ReadGeneration(root, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != generation.CommitStateCommitted || status.BootState != generation.BootStateTrying || status.CommittedAt == nil || status.CommittedByOperation != operationID {
		t.Fatalf("candidate status = %#v, want committed trying by %s", status, operationID)
	}
	if spec.Boot.UKIPath != "/efi/EFI/Linux/katl-generation-0.efi" || spec.Boot.LoaderEntryPath != "loader/entries/katl-"+candidate+".conf" {
		t.Fatalf("boot selection in spec = %#v", spec.Boot)
	}
	assertFileContains(t, filepath.Join(root, "efi", spec.Boot.LoaderEntryPath), "katl.generation="+candidate)
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "generation-0" ||
		selection.TargetBootGenerationID != candidate ||
		selection.TrialGenerationID != candidate ||
		selection.PreviousKnownGoodGenerationID != "generation-0" ||
		selection.Generation0FallbackID != "generation-0" ||
		!selection.PendingHealthValidation ||
		selection.PersistentDefaultPromotion != generation.DefaultPromotionPending ||
		selection.PendingTransactionID != operationID {
		t.Fatalf("boot selection = %#v, want config apply generation %s armed for boot health", selection, candidate)
	}
	if selection.TargetBootEntry != spec.Boot.LoaderEntryPath || selection.TrialBootEntry != spec.Boot.LoaderEntryPath || selection.DefaultBootEntry != "loader/entries/katl-generation-0.conf" {
		t.Fatalf("boot entries = %#v, want staged target and unchanged default", selection)
	}
}

func assertConfigApplyGenerationKernelOptions(t *testing.T, root string, candidate string, options ...string) {
	t.Helper()
	spec, _, err := generation.ReadGeneration(root, candidate)
	if err != nil {
		t.Fatal(err)
	}
	gotOptions := strings.Join(spec.KernelCommandLine, " ")
	entryPath := filepath.Join(root, "efi", spec.Boot.LoaderEntryPath)
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("read candidate loader entry: %v", err)
	}
	entry := string(data)
	for _, option := range options {
		if !strings.Contains(gotOptions, option) {
			t.Fatalf("candidate kernelCommandLine = %q, want %q", gotOptions, option)
		}
		if !strings.Contains(entry, option) {
			t.Fatalf("candidate loader entry:\n%s\nwant option %q", entry, option)
		}
	}
	if strings.Contains(entry, "katl.generation=generation-0") {
		t.Fatalf("candidate loader entry inherited previous generation option:\n%s", entry)
	}
}

func writeProcCmdline(t *testing.T, root string, cmdline string) {
	t.Helper()
	path := filepath.Join(root, "proc/cmdline")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cmdline+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCleanGenerationZeroState(t *testing.T, root string) {
	t.Helper()
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "generation-0",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-1111-1111-1111-111111111111",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-generation-0.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/generation-0/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility:  generation.ConfextCompatibility{ID: "katl", VersionID: "0.1.0", ConfextLevel: 1},
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
	writeBootSelection(t, root, "generation-0")
}

func writeConfigApplyBaseState(t *testing.T, root string) {
	t.Helper()
	sysext := []byte("current kubernetes sysext\n")
	sysextPath := filepath.Join(root, "var/lib/katl/generations/generation-0/sysext/kubernetes.raw")
	if err := os.MkdirAll(filepath.Dir(sysextPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sysextPath, sysext, 0o644); err != nil {
		t.Fatal(err)
	}
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "generation-0",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-1111-1111-1111-111111111111",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-generation-0.efi",
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/generation-0/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/kubernetes.raw",
			SHA256:          digestBytesForAgentTest(sysext),
			ArtifactVersion: "0.1.0",
			PayloadVersion:  "v1.35.0",
			Architecture:    "x86_64",
			Compatibility:   generation.ExtensionCompatibility{RuntimeInterfaces: []string{"katl-runtime-1"}},
		}},
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/generation-0/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility:  generation.ConfextCompatibility{ID: "katl", VersionID: "0.1.0", ConfextLevel: 1},
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
	writeBootSelection(t, root, "generation-0")
	manifestPath := filepath.Join(root, "var/lib/katl/install/manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(configApplyInstallManifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeConfigApplyManifestWithKubeadmRef(t *testing.T, root string, ref string) {
	t.Helper()
	manifestPath := filepath.Join(root, "var/lib/katl/install/manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(configApplyInstallManifestJSON, `"systemRole": "control-plane"`, `"systemRole": "control-plane",
    "kubernetes": {"kubeadm": {"configRef": "`+ref+`"}}`, 1)
	if err := os.WriteFile(manifestPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStoredKubeadmConfig(t *testing.T, root string, ref string) {
	t.Helper()
	dir, err := installer.StoredKubeadmInputDir(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeInstalledClusterIntent(t *testing.T, root string, kubernetesVersion string, sysextPath string) {
	t.Helper()
	intent := `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "ClusterIntent",
  "generationID": "generation-0",
  "systemRole": "control-plane",
  "identity": {"hostname": "node-a"},
  "inventory": {"nodeName": "node-a"},
  "kubernetes": {
    "payloadVersion": "` + kubernetesVersion + `",
    "sysextPath": "` + sysextPath + `",
    "sysextSHA256": "` + strings.Repeat("d", 64) + `"
  },
  "katlosImage": {
    "localRef": "images/katlos.raw",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 1024,
    "version": "0.1.0",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  },
  "source": {},
  "installedAt": "2026-06-15T11:00:00Z"
}
`
	writeClusterIntent(t, root, []byte(intent))
}

func currentKubernetesExtensionRef(t *testing.T, root string) generation.ExtensionRef {
	t.Helper()
	spec, _, err := generation.ReadGeneration(root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range spec.Sysexts {
		if ref.Name == "kubernetes" {
			return ref
		}
	}
	t.Fatal("current generation has no Kubernetes sysext")
	return generation.ExtensionRef{}
}

func writeKubeadmMutationEvidence(t *testing.T, server *Server, operationID string, kind string, scope string) {
	t.Helper()
	completedAt := server.Now()
	_, err := server.Store.Create(operation.OperationRecord{
		OperationID:             operationID,
		OperationKind:           kind,
		Scope:                   "kubeadm-state",
		RequestDigest:           strings.Repeat("1", 64),
		Phase:                   "complete",
		PhasePlan:               []string{"accepted", "kubeadm-mutation-started", "complete"},
		CompletedPhases:         []string{"accepted", "kubeadm-mutation-started", "complete"},
		PhaseIndex:              3,
		PreviousGenerationID:    "generation-0",
		ExternalMutationStarted: true,
		MutationScopes:          []string{scope},
		ActivationMode:          operation.ActivationModeNextBoot,
		ActivationState:         operation.ActivationStateActiveLive,
		GenerationCommitState:   operation.GenerationCommitCommitted,
		PostKubeadmHealthState:  operation.PostKubeadmHealthPassed,
		ResourceLocks:           []string{"generation-state.lock", "kubeadm-state.lock"},
		Terminal:                true,
		Result:                  operation.ResultSucceeded,
		CompletedAt:             &completedAt,
		CreatedAt:               server.Now(),
		UpdatedAt:               server.Now(),
	}, "accepted", server.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func configApplyYAML(mode string) string {
	return strings.Join([]string{
		"apiVersion: katl.dev/v1alpha1",
		"kind: NodeConfigurationChange",
		"metadata:",
		"  sourceID: operator",
		"  desiredVersion: \"2\"",
		"apply:",
		"  mode: " + mode,
		"spec:",
		"  clusterDefaults:",
		"    networkd:",
		"      files:",
		"        - name: 20-uplink.network",
		"          content: |",
		"            [Match]",
		"            Name=ens3",
		"            [Network]",
		"            DHCP=yes",
		"",
	}, "\n")
}

func configApplyLiveYAML() string {
	return strings.Join([]string{
		"apiVersion: katl.dev/v1alpha1",
		"kind: NodeConfigurationChange",
		"metadata:",
		"  sourceID: operator",
		"  desiredVersion: \"3\"",
		"apply:",
		"  mode: live",
		"spec:",
		"  clusterDefaults:",
		"    sysctl:",
		"      settings:",
		"        net.ipv4.ip_forward: \"1\"",
		"",
	}, "\n")
}

type fakeConfigApplyRunner struct {
	calls      int
	exitStatus int
	stdout     string
	stderr     string
	err        error
}

func (r *fakeConfigApplyRunner) Run(ctx context.Context, command configapply.Command) (configapply.CommandResult, error) {
	r.calls++
	if r.exitStatus == 0 && r.err == nil && r.stderr == "" {
		return configapply.CommandResult{ExitStatus: 0, Stdout: r.stdout}, nil
	}
	result := configapply.CommandResult{ExitStatus: r.exitStatus, Stdout: r.stdout, Stderr: r.stderr}
	err := r.err
	r.exitStatus = 0
	r.stderr = ""
	r.err = nil
	return result, err
}

type fakeConfigApplyActivator struct {
	activated      string
	rollbackTarget string
}

func (a *fakeConfigApplyActivator) Activate(ctx context.Context, record generation.Record) error {
	a.activated = record.GenerationID
	return nil
}

func (a *fakeConfigApplyActivator) Rollback(ctx context.Context, targetGenerationID string) error {
	a.rollbackTarget = targetGenerationID
	return nil
}

func digestBytesForAgentTest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

const configApplyInstallManifestJSON = `{
  "apiVersion": "install.katl.dev/v1alpha1",
  "kind": "InstallManifest",
  "node": {
    "identity": {
      "hostname": "node-a",
      "ssh": {
        "authorizedKeys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"]
      }
    },
    "systemRole": "control-plane"
  },
  "install": {
    "wipeTarget": true,
    "targetDisk": {"byID": "disk/by-id/test"}
  },
  "katlosImage": {
    "localRef": "images/katlos.raw",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 1024,
    "version": "0.1.0",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  }
}`

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
