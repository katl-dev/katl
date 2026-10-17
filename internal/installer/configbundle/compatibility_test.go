package configbundle

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBundleInstallerFeatures(t *testing.T) {
	for _, test := range []struct {
		name, node                   string
		overlay, inherited, required bool
	}{
		{name: "plain", node: "cp-1"},
		{name: "selected overlay", node: "worker-1", overlay: true, required: true},
		{name: "unused overlay", node: "cp-1", overlay: true},
		{name: "inherited overlay", node: "cp-1", inherited: true, required: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "kubelet.yaml"), "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nhealthzPort: 0\n")
			source := validSourceConfig()
			if test.overlay {
				source = strings.Replace(source, "      kubernetes:\n        labels:\n          katl.dev/pool:", "      kubernetes:\n        kubelet:\n          configFile: ./kubelet.yaml\n        labels:\n          katl.dev/pool:", 1)
			}
			if test.inherited {
				source = strings.Replace(source, "  defaults:\n", "  defaults:\n    kubernetes:\n      kubelet:\n        configFile: ./kubelet.yaml\n", 1)
			}
			path := filepath.Join(dir, "cluster.yaml")
			writeFile(t, path, source)
			archive, built, err := BuildArchive(BuildRequest{SourcePath: path, Planning: PlanningInputs{Nodes: []string{test.node}}})
			if err != nil {
				t.Fatal(err)
			}
			features := built.Manifest.Compatibility.RequiredInstallerFeatures
			if got := slices.Contains(features, "shared-kubelet-base-v1"); got != test.required {
				t.Fatalf("required installer features = %v, shared base required = %v", features, test.required)
			}
			if built.Manifest.BundleSchemaVersion != 1 {
				t.Fatalf("bundle schema = %d", built.Manifest.BundleSchemaVersion)
			}
			selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: test.node, AllowMissingKatlosImage: true})
			if err != nil {
				t.Fatal(err)
			}
			ref := selected.InstallManifest.Node.Kubernetes.Kubeadm.ConfigRef
			if selected.KubeadmConfigs[ref].SharedKubeletBase != test.required {
				t.Fatalf("selected native input = %+v", selected.KubeadmConfigs[ref])
			}
		})
	}
}

func TestSelectedInputRequiresInstallerFeature(t *testing.T) {
	for _, test := range []struct {
		name                             string
		marked, declared, legacy, unused bool
	}{
		{name: "declared marked", marked: true, declared: true},
		{name: "undeclared marked", marked: true},
		{name: "plain"},
		{name: "unselected marked", unused: true},
		{name: "retained legacy", marked: true, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source := validSourceConfig()
			if test.marked || test.unused {
				writeFile(t, filepath.Join(dir, "kubelet.yaml"), "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nhealthzPort: 0\n")
				if test.unused {
					source = strings.Replace(source, "      kubernetes:\n        labels:\n          katl.dev/pool:", "      kubernetes:\n        kubelet:\n          configFile: ./kubelet.yaml\n        labels:\n          katl.dev/pool:", 1)
				} else {
					source = strings.Replace(source, "  defaults:\n", "  defaults:\n    kubernetes:\n      kubelet:\n        configFile: ./kubelet.yaml\n", 1)
				}
			}
			path := filepath.Join(dir, "cluster.yaml")
			writeFile(t, path, source)
			archive, _, err := BuildArchive(BuildRequest{SourcePath: path})
			if err != nil {
				t.Fatal(err)
			}
			archive = mutateBundleManifest(t, archive, func(bundle *BundleManifest) {
				if !test.declared {
					bundle.Compatibility.RequiredInstallerFeatures = []string{"config-bundle-v1"}
				}
				if test.legacy {
					for _, node := range bundle.Nodes {
						for _, input := range node.KubeadmInputs {
							delete(input.Annotations, "dev.katl.kubeadm.shared-kubelet-base")
						}
					}
				}
			})

			selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
			if test.marked && !test.declared && !test.legacy {
				if err == nil || !strings.Contains(err.Error(), "recompile the original ClusterConfig") || !strings.Contains(err.Error(), "current katlctl") {
					t.Fatalf("undeclared shared input error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ref := selected.InstallManifest.Node.Kubernetes.Kubeadm.ConfigRef
			plan := selected.KubeadmConfigs[ref]
			if plan.SharedKubeletBase != (test.marked && !test.legacy) || plan.NodeLocalKubelet != test.marked {
				t.Fatalf("retained input metadata = shared %v, local %v", plan.SharedKubeletBase, plan.NodeLocalKubelet)
			}
		})
	}
}
