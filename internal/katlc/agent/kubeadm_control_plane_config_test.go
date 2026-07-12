package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestValidateKubeadmControlPlaneConfigRequest(t *testing.T) {
	req := validControlPlaneConfigRequest()
	if err := validateKubeadmControlPlaneConfigRequest(OperationKindKubeadmControlPlaneConfig, req); err != nil {
		t.Fatalf("validate request: %v", err)
	}
	req.SupportedFieldDelta = append(req.SupportedFieldDelta, "ClusterConfiguration.networking.podSubnet")
	if err := validateKubeadmControlPlaneConfigRequest(OperationKindKubeadmControlPlaneConfig, req); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported delta error = %v", err)
	}
}

func TestAcceptKubeadmControlPlaneConfigBindsActiveGeneration(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	config := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\napiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n"
	path := filepath.Join(server.Root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{GenerationID: "generation-0", PreviousGeneration: "previous", RequestedApplyMode: generation.ApplyModeNextBoot, AcceptedApplyMode: generation.ApplyModeNextBoot, ChangedDomains: []string{"selected-kubeadm-config"}, HealthState: "healthy", Kubeadm: generation.KubeadmActionRequired{Required: true, SelectedConfigName: "control-plane"}, UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	statusPath, _ := generation.ConfigApplyStatusPath(server.Root, "generation-0")
	if err := generation.WriteConfigApplyStatus(statusPath, status); err != nil {
		t.Fatal(err)
	}
	body := validControlPlaneConfigRequest()
	body.DesiredGenerationId = "generation-0"
	var dispatched atomic.Int32
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error {
		dispatched.Add(1)
		return nil
	})
	submit := &agentapi.SubmitOperationRequest{ApiVersion: APIVersion, Kind: RequestKind, OperationKind: OperationKindKubeadmControlPlaneConfig, ClientRequestId: "req", Actor: "test", ExpectedMachineId: "0123456789abcdef0123456789abcdef", ExpectedCurrentGenerationId: "generation-0", KubeadmControlPlaneConfig: body}
	accepted, err := server.SubmitOperation(context.Background(), submit)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	wantDesired, _ := kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(config))
	if dispatched.Load() != 1 || record.KubeadmControlPlaneConfig.ConfigName != "control-plane" || record.PreviousGenerationID != "generation-0" || record.KubeadmControlPlaneConfig.DesiredConfigSHA256 != wantDesired || record.KubeadmControlPlaneConfig.KubernetesPayloadVersion != "v1.35.0" {
		t.Fatalf("dispatches=%d record=%#v accepted=%#v", dispatched.Load(), record, accepted)
	}
}

func TestExecuteKubeadmControlPlaneConfigRunsBoundedPhases(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.CoordinatorUpload = true
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	liveConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n"
	desiredConfig := liveConfig + "apiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n"
	desiredPath := filepath.Join(root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desiredPath, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(desiredConfig))
	body.ExpectedLiveConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(liveConfig))
	for _, name := range controlPlaneManifestNames {
		path := filepath.Join(root, "etc/kubernetes/manifests", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("manifest "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-1", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body, ResourceLocks: []string{"kubeadm-state.lock"}}, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.Now = func() time.Time { return now }
	executor.RunTool = func(_ context.Context, argv []string, started func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		if slices.Contains(argv, "jsonpath={.data.ClusterConfiguration}") {
			return ToolResult{Stdout: []byte(liveConfig)}
		}
		return ToolResult{}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}"},
		{"/usr/bin/kubeadm", "init", "phase", "control-plane", "all", "--config", "/etc/katl/kubeadm/control-plane/config.yaml", "--dry-run"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "node", "cp-1", "-o", "jsonpath={.spec.unschedulable}"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "cordon", "cp-1"},
		{"/usr/bin/kubeadm", "init", "phase", "control-plane", "all", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubeadm", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "uncordon", "cp-1"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || !completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}
	if completed.KubeadmControlPlaneConfig.ExpectedLiveConfigSHA256 == "" || len(completed.KubeadmControlPlaneConfig.SupportedFieldDelta) != 1 {
		t.Fatalf("operation did not persist internally observed state: %#v", completed.KubeadmControlPlaneConfig)
	}
}

func TestExecuteKubeadmControlPlaneConfigStopsAfterMutationUncertainty(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	liveConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n"
	desiredConfig := liveConfig + "apiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n"
	path := filepath.Join(root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(desiredConfig))
	body.ExpectedLiveConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(liveConfig))
	record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-fail", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if slices.Contains(argv, "jsonpath={.data.ClusterConfiguration}") {
			return ToolResult{Stdout: []byte(liveConfig)}
		}
		return ToolResult{}
	}
	err = executor.Execute(context.Background(), record)
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	failed, readErr := store.Read(record.OperationID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !failed.Terminal || !failed.RecoveryRequired || failed.Result != operation.ResultFailedNeedsRepair || failed.PostMutationRollbackAllowed || failed.HostRollback != "" {
		t.Fatalf("failed record = %#v", failed)
	}
	if !strings.Contains(failed.NextAction, "stop rollout") {
		t.Fatalf("next action = %q", failed.NextAction)
	}
}

func validControlPlaneConfigRequest() *agentapi.KubeadmControlPlaneConfigOperationRequest {
	return &agentapi.KubeadmControlPlaneConfigOperationRequest{
		RolloutId: "rollout-1", NodePosition: 1, NodeCount: 3, CoordinatorNode: "cp-3", NodeName: "cp-1", DesiredGenerationId: "gen-2", ConfigName: "control-plane",
	}
}
