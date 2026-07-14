package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testManifest = `: "${KATL_KUBERNETES_MINOR:=v1.36}"
KATL_KUBERNETES_PAYLOAD_DEFAULT=v1.36.0
KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT=4
: "${KATL_KUBERNETES_KUBEADM_VERSION:=0:1.36.0-1}"
: "${KATL_KUBERNETES_KUBELET_VERSION:=0:1.36.0-1}"
: "${KATL_KUBERNETES_KUBECTL_VERSION:=0:1.36.0-1}"
: "${KATL_KUBERNETES_CRITOOLS_VERSION:=0:1.36.0-1}"
`

func TestPrepare(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubernetes.env")
	if err := os.WriteFile(path, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{
		"kubeadm":   "0:1.36.1-150500.1.1",
		"kubelet":   "0:1.36.1-150500.1.1",
		"kubectl":   "0:1.36.1-150500.1.1",
		"cri-tools": "0:1.36.0-150500.1.1",
	}
	query := func(name, selector, baseURL, command string) (string, error) {
		if baseURL != "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/" || command != "dnf" {
			t.Fatalf("query %s baseURL=%q command=%q", name, baseURL, command)
		}
		return versions[name], nil
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"prepare", "--manifest", path, "--payload-version", "v1.36.1"}, &stdout, &stderr, query); err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"KATL_KUBERNETES_PAYLOAD_DEFAULT=v1.36.1",
		"KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT=1",
		"KATL_KUBERNETES_KUBEADM_VERSION:=0:1.36.1-150500.1.1",
		"KATL_KUBERNETES_CRITOOLS_VERSION:=0:1.36.0-150500.1.1",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("updated manifest missing %q:\n%s", want, data)
		}
	}
}

func TestIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubernetes.env")
	if err := os.WriteFile(path, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"identity", "--manifest", path}, &stdout, &stderr, nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	want := `{"payloadVersion":"v1.36.0","artifactVersion":"v1.36.0-katl.4","image":"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.4"}`
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("identity = %s, want %s", stdout.String(), want)
	}
}

func TestRecordCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KubernetesCompatibilityCatalog",
  "entries": [{
    "kubernetesVersion": "v1.36.0",
    "bundle": "ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "architectures": ["x86_64"],
    "runtimeInterfaces": ["katl-runtime-1"]
  }]
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	digest := "sha256:" + strings.Repeat("b", 64)
	if err := run([]string{
		"record-compatibility",
		"--catalog", path,
		"--payload-version", "v1.36.1",
		"--artifact-version", "v1.36.1-katl.2",
		"--manifest-digest", digest,
		"--architecture", "x86_64",
		"--runtime-interfaces", "katl-runtime-1, katl-runtime-legacy",
	}, &stdout, &stderr, nil); err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"kubernetesVersion": "v1.36.0"`,
		`"kubernetesVersion": "v1.36.1"`,
		`"bundle": "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.2@` + digest + `"`,
		`"katl-runtime-legacy"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("updated catalog missing %q:\n%s", want, data)
		}
	}
}

func TestPrepareRejectsPackageMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubernetes.env")
	if err := os.WriteFile(path, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	query := func(name, selector, baseURL, command string) (string, error) {
		return "0:1.36.0-1", nil
	}
	if err := run([]string{"prepare", "--manifest", path, "--payload-version", "v1.36.1"}, &bytes.Buffer{}, &bytes.Buffer{}, query); err == nil || !strings.Contains(err.Error(), "want Kubernetes 1.36.1") {
		t.Fatalf("run() error = %v", err)
	}
}
