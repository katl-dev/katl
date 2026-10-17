package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/bootstrapruntime"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func TestBootstrapSelectedInput(t *testing.T) {
	for _, tt := range []struct{ kind, role, ref, profile, address string }{
		{"bootstrap-init", "control-plane", "default", "default", ""},
		{"bootstrap-init", "control-plane", "selected-control-plane", "selected-control-plane", ""},
		{"bootstrap-init", "control-plane", "node-address", "node-address", "192.0.2.27"},
		{"bootstrap-join-worker", "worker", "selected-worker", "join-profile", ""},
		{"bootstrap-join-control-plane", "control-plane", "selected-control-plane", "join-profile", ""},
	} {
		t.Run(tt.kind+"/"+tt.ref, func(t *testing.T) {
			server := newTestServer(t)
			seedBootstrapRuntimeRootForRole(t, server.Root, tt.role)
			selected := publishBootstrapInput(t, server.Root, tt.role, tt.ref, tt.profile, true)
			if tt.address != "" {
				configuration, err := configapply.ReadLocalGenerationConfiguration(server.Root, "configured")
				if err != nil {
					t.Fatal(err)
				}
				configuration.Manifest.Node.Kubernetes.Address = tt.address
				if err := configapply.WriteGenerationConfiguration(server.Root, "configured", configuration); err != nil {
					t.Fatal(err)
				}
			}
			writeTestFile(t, filepath.Join(server.Root, "var/lib/katl/cluster/kubeadm/default/katl-input.json"), `{"nodeLocalKubelet":true}`)
			installedPath := filepath.Join(server.Root, "var/lib/katl/cluster/kubeadm/default/config.yaml")
			installed, err := os.ReadFile(installedPath)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(server.Root, server.Store, "test")
			source, bundle := configureExecutorBundle(t, executor, "v1.35.0", "selected input runtime")
			server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
			req := submitRequest("selected-input")
			req.OperationKind = tt.kind
			req.ExpectedCurrentGenerationId = "configured"
			req.Bootstrap.SystemRole, req.Bootstrap.BootstrapProfileRef = tt.role, tt.profile
			setSubmitRequestBundle(req, source, bundle)
			if tt.kind == "bootstrap-join-worker" {
				req.Bootstrap.WorkerJoinMaterial = validWorkerJoinMaterial()
			} else if tt.kind == "bootstrap-join-control-plane" {
				req.Bootstrap.WorkerJoinMaterial = validControlPlaneJoinMaterial()
			}
			incomingDigest, err := RequestDigest(req)
			if err != nil {
				t.Fatal(err)
			}

			accepted, err := server.SubmitOperation(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			record, err := server.Store.Read(accepted.OperationId)
			if err != nil {
				t.Fatal(err)
			}
			if record.BootstrapRequest.KubeadmInputDigest == "" || req.Bootstrap.KubeadmInputDigest != "" || record.RequestDigest != incomingDigest {
				t.Fatal("acceptance must pin the receipt without enriching the incoming request")
			}
			unchanged, err := os.ReadFile(installedPath)
			if err != nil || !bytes.Equal(unchanged, installed) {
				t.Fatalf("acceptance rewrote installed input: %s, %v", unchanged, err)
			}
			if tt.kind == "bootstrap-init" {
				if !slices.Contains(record.ExecutorPlan.Argv, selected.Config.RenderPath) {
					t.Fatalf("init path did not select generation input: %v", record.ExecutorPlan.Argv)
				}
			} else {
				join, err := os.ReadFile(filepath.Join(server.Root, strings.TrimPrefix(record.BootstrapRequest.TemporaryJoinConfigPath, "/")))
				if err != nil || !bytes.Contains(join, []byte("name: desired-node")) || !bytes.Contains(join, []byte("directory: /etc/katl/kubeadm/"+tt.ref+"/patches")) {
					t.Fatalf("join did not use selected input and patch tree: %s, %v", join, err)
				}
			}
			plan, err := bootstrapplan.FromOperationWithBundleClient(server.Root, record, executor.BundleClient)
			if err != nil {
				t.Fatal(err)
			}
			reconstructed := plan.SelectedInput.Configuration.KubeadmConfigs[tt.ref]
			if (tt.address == "" && !bytes.Equal(reconstructed.Config.Content, selected.Config.Content)) || !reconstructed.SharedKubeletBase {
				t.Fatal("reconstruction lost selected bytes or ownership")
			}
			if tt.address != "" {
				var native struct {
					NodeRegistration struct {
						KubeletExtraArgs []struct{ Name, Value string } `yaml:"kubeletExtraArgs"`
					} `yaml:"nodeRegistration"`
					LocalAPIEndpoint struct {
						AdvertiseAddress string `yaml:"advertiseAddress"`
					} `yaml:"localAPIEndpoint"`
				}
				decoder := yaml.NewDecoder(bytes.NewReader(reconstructed.Config.Content))
				if err := decoder.Decode(&native); err != nil {
					t.Fatal(err)
				}
				if len(native.NodeRegistration.KubeletExtraArgs) != 1 || native.NodeRegistration.KubeletExtraArgs[0].Name != "node-ip" || native.NodeRegistration.KubeletExtraArgs[0].Value != tt.address || native.LocalAPIEndpoint.AdvertiseAddress != tt.address {
					t.Fatalf("selected node address was not projected once: %+v", native)
				}
			}
			// A changed installation cache must not affect a generation-owned
			// snapshot, including between planning and candidate persistence.
			writeTestFile(t, installedPath, "invalid: [installation cache changed")
			plan, err = bootstrapplan.FromOperationWithBundleClient(server.Root, record, executor.BundleClient)
			if err != nil {
				t.Fatalf("installation cache replaced generation authority: %v", err)
			}
			result, err := bootstrapruntime.Prepare(server.Root, plan, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := configapply.ReadLocalGenerationConfiguration(server.Root, result.Record.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			got := persisted.KubeadmConfigs[tt.ref]
			if !bytes.Equal(got.Config.Content, reconstructed.Config.Content) || !got.NodeLocalKubelet || !got.SharedKubeletBase || len(got.Patches) != 1 || !bytes.Equal(got.Patches[0].Content, selected.Patches[0].Content) {
				t.Fatalf("candidate lost desired input snapshot: %+v", got)
			}
			rendered := filepath.Join(server.Root, "var/lib/katl/generations", result.Record.GenerationID, "confext", strings.TrimPrefix(selected.Config.RenderPath, "/"))
			runtime, err := os.ReadFile(rendered)
			if err != nil || !bytes.Contains(runtime, []byte("name: desired-node")) || !bytes.Contains(runtime, []byte("maxPods: 110")) {
				t.Fatalf("rendered candidate did not use selected base: %s, %v", runtime, err)
			}
			cache, err := os.ReadFile(installedPath)
			if err != nil || bytes.Equal(cache, installed) || string(cache) != "invalid: [installation cache changed" {
				t.Fatalf("bootstrap rewrote installation cache: %s, %v", cache, err)
			}
		})
	}
}

func TestBootstrapInputBinding(t *testing.T) {
	for _, scenario := range []string{"changed selected input", "explicit wrong digest", "stale profile alias", "corrupt selected input", "broken ancestry", "legacy projected input", "legacy receipt"} {
		t.Run(scenario, func(t *testing.T) {
			server := newTestServer(t)
			seedBootstrapRuntimeRoot(t, server.Root)
			ref := "default"
			if scenario == "stale profile alias" {
				ref = "changed-ref"
			}
			publishBootstrapInput(t, server.Root, "control-plane", ref, ref, scenario != "legacy projected input")
			if scenario == "stale profile alias" {
				configuration, err := configapply.ReadLocalGenerationConfiguration(server.Root, "configured")
				if err != nil {
					t.Fatal(err)
				}
				configuration.Manifest.Node.Bootstrap.BootstrapProfileRef = "default"
				configuration.Manifest.Node.Bootstrap.ProfileResolvedID = "kubeadm:default"
				if err := configapply.WriteGenerationConfiguration(server.Root, "configured", configuration); err != nil {
					t.Fatal(err)
				}
			}
			executor := NewExecutor(server.Root, server.Store, "test")
			source, bundle := configureExecutorBundle(t, executor, "v1.35.0", "input binding runtime")
			server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
			req := submitRequest("input-binding")
			req.ExpectedCurrentGenerationId = "configured"
			setSubmitRequestBundle(req, source, bundle)
			inputPath := filepath.Join(server.Root, "var/lib/katl/generations/configured/kubeadm-input", ref, "config.yaml")
			switch scenario {
			case "explicit wrong digest":
				req.Bootstrap.KubeadmInputDigest = strings.Repeat("f", 64)
			case "corrupt selected input":
				writeTestFile(t, inputPath, "invalid: [")
			case "broken ancestry":
				if err := os.RemoveAll(filepath.Join(server.Root, "var/lib/katl/generations/configured/kubeadm-input")); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(server.Root, "var/lib/katl/generations/0")); err != nil {
					t.Fatal(err)
				}
			}
			accepted, err := server.SubmitOperation(context.Background(), req)
			if scenario == "explicit wrong digest" || scenario == "stale profile alias" || scenario == "corrupt selected input" || scenario == "broken ancestry" {
				want := map[string]string{"explicit wrong digest": "kubeadmInputDigest", "stale profile alias": "bootstrapProfileRef", "corrupt selected input": "read bootstrap source generation", "broken ancestry": "read bootstrap source generation"}[scenario]
				if err == nil || accepted != nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("invalid selection accepted: %+v, %v", accepted, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			record, err := server.Store.Read(accepted.OperationId)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "changed selected input" {
				writeTestFile(t, inputPath, "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  name: changed-after-acceptance\n")
				if _, err := server.SubmitOperation(context.Background(), proto.Clone(req).(*agentapi.SubmitOperationRequest)); err != nil {
					t.Fatalf("duplicate receipt reselected changed desired input: %v", err)
				}
			} else if scenario == "legacy receipt" {
				record.BootstrapRequest.KubeadmInputDigest = ""
			}
			plan, err := bootstrapplan.FromOperationWithBundleClient(server.Root, record, executor.BundleClient)
			if scenario == "changed selected input" || scenario == "legacy receipt" {
				if err == nil {
					t.Fatal("reconstruction accepted unpinned or changed source input")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bootstrapruntime.Prepare(server.Root, plan, time.Now()); err == nil || !strings.Contains(err.Error(), "katlctl cluster apply --config") {
				t.Fatalf("selected legacy input was published: %v", err)
			}
			if _, err := os.Stat(filepath.Join(server.Root, "var/lib/katl/generations", record.CandidateGenerationID)); !os.IsNotExist(err) {
				t.Fatalf("candidate materialized after legacy refusal: %v", err)
			}
		})
	}
}

func TestBootstrapInstalledProfileAlias(t *testing.T) {
	server := newTestServer(t)
	seedBootstrapRuntimeRoot(t, server.Root)
	selected := publishBootstrapInput(t, server.Root, "control-plane", "control-plane", "selected-profile", true)
	installed := bootstrapRuntimeManifest("control-plane")
	installed.Node.Kubernetes.Kubeadm.ConfigRef = "control-plane"
	installed.Node.Bootstrap.ProfileResolvedID = "kubeadm:control-plane"
	if _, err := installer.WriteClusterIntent(installer.ClusterIntentRequest{
		TargetRoot: server.Root, Manifest: installed, KubeadmConfigs: map[string]kubeadmconfig.Plan{"control-plane": selected},
		KubernetesVersion: "v1.35.0", GenerationID: "0", RequestDigest: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
	req := submitRequest("installed-alias")
	req.ExpectedCurrentGenerationId = "configured"
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatalf("bound installed alias refused: %v", err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil || !slices.Contains(record.ExecutorPlan.Argv, "/etc/katl/kubeadm/control-plane/config.yaml") {
		t.Fatalf("installed alias changed selected native path: %+v, %v", record, err)
	}
}

func publishBootstrapInput(t *testing.T, root, role, ref, profile string, shared bool) kubeadmconfig.Plan {
	t.Helper()
	configuration, err := configapply.ReadGenerationConfiguration(root, "0")
	if err != nil {
		t.Fatal(err)
	}
	configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef = ref
	configuration.Manifest.Node.Bootstrap.BootstrapProfileRef = profile
	configuration.Manifest.Node.Bootstrap.ProfileResolvedID = "kubeadm:" + ref
	base := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"
	if role == "worker" {
		base = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"
	}
	base += "nodeRegistration:\n  name: desired-node\npatches:\n  directory: /etc/katl/kubeadm/" + ref + "/patches\n"
	if role != "worker" {
		base += "---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: desired-cluster\n"
	}
	base += "---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\nhealthzPort: 10248\n"
	selected, err := kubeadmconfig.PlanFromRenderedFiles(ref, []kubeadmconfig.File{
		{RenderPath: "/etc/katl/kubeadm/" + ref + "/config.yaml", Content: []byte(base), Mode: 0o644},
		{RenderPath: "/etc/katl/kubeadm/" + ref + "/patches/kubeletconfiguration+json.json", Content: []byte(`[{"op":"test","path":"/maxPods","value":110},{"op":"replace","path":"/maxPods","value":112}]`), Mode: 0o644},
	})
	if err != nil {
		t.Fatal(err)
	}
	selected.NodeLocalKubelet, selected.SharedKubeletBase = true, shared
	configuration.KubeadmConfigs = map[string]kubeadmconfig.Plan{ref: selected}
	spec, _, err := generation.ReadGeneration(root, "0")
	if err != nil {
		t.Fatal(err)
	}
	spec.GenerationID, spec.PreviousGenerationID = "configured", "0"
	spec.Confexts = slices.Clone(spec.Confexts)
	spec.Confexts[0].Path = "/var/lib/katl/generations/configured/confext"
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := configapply.WriteGenerationConfiguration(root, "configured", configuration); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(root, "var/lib/katl/generations/configured/confext/etc"), os.DirFS(filepath.Join(root, "var/lib/katl/generations/0/confext/etc"))); err != nil {
		t.Fatal(err)
	}
	writeBootSelection(t, root, "configured")
	return selected
}
