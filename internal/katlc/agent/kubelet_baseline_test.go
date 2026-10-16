package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestKubeletBaseline(t *testing.T) {
	for _, tc := range []struct {
		name              string
		readyAfterCapture bool
		nodeUnavailable   bool
		criUnavailable    bool
		restartDuringCP   bool
	}{
		{name: "CNI baseline survives pending control plane"},
		{name: "Ready tightens while control plane pending", readyAfterCapture: true},
		{name: "unavailable Node cannot grant CNI", nodeUnavailable: true},
		{name: "unavailable CRI cannot grant CNI", criUnavailable: true},
		{name: "restart during completion invalidates evidence", restartDuringCP: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", "spec:\n  containers:\n    - name: kube-apiserver\n      command:\n        - kube-apiserver\n        - --advertise-address=192.0.2.10\n        - --secure-port=6443\n")
			store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "cp-1"})
			node, cri := configPendingNode, configPendingCRI
			cpReady, restarted := false, false
			cpObservations := 0
			executor := NewExecutor(root, store, "agent")
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				switch {
				case argv[0] == "/usr/bin/systemctl":
					if restarted {
						return ToolResult{Stdout: []byte("ActiveState=active\nMainPID=124\nInvocationID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")}
					}
					result, _ := readyKubeletHealthProbe(argv)
					return result
				case slices.Contains(argv, "node"):
					if tc.nodeUnavailable {
						return ToolResult{Err: fmt.Errorf("Node API unavailable"), ExitStatus: 1}
					}
					return ToolResult{Stdout: []byte(strings.ReplaceAll(node, "worker-1", "cp-1"))}
				case argv[0] == "/usr/bin/crictl":
					if tc.criUnavailable {
						return ToolResult{Err: fmt.Errorf("CRI unavailable"), ExitStatus: 1}
					}
					return ToolResult{Stdout: []byte(cri)}
				case slices.Contains(argv, "--raw=/readyz"):
					cpObservations++
					return ToolResult{}
				case slices.Contains(argv, "pods"):
					status := "False"
					if cpReady {
						status = "True"
						restarted = tc.restartDuringCP
					}
					conditions := fmt.Sprintf(`[{"type":"Ready","status":"False"},{"type":"ContainersReady","status":%q}]`, status)
					return ToolResult{Stdout: []byte(mirrorList(mirrorPod("etcd", "etcd", conditions), mirrorPod("kube-apiserver", "api", conditions), mirrorPod("kube-controller-manager", "controller", conditions), mirrorPod("kube-scheduler", "scheduler", conditions)))}
				default:
					t.Fatalf("unexpected health command: %v", argv)
					return ToolResult{}
				}
			}

			err := executor.captureKubeletHealth(context.Background(), record)
			persisted, readErr := store.Read(record.OperationID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.nodeUnavailable || tc.criUnavailable {
				if err == nil || persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "" {
					t.Fatalf("unavailable native evidence established a baseline: err=%v requirement=%q", err, persisted.KubeadmControlPlaneConfig.ReadinessRequirement)
				}
				return
			}
			if err != nil || persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "cni-uninitialized" {
				t.Fatalf("dependent control-plane health changed fresh CNI baseline: err=%v requirement=%q", err, persisted.KubeadmControlPlaneConfig.ReadinessRequirement)
			}
			if cpObservations != 0 {
				t.Fatal("baseline capture depends on control-plane completion health")
			}
			if tc.readyAfterCapture {
				node, cri = configReadyNode, configReadyCRI
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			result := executor.runKubeletConfigHealth(ctx, record)
			cancel()
			if result.Err == nil && result.ExitStatus == 0 {
				t.Fatal("pending control-plane containers satisfied completion")
			}
			persisted, err = store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			required := "cni-uninitialized"
			if tc.readyAfterCapture {
				required = "ready"
			}
			if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != required {
				t.Fatalf("requirement while control plane pending = %q, want %q", persisted.KubeadmControlPlaneConfig.ReadinessRequirement, required)
			}

			cpReady = true
			completionBudget := 5 * time.Second
			if tc.restartDuringCP {
				completionBudget = 500 * time.Millisecond
			}
			ctx, cancel = context.WithTimeout(context.Background(), completionBudget)
			result = executor.runKubeletConfigHealth(ctx, record)
			cancel()
			if failed := result.Err != nil || result.ExitStatus != 0; failed != tc.restartDuringCP {
				t.Fatalf("completion after containers recovered = %+v, want failure %t", result, tc.restartDuringCP)
			}
		})
	}
}

func TestKubeletReadyRequirement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unavailable bool
		capture     bool
		restarted   bool
	}{
		{name: "negative CRI"},
		{name: "unavailable CRI", unavailable: true},
		{name: "capture negative CRI", capture: true},
		{name: "capture unavailable CRI", capture: true, unavailable: true},
		{name: "changed invocation invalidates Ready", restarted: true},
		{name: "unavailable CRI and changed invocation", unavailable: true, restarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: "cni-uninitialized"})
			runtimeObserved := false
			executor := NewExecutor(root, store, "agent")
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				switch {
				case argv[0] == "/usr/bin/systemctl":
					if tc.restarted && runtimeObserved {
						return ToolResult{Stdout: []byte("ActiveState=active\nMainPID=124\nInvocationID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")}
					}
					result, _ := readyKubeletHealthProbe(argv)
					return result
				case slices.Contains(argv, "node"):
					return ToolResult{Stdout: []byte(configReadyNode)}
				case argv[0] == "/usr/bin/crictl":
					runtimeObserved = true
					if tc.unavailable {
						return ToolResult{Err: fmt.Errorf("CRI unavailable"), ExitStatus: 1}
					}
					return ToolResult{Stdout: []byte(`{"status":{"conditions":[{"type":"RuntimeReady","status":false},{"type":"NetworkReady","status":true}]}}`)}
				default:
					t.Fatalf("unexpected command: %v", argv)
					return ToolResult{}
				}
			}

			if tc.capture {
				err := executor.captureKubeletHealth(context.Background(), record)
				if (err != nil) != tc.unavailable {
					t.Fatalf("capture error = %v, want unavailable=%t", err, tc.unavailable)
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				result := executor.runKubeletConfigHealth(ctx, record)
				cancel()
				if result.Err == nil && result.ExitStatus == 0 {
					t.Fatal("unhealthy runtime completed configuration")
				}
			}
			persisted, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.restarted {
				if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "cni-uninitialized" {
					t.Fatal("Ready from a superseded invocation changed the requirement")
				}
				return
			}
			if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "ready" {
				t.Fatalf("validated Ready did not tighten requirement: %q", persisted.KubeadmControlPlaneConfig.ReadinessRequirement)
			}

			executor = NewExecutor(root, store, "reconstructed-agent")
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if argv[0] == "/usr/bin/crictl" {
					return ToolResult{Stdout: []byte(configPendingCRI)}
				}
				if slices.Contains(argv, "node") {
					return ToolResult{Stdout: []byte(configPendingNode)}
				}
				result, _ := readyKubeletHealthProbe(argv)
				return result
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if result := executor.runKubeletConfigHealth(ctx, record); result.Err == nil && result.ExitStatus == 0 {
				t.Fatal("later sole-CNI report relaxed a validated Ready requirement")
			}
		})
	}
}
