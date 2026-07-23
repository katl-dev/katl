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

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/manifest"
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
	if bundle.Nodes[0].SystemRole != string(inventory.RoleControlPlane) || bundle.Nodes[1].SystemRole != string(inventory.RoleWorker) {
		t.Fatalf("compiled node roles = %#v", bundle.Nodes)
	}
	if bundle.Nodes[0].InstallMaterial.Digest == "" {
		t.Fatalf("control-plane node record = %#v", bundle.Nodes[0])
	}
	if len(bundle.Nodes[0].KubeadmInputs) != 1 || bundle.Nodes[0].KubeadmInputs[0].Annotations["dev.katl.kubeadm.resolved-id"] != "control-plane" {
		t.Fatalf("control-plane kubeadm inputs = %#v", bundle.Nodes[0].KubeadmInputs)
	}
	if bundle.Cluster.KubernetesPayloads[0].OCIManifestDigest != "sha256:1793f4aed888b48891e659cf286a88088f39a87311d5710c889341aff3f5c537" || bundle.Cluster.KubernetesPayloads[0].ArtifactVersion != "v1.36.1-katl.1" {
		t.Fatalf("kubernetes payloads = %#v", bundle.Cluster.KubernetesPayloads)
	}
	if payload := bundle.Cluster.KubernetesPayloads[0]; payload.ResolverVersion != "release-compatibility-v1" || payload.Architecture != "x86_64" || len(payload.SupportedRuntimeInterfaces) != 1 || payload.SupportedRuntimeInterfaces[0] != "katl-runtime-1" {
		t.Fatalf("resolved Kubernetes compatibility = %#v", payload)
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

func TestBuildArchiveCompilesBoundedNativeKubeadmInputByRole(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "kubeadm.yaml")
	writeFile(t, configPath, `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: profiling
      value: "false"
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
shutdownGracePeriod: 45s
`)
	patchesDir := filepath.Join(dir, "kubeadm-patches")
	if err := os.MkdirAll(patchesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(patchesDir, "kube-apiserver0+merge.yaml"), "metadata:\n  labels:\n    katl.dev/profile: homelab\n")
	writeFile(t, filepath.Join(patchesDir, "kubeletconfiguration0+merge.yaml"), "shutdownGracePeriod: 45s\n")
	source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml\n      patchesDir: ./kubeadm-patches", 1)
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, source)
	archive, result, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	controlPlane, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode(control-plane) error = %v", err)
	}
	cp := controlPlane.KubeadmConfigs["control-plane"]
	cpConfig := string(cp.Config.Content)
	for _, want := range []string{"kind: InitConfiguration", "kind: ClusterConfiguration", "kubernetesVersion: v1.36.1", "name: profiling", "kind: KubeletConfiguration", "volumePluginDir: /var/lib/kubelet/plugins/volume/exec", "directory: /etc/katl/kubeadm/control-plane/patches"} {
		if !strings.Contains(cpConfig, want) {
			t.Fatalf("control-plane kubeadm input missing %q:\n%s", want, cpConfig)
		}
	}
	if len(cp.Patches) != 2 || cp.Patches[0].RenderPath != "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml" {
		t.Fatalf("control-plane patches = %#v", cp.Patches)
	}

	worker, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "worker-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode(worker) error = %v", err)
	}
	workerPlan := worker.KubeadmConfigs["worker"]
	workerConfig := string(workerPlan.Config.Content)
	for _, want := range []string{"kind: JoinConfiguration", "kind: KubeletConfiguration", "volumePluginDir: /var/lib/kubelet/plugins/volume/exec", "directory: /etc/katl/kubeadm/worker/patches"} {
		if !strings.Contains(workerConfig, want) {
			t.Fatalf("worker kubeadm input missing %q:\n%s", want, workerConfig)
		}
	}
	if strings.Contains(workerConfig, "kind: ClusterConfiguration") || strings.Contains(workerConfig, "name: profiling") {
		t.Fatalf("worker kubeadm input contains control-plane documents:\n%s", workerConfig)
	}
	if len(workerPlan.Patches) != 1 || workerPlan.Patches[0].RenderPath != "/etc/katl/kubeadm/worker/patches/kubeletconfiguration0+merge.yaml" {
		t.Fatalf("worker patches = %#v", workerPlan.Patches)
	}

	writeFile(t, filepath.Join(patchesDir, "kubeletconfiguration0+merge.yaml"), "shutdownGracePeriod: 60s\n")
	_, changed, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive(changed kubeadm input) error = %v", err)
	}
	if changed.Manifest.Source.SourceDigest == result.Manifest.Source.SourceDigest {
		t.Fatalf("source digest did not change with referenced kubeadm input: %s", changed.Manifest.Source.SourceDigest)
	}
}

