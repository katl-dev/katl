package configbundle

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildArchiveWritesDeterministicBundle(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, validSourceConfig())

	first, firstResult, err := BuildArchive(BuildRequest{SourcePath: sourcePath, KatlctlVersion: "test", KatlctlCommit: "abc123"})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	second, secondResult, err := BuildArchive(BuildRequest{SourcePath: sourcePath, KatlctlVersion: "test", KatlctlCommit: "abc123"})
	if err != nil {
		t.Fatalf("BuildArchive() second error = %v", err)
	}
	if !bytes.Equal(first, second) || firstResult.Digest != secondResult.Digest {
		t.Fatalf("bundle output is not deterministic")
	}
	files := readTarFiles(t, first)
	for _, name := range []string{"oci-layout", "index.json"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("archive missing %s", name)
		}
	}
	manifestBlob := "blobs/sha256/" + strings.TrimPrefix(firstResult.Digest, "sha256:")
	manifestData, ok := files[manifestBlob]
	if !ok {
		t.Fatalf("archive missing manifest blob %s", manifestBlob)
	}
	var bundle BundleManifest
	if err := json.Unmarshal(manifestData, &bundle); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if bundle.Kind != "KatlConfigBundle" || bundle.ClusterName != "lab" || len(bundle.Nodes) != 2 {
		t.Fatalf("bundle manifest = %#v", bundle)
	}
	if bundle.Nodes[0].NodeClass != "ms01" || bundle.Nodes[0].InstallMaterial.Digest == "" {
		t.Fatalf("control-plane node record = %#v", bundle.Nodes[0])
	}
	if len(bundle.Nodes[0].KubeadmInputs) != 1 || bundle.Nodes[0].KubeadmInputs[0].Annotations["dev.katl.kubeadm.resolved-id"] != "control-plane" {
		t.Fatalf("control-plane kubeadm inputs = %#v", bundle.Nodes[0].KubeadmInputs)
	}
	if bundle.Cluster.KubernetesPayloads[0].BundleManifestDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("kubernetes payloads = %#v", bundle.Cluster.KubernetesPayloads)
	}
	if !hasDescriptor(bundle.Descriptors, "source-normalized", "source/cluster.normalized.yaml") ||
		!hasDescriptor(bundle.Descriptors, "node-install-material", "nodes/cp-1/install/material.json") ||
		!hasDescriptor(bundle.Descriptors, "node-native-config", "nodes/worker-1/config/native.json") {
		t.Fatalf("descriptors missing expected members: %#v", bundle.Descriptors)
	}
	for _, desc := range bundle.Descriptors {
		blobPath := "blobs/sha256/" + strings.TrimPrefix(desc.Digest, "sha256:")
		if _, ok := files[blobPath]; !ok {
			t.Fatalf("descriptor %s missing blob %s", desc.FileName, blobPath)
		}
	}
}

func TestBuildArchiveRejectsUnsafeDiskDefaults(t *testing.T) {
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, strings.Replace(validSourceConfig(), "minSizeMiB: 65536", "serial: shared-root", 1))})
	if err == nil || !strings.Contains(err.Error(), "targetDiskDefaults must not set byID, wwn, or serial") {
		t.Fatalf("BuildArchive() error = %v, want unsafe disk defaults", err)
	}
}

func TestBuildArchiveResolvesKubeadmConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kubeadm", "control-plane.yaml"), controlPlaneKubeadmConfig())
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, strings.Replace(validSourceConfig(), controlPlaneInlineSource(), "    control-plane:\n      configFile: kubeadm/control-plane.yaml\n", 1))

	_, result, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	if result.Manifest.Nodes[0].KubeadmInputs[0].Digest == "" {
		t.Fatalf("control-plane kubeadm inputs = %#v", result.Manifest.Nodes[0].KubeadmInputs)
	}
}

func TestBuildArchiveRejectsUnsafeKubeadmConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kubeadm", "control-plane.yaml"), strings.Replace(controlPlaneKubeadmConfig(), "nodeRegistration:", "staticPodPath: /etc/kubernetes/manifests\nnodeRegistration:", 1))
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, strings.Replace(validSourceConfig(), controlPlaneInlineSource(), "    control-plane:\n      configFile: kubeadm/control-plane.yaml\n", 1))

	_, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err == nil || !strings.Contains(err.Error(), "host path /etc/kubernetes/manifests is denied") {
		t.Fatalf("BuildArchive() error = %v, want unsafe host path rejection", err)
	}
}

func TestBuildArchiveRejectsUnknownTemplateFields(t *testing.T) {
	path := writeSource(t, `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  nodeTemplate:
    count: 3
`)
	_, _, err := BuildArchive(BuildRequest{SourcePath: path})
	if err == nil || !strings.Contains(err.Error(), "field nodeTemplate not found") {
		t.Fatalf("BuildArchive() error = %v, want unknown template field", err)
	}
}

func readTarFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read tar: %v", err)
		}
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(tr); err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		files[header.Name] = buf.Bytes()
	}
	return files
}

func hasDescriptor(descriptors []Descriptor, role, fileName string) bool {
	for _, desc := range descriptors {
		if desc.Role == role && desc.FileName == fileName {
			return true
		}
	}
	return false
}

func writeSource(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	writeFile(t, path, content)
	return path
}

func validSourceConfig() string {
	return `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  controlPlaneEndpoint: api.katl.test:6443
  kubernetes:
    version: v1.36.1
    bundle:
      source: https://artifacts.example.test/kubernetes
      ref: v1.36.1@sha256:` + strings.Repeat("b", 64) + `
  katlosImage:
    url: https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs
    sha256: ` + strings.Repeat("a", 64) + `
    sizeBytes: 1073741824
    version: 2026.06.04
    architecture: x86_64
    runtimeInterface: katl-runtime-1
    role: install
  defaults:
    install:
      wipeTarget: true
      targetDiskDefaults:
        minSizeMiB: 32768
      extraDisks:
        - name: data
          selector:
            byID: /dev/disk/by-id/ata-data
          filesystem: xfs
          mount:
            path: /srv/data
    identity:
      ssh:
        authorizedKeys:
          - ` + testSSHKey + `
    networkd:
      files:
        - name: 10-common.network
          content: |
            [Match]
            Name=enp1s0

            [Network]
            DHCP=yes
    bootstrap:
      access:
        method: agent
        credentialRef: vsock:1234:10240
  nodeClasses:
    ms01:
      install:
        targetDiskDefaults:
          minSizeMiB: 65536
      networkd:
        files:
          - name: 15-ms01.network
            content: |
              [Match]
              Name=enp3s0
      kubernetes:
        labels:
          katl.dev/hardware-class: ms01
  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadm:
          configRef: control-plane
    worker:
      kubernetes:
        kubeadm:
          configRef: worker
  kubeadmConfigs:
    control-plane:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: InitConfiguration
        nodeRegistration:
          criSocket: unix:///run/containerd/containerd.sock
        ---
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.36.1
    worker:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: JoinConfiguration
        nodeRegistration:
          criSocket: unix:///run/containerd/containerd.sock
  nodes:
    - name: cp-1
      systemRole: control-plane
      nodeClass: ms01
      overrides:
        bootstrap:
          address: 10.0.0.11
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-cp-root
    - name: worker-1
      systemRole: worker
      overrides:
        bootstrap:
          address: 10.0.0.21
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-worker-root
`
}

const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func controlPlaneInlineSource() string {
	return `    control-plane:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: InitConfiguration
        nodeRegistration:
          criSocket: unix:///run/containerd/containerd.sock
        ---
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.36.1
`
}

func controlPlaneKubeadmConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`
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
