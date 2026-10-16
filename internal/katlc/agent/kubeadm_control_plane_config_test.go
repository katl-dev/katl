package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
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
	req.NodeLocalKubelet = true
	req.CoordinatorUpload = true
	if err := validateKubeadmControlPlaneConfigRequest(OperationKindKubeadmControlPlaneConfig, req); err == nil || !strings.Contains(err.Error(), "must not be uploaded") {
		t.Fatalf("node-local upload error = %v", err)
	}
}

func TestAcceptKubeadmControlPlaneConfigBindsActiveGeneration(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
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
			if err := os.WriteFile(filepath.Join(server.Root, "etc/katl/node.json"), []byte(fmt.Sprintf(`{"kubeadm":{"configRef":"control-plane","nodeLocalKubelet":%t}}`, local)), 0o600); err != nil {
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
		})
	}
}

func TestExecuteKubeadmControlPlaneConfigRunsBoundedPhases(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
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
	writeControlPlaneManifests(t, root)
	record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-1", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body, ResourceLocks: []string{"kubeadm-state.lock"}}, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.Now = func() time.Time { return now }
	executor.RunTool = func(_ context.Context, argv []string, started func(int)) ToolResult {
		if result, ok := controlPlaneMirrorProbe(argv); ok {
			return result
		}
		if result, ok := healthyControlPlaneProbe(argv); ok {
			return result
		}
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		commands = append(commands, append([]string(nil), argv...))
		if slices.Contains(argv, "jsonpath={.data.ClusterConfiguration}") {
			return ToolResult{Stdout: []byte(liveConfig)}
		}
		return ToolResult{}
	}
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

func TestControlPlaneRestart(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "restart consumes desired manifests"
		if malformed {
			name = "malformed startup evidence refuses mutation"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			if malformed {
				writeKubernetesStatusFile(t, root, "run/katl/kubelet-startup.json", "{")
			}
			writeControlPlaneManifests(t, root)
			link := filepath.Join(root, "run/confexts/zzzz-katl-node")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/var/lib/katl/generations/2/confext", link); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, "etc/kubernetes/manifests/kube-apiserver.yaml")
			original, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			desiredManifest := strings.Replace(string(original), "    - --secure-port=6443\n", "    - --secure-port=6443\n    - --profiling=false\n", 1)
			liveConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"
			desiredConfig := liveConfig + "apiServer:\n  extraArgs:\n  - name: profiling\n    value: \"false\"\n"
			writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/control-plane/config.yaml", desiredConfig)
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.CoordinatorUpload = false
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, err = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(desiredConfig))
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.Create(operation.OperationRecord{
				OperationID: "cp-rebind", OperationKind: OperationKindKubeadmControlPlaneConfig,
				Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body,
			}, "accepted", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			restarts := 0
			mutations := 0
			executor := NewExecutor(root, store, "agent")
			executor.Async = false
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "restart") {
					mutations++
					// Native notify can return before the initial static-pod sync. Its
					// first scan must therefore see the final desired manifest on disk.
					data, err := os.ReadFile(manifestPath)
					if err != nil || !strings.Contains(string(data), "--profiling=false") {
						t.Fatalf("kubelet restarted before desired manifest was written: %v\n%s", err, data)
					}
					restarts++
					return ToolResult{}
				}
				if argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "control-plane") {
					if !slices.Contains(argv, "--dry-run") {
						mutations++
						if err := os.WriteFile(manifestPath, []byte(desiredManifest), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return ToolResult{}
				}
				if result, ok := controlPlaneMirrorProbe(argv); ok {
					if restarts > 0 {
						result.Stdout = []byte(strings.ReplaceAll(string(result.Stdout), "old-kube-apiserver", "new-kube-apiserver"))
					}
					return result
				}
				if result, ok := healthyControlPlaneProbe(argv); ok {
					return result
				}
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if slices.Contains(argv, "jsonpath={.data.ClusterConfiguration}") {
					return ToolResult{Stdout: []byte(liveConfig)}
				}
				if slices.Contains(argv, "jsonpath={.spec.unschedulable}") || slices.Contains(argv, "cordon") || slices.Contains(argv, "uncordon") {
					if slices.Contains(argv, "cordon") || slices.Contains(argv, "uncordon") {
						mutations++
					}
					return ToolResult{}
				}
				t.Fatalf("unexpected configuration command %v", argv)
				return ToolResult{}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			executeErr := executor.Execute(ctx, record)
			completed, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if malformed {
				if executeErr == nil || completed.Phase != "preflight" || completed.ExternalMutationStarted || completed.MutatingToolRan || mutations != 0 {
					t.Fatalf("invalid startup evidence did not refuse before mutation: result=%+v mutations=%d error=%v", completed, mutations, executeErr)
				}
				retained, err := os.ReadFile(manifestPath)
				if err != nil || string(retained) != string(original) {
					t.Fatalf("refusal changed the original manifest: %v\n%s", err, retained)
				}
				return
			}
			if executeErr != nil || completed.Result != operation.ResultSucceeded || restarts != 1 {
				t.Fatalf("configuration result=%+v restarts=%d error=%v", completed, restarts, executeErr)
			}
			backup, err := os.ReadFile(filepath.Join(store.Root, record.OperationID, "manifest-backup/kube-apiserver.yaml"))
			if err != nil || string(backup) != string(original) {
				t.Fatalf("original manifest backup was not preserved: %v\n%s", err, backup)
			}
		})
	}
}

