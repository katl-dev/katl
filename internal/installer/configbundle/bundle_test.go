package configbundle

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type bundleGolden struct {
	ClusterName          string             `json:"clusterName"`
	SourceDigest         string             `json:"sourceDigest"`
	KubernetesPayloadRef string             `json:"kubernetesPayloadRef,omitempty"`
	Nodes                []bundleGoldenNode `json:"nodes"`
}

type bundleGoldenNode struct {
	Name             string   `json:"name"`
	SystemRole       string   `json:"systemRole"`
	NodeClass        string   `json:"nodeClass,omitempty"`
	TargetDiskByID   string   `json:"targetDiskByID"`
	NetworkdFiles    []string `json:"networkdFiles,omitempty"`
	KubeadmInputRefs []string `json:"kubeadmInputRefs,omitempty"`
}

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

func TestBuildArchiveNodeClassGoldenScenarios(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bundleGolden
	}{
		{
			name: "all same hardware",
			raw:  allSameHardwareSourceConfig(),
			want: bundleGolden{
				ClusterName:          "lab",
				SourceDigest:         "non-empty",
				KubernetesPayloadRef: "v1.36.1@sha256:" + strings.Repeat("b", 64),
				Nodes: []bundleGoldenNode{
					{
						Name:             "cp-1",
						SystemRole:       "control-plane",
						NodeClass:        "ms01",
						TargetDiskByID:   "/dev/disk/by-id/ata-cp-root",
						NetworkdFiles:    []string{"10-common.network", "15-ms01.network"},
						KubeadmInputRefs: []string{"control-plane"},
					},
					{
						Name:             "worker-1",
						SystemRole:       "worker",
						NodeClass:        "ms01",
						TargetDiskByID:   "/dev/disk/by-id/ata-worker-root",
						NetworkdFiles:    []string{"10-common.network", "15-ms01.network"},
						KubeadmInputRefs: []string{"worker"},
					},
				},
			},
		},
		{
			name: "mixed ms01 msa2",
			raw:  mixedMS01MSA2SourceConfig(),
			want: bundleGolden{
				ClusterName:          "lab",
				SourceDigest:         "non-empty",
				KubernetesPayloadRef: "v1.36.1@sha256:" + strings.Repeat("b", 64),
				Nodes: []bundleGoldenNode{
					{
						Name:             "cp-1",
						SystemRole:       "control-plane",
						NodeClass:        "ms01",
						TargetDiskByID:   "/dev/disk/by-id/ata-cp-root",
						NetworkdFiles:    []string{"10-common.network", "15-ms01.network"},
						KubeadmInputRefs: []string{"control-plane"},
					},
					{
						Name:             "worker-1",
						SystemRole:       "worker",
						NodeClass:        "msa2",
						TargetDiskByID:   "/dev/disk/by-id/ata-worker-root",
						NetworkdFiles:    []string{"10-common.network", "16-msa2.network"},
						KubeadmInputRefs: []string{"worker"},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive, result, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, tt.raw)})
			if err != nil {
				t.Fatalf("BuildArchive() error = %v", err)
			}
			got := bundleGoldenFromArchive(t, archive, result.Manifest)
			if got.SourceDigest == "" {
				t.Fatalf("source digest is empty")
			}
			got.SourceDigest = "non-empty"
			assertJSONEqual(t, got, tt.want)
		})
	}
}

func TestBuildArchiveRejectsUnsafeDiskDefaults(t *testing.T) {
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, strings.Replace(validSourceConfig(), "minSizeMiB: 65536", "serial: shared-root", 1))})
	if err == nil || !strings.Contains(err.Error(), "targetDiskDefaults must not set byID, wwn, or serial") {
		t.Fatalf("BuildArchive() error = %v, want unsafe disk defaults", err)
	}
}

