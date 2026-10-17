package configbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubeletPatchPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kubeadm.yaml"), "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")
	writeFile(t, filepath.Join(dir, "node.yaml"), "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nhealthzPort: 0\n")
	for _, sub := range []string{"a", "zzzz"} {
		writeFile(t, filepath.Join(dir, "patches", sub, "kubeletconfigurationzz+merge.yaml"), "maxPods: 121\n")
	}
	source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml\n      patchesDir: ./patches", 1)
	source = strings.Replace(source, "  defaults:\n", "  defaults:\n    kubernetes:\n      kubelet:\n        configFile: ./node.yaml\n", 1)
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, source)
	archive, first, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	plan := selected.KubeadmConfigs["node-cp-1"]
	if !plan.SharedKubeletBase || !strings.Contains(string(plan.Config.Content), "maxPods: 110") || strings.Contains(string(plan.Config.Content), "healthzPort:") {
		t.Fatalf("shared base = %#v", plan)
	}
	want := map[string]string{
		"/etc/katl/kubeadm/node-cp-1/patches/00-native/a/kubeletconfigurationzz+merge.yaml":    "maxPods: 121\n",
		"/etc/katl/kubeadm/node-cp-1/patches/00-native/zzzz/kubeletconfigurationzz+merge.yaml": "maxPods: 121\n",
		"/etc/katl/kubeadm/node-cp-1/patches/10-node/kubeletconfiguration+merge.yaml":          "healthzPort: 0\n",
	}
	if len(plan.Patches) != len(want) {
		t.Fatalf("patches = %#v", plan.Patches)
	}
	for _, patch := range plan.Patches {
		if want[patch.RenderPath] != string(patch.Content) {
			t.Fatalf("patch %s = %q", patch.RenderPath, patch.Content)
		}
	}
	if err := os.Rename(filepath.Join(dir, "patches/a"), filepath.Join(dir, "patches/b")); err != nil {
		t.Fatal(err)
	}
	_, moved, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.Source.SourceDigest == moved.Manifest.Source.SourceDigest {
		t.Fatal("moving native patch changed ordering but not source identity")
	}
}
