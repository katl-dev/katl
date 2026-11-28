package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
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
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				writeKubeletStartup(t, root)
				writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
				writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", "spec:\n  containers:\n    - name: kube-apiserver\n      command:\n        - kube-apiserver\n        - --advertise-address=192.0.2.10\n        - --secure-port=6443\n")
				store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "cp-1"})
				node, cri := configPendingNode, configPendingCRI
				cpReady, restarted := false, false
				cpObservations := 0
				executor := NewExecutor(root, store, "agent")
				executor.Now = kubeletFixtureNow
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

				captureCtx := context.Background()
				if tc.nodeUnavailable {
					// Capture waits for an unavailable API; bound that wait.
					var cancel context.CancelFunc
					captureCtx, cancel = context.WithTimeout(captureCtx, 30*time.Millisecond)
					defer cancel()
				}
				err := executor.captureKubeletHealth(captureCtx, record)
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
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				writeKubeletStartup(t, root)
				store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{Component: "kubelet", NodeName: "worker-1", ReadinessRequirement: "cni-uninitialized"})
				runtimeObserved := false
				executor := NewExecutor(root, store, "agent")
				executor.Now = kubeletFixtureNow
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
				executor.Now = kubeletFixtureNow
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
		})
	}
}

// kubeletReport is the Ready condition kubelet last posted for worker-1.
type kubeletReport struct {
	status, reason, message string
	heartbeat               time.Time
}

func notReadyReport(message string, heartbeat time.Time) kubeletReport {
	return kubeletReport{status: "False", reason: "KubeletNotReady", message: message, heartbeat: heartbeat}
}

func readyReport(heartbeat time.Time) kubeletReport {
	return kubeletReport{status: "True", reason: "KubeletReady", message: "kubelet is posting ready status", heartbeat: heartbeat}
}

