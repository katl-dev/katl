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

func TestValidateKubeadmKubeletConfigRequestAllowsAnyRolloutSize(t *testing.T) {
	req := validControlPlaneConfigRequest()
	req.NodeCount = 1
	req.NodePosition = 1
	req.SupportedFieldDelta = []string{kubeadmConfigComponentKubelet}
	if err := validateKubeadmControlPlaneConfigRequest(OperationKindKubeadmControlPlaneConfig, req); err != nil {
		t.Fatalf("validate request: %v", err)
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
	if err := os.WriteFile(filepath.Join(server.Root, "etc/katl/node.json"), []byte(`{"kubeadm":{"configRef":"control-plane"}}`), 0o600); err != nil {
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
	liveConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\ncontrolPlaneEndpoint: api.katl.test:6443\nkubernetesVersion: v1.36.1\n"
	desiredConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\napiServer:\n  extraArgs:\n    - name: audit-log-maxage\n      value: \"7\"\n"
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
		{"/usr/bin/kubeadm", "init", "phase", "control-plane", "apiserver", "--config", "/var/lib/katl/operations/cp-config-1/effective-kubeadm.yaml", "--dry-run"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "node", "cp-1", "-o", "jsonpath={.spec.unschedulable}"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "cordon", "cp-1"},
		{"/usr/bin/kubeadm", "init", "phase", "control-plane", "apiserver", "--config", "/var/lib/katl/operations/cp-config-1/effective-kubeadm.yaml"},
		{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubeadm", "--config", "/var/lib/katl/operations/cp-config-1/effective-kubeadm.yaml"},
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
	effective, err := os.ReadFile(filepath.Join(store.Root, record.OperationID, "effective-kubeadm.yaml"))
	if err != nil || !strings.Contains(string(effective), "controlPlaneEndpoint: api.katl.test:6443") {
		t.Fatalf("effective kubeadm config did not preserve live endpoint: %v\n%s", err, effective)
	}
}

func TestExecuteKubeadmControlPlaneConfigNoChangeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	liveConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n"
	desiredConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"
	path := filepath.Join(root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range controlPlaneManifestNames {
		manifest := filepath.Join(root, "etc/kubernetes/manifests", name)
		if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(desiredConfig))
	record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-no-change", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("b", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		return ToolResult{Stdout: []byte(liveConfig)}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || !slices.Contains(commands[0], "jsonpath={.data.ClusterConfiguration}") {
		t.Fatalf("commands = %#v, want only read-only live config collection", commands)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
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

func TestExecuteKubeletConfigUploadsUpdatesAndRestarts(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	desiredConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\n"
	desiredPath := filepath.Join(root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desiredPath, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(root, "var/lib/kubelet/config.yaml")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.NodeCount = 1
	body.NodePosition = 1
	body.CoordinatorUpload = true
	body.Component = "kubelet"
	body.SupportedFieldDelta = nil
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desiredConfig))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{OperationID: "kubelet-config-1", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body, ResourceLocks: []string{"kubeadm-state.lock"}}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		if slices.Contains(argv, "jsonpath={.data.kubelet}") {
			return ToolResult{Stdout: []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")}
		}
		if reflect.DeepEqual(argv, []string{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config"}) {
			data := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\ncgroupDriver: systemd\n"
			if err := os.WriteFile(livePath, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return ToolResult{}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}"},
		{"/usr/bin/kubeadm", "config", "validate", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--dry-run"},
		{"/usr/bin/kubeadm", "init", "phase", "upload-config", "kubelet", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config"},
		{"/usr/bin/systemctl", "restart", "kubelet.service"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Terminal || completed.Result != operation.ResultSucceeded || !completed.KubeadmControlPlaneConfig.ConfigUploadRan || completed.KubeadmControlPlaneConfig.BeforeKubeletConfigSHA256 == "" || completed.KubeadmControlPlaneConfig.AfterKubeletConfigSHA256 == "" {
		t.Fatalf("completed = %#v", completed)
	}
	if _, err := os.Stat(filepath.Join(store.Root, record.OperationID, "kubelet-config-backup", "config.yaml")); err != nil {
		t.Fatalf("backup kubelet config: %v", err)
	}
}

func TestExecuteKubeletConfigNoChangeDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	desiredConfig := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\n"
	desiredPath := filepath.Join(root, "etc/katl/kubeadm/worker/config.yaml")
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desiredPath, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(root, "var/lib/kubelet/config.yaml")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, []byte(desiredConfig+"cgroupDriver: systemd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.Component = "kubelet"
	body.ConfigName = "worker"
	body.ConfigPath = "/etc/katl/kubeadm/worker/config.yaml"
	body.CoordinatorUpload = false
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desiredConfig))
	record, err := store.Create(operation.OperationRecord{OperationID: "kubelet-config-no-change", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("e", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		return ToolResult{}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("commands = %#v, want no kubeadm or restart calls", commands)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}
}

func TestExecuteKubeProxyConfigUpdatesAddonOnline(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	desiredConfig := "apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: nftables\n"
	desiredPath := filepath.Join(root, "etc/katl/kubeadm/control-plane/config.yaml")
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desiredPath, []byte(desiredConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.NodeCount = 1
	body.NodePosition = 1
	body.CoordinatorUpload = true
	body.Component = "kube-proxy"
	body.SupportedFieldDelta = nil
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeProxyConfigurationSHA256([]byte(desiredConfig))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{OperationID: "kube-proxy-config-1", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body, ResourceLocks: []string{"kubeadm-state.lock"}}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	updated := false
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		if slices.Contains(argv, "jsonpath={.data.config\\.conf}") {
			mode := "iptables"
			if updated {
				mode = "nftables"
			}
			return ToolResult{Stdout: []byte("apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: " + mode + "\n")}
		}
		if reflect.DeepEqual(argv, []string{"/usr/bin/kubeadm", "init", "phase", "addon", "kube-proxy", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}) {
			updated = true
		}
		return ToolResult{}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "jsonpath={.data.config\\.conf}"},
		{"/usr/bin/kubeadm", "config", "validate", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubeadm", "init", "phase", "addon", "kube-proxy", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "jsonpath={.data.config\\.conf}"},
		{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-system", "rollout", "status", "daemonset/kube-proxy", "--timeout=5m"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || !completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}
}

func validControlPlaneConfigRequest() *agentapi.KubeadmControlPlaneConfigOperationRequest {
	return &agentapi.KubeadmControlPlaneConfigOperationRequest{
		RolloutId: "rollout-1", NodePosition: 1, NodeCount: 3, CoordinatorNode: "cp-3", NodeName: "cp-1", DesiredGenerationId: "gen-2", ConfigName: "control-plane",
	}
}
