package config

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestCompiledKubeletConfig(t *testing.T) {
	plan, err := kubeadmconfig.PlanFromRenderedFiles("node-cp-1", []kubeadmconfig.File{
		{RenderPath: "/etc/katl/kubeadm/node-cp-1/config.yaml", Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n")},
		{RenderPath: "/etc/katl/kubeadm/node-cp-1/patches/kubeletconfiguration999+merge.yaml", Content: []byte("maxPods: 111\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.NodeLocalKubelet = true
	data, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{
		NodeName: "cp-1", SourceID: "lab", DesiredVersion: "2", ApplyMode: "live", KubeadmOnly: true,
		Manifest:       manifest.Manifest{Node: manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "node-cp-1"}}}},
		KubeadmConfigs: map[string]kubeadmconfig.Plan{"node-cp-1": plan},
	})
	if err != nil {
		t.Fatal(err)
	}

	if result := ValidateNodeConfigurationChange(string(data), Options{CheckKubeadmRefs: true}); !result.Accepted() {
		t.Fatalf("compiled configuration rejected: %v", result.Strings())
	}
	decoded, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(string(data)), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.KubeadmConfigs["node-cp-1"]
	if !got.NodeLocalKubelet || len(got.Patches) != 1 || string(got.Patches[0].Content) != "maxPods: 111\n" {
		t.Fatalf("node-local kubelet input lost: %#v", got)
	}
}

func TestKubeadmValidationContract(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		accepted     bool
	}{
		{"node-local", "      nodeLocalKubelet: true\n      patches:\n        kubeletconfiguration999+merge.yaml: |\n          maxPods: 111\n", true},
		{"nested patch", "      patches:\n        nested/kube-apiserver+merge.yaml: |\n          metadata: {}\n", true},
		{"unknown field", "      unknown: true\n", false},
		{"invalid boolean", "      nodeLocalKubelet: sometimes\n", false},
		{"traversal", "      patches:\n        ../config.yaml: 'kind: Unexpected'\n", false},
		{"normalized traversal", "      patches:\n        nested/../patch.yaml: 'metadata: {}'\n", false},
		{"absolute path", "      patches:\n        /patch.yaml: 'metadata: {}'\n", false},
		{"empty patch", "      patches:\n        kubeletconfiguration999+merge.yaml: ''\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := "apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\nspec:\n  kubeadmConfigs:\n    control-plane:\n      config: |\n        apiVersion: kubeadm.k8s.io/v1beta4\n        kind: ClusterConfiguration\n        kubernetesVersion: v1.36.1\n" + tc.fields
			result := ValidateNodeConfigurationChange(document, Options{})
			if result.Accepted() != tc.accepted {
				t.Fatalf("validation accepted=%v: %v", result.Accepted(), result.Strings())
			}
			_, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(document), configapply.TrustedBundleRequest{})
			if (err == nil) != tc.accepted {
				t.Fatalf("runtime decode error=%v, want accepted=%v", err, tc.accepted)
			}
		})
	}
}