func TestBuildArchiveRejectsInvalidSourceRefsAndLayering(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "unresolved catalog ref",
			raw: strings.Replace(validSourceConfig(), `version: v1.36.1
    bundle:
      source: https://artifacts.example.test/kubernetes
      ref: v1.36.1@sha256:`+strings.Repeat("b", 64), "catalogRef: missing-catalog", 1),
			want: "invalid Kubernetes sysext catalog",
		},
		{
			name: "unknown node class",
			raw:  strings.Replace(validSourceConfig(), "nodeClass: ms01", "nodeClass: missing", 1),
			want: `nodeClass "missing" is not defined`,
		},
		{
			name: "conflicting networkd output",
			raw: strings.Replace(validSourceConfig(), `overrides:
        bootstrap:`, `overrides:
        networkd:
          files:
            - name: 10-common.network
              content: |
                [Match]
                Name=enp9s0
        bootstrap:`, 1),
			want: "networkd file",
		},
		{
			name: "unsupported node range",
			raw: `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  nodeRange:
    prefix: worker
`,
			want: "field nodeRange not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, tt.raw)})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildArchive() error = %v, want %q", err, tt.want)
			}
		})
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

func TestReadSelectedNodeVerifiesBundleAndSelectsNodeMaterial(t *testing.T) {
	archive, result, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{
		ExpectedDigest: result.Digest,
		NodeName:       "cp-1",
	})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	if selected.BundleDigest != result.Digest || selected.SourceDigest == "" || selected.NodeMaterialDigest == "" || selected.InstallMaterialDigest == "" {
		t.Fatalf("selected digests = %#v", selected)
	}
	if selected.Node.Name != "cp-1" || selected.InstallManifest.Node.Identity.Hostname != "cp-1" {
		t.Fatalf("selected node/install material = %#v / %#v", selected.Node, selected.InstallManifest.Node.Identity)
	}
	if _, ok := selected.KubeadmConfigs["control-plane"]; !ok {
		t.Fatalf("KubeadmConfigs = %#v, want control-plane", selected.KubeadmConfigs)
	}
}

func TestReadSelectedNodeRejectsMissingNodeSelection(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	_, err = ReadSelectedNode(bytes.NewReader(archive), ReadOptions{})
	if err == nil || !strings.Contains(err.Error(), "selected node is required") {
		t.Fatalf("ReadSelectedNode() error = %v, want selected node rejection", err)
	}
}

func TestReadSelectedNodeRejectsBundleDigestMismatch(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	_, err = ReadSelectedNode(bytes.NewReader(archive), ReadOptions{
		ExpectedDigest: "sha256:" + strings.Repeat("0", 64),
		NodeName:       "cp-1",
	})
	if err == nil || !strings.Contains(err.Error(), "config bundle digest mismatch") {
		t.Fatalf("ReadSelectedNode() error = %v, want digest mismatch", err)
	}
}

func TestReadSelectedNodeRejectsAmbiguousSelection(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	mutated := mutateBundleManifest(t, archive, func(bundle *BundleManifest) {
		bundle.Nodes = append(bundle.Nodes, bundle.Nodes[0])
	})

	_, err = ReadSelectedNode(bytes.NewReader(mutated), ReadOptions{NodeName: "cp-1"})
	if err == nil || !strings.Contains(err.Error(), "duplicate selected node") {
		t.Fatalf("ReadSelectedNode() error = %v, want duplicate selected node", err)
	}
}

func TestReadSelectedNodeRejectsIncompatibleBundleMetadata(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*BundleManifest)
		want   string
	}{
		{
			name: "schema",
			mutate: func(bundle *BundleManifest) {
				bundle.BundleSchemaVersion = 2
			},
			want: "unsupported config bundle schema version",
		},
		{
			name: "runtime interface",
			mutate: func(bundle *BundleManifest) {
				bundle.Compatibility.SupportedKatlOSRuntimeInterfaces = []string{"katl-runtime-2"}
			},
			want: "runtime interface",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := mutateBundleManifest(t, archive, tt.mutate)
			_, err = ReadSelectedNode(bytes.NewReader(mutated), ReadOptions{NodeName: "cp-1"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadSelectedNode() error = %v, want %q", err, tt.want)
			}
		})
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

func mutateBundleManifest(t *testing.T, archive []byte, mutate func(*BundleManifest)) []byte {
	t.Helper()
	files := readTarFiles(t, archive)
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("index manifests = %#v", index.Manifests)
	}
	oldDigest := index.Manifests[0].Digest
	oldPath := "blobs/sha256/" + strings.TrimPrefix(oldDigest, "sha256:")
	var bundle BundleManifest
	if err := json.Unmarshal(files[oldPath], &bundle); err != nil {
		t.Fatalf("decode bundle manifest: %v", err)
	}
	mutate(&bundle)
	data, err := marshalCanonical(bundle)
	if err != nil {
		t.Fatalf("marshal mutated bundle manifest: %v", err)
	}
	newDigest := digestBytes(data)
	members := []member{{
		descriptor: Descriptor{Digest: newDigest},
		data:       data,
	}}
	for name, data := range files {
		if !strings.HasPrefix(name, "blobs/sha256/") || name == oldPath {
			continue
		}
		digest := "sha256:" + strings.TrimPrefix(name, "blobs/sha256/")
		members = append(members, member{
			descriptor: Descriptor{Digest: digest},
			data:       data,
		})
	}
	out, err := writeOCIArchive(newDigest, members)
	if err != nil {
		t.Fatalf("write mutated archive: %v", err)
	}
	return out
}

