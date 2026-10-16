package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
)

const (
	configReadyNode   = `{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[{"type":"Ready","lastHeartbeatTime":"2026-01-01T00:00:01Z","status":"True","reason":"KubeletReady","message":"kubelet is posting ready status"}]}}`
	configPendingNode = `{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[{"type":"Ready","lastHeartbeatTime":"2026-01-01T00:00:01Z","status":"False","reason":"KubeletNotReady","message":"container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: cni plugin not initialized"}]}}`
	configReadyCRI    = `{"status":{"conditions":[{"type":"RuntimeReady","status":true},{"type":"NetworkReady","status":true}]}}`
	configPendingCRI  = `{"status":{"conditions":[{"type":"RuntimeReady","status":true},{"type":"NetworkReady","status":false,"reason":"NetworkPluginNotReady","message":"Network plugin returns error: cni plugin not initialized"}]}}`
)

func readyKubeletHealthProbe(argv []string) (ToolResult, bool) {
	if result, ok := controlPlaneMirrorProbe(argv); ok {
		return result, true
	}
	if argv[0] == "/usr/bin/crictl" {
		return ToolResult{Stdout: []byte(configReadyCRI)}, true
	}
	if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show") {
		return ToolResult{Stdout: []byte("ActiveState=active\nMainPID=123\nInvocationID=0123456789abcdef0123456789abcdef\n")}, true
	}
	if argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "is-active") {
		return ToolResult{}, true
	}
	if argv[0] == "/usr/bin/kubectl" && argv[len(argv)-1] == "json" {
		index := slices.Index(argv, "node")
		if index >= 0 && index+1 < len(argv) {
			return ToolResult{Stdout: []byte(strings.ReplaceAll(configReadyNode, "worker-1", argv[index+1]))}, true
		}
	}
	return ToolResult{}, false
}

func writeKubeletStartup(t *testing.T, root string) {
	t.Helper()
	writeKubernetesStatusFile(t, root, "proc/sys/kernel/random/boot_id", "boot-1\n")
	writeKubernetesStatusFile(t, root, "run/katl/kubelet-startup.json", `{"invocationID":"0123456789abcdef0123456789abcdef","bootID":"boot-1","confextTarget":"/var/lib/katl/generations/1/confext","notBefore":"2026-01-01T00:00:01Z"}`)
	path := filepath.Join(root, "run/confexts/zzzz-katl-node")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/katl/generations/1/confext", path); err != nil {
		t.Fatal(err)
	}
}