func (r kubeletReport) node(t *testing.T) []byte {
	condition, err := json.Marshal(map[string]string{"type": "Ready", "status": r.status, "reason": r.reason, "message": r.message, "lastHeartbeatTime": r.heartbeat.Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"metadata":{"name":"worker-1"},"status":{"nodeInfo":{"bootID":"boot-1"},"conditions":[` + string(condition) + `]}}`)
}

func TestKubeletApplyBaseline(t *testing.T) {
	// writeKubeletStartup records kubelet's start at this time.
	kubeletStarted := time.Date(2026, time.January, 1, 0, 0, 1, 0, time.UTC)
	// Kubelet aggregates its Ready errors; the second is text no list of known
	// startup errors contains.
	cniAndUnknown := "[" + missingCNINodeMessage + ", subsystem katl-fixture has not finished warming up]"
	cniOnly := func(_ int, now time.Time) kubeletReport { return notReadyReport(missingCNINodeMessage, now) }
	settlesToCNI := func(read int, now time.Time) kubeletReport {
		if read == 0 {
			return notReadyReport(cniAndUnknown, now)
		}
		return cniOnly(read, now)
	}
	// Kubelet posts each change at once; this report changes every 10s update period.
	changing := func(_ int, now time.Time) kubeletReport {
		period := now.Sub(kubeletStarted.Add(2*time.Second)) / (10 * time.Second)
		return notReadyReport(fmt.Sprintf("[%s, PLEG is not healthy: pleg was last seen active %ds ago]", missingCNINodeMessage, 180+10*period), kubeletStarted.Add(2*time.Second+period*10*time.Second))
	}
	for _, tc := range []struct {
		name string
		// startAge is how long kubelet has run when the apply starts.
		startAge time.Duration
		// before is the report the nth Node read sees before kubelet restarts;
		// after is the report once it has.
		before, after func(read int, now time.Time) kubeletReport
		cri           string
		want          string
		refused       bool
		// The fake time from the apply's start to its first mutation, or to its
		// refusal, must lie in [minWait, maxWait].
		minWait, maxWait time.Duration
	}{
		{name: "unknown startup error settles to sole CNI", startAge: 2 * time.Second, before: settlesToCNI, after: cniOnly, cri: configPendingCRI, want: "cni-uninitialized", minWait: time.Millisecond, maxWait: 2 * time.Second},
		{name: "fresh report from a long-running kubelet settles to sole CNI", startAge: 10 * time.Minute, before: settlesToCNI, after: cniOnly, cri: configPendingCRI, want: "cni-uninitialized", minWait: time.Millisecond, maxWait: 2 * time.Second},
		{
			name: "NotReady report older than the window is settled", startAge: settledReportAge + 15*time.Second,
			before: func(int, time.Time) kubeletReport {
				return notReadyReport("["+missingCNINodeMessage+", CSINode is not yet initialized]", kubeletStarted)
			},
			after: func(_ int, now time.Time) kubeletReport { return readyReport(now) }, cri: configPendingCRI, want: "ready",
		},
		{
			name: "unchanged NotReady report settles within the window", startAge: 2 * time.Second,
			before: func(int, time.Time) kubeletReport {
				return notReadyReport("["+missingCNINodeMessage+", PLEG is not healthy: pleg has yet to be successful]", kubeletStarted.Add(2*time.Second))
			},
			after: func(_ int, now time.Time) kubeletReport { return readyReport(now) }, cri: configPendingCRI, want: "ready",
			minWait: settledReportAge, maxWait: settledReportAge + 2*time.Second,
		},
		{
			// A nodeStatusReportFrequency under 30s reposts the unchanged report with a fresh heartbeat.
			name: "unchanged NotReady report with fresh heartbeats settles within the window", startAge: 2 * time.Second,
			before: func(_ int, now time.Time) kubeletReport {
				return notReadyReport("["+missingCNINodeMessage+", PLEG is not healthy: pleg has yet to be successful]", now)
			},
			after: func(_ int, now time.Time) kubeletReport { return readyReport(now) }, cri: configPendingCRI, want: "ready",
			minWait: settledReportAge, maxWait: settledReportAge + 2*time.Second,
		},
		{name: "fresh sole CNI", startAge: 2 * time.Second, before: cniOnly, after: cniOnly, cri: configPendingCRI, want: "cni-uninitialized"},
		{
			name: "NotReady report that keeps changing", startAge: 2 * time.Second, before: changing, after: changing,
			cri: configPendingCRI, refused: true, minWait: postKubeadmHealthTimeout, maxWait: postKubeadmHealthTimeout + 2*time.Second,
		},
		{
			name: "Ready node", startAge: 2 * time.Second,
			before: func(_ int, now time.Time) kubeletReport { return readyReport(now) },
			after:  func(_ int, now time.Time) kubeletReport { return readyReport(now) }, cri: configReadyCRI, want: "ready",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				writeKubeletStartup(t, root)
				writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
				writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
				store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
				if err != nil {
					t.Fatal(err)
				}
				desiredConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\n"
				writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/node-worker-1/config.yaml", desiredConfig)
				livePath := filepath.Join(root, "var/lib/kubelet/config.yaml")
				writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")
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
				writeKubeletRefreshInputs(t, root, body, desiredConfig, map[string]string{"kubeletconfiguration+merge.yaml": "maxPods: 120\n"})
				record, err := store.Create(operation.OperationRecord{OperationID: "kubelet-baseline", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}

				// The node's clock advances with the bubble's fake time.
				start := time.Now()
				now := func() time.Time { return kubeletStarted.Add(tc.startAge + time.Since(start)) }
				const rendered = "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 120\ncgroupDriver: systemd\n"
				var mutations [][]string
				var mutatedAfter time.Duration
				var requirementAtMutation, healthPhase, lastMessage string
				nodeReads := 0
				restarted := false
				executor := NewExecutor(root, store, "agent-start")
				executor.Async = false
				executor.Now = now
				executor.RunTool = func(ctx context.Context, argv []string, _ func(int)) ToolResult {
					if writeKubeletPreview(t, root, argv, rendered) {
						return ToolResult{}
					}
					switch {
					case argv[0] == "/usr/bin/kubectl" && slices.Contains(argv, "node"):
						if restarted {
							if healthPhase == "" {
								current, err := store.Read(record.OperationID)
								if err != nil {
									t.Fatal(err)
								}
								healthPhase = current.Phase
							}
							return ToolResult{Stdout: tc.after(0, now()).node(t)}
						}
						report := tc.before(nodeReads, now())
						nodeReads++
						if ctx.Err() == nil {
							lastMessage = report.message
						}
						return ToolResult{Stdout: report.node(t)}
					case argv[0] == "/usr/bin/crictl":
						if restarted && tc.after(0, now()).status == "True" {
							return ToolResult{Stdout: []byte(configReadyCRI)}
						}
						return ToolResult{Stdout: []byte(tc.cri)}
					}
					if result, ok := readyKubeletHealthProbe(argv); ok {
						return result
					}
					if len(mutations) == 0 {
						mutatedAfter = time.Since(start)
						current, err := store.Read(record.OperationID)
						if err != nil {
							t.Fatal(err)
						}
						requirementAtMutation = current.KubeadmControlPlaneConfig.ReadinessRequirement
					}
					mutations = append(mutations, slices.Clone(argv))
					restarted = restarted || slices.Contains(argv, "restart")
					if argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "upgrade") {
						if err := os.WriteFile(livePath, []byte(rendered), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return ToolResult{}
				}

				// Post-apply health runs until its context ends; bound it in fake time.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				err = executor.Execute(ctx, record)
				elapsed := time.Since(start)
				persisted, readErr := store.Read(record.OperationID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if tc.refused {
					if err == nil || persisted.Phase != "preflight" || persisted.ExternalMutationStarted || persisted.MutatingToolRan || len(mutations) != 0 {
						t.Fatalf("changing NotReady baseline did not refuse before mutation: err=%v phase=%q mutations=%v", err, persisted.Phase, mutations)
					}
					if elapsed < tc.minWait || elapsed > tc.maxWait {
						t.Fatalf("refused after %s, want within [%s, %s]", elapsed, tc.minWait, tc.maxWait)
					}
					if persisted.KubeadmControlPlaneConfig.ReadinessRequirement != "" {
						t.Fatalf("refused baseline recorded requirement %q", persisted.KubeadmControlPlaneConfig.ReadinessRequirement)
					}
					if lastMessage == "" || !strings.Contains(persisted.FailureReason, lastMessage) || !strings.Contains(persisted.FailureReason, "katlctl node logs worker-1 --unit kubelet") {
						t.Fatalf("refusal lacks the last report %q or its next step: %q", lastMessage, persisted.FailureReason)
					}
					return
				}
				if err != nil || persisted.Result != operation.ResultSucceeded {
					t.Fatalf("apply = %v, result %q: %s", err, persisted.Result, persisted.FailureReason)
				}
				if requirementAtMutation != tc.want {
					t.Fatalf("readiness requirement before mutation = %q, want %q", requirementAtMutation, tc.want)
				}
				if mutatedAfter < tc.minWait || mutatedAfter > tc.maxWait {
					t.Fatalf("first mutation after %s, want within [%s, %s]", mutatedAfter, tc.minWait, tc.maxWait)
				}
				if !slices.ContainsFunc(mutations, func(argv []string) bool { return slices.Contains(argv, "restart") }) {
					t.Fatal("changed kubelet configuration was not restarted")
				}
				if healthPhase != "post-kubelet-health-running" {
					t.Fatalf("progress during post-kubelet health = %q, want post-kubelet-health-running", healthPhase)
				}
			})
		})
	}
}
