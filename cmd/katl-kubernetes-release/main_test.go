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

func TestPrepareSupportedAddsPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supported-versions.json")
	if err := os.WriteFile(path, []byte(`{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KubernetesSupportedVersions",
  "recipeDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "versions": [{
    "payloadVersion": "v1.36.0",
    "artifactRevision": 3,
    "packages": {
      "kubeadm": "0:1.36.0-1",
      "kubelet": "0:1.36.0-1",
      "kubectl": "0:1.36.0-1",
      "criTools": "0:1.36.0-1"
    }
  }]
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{
		"kubeadm":   "0:1.36.1-150500.1.1",
		"kubelet":   "0:1.36.1-150500.1.1",
		"kubectl":   "0:1.36.1-150500.1.1",
		"cri-tools": "0:1.36.0-150500.1.1",
	}
	query := func(name, selector, baseURL, command string) (string, error) {
		return versions[name], nil
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"prepare-supported", "--supported-versions", path, "--payload-version", "v1.36.1"}, &stdout, &stderr, query); err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"payloadVersion": "v1.36.0"`,
		`"payloadVersion": "v1.36.1"`,
		`"artifactRevision": 1`,
		`"kubeadm": "0:1.36.1-150500.1.1"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("updated supported versions missing %q:\n%s", want, data)
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

func TestMatrixSelectsSupportedPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supported-versions.json")
	if err := os.WriteFile(path, []byte(`{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KubernetesSupportedVersions",
  "recipeDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "versions": [{
    "payloadVersion": "v1.35.9",
    "artifactRevision": 3,
    "packages": {
      "kubeadm": "0:1.35.9-1",
      "kubelet": "0:1.35.9-1",
      "kubectl": "0:1.35.9-1",
      "criTools": "0:1.35.0-1"
    }
  }, {
    "payloadVersion": "v1.36.3",
    "artifactRevision": 2,
    "packages": {
      "kubeadm": "0:1.36.3-2",
      "kubelet": "0:1.36.3-2",
      "kubectl": "0:1.36.3-2",
      "criTools": "0:1.36.0-1"
    }
  }]
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"matrix", "--supported-versions", path, "--payload-version", "v1.36.3"}, &stdout, &stderr, nil); err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	for _, want := range []string{
		`"payloadVersion":"v1.36.3"`,
		`"artifactVersion":"v1.36.3-katl.2"`,
		`"minor":"v1.36"`,
		`"kubeadmVersion":"0:1.36.3-2"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("matrix missing %q: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "v1.35.9") {
		t.Fatalf("matrix included unselected payload: %s", stdout.String())
	}
}

func TestMatrixSelectsChangedPayloads(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	previousPath := filepath.Join(dir, "previous.json")
	current := `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KubernetesSupportedVersions",
  "recipeDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "versions": [{
    "payloadVersion": "v1.36.0",
    "artifactRevision": 2,
    "packages": {
      "kubeadm": "0:1.36.0-1",
      "kubelet": "0:1.36.0-1",
      "kubectl": "0:1.36.0-1",
      "criTools": "0:1.36.0-1"
    }
  }, {
    "payloadVersion": "v1.36.1",
    "artifactRevision": 1,
    "packages": {
      "kubeadm": "0:1.36.1-1",
      "kubelet": "0:1.36.1-1",
      "kubectl": "0:1.36.1-1",
      "criTools": "0:1.36.0-1"
    }
  }]
}
`
	previous := strings.Replace(current, `"artifactRevision": 2`, `"artifactRevision": 1`, 1)
	if err := os.WriteFile(currentPath, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previousPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"matrix", "--supported-versions", currentPath, "--previous-supported-versions", previousPath}, &stdout, &stderr, nil); err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"payloadVersion":"v1.36.0"`) {
		t.Fatalf("matrix missing changed payload: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "v1.36.1") {
		t.Fatalf("matrix included unchanged payload: %s", stdout.String())
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

func TestRecordCompatibilityRejectsMalformedArtifactVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KubernetesCompatibilityCatalog",
  "entries": []
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{
		"v1x36x3-katl.2",
		"v1.36.3-katl.0",
		"v1.36.3-katl.one",
		"v1.36.3-katl.2-extra",
		"v1.36.4-katl.2",
	} {
		t.Run(artifact, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{
				"record-compatibility",
				"--catalog", path,
				"--payload-version", "v1.36.3",
				"--artifact-version", artifact,
				"--manifest-digest", "sha256:" + strings.Repeat("a", 64),
			}, &stdout, &stderr, nil)
			if err == nil || !strings.Contains(err.Error(), "artifact-version") {
				t.Fatalf("run() error = %v", err)
			}
		})
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