func TestKubeletConfigHealth(t *testing.T) {
	for _, tc := range []struct {
		name        string
		baseline    string
		node        string
		cri         string
		inactive    bool
		nodeFailure bool
		criFailure  bool
		race        bool
		missing     bool
		malformed   bool
		confext     bool
		wantError   bool
	}{
		{name: "ready", baseline: "ready", node: configReadyNode, cri: configReadyCRI},
		{name: "existing CNI pending", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI},
		{name: "CNI becomes ready", baseline: "cni-uninitialized", node: configReadyNode, cri: configReadyCRI},
		{name: "legacy ready", node: configReadyNode, cri: configReadyCRI},
		{name: "ready regresses", baseline: "ready", node: configPendingNode, cri: configPendingCRI, wantError: true},
		{name: "legacy does not authorize CNI", node: configPendingNode, cri: configPendingCRI, wantError: true},
		{name: "inactive", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, inactive: true, wantError: true},
		{name: "API error with plausible output", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, nodeFailure: true, wantError: true},
		{name: "CRI transport failure", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, criFailure: true, wantError: true},
		{name: "malformed node", baseline: "cni-uninitialized", node: `{"metadata":{"name":"worker-1"},"status":`, cri: configPendingCRI, wantError: true},
		{name: "missing Ready", baseline: "cni-uninitialized", node: `{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[]}}`, cri: configPendingCRI, wantError: true},
		{name: "unknown Ready", baseline: "cni-uninitialized", node: `{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[{"type":"Ready","lastHeartbeatTime":"2026-01-01T00:00:01Z","status":"Unknown","reason":"NodeStatusUnknown"}]}}`, cri: configPendingCRI, wantError: true},
		{name: "additional PLEG failure", baseline: "cni-uninitialized", node: `{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[{"type":"Ready","lastHeartbeatTime":"2026-01-01T00:00:01Z","status":"False","reason":"KubeletNotReady","message":"[container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: cni plugin not initialized, PLEG is not healthy]"}]}}`, cri: configPendingCRI, wantError: true},
		{name: "runtime failure", baseline: "cni-uninitialized", node: configPendingNode, cri: `{"status":{"conditions":[{"type":"RuntimeReady","status":false},{"type":"NetworkReady","status":false,"reason":"NetworkPluginNotReady","message":"Network plugin returns error: cni plugin not initialized"}]}}`, wantError: true},
		{name: "missing runtime condition", baseline: "cni-uninitialized", node: configPendingNode, cri: `{"status":{"conditions":[{"type":"NetworkReady","status":false}]}}`, wantError: true},
		{name: "stale Ready cannot mask network failure", baseline: "ready", node: configReadyNode, cri: configPendingCRI, wantError: true},
		{name: "active process with old heartbeat", baseline: "cni-uninitialized", node: strings.Replace(configPendingNode, "00:00:01Z", "00:00:00Z", 1), cri: configPendingCRI, wantError: true},
		{name: "report from previous boot", baseline: "cni-uninitialized", node: strings.Replace(configPendingNode, "boot-1", "boot-0", 1), cri: configPendingCRI, wantError: true},
		{name: "automatic restart during probes", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, race: true, wantError: true},
		{name: "startup record missing", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, missing: true, wantError: true},
		{name: "startup record malformed", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, malformed: true, wantError: true},
		{name: "configuration activated after startup", baseline: "cni-uninitialized", node: configPendingNode, cri: configPendingCRI, confext: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubeletStartup(t, root)
			if tc.missing {
				if err := os.Remove(filepath.Join(root, "run/katl/kubelet-startup.json")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.malformed {
				writeKubernetesStatusFile(t, root, "run/katl/kubelet-startup.json", "{")
			}
			if tc.confext {
				link := filepath.Join(root, "run/confexts/zzzz-katl-node")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/var/lib/katl/generations/2/confext", link); err != nil {
					t.Fatal(err)
				}
			}
			request := &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: tc.baseline}
			store, record := kubeletHealthOperation(t, root, request)
			serviceReads := 0
			executor := &Executor{Root: root, Store: store, RunTool: func(_ context.Context, argv []string, _ func(int)) ToolResult {
				switch {
				case argv[0] == "/usr/bin/crictl":
					if tc.criFailure {
						return ToolResult{Err: fmt.Errorf("CRI unavailable"), ExitStatus: 1}
					}
					return ToolResult{Stdout: []byte(tc.cri)}
				case argv[0] == "/usr/bin/kubectl":
					if tc.nodeFailure {
						return ToolResult{Stdout: []byte(tc.node), ExitStatus: 1}
					}
					return ToolResult{Stdout: []byte(tc.node)}
				case argv[0] == "/usr/bin/systemctl" && slices.Contains(argv, "show"):
					serviceReads++
					if tc.inactive {
						return ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}
					}
					result, _ := readyKubeletHealthProbe(argv)
					if tc.race && serviceReads > 1 {
						result.Stdout = []byte("ActiveState=active\nMainPID=124\nInvocationID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
					}
					return result
				default:
					t.Fatalf("unexpected health command: %s", strings.Join(argv, " "))
					return ToolResult{}
				}
			}}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			result := executor.runKubeletConfigHealth(ctx, record)
			if gotError := result.Err != nil || result.ExitStatus != 0; gotError != tc.wantError {
				t.Fatalf("health result = %#v, want error %v", result, tc.wantError)
			}
		})
	}
}

func TestKubeletControlPlaneHealth(t *testing.T) {
	for _, tc := range []struct {
		name, podReady, containersReady string
		apiFailure, podFailure          bool
		wantError                       bool
	}{
		{name: "healthy", podReady: "True", containersReady: "True"},
		{name: "node controller marks pods unready", podReady: "False", containersReady: "True"},
		{name: "unready containers", podReady: "True", containersReady: "False", wantError: true},
		{name: "local API unavailable", podReady: "True", containersReady: "True", apiFailure: true, wantError: true},
		{name: "pod observation unavailable", podReady: "True", containersReady: "True", podFailure: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", "spec:\n  containers:\n    - name: kube-apiserver\n      command:\n        - kube-apiserver\n        - --advertise-address=192.0.2.10\n        - --secure-port=6443\n")
			request := &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "cp-1", ReadinessRequirement: "cni-uninitialized"}
			store, record := kubeletHealthOperation(t, root, request)
			executor := &Executor{
				Root:  root,
				Store: store,
				RunTool: func(_ context.Context, argv []string, _ func(int)) ToolResult {
					if argv[0] == "/usr/bin/systemctl" {
						result, _ := readyKubeletHealthProbe(argv)
						return result
					}
					if tc.apiFailure && slices.Contains(argv, "--raw=/readyz") {
						return ToolResult{ExitStatus: 1}
					}
					if _, ok := controlPlaneMirrorProbe(argv); ok {
						if tc.podFailure {
							return ToolResult{Err: fmt.Errorf("API connection failed"), ExitStatus: 1}
						}
						conditions := fmt.Sprintf(`[{"type":"Ready","status":%q},{"type":"ContainersReady","status":%q}]`, tc.podReady, tc.containersReady)
						return ToolResult{Stdout: []byte(mirrorList(
							mirrorPod("etcd", "etcd", conditions),
							mirrorPod("kube-apiserver", "api", conditions),
							mirrorPod("kube-controller-manager", "controller", conditions),
							mirrorPod("kube-scheduler", "scheduler", conditions),
						))}
					}
					if argv[0] == "/usr/bin/crictl" {
						return ToolResult{Stdout: []byte(configPendingCRI)}
					}
					if argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node") {
						return ToolResult{Stdout: []byte(strings.ReplaceAll(configPendingNode, "worker-1", "cp-1"))}
					}
					if slices.Contains(argv, "wait") {
						condition := tc.podReady
						if slices.Contains(argv, "--for=condition=ContainersReady") {
							condition = tc.containersReady
						}
						if condition != "True" {
							return ToolResult{ExitStatus: 1}
						}
					}
					return ToolResult{}
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			result := executor.runKubeletConfigHealth(ctx, record)
			if gotError := result.Err != nil || result.ExitStatus != 0; gotError != tc.wantError {
				t.Fatalf("health result = %#v, want error %t", result, tc.wantError)
			}
		})
	}
}