func TestExecuteKubeadmControlPlaneConfigNoChangeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
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
	writeControlPlaneManifests(t, root)
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
	checkedNativeHealth := false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		// A stopped kubelet must not prevent unchanged control-plane
		// reconciliation from completing before the kubelet repair runs.
		if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show") {
			return ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}
		}
		if argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node") {
			return ToolResult{Stdout: []byte(`{"metadata":{"name":"cp-1"},"status":{"conditions":[{"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown"}]}}`)}
		}
		if argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "exec") {
			t.Fatal("control-plane health depends on the unavailable kubelet exec endpoint")
		}
		if result, ok := healthyControlPlaneProbe(argv); ok {
			if filepath.Base(argv[0]) == "crictl" && slices.Contains(argv, "exec") {
				checkedNativeHealth = true
			}
			return result
		}
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		commands = append(commands, append([]string(nil), argv...))
		return ToolResult{Stdout: []byte(liveConfig)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := executor.Execute(ctx, record); err != nil {
		t.Fatal(err)
	}
	if !checkedNativeHealth {
		t.Fatal("unchanged control-plane reconciliation skipped native component health")
	}
	if len(commands) != 1 || !slices.Contains(commands[0], "jsonpath={.data.ClusterConfiguration}") {
		t.Fatalf("commands = %#v, want only read-only live config collection", commands)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}

	failedAt := time.Now().UTC().Add(-time.Minute)
	_, err = store.Create(operation.OperationRecord{OperationID: "earlier-mutation", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("c", 64), Phase: "post-upload-health", Terminal: true, CompletedAt: &failedAt, Result: operation.ResultFailedNeedsRepair, RecoveryRequired: true, ExternalMutationStarted: true, FailureReason: "health failed", NextAction: "inspect retained manifests", KubeadmControlPlaneConfig: &body}, "failed", failedAt)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.Create(operation.OperationRecord{OperationID: "cp-config-retry", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), retry); err == nil || !strings.Contains(err.Error(), "inspect retained manifests") {
		t.Fatalf("unresolved mutation was hidden by a matching shared config: %v", err)
	}
}

func TestExecuteKubeadmControlPlaneConfigStopsAfterMutationUncertainty(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
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
	writeControlPlaneManifests(t, root)
	record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-fail", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if result, ok := controlPlaneMirrorProbe(argv); ok {
			return result
		}
		if slices.Contains(argv, "--raw=/readyz") {
			return ToolResult{ExitStatus: 1, Stderr: []byte("local API is unhealthy")}
		}
		if result, ok := healthyControlPlaneProbe(argv); ok {
			return result
		}
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		if slices.Contains(argv, "jsonpath={.data.ClusterConfiguration}") {
			return ToolResult{Stdout: []byte(liveConfig)}
		}
		return ToolResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = executor.Execute(ctx, record)
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
	if !strings.Contains(failed.FailureReason, "post-manifest health failed") {
		t.Fatalf("operation failed before the intended native health check: %q", failed.FailureReason)
	}
}

func writeControlPlaneManifests(t *testing.T, root string) {
	t.Helper()
	for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		content := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + component + "\n  namespace: kube-system\nspec:\n  containers:\n  - name: " + component + "\n    image: registry.k8s.io/" + component + ":v1.36.1\n    command:\n    - " + component + "\n"
		if component == "kube-apiserver" {
			content += "    - --advertise-address=192.0.2.10\n    - --secure-port=6443\n"
		}
		writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/"+component+".yaml", content)
	}
}