func TestBuildArchiveRejectsUnsafeAdvancedKubeadmInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kubeadm.yaml"), `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraVolumes:
    - name: host-ssh
      hostPath: /etc/ssh
      mountPath: /safe/in-container
`)
	source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml", 1)
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, source)
	_, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err == nil || !strings.Contains(err.Error(), "host path /etc/ssh is denied") {
		t.Fatalf("BuildArchive() error = %v", err)
	}
}

func TestBuildArchiveRejectsEmptyAdvancedKubeadmInput(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm: {}", 1)
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
	if err == nil || !strings.Contains(err.Error(), "spec.kubernetes.kubeadm.configFile is required") {
		t.Fatalf("BuildArchive() error = %v", err)
	}
}

func TestBuildArchiveDefaultsMinimalSource(t *testing.T) {
	sourcePath := writeSource(t, `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  kubernetes:
    version: v1.36.1
  defaults:
    identity:
      ssh:
        authorizedKeys:
          - `+testSSHKey+`
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-cp-root
      bootstrap:
        address: 192.0.2.11
`)
	archive, result, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	if result.Manifest.Cluster.BootstrapInventory.ControlPlaneEndpoint != "192.0.2.11:6443" {
		t.Fatalf("bootstrap inventory = %#v", result.Manifest.Cluster.BootstrapInventory)
	}
	if len(result.Manifest.Cluster.KubernetesPayloads) != 1 || !strings.Contains(result.Manifest.Cluster.KubernetesPayloads[0].Ref, "v1.36.1-katl.1@sha256:") {
		t.Fatalf("Kubernetes payloads = %#v", result.Manifest.Cluster.KubernetesPayloads)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	if selected.InstallManifest.Node.Identity.Hostname != "cp-1" || selected.InstallManifest.Node.Bootstrap.Access.CredentialRef != "" {
		t.Fatalf("defaulted install manifest = %#v", selected.InstallManifest)
	}
	if len(selected.InstallManifest.Node.Networkd.Files) != 1 || !strings.Contains(selected.InstallManifest.Node.Networkd.Files[0].Content, "DHCP=yes") {
		t.Fatalf("defaulted networkd = %#v", selected.InstallManifest.Node.Networkd)
	}
	if selected.NodeMaterial.KubeadmConfig.Ref != "control-plane" || selected.KubeadmConfigs["control-plane"].Config.RenderPath == "" {
		t.Fatalf("defaulted kubeadm = material %#v configs %#v", selected.NodeMaterial.KubeadmConfig, selected.KubeadmConfigs)
	}
	if config := string(selected.KubeadmConfigs["control-plane"].Config.Content); !strings.Contains(config, "volumePluginDir: /var/lib/kubelet/plugins/volume/exec") || !strings.Contains(config, "taints: []") || !strings.Contains(config, "podSubnet: 10.244.0.0/16") {
		t.Fatalf("defaulted kubeadm does not keep plugins on writable state, allow control-plane scheduling, and allocate Pod CIDRs:\n%s", config)
	}
}

func TestBuildArchiveKeepsOperatorClusterConfigurationAuthoritative(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kubeadm.yaml"), `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: operator-cluster
`)
	source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml", 1)
	sourcePath := filepath.Join(dir, "cluster.yaml")
	writeFile(t, sourcePath, source)

	archive, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	config := string(selected.KubeadmConfigs["control-plane"].Config.Content)
	if !strings.Contains(config, "clusterName: operator-cluster") {
		t.Fatalf("control-plane kubeadm input lost operator ClusterConfiguration:\n%s", config)
	}
	if strings.Contains(config, "podSubnet:") {
		t.Fatalf("control-plane kubeadm input silently supplemented operator ClusterConfiguration:\n%s", config)
	}
}

func TestBuildArchiveRequiresControlPlaneNode(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "      controlPlane: true\n", "", 1)
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
	if err == nil || !strings.Contains(err.Error(), "at least one node with controlPlane: true") {
		t.Fatalf("BuildArchive() error = %v, want missing control-plane guidance", err)
	}
}

func TestBuildArchiveAccountsForOperationInputsWithoutAddingThemToIntent(t *testing.T) {
	image := testKatlosImage()
	archive, result, err := BuildArchive(BuildRequest{
		SourcePath: writeSource(t, validSourceConfig()),
		Planning: PlanningInputs{
			KatlosImage:      image,
			KubernetesBundle: "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.2",
			BootstrapAccess: map[string]inventory.Access{
				"cp-1": {Method: "agent", CredentialRef: "vsock:1234:10240"},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{ExpectedDigest: result.Digest, NodeName: "cp-1"})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	if selected.InstallManifest.KatlosImage != image {
		t.Fatalf("operation KatlOS image = %#v", selected.InstallManifest.KatlosImage)
	}
	if got := selected.InstallManifest.Node.Bootstrap.Access.CredentialRef; got != "vsock:1234:10240" {
		t.Fatalf("operation bootstrap access = %q", got)
	}
	if got := result.Manifest.Cluster.KubernetesPayloads[0].Ref; got != "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.2" {
		t.Fatalf("operation Kubernetes bundle = %q", got)
	}
	files := readTarFiles(t, archive)
	normalized := files["blobs/sha256/"+strings.TrimPrefix(result.Manifest.Source.NormalizedConfig.Digest, "sha256:")]
	for _, internal := range []string{"katlosImage", "kubernetes:v1.36.1-katl.2", "credentialRef", "vsock:1234:10240"} {
		if bytes.Contains(normalized, []byte(internal)) {
			t.Fatalf("normalized ClusterConfig contains operation input %q:\n%s", internal, normalized)
		}
	}
}

func TestBuildArchiveDefersKatlosImageToInstallMedia(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	defaultImage := manifest.KatlosImage{
		LocalRef: "images/katlos-install.squashfs", SHA256: strings.Repeat("a", 64),
		SizeBytes: 1024, Version: "2026.7.0", Architecture: "x86_64",
		RuntimeInterface: "katl-runtime-1", Role: "install",
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", DefaultKatlosImage: defaultImage})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	if !selected.KatlosImageFromMedia || selected.InstallManifest.KatlosImage != defaultImage {
		t.Fatalf("selected media image = %#v, from media = %v", selected.InstallManifest.KatlosImage, selected.KatlosImageFromMedia)
	}
	if _, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1"}); err == nil || !strings.Contains(err.Error(), "katlosImage") {
		t.Fatalf("ReadSelectedNode() error = %v, want missing media image rejection", err)
	}
	selected, err = ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode() runtime rendering error = %v", err)
	}
	if !manifest.KatlosImageEmpty(selected.InstallManifest.KatlosImage) || selected.KatlosImageFromMedia {
		t.Fatalf("selected runtime image = %#v, from media = %v", selected.InstallManifest.KatlosImage, selected.KatlosImageFromMedia)
	}
}

func TestInstallingGuideClusterConfigCompiles(t *testing.T) {
	data, err := os.ReadFile("../../../docs/installing.md")
	if err != nil {
		t.Fatalf("read installing guide: %v", err)
	}
	_, section, ok := strings.Cut(string(data), "## Author One ClusterConfig")
	if !ok {
		t.Fatal("installing guide is missing ClusterConfig section")
	}
	_, example, ok := strings.Cut(section, "```yaml\n")
	if !ok {
		t.Fatal("installing guide is missing ClusterConfig YAML example")
	}
	example, _, ok = strings.Cut(example, "\n```")
	if !ok {
		t.Fatal("installing guide ClusterConfig example is not terminated")
	}
	_, result, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, example)})
	if err != nil {
		t.Fatalf("compile installing guide ClusterConfig: %v", err)
	}
	if result.Manifest.ClusterName != "katl-lab" || len(result.Manifest.Nodes) != 2 {
		t.Fatalf("installing guide bundle = cluster %q nodes %#v", result.Manifest.ClusterName, result.Manifest.Nodes)
	}
}