func TestKubeletHealthProbeBudget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeKubeletStartup(t, root)
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	executor := NewExecutor(root, operation.Store{}, "agent")
	executor.RunTool = func(ctx context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/kubectl" || argv[0] == "/usr/bin/crictl" {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ToolResult{Err: ctx.Err(), ExitStatus: 1}
			}
		}
		result, ok := readyKubeletHealthProbe(argv)
		if !ok {
			t.Fatalf("unexpected health command %v", argv)
		}
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	observed, err := executor.observeKubeletHealth(ctx, &operation.KubeadmControlPlaneConfig{NodeName: "worker-1"})
	if err != nil || observed.state != kubeletHealthReady {
		t.Fatalf("individually healthy native probes exceeded a shared command budget: %+v, %v", observed, err)
	}
}

func kubeletHealthOperation(t *testing.T, root string, request *operation.KubeadmControlPlaneConfig) (operation.Store, operation.OperationRecord) {
	t.Helper()
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.Component, body.NodeName, body.ReadinessRequirement = request.Component, request.NodeName, request.ReadinessRequirement
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	body.DesiredConfigSHA256 = strings.Repeat("d", 64)
	*request = body
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{
		OperationID: "config-health", OperationKind: OperationKindKubeadmControlPlaneConfig,
		Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted",
		KubeadmControlPlaneConfig: request,
	}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return store, record
}

func TestKubeletConfigHealthTightensReadiness(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubeletStartup(t, root)
	store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{
		Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: "cni-uninitialized",
	})
	executor := NewExecutor(root, store, "agent-1")
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		result, ok := readyKubeletHealthProbe(argv)
		if !ok {
			t.Fatalf("unexpected command %v", argv)
		}
		return result
	}
	if result := executor.runKubeletConfigHealth(context.Background(), record); result.Err != nil || result.ExitStatus != 0 {
		t.Fatalf("Ready post-health failed: %+v", result)
	}

	reopened, err := operation.NewStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Read(record.OperationID)
	if err != nil || persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "ready" {
		t.Fatalf("Ready observation was not durable: %+v, %v", persisted, err)
	}
	// A later phase or reconstructed retry must honor the durable requirement,
	// even if its caller retained the earlier CNI-pending operation snapshot.
	staleRequest := *record.KubeadmControlPlaneConfig
	staleRequest.ReadinessRequirement = "cni-uninitialized"
	record.KubeadmControlPlaneConfig = &staleRequest
	executor = NewExecutor(root, reopened, "agent-2")
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "/usr/bin/kubectl":
			return ToolResult{Stdout: []byte(configPendingNode)}
		case "/usr/bin/crictl":
			return ToolResult{Stdout: []byte(configPendingCRI)}
		default:
			result, ok := readyKubeletHealthProbe(argv)
			if !ok {
				t.Fatalf("unexpected health command %v", argv)
			}
			return result
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if result := executor.runKubeletConfigHealth(ctx, record); result.Err == nil && result.ExitStatus == 0 {
		t.Fatal("a stale pre-CNI snapshot weakened the recorded Ready requirement")
	}
}

