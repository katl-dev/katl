package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestKubeadmUpgradePatches(t *testing.T) {
	for _, role := range []string{"apply", "control-plane", "worker"} {
		for _, withPatches := range []bool{false, true} {
			name := role + "/without patches"
			if withPatches {
				name = role + "/selected patches"
			}
			t.Run(name, func(t *testing.T) {
				root, store, record, now := kubeadmUpgradeFixture(t, role)
				var patches map[string]string
				if withPatches {
					patches = map[string]string{
						"kubeletconfiguration999+merge.yaml": "shutdownGracePeriod: 45s\nshutdownGracePeriodCriticalPods: 15s\n",
						"kube-apiserver0+merge.yaml":         "metadata:\n  annotations:\n    operator: retained\n",
					}
				}
				writeKubeadmConfigFixture(t, root, "gen0", "operator-current", "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n", patches)
				// Neither the installed input nor the current projection is authority.
				writeTestFile(t, filepath.Join(root, "var/lib/katl/cluster/kubeadm/operator-current/patches/kubeletconfiguration999+merge.yaml"), "shutdownGracePeriod: 90s\n")
				writeTestFile(t, filepath.Join(root, "etc/katl/kubeadm/operator-current/patches/kubeletconfiguration999+merge.yaml"), "shutdownGracePeriod: 0s\n")
				writeTestFile(t, filepath.Join(root, "var/lib/katl/operations", record.OperationID, "tools/kubeadm-patches/kubeletconfiguration000+merge.yaml"), "maxPods: 9\n")

				executor := NewExecutor(root, store, "agent-test")
				executor.Async = false
				executor.Now = func() time.Time { return now.Add(time.Minute) }
				executor.SetBootDefault = func(context.Context, string, string) error { return nil }
				executor.SetBootOneshot = func(context.Context, string, string) error { return nil }
				upgraded := false
				executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
					joined := strings.Join(argv, " ")
					switch {
					case strings.Contains(joined, "kubeadm version"):
						return ToolResult{Stdout: []byte("v1.36.2\n")}
					case strings.Contains(joined, "get configmap kubeadm-config"):
						return ToolResult{Stdout: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")}
					case strings.Contains(joined, "get endpoints kubernetes"):
						return ToolResult{Stdout: testKubeAPIServerEndpoints("10.0.0.1")}
					case strings.Contains(joined, "kubelet --version"):
						return ToolResult{Stdout: []byte("Kubernetes v1.36.2\n")}
					case strings.Contains(joined, "kubeadm upgrade apply"), strings.Contains(joined, "kubeadm upgrade node"):
						upgraded = true
						assertKubeadmPatches(t, root, argv, patches)
					}
					return ToolResult{}
				}
				if err := executor.Execute(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				if !upgraded {
					t.Fatal("kubeadm upgrade did not run")
				}
				completed, err := store.Read(record.OperationID)
				if err != nil || completed.Result != operation.ResultSucceeded {
					t.Fatalf("upgrade result = %+v, %v", completed, err)
				}
			})
		}
	}
}

func TestKubeadmUpgradeRejectsBrokenInput(t *testing.T) {
	for _, name := range []string{"corrupt input", "missing selected input"} {
		t.Run(name, func(t *testing.T) {
			root, store, record, now := kubeadmUpgradeFixture(t, "worker")
			if name == "corrupt input" {
				writeTestFile(t, filepath.Join(root, "var/lib/katl/generations/gen0/kubeadm-input/default/config.yaml"), "not: [valid YAML\n")
			} else {
				value := validVolumeStatusManifest()
				value.Node.Kubernetes.Kubeadm.ConfigRef = "missing"
				if err := configapply.WriteGenerationManifest(root, "gen0", value); err != nil {
					t.Fatal(err)
				}
			}
			executor := NewExecutor(root, store, "agent-test")
			executor.Async = false
			executor.Now = func() time.Time { return now.Add(time.Minute) }
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if slices.Contains(argv, "upgrade") && (slices.Contains(argv, "node") || slices.Contains(argv, "apply")) {
					t.Fatalf("broken selected input reached kubeadm mutation: %v", argv)
				}
				return ToolResult{Stdout: []byte("v1.36.2\n")}
			}

			if err := executor.Execute(context.Background(), record); err == nil || !strings.Contains(err.Error(), "read selected kubeadm patches") {
				t.Fatalf("upgrade error = %v", err)
			}
			failed, err := store.Read(record.OperationID)
			if err != nil || !failed.Terminal || failed.RecoveryRequired || failed.ExternalMutationStarted || failed.MutatingToolRan {
				t.Fatalf("pre-mutation failure = %+v, %v", failed, err)
			}
		})
	}
}

func writeKubeadmConfigFixture(t *testing.T, root, generationID, ref, config string, patches map[string]string) {
	t.Helper()
	files := []kubeadmconfig.File{{RenderPath: "/etc/katl/kubeadm/" + ref + "/config.yaml", Content: []byte(config)}}
	for name, content := range patches {
		files = append(files, kubeadmconfig.File{RenderPath: "/etc/katl/kubeadm/" + ref + "/patches/" + name, Content: []byte(content)})
	}
	plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, files)
	if err != nil {
		t.Fatal(err)
	}
	value := validVolumeStatusManifest()
	value.Node.Kubernetes.Kubeadm.ConfigRef = ref
	if err := configapply.WriteGenerationConfiguration(root, generationID, configapply.GenerationConfiguration{
		Manifest: value, KubeadmConfigs: map[string]kubeadmconfig.Plan{ref: plan},
	}); err != nil {
		t.Fatal(err)
	}
}

func assertKubeadmPatches(t *testing.T, root string, argv []string, patches map[string]string) {
	t.Helper()
	index := slices.Index(argv, "--patches")
	if len(patches) == 0 {
		if index >= 0 {
			t.Fatalf("empty desired patch set supplied --patches: %v", argv)
		}
		return
	}
	if index < 0 || index+1 >= len(argv) {
		t.Fatalf("native patches omitted: %v", argv)
	}
	dir := filepath.Join(root, argv[index+1])
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(patches) {
		t.Fatalf("patch directory contains %v, error %v; want %d files", entries, err, len(patches))
	}
	for name, want := range patches {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("patch %s = %q, %v; want %q", name, data, err, want)
		}
	}
}