func bundleGoldenFromArchive(t *testing.T, archive []byte, manifest BundleManifest) bundleGolden {
	t.Helper()
	oci, err := readOCIArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	out := bundleGolden{
		ClusterName:          manifest.ClusterName,
		SourceDigest:         manifest.Source.SourceDigest,
		KubernetesPayloadRef: manifest.Cluster.KubernetesPayloads[0].Ref,
	}
	for _, node := range manifest.Nodes {
		out.Nodes = append(out.Nodes, bundleGoldenNode{
			Name:             node.Name,
			SystemRole:       node.SystemRole,
			NodeClass:        node.NodeClass,
			TargetDiskByID:   targetDiskByIDFromDescriptor(t, oci, node.InstallMaterial),
			NetworkdFiles:    networkdNamesFromDescriptor(t, oci, node.NativeConfig),
			KubeadmInputRefs: kubeadmInputRefs(node.KubeadmInputs),
		})
	}
	return out
}

func targetDiskByIDFromDescriptor(t *testing.T, archive ociArchive, desc Descriptor) string {
	t.Helper()
	var install struct {
		Install struct {
			TargetDisk struct {
				ByID string `json:"byID"`
			} `json:"targetDisk"`
		} `json:"install"`
	}
	data, err := archive.descriptorData(desc)
	if err != nil {
		t.Fatalf("read install descriptor %s: %v", desc.FileName, err)
	}
	if err := json.Unmarshal(data, &install); err != nil {
		t.Fatalf("decode install descriptor %s: %v", desc.FileName, err)
	}
	return install.Install.TargetDisk.ByID
}

func networkdNamesFromDescriptor(t *testing.T, archive ociArchive, desc Descriptor) []string {
	t.Helper()
	var files []struct {
		Path string `json:"path"`
	}
	data, err := archive.descriptorData(desc)
	if err != nil {
		t.Fatalf("read native config descriptor %s: %v", desc.FileName, err)
	}
	if err := json.Unmarshal(data, &files); err != nil {
		t.Fatalf("decode native config descriptor %s: %v", desc.FileName, err)
	}
	var names []string
	for _, file := range files {
		if strings.HasPrefix(file.Path, "/etc/systemd/network/") && strings.HasSuffix(file.Path, ".network") {
			names = append(names, filepath.Base(file.Path))
		}
	}
	sort.Strings(names)
	return names
}

func kubeadmInputRefs(descriptors []Descriptor) []string {
	seen := map[string]struct{}{}
	for _, desc := range descriptors {
		ref := desc.Annotations["dev.katl.kubeadm.resolved-id"]
		if ref != "" {
			seen[ref] = struct{}{}
		}
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("golden mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
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

func allSameHardwareSourceConfig() string {
	return strings.Replace(validSourceConfig(), `    - name: worker-1
      systemRole: worker`, `    - name: worker-1
      systemRole: worker
      nodeClass: ms01`, 1)
}

func mixedMS01MSA2SourceConfig() string {
	withClass := strings.Replace(validSourceConfig(), `    ms01:
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
          katl.dev/hardware-class: ms01`, `    ms01:
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
    msa2:
      install:
        targetDiskDefaults:
          minSizeMiB: 32768
      networkd:
        files:
          - name: 16-msa2.network
            content: |
              [Match]
              Name=enp4s0`, 1)
	return strings.Replace(withClass, `    - name: worker-1
      systemRole: worker`, `    - name: worker-1
      systemRole: worker
      nodeClass: msa2`, 1)
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