func controlPlaneMirrorProbe(argv []string) (ToolResult, bool) {
	if argv[0] != "/usr/bin/kubectl" || !slices.Contains(argv, "pods") || argv[len(argv)-1] != "json" {
		return ToolResult{}, false
	}
	var items []map[string]any
	for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": component + "-cp-1", "annotations": map[string]string{"kubernetes.io/config.hash": "old-" + component, "kubernetes.io/config.mirror": "old-" + component}},
			"spec":     map[string]any{"nodeName": "cp-1"},
			"status":   map[string]any{"conditions": []map[string]string{{"type": "Ready", "status": "False"}, {"type": "ContainersReady", "status": "True"}}},
		})
	}
	if slices.Contains(argv, "etcd-cp-1") {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": "etcd-cp-1", "annotations": map[string]string{"kubernetes.io/config.hash": "etcd", "kubernetes.io/config.mirror": "etcd"}},
			"spec":     map[string]any{"nodeName": "cp-1"},
			"status":   map[string]any{"conditions": []map[string]string{{"type": "Ready", "status": "False"}, {"type": "ContainersReady", "status": "True"}}},
		})
	}
	data, _ := json.Marshal(map[string]any{"items": items})
	return ToolResult{Stdout: data}, true
}

func healthyControlPlaneProbe(argv []string) (ToolResult, bool) {
	if slices.Contains(argv, "--raw=/readyz") {
		return ToolResult{Stdout: []byte("ok")}, true
	}
	if filepath.Base(argv[0]) == "crictl" {
		if slices.Contains(argv, "ps") {
			return ToolResult{Stdout: []byte("native-container\n")}, true
		}
		if slices.Contains(argv, "exec") {
			return ToolResult{}, true
		}
	}
	return ToolResult{}, false
}