func TestBuildArchiveRejectsUnsafeDiskDefaults(t *testing.T) {
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, strings.Replace(validSourceConfig(), "minSizeMiB: 65536", "serial: shared-root", 1))})
	if err == nil || !strings.Contains(err.Error(), "targetDiskDefaults must not set byID, wwn, or serial") {
		t.Fatalf("BuildArchive() error = %v, want unsafe disk defaults", err)
	}
}

func TestBuildArchiveRejectsRemovedIntentMechanisms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "KatlOS image",
			raw:  strings.Replace(validSourceConfig(), "  defaults:\n", "  katlosImage: {}\n  defaults:\n", 1),
			want: "spec.katlosImage: field is not supported",
		},
		{
			name: "wipe authorization",
			raw:  strings.Replace(validSourceConfig(), "      targetDiskDefaults:\n", "      wipeTarget: true\n      targetDiskDefaults:\n", 1),
			want: "spec.defaults.install.wipeTarget: field is not supported",
		},
		{
			name: "hostname alias",
			raw:  strings.Replace(validSourceConfig(), "      ssh:\n", "      hostname: cp-alias\n      ssh:\n", 1),
			want: "spec.defaults.identity.hostname: field is not supported",
		},
		{
			name: "Kubernetes bundle",
			raw:  strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    bundle: ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1", 1),
			want: "spec.kubernetes.bundle: field is not supported",
		},
		{
			name: "Kubernetes catalog",
			raw:  strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    catalogRef: stable", 1),
			want: "spec.kubernetes.catalogRef: field is not supported",
		},
		{
			name: "node classes",
			raw:  strings.Replace(validSourceConfig(), "  defaults:\n", "  nodeClasses: {}\n  defaults:\n", 1),
			want: "spec.nodeClasses: field is not supported",
		},
		{
			name: "role defaults",
			raw:  strings.Replace(validSourceConfig(), "  nodes:\n", "  systemRoleDefaults: {}\n  nodes:\n", 1),
			want: "spec.systemRoleDefaults: field is not supported",
		},
		{
			name: "system role alias",
			raw:  strings.Replace(validSourceConfig(), "      controlPlane: true\n", "      systemRole: control-plane\n", 1),
			want: "spec.nodes[0].systemRole: field is not supported",
		},
		{
			name: "platform endpoint",
			raw:  strings.Replace(validSourceConfig(), "  defaults:\n", "  platformAPIEndpoint: {}\n  defaults:\n", 1),
			want: "spec.platformAPIEndpoint: field is not supported",
		},
		{
			name: "node overrides wrapper",
			raw:  strings.Replace(validSourceConfig(), "      install:\n", "      overrides:\n        install:\n", 1),
			want: "spec.nodes[0].overrides: field is not supported",
		},
		{
			name: "node labels alias",
			raw:  strings.Replace(validSourceConfig(), "        labels:\n", "        nodeLabels:\n", 1),
			want: "spec.nodes[0].kubernetes.nodeLabels: field is not supported",
		},
		{
			name: "bootstrap credentials",
			raw:  strings.Replace(validSourceConfig(), "        address: 10.0.0.11", "        address: 10.0.0.11\n        access:\n          credentialRef: file:/tmp/token", 1),
			want: "spec.nodes[0].bootstrap.access: field is not supported",
		},
		{
			name: "kubeadm profiles",
			raw:  strings.Replace(validSourceConfig(), "  nodes:\n", "  kubeadmConfigs: {}\n  nodes:\n", 1),
			want: "spec.kubeadmConfigs: field is not supported",
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
			want: "spec.nodeRange: field is not supported",
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
	if err == nil || !strings.Contains(err.Error(), "spec.nodeTemplate: field is not supported") {
		t.Fatalf("BuildArchive() error = %v, want unknown template field", err)
	}
}