func TestKubeletReadinessRecapture(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inactive   bool
		concurrent bool
	}{
		{name: "CNI observation"},
		{name: "inactive service", inactive: true},
		{name: "tightened during observation", concurrent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubeletStartup(t, root)
			store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{
				Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: "cni-uninitialized",
			})
			tighten := func() {
				t.Helper()
				if _, err := store.Update(record.OperationID, "ready-observed", "accepted", func(current operation.OperationRecord) (operation.OperationRecord, error) {
					current.KubeadmControlPlaneConfig.ReadinessRequirement = "ready"
					return current, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.concurrent {
				tighten()
			}
			reopened, err := operation.NewStore(store.Root)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(root, reopened, "reconstructed-agent")
			tightened := !tc.concurrent
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if !tightened {
					tighten()
					tightened = true
				}
				switch argv[0] {
				case "/usr/bin/systemctl":
					if tc.inactive {
						return ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}
					}
					result, _ := readyKubeletHealthProbe(argv)
					return result
				case "/usr/bin/kubectl":
					return ToolResult{Stdout: []byte(configPendingNode)}
				case "/usr/bin/crictl":
					return ToolResult{Stdout: []byte(configPendingCRI)}
				default:
					t.Fatalf("unexpected recapture command %v", argv)
					return ToolResult{}
				}
			}

			for attempt := 0; attempt < 2; attempt++ {
				// The caller's stale snapshot must neither relax durable Ready nor
				// make a harmless recapture fail an immutable-field check.
				stale := *record.KubeadmControlPlaneConfig
				stale.ReadinessRequirement = "cni-uninitialized"
				record.KubeadmControlPlaneConfig = &stale
				if err := executor.captureKubeletHealth(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				persisted, err := reopened.Read(record.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "ready" || record.KubeadmControlPlaneConfig.ReadinessRequirement != "ready" {
					t.Fatalf("recapture weakened Ready: stored=%q caller=%q", persisted.KubeadmControlPlaneConfig.ReadinessRequirement, record.KubeadmControlPlaneConfig.ReadinessRequirement)
				}
			}
		})
	}
}

func TestKubeletConfigHealthCaptureRequiresObservation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		controlPlane bool
		probe        ToolResult
		cancel       bool
		wantError    bool
	}{
		{name: "inactive is observed", probe: ToolResult{Stdout: []byte("ActiveState=inactive\nMainPID=0\nInvocationID=\n")}},
		{name: "execution unavailable", probe: ToolResult{Err: fmt.Errorf("executable unavailable"), ExitStatus: -1}, wantError: true},
		{name: "probe canceled", cancel: true, wantError: true},
		{name: "local API cancellation is outside baseline", controlPlane: true, cancel: true},
		{name: "local API health is outside baseline", controlPlane: true, probe: ToolResult{ExitStatus: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubeletStartup(t, root)
			if tc.controlPlane {
				writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
				writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", "spec:\n  containers:\n    - name: kube-apiserver\n      command:\n        - kube-apiserver\n        - --advertise-address=192.0.2.10\n        - --secure-port=6443\n")
			}
			store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "worker-1"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executor := NewExecutor(root, store, "agent")
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if !tc.controlPlane && argv[0] == "/usr/bin/systemctl" || tc.controlPlane && slices.Contains(argv, "--raw=/readyz") {
					if tc.cancel {
						cancel()
						return ToolResult{Err: context.Canceled, ExitStatus: 1}
					}
					return tc.probe
				}
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if tc.controlPlane && argv[0] == "/usr/bin/kubectl" {
					return ToolResult{}
				}
				t.Fatalf("unexpected command %v", argv)
				return ToolResult{}
			}

			err := executor.captureKubeletHealth(ctx, record)
			if (err != nil) != tc.wantError {
				t.Fatalf("capture error = %v, want error %v", err, tc.wantError)
			}
			persisted, readErr := store.Read(record.OperationID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantError && persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "" {
				t.Fatal("unavailable probe established a mutation baseline")
			}
			if !tc.wantError && (persisted.KubeadmControlPlaneConfig.ReadinessRequirement == "" || persisted.KubeadmControlPlaneConfig.ReadinessRequirement == "cni-uninitialized") {
				t.Fatal("confirmed negative state was not retained for strict repair")
			}
		})
	}
}

func TestKubeletConfigHealthRequiresPersistence(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubeletStartup(t, root)
	store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{
		Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: "cni-uninitialized",
	})
	executor := NewExecutor(root, store, "agent")
	storeLost := false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/systemctl" && !storeLost {
			storeLost = true
			// Lose access to the durable store after reading the baseline but
			// before saving the newly observed Ready requirement.
			if err := os.Rename(store.Root, store.Root+".retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Root, []byte("unavailable"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		result, ok := readyKubeletHealthProbe(argv)
		if !ok {
			t.Fatalf("unexpected command %v", argv)
		}
		return result
	}
	if result := executor.runKubeletConfigHealth(context.Background(), record); result.Err == nil && result.ExitStatus == 0 {
		t.Fatal("health succeeded without saving its tightened readiness requirement")
	}
}