func TestExecuteKubeletConfigUploadsUpdatesAndRestarts(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
	writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", "spec:\n  containers:\n    - name: kube-apiserver\n      command:\n        - kube-apiserver\n        - --advertise-address=192.0.2.10\n")
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
	sharedConfig := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n"
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		if argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, filepath.Join(root, "etc/kubernetes/admin.conf")) {
			return ToolResult{Stdout: []byte("True")}
		}
		if reflect.DeepEqual(argv, []string{"/usr/bin/systemctl", "is-active", "--quiet", "kubelet.service"}) {
			return ToolResult{}
		}
		commands = append(commands, append([]string(nil), argv...))
		if slices.Contains(argv, "jsonpath={.data.kubelet}") {
			return ToolResult{Stdout: []byte(sharedConfig)}
		}
		if slices.Contains(argv, "upload-config") {
			sharedConfig = desiredConfig + "cgroupDriver: systemd\n"
		}
		if reflect.DeepEqual(argv, []string{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config"}) {
			data := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\ncgroupDriver: systemd\n"
			if err := os.WriteFile(livePath, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return ToolResult{}
	}
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

	repeat, err := store.Create(operation.OperationRecord{OperationID: "kubelet-config-repeat", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("e", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	commands = nil
	if err := executor.Execute(context.Background(), repeat); err != nil {
		t.Fatal(err)
	}
	for _, argv := range commands {
		if argv[0] == "/usr/bin/kubeadm" || slices.Contains(argv, "restart") {
			t.Fatalf("defaulted shared configuration caused a repeat mutation: %#v", argv)
		}
	}
}

func TestKubeletNoChangeHealth(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
	writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
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
	body.NodeName = "worker-1"
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
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		commands = append(commands, append([]string(nil), argv...))
		return ToolResult{Stdout: []byte("True")}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	for _, argv := range commands {
		if argv[0] == "/usr/bin/kubeadm" || slices.Contains(argv, "restart") {
			t.Fatalf("unchanged apply mutated kubelet: %#v", argv)
		}
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.MutatingToolRan {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}

	unhealthy, err := store.Create(operation.OperationRecord{OperationID: "kubelet-config-unhealthy", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show") {
			return ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}
		}
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		return ToolResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := executor.Execute(ctx, unhealthy); err == nil {
		t.Fatal("matching config on an unhealthy kubelet was reported successful")
	}
	failed, err := store.Read(unhealthy.OperationID)
	if err != nil || failed.Result == operation.ResultSucceeded || failed.Phase != "post-kubelet-health" || !failed.MutatingToolRan {
		t.Fatalf("unhealthy result = %#v, error = %v", failed, err)
	}
}

func TestKubeletContextRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		missing     bool
		pending     bool
		wantFailure bool
	}{
		{name: "new confext", pending: true},
		{name: "missing evidence becomes Ready", missing: true},
		{name: "missing evidence cannot grant CNI", missing: true, pending: true, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			desired := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\nhealthzPort: 0\n"
			writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/worker/config.yaml", desired)
			writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", desired)
			if tc.missing {
				if err := os.Remove(filepath.Join(root, "run/katl/kubelet-startup.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				link := filepath.Join(root, "run/confexts/zzzz-katl-node")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/var/lib/katl/generations/2/confext", link); err != nil {
					t.Fatal(err)
				}
			}
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.Component, body.NodeName, body.ConfigName = "kubelet", "worker-1", "worker"
			body.ConfigPath = "/etc/katl/kubeadm/worker/config.yaml"
			body.CoordinatorUpload = false
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desired))
			if err != nil {
				t.Fatal(err)
			}
			invocation := "0123456789abcdef0123456789abcdef"
			node, cri := configPendingNode, configPendingCRI
			restarts := 0
			wantRequirement := "cni-uninitialized"
			if tc.missing {
				wantRequirement = "ready"
			}

			for attempt := 0; attempt < 2; attempt++ {
				request := body
				record, err := store.Create(operation.OperationRecord{
					OperationID: fmt.Sprintf("context-restart-%d", attempt), OperationKind: OperationKindKubeadmControlPlaneConfig,
					Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubeadmControlPlaneConfig: &request,
				}, "accepted", time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				reopened, err := operation.NewStore(store.Root)
				if err != nil {
					t.Fatal(err)
				}
				executor := NewExecutor(root, reopened, fmt.Sprintf("agent-%d", attempt))
				executor.Async = false
				executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
					switch {
					case argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show"):
						return ToolResult{Stdout: []byte("ActiveState=active\nMainPID=123\nInvocationID=" + invocation + "\n")}
					case argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node"):
						return ToolResult{Stdout: []byte(node)}
					case argv[0] == "/usr/bin/crictl":
						return ToolResult{Stdout: []byte(cri)}
					case argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "restart"):
						persisted, err := reopened.Read(record.OperationID)
						if err != nil {
							t.Fatal(err)
						}
						if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != wantRequirement {
							t.Fatalf("restart began without its verified readiness requirement: %+v", persisted.KubeadmControlPlaneConfig)
						}
						restarts++
						invocation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
						target := "/var/lib/katl/generations/2/confext"
						if tc.missing {
							target = "/var/lib/katl/generations/1/confext"
						}
						writeKubernetesStatusFile(t, root, "run/katl/kubelet-startup.json", fmt.Sprintf(`{"invocationID":%q,"bootID":"boot-1","confextTarget":%q,"notBefore":"2026-01-01T00:00:02Z"}`, invocation, target))
						node, cri = configReadyNode, configReadyCRI
						if tc.pending {
							node, cri = configPendingNode, configPendingCRI
						}
						node = strings.Replace(node, "00:00:01Z", "00:00:02Z", 1)
						return ToolResult{}
					default:
						t.Fatalf("matching native configuration must not be rewritten: %v", argv)
						return ToolResult{}
					}
				}

				ctx := context.Background()
				cancel := func() {}
				if tc.wantFailure {
					ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				}
				err = executor.Execute(ctx, record)
				cancel()
				completed, readErr := reopened.Read(record.OperationID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if tc.wantFailure {
					if err == nil || completed.Result != operation.ResultFailedNeedsRepair || completed.Phase != "post-kubelet-health" || restarts != 1 {
						t.Fatalf("missing evidence authorized pre-CNI success: result=%+v restarts=%d error=%v", completed, restarts, err)
					}
					return
				}
				if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || restarts != 1 {
					t.Fatalf("attempt %d: result=%+v restarts=%d error=%v", attempt, completed, restarts, err)
				}
				if completed.MutatingToolRan != (attempt == 0) {
					t.Fatalf("attempt %d mutation=%v", attempt, completed.MutatingToolRan)
				}
				data, err := os.ReadFile(filepath.Join(root, "var/lib/kubelet/config.yaml"))
				if err != nil || string(data) != desired {
					t.Fatalf("native configuration changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestNodeLocalKubeletConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		maxPods int
	}{
		{"converged", 120},
		{"verification failed", 110},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			desiredConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\ntopologyManagerPolicy: restricted\n"
			desiredPath := filepath.Join(root, "etc/katl/kubeadm/node-worker-1/config.yaml")
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
			body.Component = "kubelet"
			body.NodeName = "worker-1"
			body.ConfigName = "node-worker-1"
			body.ConfigPath = "/etc/katl/kubeadm/node-worker-1/config.yaml"
			body.CoordinatorUpload = false
			body.NodeLocalKubelet = true
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desiredConfig))
			record, err := store.Create(operation.OperationRecord{OperationID: "node-local-kubelet", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			var commands [][]string
			executor := NewExecutor(root, store, "agent-start")
			executor.Async = false
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node") {
					return ToolResult{Stdout: []byte("True")}
				}
				if reflect.DeepEqual(argv, []string{"/usr/bin/systemctl", "is-active", "--quiet", "kubelet.service"}) {
					return ToolResult{}
				}
				commands = append(commands, append([]string(nil), argv...))
				if reflect.DeepEqual(argv, []string{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--patches", "/etc/katl/kubeadm/node-worker-1/patches"}) {
					updated := fmt.Sprintf("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: %d\ntopologyManagerPolicy: restricted\ncgroupDriver: systemd\n", test.maxPods)
					if err := os.WriteFile(livePath, []byte(updated), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return ToolResult{}
			}
			err = executor.Execute(context.Background(), record)
			if test.maxPods != 120 {
				if err == nil || !strings.Contains(err.Error(), "maxPods: desired 120, actual 110") {
					t.Fatalf("verification error = %v", err)
				}
				failed, err := store.Read(record.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if !failed.Terminal || !failed.RecoveryRequired || failed.Result != operation.ResultFailedNeedsRepair || !strings.Contains(failed.NextAction, "rerun katlctl cluster apply") {
					t.Fatalf("failure lacks recovery guidance: %#v", failed)
				}
				for _, argv := range commands {
					if slices.Contains(argv, "restart") {
						t.Fatal("restarted kubelet before its configuration was verified")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := [][]string{
				{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--patches", "/etc/katl/kubeadm/node-worker-1/patches", "--dry-run"},
				{"/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--patches", "/etc/katl/kubeadm/node-worker-1/patches"},
				{"/usr/bin/systemctl", "restart", "kubelet.service"},
			}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("commands = %#v, want %#v", commands, want)
			}
			completed, err := store.Read(record.OperationID)
			if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.KubeadmControlPlaneConfig.ConfigUploadRan {
				t.Fatalf("completed = %#v, err = %v", completed, err)
			}
		})
	}
}

func TestExecuteKubeProxyConfigUpdatesAddonOnline(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
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
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
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

func TestKubeletRetryCompletesRestart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialCNI   bool
		recoveredCNI bool
		legacy       bool
		wantError    bool
	}{
		{name: "Ready"},
		{name: "CNI pending", initialCNI: true, recoveredCNI: true},
		{name: "CNI installed during repair", initialCNI: true},
		{name: "Ready cannot regress", recoveredCNI: true, wantError: true},
		{name: "legacy cannot gain exception", initialCNI: true, recoveredCNI: true, legacy: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			desired := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\n"
			initial := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n"
			writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/worker/config.yaml", desired)
			writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", initial)
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.Component = "kubelet"
			body.NodeName = "worker-1"
			body.ConfigName = "worker"
			body.ConfigPath = "/etc/katl/kubeadm/worker/config.yaml"
			body.CoordinatorUpload = false
			body.NodeLocalKubelet = true
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desired))
			livePath := filepath.Join(root, "var/lib/kubelet/config.yaml")
			running, node, cri := initial, configReadyNode, configReadyCRI
			if tc.initialCNI {
				node, cri = configPendingNode, configPendingCRI
			}
			active, restarts := true, 0
			unknownNode := `{"metadata":{"name":"worker-1"},"status":{"conditions":[{"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown"}]}}`
			run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
				switch {
				case argv[0] == "/usr/bin/crictl":
					return ToolResult{Stdout: []byte(cri)}
				case argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node"):
					return ToolResult{Stdout: []byte(node)}
				case argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show"):
					if !active {
						return ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}
					}
					result, _ := readyKubeletHealthProbe(argv)
					return result
				case argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "kubelet-config") && !slices.Contains(argv, "--dry-run"):
					writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", desired)
				case argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "restart"):
					restarts++
					if restarts == 1 {
						active, node = false, unknownNode
						return ToolResult{ExitStatus: 1, Stderr: []byte("restart interrupted")}
					}
					data, err := os.ReadFile(livePath)
					if err != nil {
						t.Fatal(err)
					}
					running, active = string(data), true
					node, cri = configReadyNode, configReadyCRI
					if tc.recoveredCNI {
						node, cri = configPendingNode, configPendingCRI
					}
				}
				return ToolResult{}
			}

			for attempt := 0; attempt < 3; attempt++ {
				request := body
				record := operation.OperationRecord{OperationID: fmt.Sprintf("kubelet-retry-%d", attempt), OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubeadmControlPlaneConfig: &request}
				if tc.legacy && attempt == 0 {
					// A retained older operation has no captured baseline.
					completed := time.Now().UTC()
					record.ExternalMutationStarted = true
					record.Terminal = true
					record.Result = operation.ResultFailedNeedsRepair
					record.CompletedAt = &completed
					writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", desired)
					active, node, restarts = false, unknownNode, 1
				}
				created, err := store.Create(record, "accepted", time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if tc.legacy && attempt == 0 {
					continue
				}
				// Reconstruct both store and executor so recovery must use durable evidence.
				reopened, err := operation.NewStore(store.Root)
				if err != nil {
					t.Fatal(err)
				}
				executor := NewExecutor(root, reopened, fmt.Sprintf("agent-%d", attempt))
				executor.Async = false
				executor.RunTool = run
				ctx := context.Background()
				if tc.wantError && attempt == 1 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				}
				err = executor.Execute(ctx, created)
				if attempt == 0 {
					if err == nil || !strings.Contains(err.Error(), "restart interrupted") {
						t.Fatalf("first apply: %v", err)
					}
					data, readErr := os.ReadFile(livePath)
					if readErr != nil || string(data) != desired || running != initial {
						t.Fatalf("failure did not leave a written but unloaded config: %q, %v", data, readErr)
					}
					continue
				}
				if tc.wantError {
					if err == nil {
						t.Fatal("repair granted a readiness exception absent from the failed operation")
					}
					failed, readErr := store.Read(created.OperationID)
					if readErr != nil || failed.Result != operation.ResultFailedNeedsRepair || !failed.RecoveryRequired {
						t.Fatalf("failed repair = %+v, %v", failed, readErr)
					}
					return
				}
				if err != nil || running != desired || restarts != 2 {
					t.Fatalf("attempt %d: error %v, running config %q, restarts %d; want desired config and failed restart plus one recovery", attempt, err, running, restarts)
				}
			}
		})
	}
}

func TestKubeletApplyResetsRemovedFields(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
	writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	desired := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n"
	previous := desired + "maxPods: 120\n"
	shared := desired + "maxPods: 110\ncgroupDriver: systemd\n"
	writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/worker/config.yaml", desired)
	writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", previous)
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.Component = "kubelet"
	body.NodeName = "worker-1"
	body.ConfigName = "worker"
	body.ConfigPath = "/etc/katl/kubeadm/worker/config.yaml"
	body.CoordinatorUpload = false
	body.NodeLocalKubelet = true
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(previous))
	completedAt := time.Now().UTC().Add(-time.Minute)
	_, err = store.Create(operation.OperationRecord{OperationID: "previous", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: operation.HostBookkeepingCompletionPhase, Terminal: true, CompletedAt: &completedAt, Result: operation.ResultSucceeded, ExternalMutationStarted: true, KubeadmControlPlaneConfig: &body}, "completed", completedAt)
	if err != nil {
		t.Fatal(err)
	}
	body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desired))
	record, err := store.Create(operation.OperationRecord{OperationID: "remove-field", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("b", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	running := previous
	executor := NewExecutor(root, store, "agent-start")
	executor.Async = false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		if argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "kubelet-config") && !slices.Contains(argv, "--dry-run") {
			writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", shared)
		}
		if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "restart") {
			data, err := os.ReadFile(filepath.Join(root, "var/lib/kubelet/config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			running = string(data)
		}
		return ToolResult{Stdout: []byte("True")}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if running != shared {
		t.Fatalf("removed node-local field survived in running config: %s", running)
	}
}
