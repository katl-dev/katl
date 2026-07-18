package kubeadmconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/confext"
)

func TestResolveAcceptsInitConfigAndPatches(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "kubeadm", "control-plane.yaml"), initConfig())
	writeFile(t, filepath.Join(repo, "kubeadm", "patches", "control-plane", "kube-apiserver0+merge.yaml"), "spec:\n  containers: []\n")

	object := Object{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "control-plane"},
		Spec: Spec{
			ConfigFile:        "kubeadm/control-plane.yaml",
			PatchesDir:        "kubeadm/patches/control-plane",
			KubernetesVersion: "v1.34.8",
		},
	}
	plan, err := Resolve(ResolveRequest{RepoRoot: repo, Object: object, KubernetesVersion: "v1.34.8"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if plan.Config.RenderPath != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("config render path = %q", plan.Config.RenderPath)
	}
	if len(plan.Patches) != 1 || plan.Patches[0].RenderPath != "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml" {
		t.Fatalf("patches = %#v", plan.Patches)
	}
	gotDocs := docKinds(plan.Documents)
	wantDocs := []string{
		"kubeadm.k8s.io/v1beta4/InitConfiguration",
		"kubeadm.k8s.io/v1beta4/ClusterConfiguration",
		"kubelet.config.k8s.io/v1beta1/KubeletConfiguration",
	}
	if !reflect.DeepEqual(gotDocs, wantDocs) {
		t.Fatalf("documents = %#v, want %#v", gotDocs, wantDocs)
	}
	files := plan.NativeEtcFiles()
	if len(files) != 2 || files[0].Path != plan.Config.RenderPath || files[1].Path != plan.Patches[0].RenderPath {
		t.Fatalf("native files = %#v", files)
	}
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: t.TempDir(),
		GenerationID:    "2026.06.04-001",
		Files:           files,
		Extension: confext.ExtensionRelease{
			Name:         "katl-node",
			ID:           "katlos",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
		Chown: func(string, int, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("RenderGenerationTree() error = %v", err)
	}
	assertFile(t, filepath.Join(tree.ConfextDir, "etc", "katl", "kubeadm", "control-plane", "config.yaml"), initConfig())
	assertFile(t, filepath.Join(tree.ConfextDir, "etc", "katl", "kubeadm", "control-plane", "patches", "kube-apiserver0+merge.yaml"), "spec:\n  containers: []\n")
}

func TestPlanFromRenderedFilesReconstructsStoredInput(t *testing.T) {
	plan, err := PlanFromRenderedFiles("control-plane", []File{
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml",
			Content:    []byte("spec:\n  containers: []\n"),
			Mode:       0o640,
		},
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
			Content:    []byte(initConfig()),
			Mode:       0o600,
		},
	})
	if err != nil {
		t.Fatalf("PlanFromRenderedFiles() error = %v", err)
	}
	if plan.Name != "control-plane" || plan.Config.RenderPath != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("plan config = %+v", plan.Config)
	}
	if len(plan.Patches) != 1 || plan.Patches[0].RenderPath != "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml" {
		t.Fatalf("patches = %+v", plan.Patches)
	}
	if gotDocs := docKinds(plan.Documents); !reflect.DeepEqual(gotDocs, []string{
		"kubeadm.k8s.io/v1beta4/InitConfiguration",
		"kubeadm.k8s.io/v1beta4/ClusterConfiguration",
		"kubelet.config.k8s.io/v1beta1/KubeletConfiguration",
	}) {
		t.Fatalf("documents = %#v", gotDocs)
	}
}

func TestPlanFromRenderedFilesRejectsUnsafePatch(t *testing.T) {
	_, err := PlanFromRenderedFiles("control-plane", []File{
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
			Content:    []byte(initConfig()),
			Mode:       0o600,
		},
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml",
			Content:    []byte("apiVersion: v1\nkind: Pod\nspec:\n  volumes:\n    - name: host\n      hostPath:\n        path: /etc/kubernetes\n"),
			Mode:       0o640,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "host path /etc/kubernetes is denied") {
		t.Fatalf("PlanFromRenderedFiles() error = %v, want unsafe patch rejection", err)
	}
}

func TestResolveAcceptsJoinConfig(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "kubeadm", "worker.yaml"), joinConfig())
	object := Object{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "worker"},
		Spec:       Spec{ConfigFile: "kubeadm/worker.yaml"},
	}
	plan, err := Resolve(ResolveRequest{RepoRoot: repo, Object: object})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got, want := docKinds(plan.Documents), []string{"kubeadm.k8s.io/v1beta4/JoinConfiguration"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("documents = %#v, want %#v", got, want)
	}
}

func TestDecodeRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	_, err := Decode(strings.NewReader(`apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: control-plane
spec:
  configFile: kubeadm/control-plane.yaml
  extra: nope
`))
	if err == nil || !strings.Contains(err.Error(), "field extra not found") {
		t.Fatalf("Decode() error = %v, want unknown field", err)
	}

	_, err = Decode(strings.NewReader(`apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: control-plane
spec:
  configFile: kubeadm/control-plane.yaml
---
kind: KubeadmConfig
`))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("Decode() error = %v, want multiple documents", err)
	}
}