func TestSourceSchemaExposesAuthoringContract(t *testing.T) {
	data, err := SourceSchema()
	if err != nil {
		t.Fatalf("SourceSchema() error = %v", err)
	}
	var document struct {
		ID   string `json:"$id"`
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if document.ID != SourceSchemaID {
		t.Fatalf("schema id = %q, want %q", document.ID, SourceSchemaID)
	}
	root := document.Defs["configbundle.SourceConfig"]
	for _, field := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if _, ok := root.Properties[field]; !ok {
			t.Fatalf("root schema is missing %q", field)
		}
	}
	node := document.Defs["configbundle.SourceNode"]
	if _, ok := node.Properties["controlPlane"]; !ok {
		t.Fatal("source node schema is missing controlPlane")
	}
	if _, ok := node.Properties["systemRole"]; ok {
		t.Fatal("source node schema exposes removed systemRole")
	}
}

func TestReadSelectedNodeVerifiesBundleAndSelectsNodeMaterial(t *testing.T) {
	archive, result, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{
		ExpectedDigest:     result.Digest,
		NodeName:           "cp-1",
		DefaultKatlosImage: testKatlosImage(),
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

func TestReadBundleReturnsVerifiedBootstrapInventory(t *testing.T) {
	archive, result, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	bundle, err := ReadBundle(bytes.NewReader(archive), result.Digest)
	if err != nil {
		t.Fatalf("ReadBundle() error = %v", err)
	}
	inv := bundle.Manifest.Cluster.BootstrapInventory
	if bundle.Digest != result.Digest || inv.ControlPlaneEndpoint != "api.katl.test:6443" || len(inv.Nodes) != 2 {
		t.Fatalf("verified bundle = %#v inventory = %#v", bundle, inv)
	}
	if _, err := ReadBundle(bytes.NewReader(archive), "sha256:"+strings.Repeat("f", 64)); err == nil || !strings.Contains(err.Error(), "config bundle digest mismatch") {
		t.Fatalf("ReadBundle() mismatch error = %v", err)
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
			_, err = ReadSelectedNode(bytes.NewReader(mutated), ReadOptions{NodeName: "cp-1", DefaultKatlosImage: testKatlosImage()})
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
  controlPlaneEndpoint:
    host: api.katl.test
    port: 6443
  kubernetes:
    version: v1.36.1
  defaults:
    install:
      targetDiskDefaults:
        minSizeMiB: 65536
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
  nodes:
    - name: cp-1
      controlPlane: true
      bootstrap:
        address: 10.0.0.11
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-cp-root
      kubernetes:
        labels:
          katl.dev/zone: rack-a
        taints:
          - key: node-role.kubernetes.io/control-plane
            effect: NoSchedule
    - name: worker-1
      bootstrap:
        address: 10.0.0.21
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-worker-root
      kubernetes:
        labels:
          katl.dev/pool: workers
`
}

const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func testKatlosImage() manifest.KatlosImage {
	return manifest.KatlosImage{
		LocalRef:         "images/katlos-install-test-x86_64.squashfs",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1073741824,
		Version:          "2026.7.0-test",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
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