func TestResolveRejectsUnsafeInputs(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "kubeadm", "bad.yaml"), strings.Replace(initConfig(), "cgroupDriver: systemd", "staticPodPath: /etc/kubernetes/manifests", 1))
	writeFile(t, filepath.Join(repo, "kubeadm", "usr.yaml"), strings.Replace(initConfig(), "cgroupDriver: systemd", "staticPodPath: /usr/lib/kubernetes/manifests", 1))
	writeFile(t, filepath.Join(repo, "kubeadm", "unknown.yaml"), `apiVersion: kubeadm.k8s.io/v1beta4
kind: ResetConfiguration
`)
	writeFile(t, filepath.Join(repo, "kubeadm", "patch-dir.yaml"), strings.Replace(initConfig(), "nodeRegistration:", "patches:\n  directory: /tmp/patches\nnodeRegistration:", 1))
	tests := []struct {
		name   string
		object Object
		want   string
	}{
		{
			name:   "bad name",
			object: kubeadmObject("../bad", "kubeadm/bad.yaml"),
			want:   "single path segment",
		},
		{
			name:   "leading dash name",
			object: kubeadmObject("-control-plane", "kubeadm/bad.yaml"),
			want:   "must start and end",
		},
		{
			name:   "trailing dash name",
			object: kubeadmObject("control-plane-", "kubeadm/bad.yaml"),
			want:   "must start and end",
		},
		{
			name:   "padded name",
			object: kubeadmObject(" control-plane", "kubeadm/bad.yaml"),
			want:   "must not contain leading or trailing whitespace",
		},
		{
			name:   "too long name",
			object: kubeadmObject(strings.Repeat("a", 64), "kubeadm/bad.yaml"),
			want:   "63 characters or fewer",
		},
		{
			name:   "absolute config",
			object: kubeadmObject("control-plane", "/etc/kubeadm.yaml"),
			want:   "repository-relative",
		},
		{
			name:   "path traversal config",
			object: kubeadmObject("control-plane", "../outside.yaml"),
			want:   "path traversal",
		},
		{
			name:   "unsupported document",
			object: kubeadmObject("control-plane", "kubeadm/unknown.yaml"),
			want:   "unsupported kubeadm YAML document",
		},
		{
			name:   "denied host path",
			object: kubeadmObject("control-plane", "kubeadm/bad.yaml"),
			want:   "host path /etc/kubernetes/manifests is denied",
		},
		{
			name:   "other denied host path",
			object: kubeadmObject("control-plane", "kubeadm/usr.yaml"),
			want:   "host path /usr/lib/kubernetes/manifests is denied",
		},
		{
			name:   "patch directory mismatch",
			object: kubeadmObject("control-plane", "kubeadm/patch-dir.yaml"),
			want:   "patches.directory must be /etc/katl/kubeadm/control-plane/patches",
		},
		{
			name: "version mismatch",
			object: Object{
				APIVersion: APIVersion,
				Kind:       Kind,
				Metadata:   Metadata{Name: "control-plane"},
				Spec:       Spec{ConfigFile: "kubeadm/bad.yaml", KubernetesVersion: "v1.33.0"},
			},
			want: "does not match selected Kubernetes version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(ResolveRequest{RepoRoot: repo, Object: tt.object, KubernetesVersion: "v1.34.8"})
			if err == nil {
				t.Fatalf("Resolve() error = nil, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveRejectsUnsafePatches(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "kubeadm", "control-plane.yaml"), initConfig())
	patchDir := filepath.Join(repo, "kubeadm", "patches")
	if err := os.MkdirAll(patchDir, 0o755); err != nil {
		t.Fatalf("mkdir patches: %v", err)
	}
	if err := os.Symlink("../control-plane.yaml", filepath.Join(patchDir, "link.yaml")); err != nil {
		t.Fatalf("symlink patch: %v", err)
	}
	_, err := Resolve(ResolveRequest{
		RepoRoot: repo,
		Object: Object{
			APIVersion: APIVersion,
			Kind:       Kind,
			Metadata:   Metadata{Name: "control-plane"},
			Spec:       Spec{ConfigFile: "kubeadm/control-plane.yaml", PatchesDir: "kubeadm/patches"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Resolve() error = %v, want symlink rejection", err)
	}
}

func TestResolveRejectsSymlinkedParentEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "control-plane.yaml"), initConfig())
	if err := os.Symlink(outside, filepath.Join(repo, "kubeadm")); err != nil {
		t.Fatalf("symlink kubeadm dir: %v", err)
	}
	_, err := Resolve(ResolveRequest{
		RepoRoot: repo,
		Object:   kubeadmObject("control-plane", "kubeadm/control-plane.yaml"),
	})
	if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
		t.Fatalf("Resolve() error = %v, want symlink escape rejection", err)
	}
}

func TestResolveRejectsDeniedHostPathsInPatchContents(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "kubeadm", "control-plane.yaml"), initConfig())
	writeFile(t, filepath.Join(repo, "kubeadm", "patches", "kube-apiserver0+merge.yaml"), `spec:
  volumes:
    - name: host
      hostPath:
        path: /etc/kubernetes
`)
	_, err := Resolve(ResolveRequest{
		RepoRoot: repo,
		Object: Object{
			APIVersion: APIVersion,
			Kind:       Kind,
			Metadata:   Metadata{Name: "control-plane"},
			Spec:       Spec{ConfigFile: "kubeadm/control-plane.yaml", PatchesDir: "kubeadm/patches"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "host path /etc/kubernetes is denied") {
		t.Fatalf("Resolve() error = %v, want denied patch path rejection", err)
	}
}

func TestPlanFromRenderedFilesAllowsStandardCertificatesDirectory(t *testing.T) {
	_, err := PlanFromRenderedFiles("control-plane", []File{{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
		Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\ncertificatesDir: /etc/kubernetes/pki\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func kubeadmObject(name, configFile string) Object {
	return Object{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: name},
		Spec:       Spec{ConfigFile: configFile},
	}
}

func docKinds(documents []Document) []string {
	got := make([]string, 0, len(documents))
	for _, document := range documents {
		got = append(got, document.APIVersion+"/"+document.Kind)
	}
	return got
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFile(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func initConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`
}

func joinConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
discovery:
  file:
    kubeConfigPath: /etc/katl/kubeadm/join-discovery.yaml
`
}
