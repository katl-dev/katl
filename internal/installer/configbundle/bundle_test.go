package configbundle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/kubernetescompat"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
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
	assertReleaseKubernetesPayload(t, bundle.Cluster.KubernetesPayloads, "v1.36.1")
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

func TestDecodeSourceAcceptsKernelCommandLineDefaultsAndNodeClear(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", `  defaults:
    kernel:
      commandLine:
        - intel_iommu=on
`, 1)
	source = strings.Replace(source, "    - name: worker-1\n", `    - name: worker-1
      kernel:
        commandLine: []
`, 1)

	config, err := DecodeSource(strings.NewReader(source))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v", err)
	}
	if config.Spec.Defaults.Kernel == nil {
		t.Fatalf("default kernel config = %#v", config.Spec.Defaults.Kernel)
	}
	defaultCommandLine, defaultSet := config.Spec.Defaults.Kernel.CommandLine.Get()
	if !defaultSet || !slices.Equal(defaultCommandLine, []string{"intel_iommu=on"}) {
		t.Fatalf("default kernel config = %#v", config.Spec.Defaults.Kernel)
	}
	if config.Spec.Nodes[1].Kernel == nil {
		t.Fatalf("worker kernel override = %#v", config.Spec.Nodes[1].Kernel)
	}
	workerCommandLine, workerSet := config.Spec.Nodes[1].Kernel.CommandLine.Get()
	if !workerSet || len(workerCommandLine) != 0 {
		t.Fatalf("worker kernel override = %#v", config.Spec.Nodes[1].Kernel)
	}
}

func TestLowerSourceAppliesPredictableNodeLayering(t *testing.T) {
	config, err := DecodeSource(strings.NewReader(`apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: layering
spec:
  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ` + testSSHKey + `
    kernel:
      commandLine:
        - intel_iommu=on
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
      fileSets:
        network:
          files:
            - path: /etc/systemd/network/20-common.network
              content: common
    systemExtensions:
      - name: bird
        bundle: registry.example/bird:v1
      - name: tools
        bundle: registry.example/tools:v1
    install:
      systemDisk:
        minSizeMiB: 65536
    storage:
      disks:
        - name: data
          selector:
            disk:
              minSizeMiB: 1024
          filesystem: btrfs
          wipe: false
        - name: scratch
          selector:
            disk:
              minSizeMiB: 2048
          filesystem: xfs
    kubernetes:
      labels:
        environment: lab
        topology.kubernetes.io/zone: rack-a
      taints:
        - key: inherited
          effect: NoSchedule
  nodes:
    - name: inherited
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/inherited-root
    - name: cleared
      access:
        ssh:
          authorizedKeys: []
      kernel:
        commandLine: []
      hostConfiguration:
        sysfs: []
        fileSets: {}
      systemExtensions: []
      install:
        systemDisk:
          byID: /dev/disk/by-id/cleared-root
      storage:
        disks: []
      kubernetes:
        labels: {}
        taints: []
    - name: overridden
      hostConfiguration:
        fileSets:
          network:
            state: absent
          host:
            files:
              - path: /etc/hostname-note
                content: overridden
      systemExtensions:
        - name: bird
          bundle: registry.example/bird:v2
        - name: tools
          state: absent
      install:
        systemDisk:
          byID: /dev/disk/by-id/overridden-root
      storage:
        disks:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/overridden-data
            wipe: true
          - name: scratch
            state: absent
      kubernetes:
        labels:
          topology.kubernetes.io/zone: rack-b
        taints:
          - key: dedicated
            value: build
            effect: NoSchedule
`))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v", err)
	}
	plan, err := LowerSource(config, PlanningInputs{})
	if err != nil {
		t.Fatalf("LowerSource() error = %v", err)
	}

	inherited := plan.Spec.Nodes[0].Overrides
	if !slices.Equal(inherited.SSH.AuthorizedKeys, []string{testSSHKey}) ||
		inherited.Kernel == nil || !slices.Equal(inherited.Kernel.CommandLine, []string{"intel_iommu=on"}) ||
		inherited.Install.TargetDisk == nil || inherited.Install.TargetDisk.ByID != "/dev/disk/by-id/inherited-root" ||
		inherited.Install.TargetDisk.MinSizeMiB != 65536 {
		t.Fatalf("omitted node fields did not inherit defaults: %#v", inherited)
	}

	cleared := plan.Spec.Nodes[1].Overrides
	if len(cleared.SSH.AuthorizedKeys) != 0 ||
		cleared.Kernel == nil || len(cleared.Kernel.CommandLine) != 0 ||
		len(cleared.HostConfiguration.Sysfs) != 0 || len(cleared.HostConfiguration.Sets) != 0 ||
		len(cleared.SystemExtensions) != 0 || len(cleared.Install.Volumes) != 0 ||
		len(cleared.Kubernetes.NodeLabels) != 0 || len(cleared.Kubernetes.NodeTaints) != 0 {
		t.Fatalf("explicitly empty node fields did not clear defaults: %#v", cleared)
	}
	if cleared.Install.TargetDisk == nil ||
		cleared.Install.TargetDisk.ByID != "/dev/disk/by-id/cleared-root" ||
		cleared.Install.TargetDisk.MinSizeMiB != 65536 {
		t.Fatalf("system disk constraints did not layer by field: %#v", cleared.Install.TargetDisk)
	}

	overridden := plan.Spec.Nodes[2].Overrides
	if len(overridden.HostConfiguration.Sets) != 1 || overridden.HostConfiguration.Sets["host"].Files[0].Path != "/etc/hostname-note" {
		t.Fatalf("named file sets did not replace or remove by name: %#v", overridden.HostConfiguration.Sets)
	}
	if len(overridden.SystemExtensions) != 1 || overridden.SystemExtensions[0].Name != "bird" || overridden.SystemExtensions[0].Bundle != "registry.example/bird:v2" {
		t.Fatalf("named system extensions did not replace or remove by name: %#v", overridden.SystemExtensions)
	}
	if len(overridden.Install.Volumes) != 1 {
		t.Fatalf("named storage disks did not replace or remove by name: %#v", overridden.Install.Volumes)
	}
	data := overridden.Install.Volumes[0]
	if data.Name != "data" || data.Selector.Disk == nil ||
		data.Selector.Disk.ByID != "/dev/disk/by-id/overridden-data" ||
		data.Selector.Disk.MinSizeMiB != 1024 || data.Filesystem != "btrfs" || !data.Wipe {
		t.Fatalf("storage disk fields did not inherit and override predictably: %#v", data)
	}
	if got := overridden.Kubernetes.NodeLabels; len(got) != 2 || got["environment"] != "lab" || got["topology.kubernetes.io/zone"] != "rack-b" {
		t.Fatalf("labels did not replace values by key: %#v", got)
	}
	if got := overridden.Kubernetes.NodeTaints; len(got) != 1 || got[0].Key != "dedicated" {
		t.Fatalf("taints did not replace the inherited list: %#v", got)
	}
}

func TestNormalizedSourcePreservesExplicitEmptyValues(t *testing.T) {
	config, err := DecodeSource(strings.NewReader(`apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: empty-values
spec:
  nodes:
    - name: cp-1
      controlPlane: true
      access:
        ssh:
          authorizedKeys: []
      hostConfiguration:
        fileSets: {}
      storage:
        disks: []
      kubernetes:
        labels: {}
        taints: []
`))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v", err)
	}
	normalized, err := marshalCanonical(config)
	if err != nil {
		t.Fatalf("marshalCanonical() error = %v", err)
	}
	roundTripped, err := DecodeSource(bytes.NewReader(normalized))
	if err != nil {
		t.Fatalf("DecodeSource(normalized) error = %v", err)
	}
	node := roundTripped.Spec.Nodes[0]
	for name, present := range map[string]bool{
		"authorizedKeys": optionalIsSet(node.Access.SSH.AuthorizedKeys),
		"fileSets":       optionalIsSet(node.HostConfiguration.FileSets),
		"storage.disks":  optionalIsSet(node.Storage.Disks),
		"labels":         optionalIsSet(node.Kubernetes.Labels),
		"taints":         optionalIsSet(node.Kubernetes.Taints),
	} {
		if !present {
			t.Fatalf("normalized source lost explicit empty %s:\n%s", name, normalized)
		}
	}
	if optionalIsSet(node.SystemExtensions) {
		t.Fatalf("normalized source turned omitted systemExtensions into an explicit value:\n%s", normalized)
	}
}

func optionalIsSet[T any](value Optional[T]) bool {
	_, ok := value.Get()
	return ok
}

func TestDecodeSourceRejectsKatlOwnedKernelArgument(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", `  defaults:
    kernel:
      commandLine:
        - root=/dev/sda
`, 1)
	if _, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)}); err == nil || !strings.Contains(err.Error(), "managed by Katl") {
		t.Fatalf("BuildArchive() error = %v", err)
	}
}

func TestBuildArchiveResolvesAndVendorsSystemExtensionOnce(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", `  defaults:
    systemExtensions:
      - name: bird
        bundle: registry.example/katl-dev/bird:v3.1.2-katl.1
        configuration:
          files:
            - path: /etc/bird.conf
              content: |
                router id from "bird0";
        units:
          - name: bird.service
            enable: true
            requiredForBootHealth: true
            dropIns:
              - name: 10-site.conf
                content: |
                  [Service]
                  RestartSec=2s
`, 1)
	payloadData := []byte("verified sysext bytes")
	payloadDigest := digestBytes(payloadData)
	customManifest := []byte(`{"apiVersion":"payload.katl.dev/v1alpha1","kind":"SystemExtensionBundle"}`)
	customDigest := digestBytes(customManifest)
	resolveCalls := 0
	archive, result, err := BuildArchive(BuildRequest{
		SourcePath: writeSource(t, source),
		Planning:   PlanningInputs{KatlosImage: testKatlosImage()},
		ResolveSystemExtension: func(_ context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
			resolveCalls++
			if request.Reference != "registry.example/katl-dev/bird:v3.1.2-katl.1" ||
				request.Architecture != "x86_64" || request.RuntimeInterface != "katl-runtime-1" {
				t.Fatalf("resolve request = %#v", request)
			}
			return systemextensionbundle.Resolved{
				Reference:            request.Reference,
				OCIManifestDigest:    "sha256:" + strings.Repeat("a", 64),
				BundleManifestDigest: customDigest,
				BundleManifest:       customManifest,
				Bundle: systemextensionbundle.Bundle{
					ArtifactVersion: "v3.1.2-katl.1", PayloadVersion: "v3.1.2",
					Architecture: "x86_64", SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
				},
				Payloads: []systemextensionbundle.Payload{{
					Descriptor: systemextensionbundle.Descriptor{
						Role: systemextensionbundle.SysextRole, MediaType: systemextensionbundle.SysextMediaType,
						Digest: payloadDigest, SizeBytes: int64(len(payloadData)), FileName: "katl-bird.raw",
					},
					Data: payloadData,
				}},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want one for shared defaults", resolveCalls)
	}
	files := readTarFiles(t, archive)
	normalized := files["blobs/sha256/"+strings.TrimPrefix(result.Manifest.Source.NormalizedConfig.Digest, "sha256:")]
	for _, compilerOwned := range []string{"ociManifestDigest", "bundleManifestDigest", "artifactVersion", "payloadVersion", "supportedRuntimeInterfaces", "payloads"} {
		if bytes.Contains(normalized, []byte(compilerOwned)) {
			t.Fatalf("normalized ClusterConfig exposes compiler-owned system extension field %q:\n%s", compilerOwned, normalized)
		}
	}
	if len(result.Manifest.Cluster.SystemExtensions) != 1 {
		t.Fatalf("bundle records = %#v", result.Manifest.Cluster.SystemExtensions)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.InstallManifest.Node.SystemExtensions) != 1 ||
		selected.InstallManifest.Node.SystemExtensions[0].OCIManifestDigest != "sha256:"+strings.Repeat("a", 64) ||
		len(selected.SystemExtensionPayloads) != 1 ||
		!bytes.Equal(selected.SystemExtensionPayloads[0].Data, payloadData) {
		t.Fatalf("selected system extension material = %#v / %#v", selected.InstallManifest.Node.SystemExtensions, selected.SystemExtensionPayloads)
	}
}

func assertReleaseKubernetesPayload(t *testing.T, payloads []KubernetesPayloadRecord, version string) {
	t.Helper()
	if len(payloads) != 1 {
		t.Fatalf("Kubernetes payloads = %#v", payloads)
	}
	selection, err := kubernetescompat.Resolve(kubernetescompat.Request{
		KubernetesVersion: version,
		Architecture:      "x86_64",
		RuntimeInterface:  "katl-runtime-1",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	image, err := kubernetesbundle.ParseImageReference(selection.Bundle)
	if err != nil {
		t.Fatalf("ParseImageReference() error = %v", err)
	}
	payload := payloads[0]
	if payload.RequestedVersion != version ||
		payload.ResolvedPayloadVersion != version ||
		payload.Ref != selection.Bundle ||
		payload.OCIManifestDigest != image.ManifestDigest ||
		payload.ArtifactVersion != image.ArtifactVersion ||
		payload.ResolverVersion != "release-compatibility-v1" ||
		payload.Architecture != "x86_64" ||
		len(payload.SupportedRuntimeInterfaces) != 1 ||
		payload.SupportedRuntimeInterfaces[0] != "katl-runtime-1" {
		t.Fatalf("Kubernetes payload = %#v", payload)
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
	for _, want := range []string{"kind: InitConfiguration", "criSocket: unix:///run/containerd/containerd.sock", "taints: []", "kind: ClusterConfiguration", "kubernetesVersion: v1.36.1", "name: profiling", "kind: KubeletConfiguration", "volumePluginDir: /var/lib/kubelet/plugins/volume/exec", "directory: /etc/katl/kubeadm/control-plane/patches"} {
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

func TestBuildArchiveRendersExactKubernetesAddressPerNode(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "      kubernetes:\n        labels:\n          katl.dev/zone: rack-a", "      kubernetes:\n        address: 10.254.1.1\n        labels:\n          katl.dev/zone: rack-a", 1)
	source = strings.Replace(source, "      kubernetes:\n        labels:\n          katl.dev/pool: workers", "      kubernetes:\n        address: 10.254.1.3\n        labels:\n          katl.dev/pool: workers", 1)
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}

	controlPlane, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode(control-plane) error = %v", err)
	}
	cpDocuments, err := decodeKubeadmDocuments(controlPlane.KubeadmConfigs["control-plane"].Config.Content)
	if err != nil {
		t.Fatal(err)
	}
	init := kubeadmDocument(cpDocuments, "InitConfiguration")
	if got := nestedString(init, "localAPIEndpoint", "advertiseAddress"); got != "10.254.1.1" {
		t.Fatalf("control-plane advertiseAddress = %q", got)
	}
	if got := kubeletNodeIP(init); got != "10.254.1.1" {
		t.Fatalf("control-plane kubelet node-ip = %q", got)
	}

	worker, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "worker-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode(worker) error = %v", err)
	}
	workerDocuments, err := decodeKubeadmDocuments(worker.KubeadmConfigs["worker"].Config.Content)
	if err != nil {
		t.Fatal(err)
	}
	if got := kubeletNodeIP(kubeadmDocument(workerDocuments, "JoinConfiguration")); got != "10.254.1.3" {
		t.Fatalf("worker kubelet node-ip = %q", got)
	}
}

func TestBuildArchiveLeavesKubernetesAddressAutomaticWhenOmitted(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, validSourceConfig())})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	controlPlane, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	documents, err := decodeKubeadmDocuments(controlPlane.KubeadmConfigs["control-plane"].Config.Content)
	if err != nil {
		t.Fatal(err)
	}
	init := kubeadmDocument(documents, "InitConfiguration")
	if got := nestedString(init, "localAPIEndpoint", "advertiseAddress"); got != "" {
		t.Fatalf("advertiseAddress = %q, want automatic selection", got)
	}
	if got := kubeletNodeIP(init); got != "" {
		t.Fatalf("kubelet node-ip = %q, want automatic selection", got)
	}
}

func TestBuildArchiveRejectsConflictingKatlOwnedKubeadmValues(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "Kubernetes version",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.35.0
`,
			want: "does not match spec.kubernetes.version",
		},
		{
			name: "CRI socket",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/crio/crio.sock
`,
			want: "nodeRegistration.criSocket must be",
		},
		{
			name: "control-plane taints",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  taints:
    - key: node-role.kubernetes.io/control-plane
      effect: NoSchedule
`,
			want: "nodeRegistration.taints must be empty",
		},
		{
			name: "volume plugin directory",
			config: `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
volumePluginDir: other
`,
			want: "volumePluginDir must be",
		},
		{
			name: "kubelet node IP",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  kubeletExtraArgs:
    - name: node-ip
      value: 10.254.1.1
`,
			want: "node-ip is supplied from nodes[].kubernetes.address",
		},
		{
			name: "local API advertise address",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.254.1.1
`,
			want: "advertiseAddress is supplied from nodes[].kubernetes.address",
		},
		{
			name: "join local API advertise address",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
controlPlane:
  localAPIEndpoint:
    advertiseAddress: 10.254.1.3
`,
			want: "advertiseAddress is supplied from nodes[].kubernetes.address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "kubeadm.yaml"), tt.config)
			source := strings.Replace(validSourceConfig(), "    version: v1.36.1", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml", 1)
			sourcePath := filepath.Join(dir, "cluster.yaml")
			writeFile(t, sourcePath, source)
			_, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildArchive() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildArchiveRejectsInvalidKubernetesAddress(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "      kubernetes:\n        labels:\n          katl.dev/zone: rack-a", "      kubernetes:\n        address: 10.254.1.0/31\n        labels:\n          katl.dev/zone: rack-a", 1)
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
	if err == nil || !strings.Contains(err.Error(), "must be a literal unicast IP address") {
		t.Fatalf("BuildArchive() error = %v", err)
	}
}

func TestBuildArchiveRequiresKubernetesAddressPerNode(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", "  defaults:\n    kubernetes:\n      address: 10.254.1.1\n", 1)
	_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
	if err == nil || !strings.Contains(err.Error(), "must be set per node") {
		t.Fatalf("BuildArchive() error = %v", err)
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
    access:
      ssh:
        authorizedKeys:
          - `+testSSHKey+`
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-root
      management:
        address: 192.0.2.11
`)
	archive, result, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	if result.Manifest.Cluster.BootstrapInventory.ControlPlaneEndpoint != "192.0.2.11:6443" {
		t.Fatalf("bootstrap inventory = %#v", result.Manifest.Cluster.BootstrapInventory)
	}
	assertReleaseKubernetesPayload(t, result.Manifest.Cluster.KubernetesPayloads, "v1.36.1")
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	if selected.InstallManifest.Node.Identity.Hostname != "cp-1" || selected.InstallManifest.Node.Bootstrap.Access.CredentialRef != "" {
		t.Fatalf("defaulted install manifest = %#v", selected.InstallManifest)
	}
	defaultNetwork := nativeFile(selected.NodeMaterial.NativeEtcFiles, "/etc/systemd/network/10-lan.network")
	if defaultNetwork == nil || !strings.Contains(defaultNetwork.Content, "DHCP=yes") {
		t.Fatalf("defaulted networkd files = %#v", selected.NodeMaterial.NativeEtcFiles)
	}
	if selected.NodeMaterial.KubeadmConfig.Ref != "control-plane" || selected.KubeadmConfigs["control-plane"].Config.RenderPath == "" {
		t.Fatalf("defaulted kubeadm = material %#v configs %#v", selected.NodeMaterial.KubeadmConfig, selected.KubeadmConfigs)
	}
	if config := string(selected.KubeadmConfigs["control-plane"].Config.Content); !strings.Contains(config, "volumePluginDir: /var/lib/kubelet/plugins/volume/exec") || !strings.Contains(config, "taints: []") || !strings.Contains(config, "podSubnet: 10.244.0.0/16") || !strings.Contains(config, "serviceSubnet: 10.96.0.0/12") {
		t.Fatalf("defaulted kubeadm does not keep plugins on writable state, allow control-plane scheduling, and declare the Pod and Service networks:\n%s", config)
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
	if strings.Contains(config, "podSubnet:") || strings.Contains(config, "serviceSubnet:") {
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
	if err == nil || !strings.Contains(err.Error(), "identifying selectors byID, wwn, and serial must be set per node") {
		t.Fatalf("BuildArchive() error = %v, want unsafe disk defaults", err)
	}
}

func TestBuildArchiveRejectsUnsafeStorageDefaults(t *testing.T) {
	tests := []struct {
		name        string
		old         string
		replacement string
		want        string
	}{
		{name: "by ID", old: "              minSizeMiB: 1024\n", replacement: "              byID: /dev/disk/by-id/shared-data\n", want: "spec.defaults.storage.disks[0].selector.disk.byID identifies a target"},
		{name: "WWN", old: "              minSizeMiB: 1024\n", replacement: "              wwn: shared-data\n", want: "spec.defaults.storage.disks[0].selector.disk.wwn identifies a target"},
		{name: "serial", old: "              minSizeMiB: 1024\n", replacement: "              serial: shared-data\n", want: "spec.defaults.storage.disks[0].selector.disk.serial identifies a target"},
		{name: "partition by ID", old: "            disk:\n              minSizeMiB: 1024\n", replacement: "            partition:\n              byID: /dev/disk/by-id/shared-data-part1\n", want: "spec.defaults.storage.disks[0].selector.partition identifies a target"},
		{name: "partition UUID", old: "            disk:\n              minSizeMiB: 1024\n", replacement: "            partition:\n              partUUID: 01234567-89ab-cdef-0123-456789abcdef\n", want: "spec.defaults.storage.disks[0].selector.partition identifies a target"},
		{name: "filesystem UUID", old: "            disk:\n              minSizeMiB: 1024\n", replacement: "            partition:\n              filesystemUUID: shared-data\n", want: "spec.defaults.storage.disks[0].selector.partition identifies a target"},
		{name: "convention partition", old: "            disk:\n              minSizeMiB: 1024\n", replacement: "            partition: {}\n", want: "spec.defaults.storage.disks[0].selector.partition identifies a target"},
		{name: "wipe", old: "          filesystem: xfs\n", replacement: "          filesystem: xfs\n          wipe: true\n", want: "spec.defaults.storage.disks[0].wipe must not be true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(validSourceConfig(), test.old, test.replacement, 1)
			_, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildArchive() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildArchiveAcceptsSafeStorageDefaultsAndNodeAuthority(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "          filesystem: xfs\n", "          filesystem: xfs\n          wipe: false\n", 1)
	source = strings.Replace(source, "                byID: /dev/disk/by-id/ata-cp-data\n", "                byID: /dev/disk/by-id/ata-cp-data\n            wipe: true\n", 1)
	if _, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, source)}); err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
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
			raw:  strings.Replace(validSourceConfig(), "      systemDisk:\n", "      wipeTarget: true\n      systemDisk:\n", 1),
			want: "spec.defaults.install.wipeTarget: field is not supported",
		},
		{
			name: "hostname alias",
			raw:  strings.Replace(validSourceConfig(), "      ssh:\n", "      hostname: cp-alias\n      ssh:\n", 1),
			want: "spec.defaults.access.hostname: field is not supported",
		},
		{
			name: "identity access block",
			raw:  strings.Replace(validSourceConfig(), "    access:\n", "    identity:\n", 1),
			want: "spec.defaults.identity: field is not supported",
		},
		{
			name: "bootstrap management block",
			raw:  strings.Replace(validSourceConfig(), "      management:\n", "      bootstrap:\n", 1),
			want: "spec.nodes[\"cp-1\"].bootstrap: field is not supported",
		},
		{
			name: "target disk defaults",
			raw:  strings.Replace(validSourceConfig(), "      systemDisk:\n        minSizeMiB:", "      targetDiskDefaults:\n        minSizeMiB:", 1),
			want: "spec.defaults.install.targetDiskDefaults: field is not supported",
		},
		{
			name: "target disk",
			raw: strings.Replace(validSourceConfig(),
				"      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-cp-root",
				"      install:\n        targetDisk:\n          byID: /dev/disk/by-id/ata-cp-root", 1),
			want: "spec.nodes[\"cp-1\"].install.targetDisk: field is not supported",
		},
		{
			name: "extra disks",
			raw: strings.Replace(validSourceConfig(),
				"      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-cp-root",
				"      install:\n        extraDisks: []\n        systemDisk:\n          byID: /dev/disk/by-id/ata-cp-root", 1),
			want: "spec.nodes[\"cp-1\"].install.extraDisks: field is not supported",
		},
		{
			name: "install volumes",
			raw: strings.Replace(validSourceConfig(),
				"      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-cp-root",
				"      install:\n        volumes: []\n        systemDisk:\n          byID: /dev/disk/by-id/ata-cp-root", 1),
			want: "spec.nodes[\"cp-1\"].install.volumes: field is not supported",
		},
		{
			name: "host configuration sets",
			raw:  strings.Replace(validSourceConfig(), "      fileSets:\n", "      sets:\n", 1),
			want: "spec.defaults.hostConfiguration.sets: field is not supported",
		},
		{
			name: "sysfs name",
			raw: strings.Replace(validSourceConfig(), "    hostConfiguration:\n", `    hostConfiguration:
      sysfs:
        - name: /sys/module/printk/parameters/time
          value: N
`, 1),
			want: "spec.defaults.hostConfiguration.sysfs[0].name: field is not supported",
		},
		{
			name: "file set notify",
			raw:  strings.Replace(validSourceConfig(), "        common-network:\n", "        common-network:\n          notify: {}\n", 1),
			want: "spec.defaults.hostConfiguration.fileSets.common-network.notify: field is not supported",
		},
		{
			name: "route exchange singular",
			raw: strings.Replace(validSourceConfig(), "    port: 6443\n", `    port: 6443
    advertisement:
      vip: 10.40.0.10
      bgp:
        localASN: 64512
        peers:
          - address: 10.0.0.1
            asn: 64500
        routeExchange: []
`, 1),
			want: "spec.controlPlaneEndpoint.advertisement.bgp.routeExchange: field is not supported",
		},
		{
			name: "prefix length",
			raw: strings.Replace(validSourceConfig(), "    port: 6443\n", `    port: 6443
    advertisement:
      vip: 10.40.0.10
      bgp:
        localASN: 64512
        peers:
          - address: 10.0.0.1
            asn: 64500
        routeExchanges:
          - name: cilium
            exportToFabric:
              - cidr: 10.50.0.0/16
                prefixLength: 32
`, 1),
			want: "spec.controlPlaneEndpoint.advertisement.bgp.routeExchanges[0].exportToFabric[0].prefixLength: field is not supported",
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
			want: "spec.nodes[\"cp-1\"].systemRole: field is not supported",
		},
		{
			name: "platform endpoint",
			raw:  strings.Replace(validSourceConfig(), "  defaults:\n", "  platformAPIEndpoint: {}\n  defaults:\n", 1),
			want: "spec.platformAPIEndpoint: field is not supported",
		},
		{
			name: "node overrides wrapper",
			raw:  strings.Replace(validSourceConfig(), "      install:\n", "      overrides:\n        install:\n", 1),
			want: "spec.nodes[\"cp-1\"].overrides: field is not supported",
		},
		{
			name: "node labels alias",
			raw:  strings.Replace(validSourceConfig(), "        labels:\n", "        nodeLabels:\n", 1),
			want: "spec.nodes[\"cp-1\"].kubernetes.nodeLabels: field is not supported",
		},
		{
			name: "bootstrap credentials",
			raw:  strings.Replace(validSourceConfig(), "        address: 10.0.0.11", "        address: 10.0.0.11\n        access:\n          credentialRef: file:/tmp/token", 1),
			want: "spec.nodes[\"cp-1\"].management.access: field is not supported",
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
	var document sourceSchema
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
	for _, field := range []string{"access", "controlPlane", "install", "kubernetes", "management", "storage"} {
		if _, ok := node.Properties[field]; !ok {
			t.Fatalf("source node schema is missing %q", field)
		}
	}
	for _, field := range []string{"bootstrap", "identity", "systemRole"} {
		if _, ok := node.Properties[field]; ok {
			t.Fatalf("source node schema exposes removed %q", field)
		}
	}
	assertSchemaFields(t, document.Defs, "configbundle.SourceInstallLayer", []string{"systemDisk"}, []string{"extraDisks", "targetDisk", "targetDiskDefaults", "volumes"})
	assertSchemaFields(t, document.Defs, "configbundle.SourceStorageLayer", []string{"disks"}, nil)
	assertSchemaFields(t, document.Defs, "configbundle.SourceHostConfiguration", []string{"fileSets", "sysfs"}, []string{"sets"})
	assertSchemaFields(t, document.Defs, "configbundle.SourceHostConfigurationSysfsSetting", []string{"path", "value"}, []string{"name"})
	assertSchemaFields(t, document.Defs, "configbundle.SourceHostConfigurationFileSet", []string{"files", "onChange", "state"}, []string{"notify"})
	assertSchemaFields(t, document.Defs, "configbundle.SourceSystemExtension",
		[]string{"bundle", "configuration", "name", "state", "units"},
		[]string{"architecture", "artifactVersion", "bundleManifestDigest", "ociManifestDigest", "payloadVersion", "payloads", "supportedRuntimeInterfaces"})
	assertSchemaFields(t, document.Defs, "controlplaneendpoint.BGP", []string{"routeExchanges"}, []string{"routeExchange"})
	assertSchemaFields(t, document.Defs, "controlplaneendpoint.PrefixEnvelope", []string{"exactPrefixLength"}, []string{"prefixLength"})
	assertSchemaRequired(t, document.Defs, "configbundle.SourceSpec", "nodes")
	assertSchemaRequired(t, document.Defs, "configbundle.SourceNode", "name")
	assertSchemaRequired(t, document.Defs, "configbundle.SourceHostConfigurationSysfsSetting", "path", "value")
	if selector := document.Defs["configbundle.SourceVolumeSelector"]; len(selector.OneOf) != 2 {
		t.Fatalf("volume selector oneOf branches = %d, want 2", len(selector.OneOf))
	}
	filesystem := document.Defs["configbundle.SourceStorageDisk"].Properties["filesystem"]
	if !slices.Equal(filesystem.Enum, []any{"xfs", "ext4", "btrfs"}) || filesystem.Description == "" {
		t.Fatalf("filesystem schema = %#v", filesystem)
	}
	port := document.Defs["controlplaneendpoint.Config"].Properties["port"]
	if port.Minimum == nil || *port.Minimum != 0 || port.Maximum == nil || *port.Maximum != 65535 || port.Default != float64(6443) {
		t.Fatalf("control-plane port schema = %#v", port)
	}
}

func assertSchemaFields(t *testing.T, definitions map[string]schemaObject, definition string, present, absent []string) {
	t.Helper()
	properties := definitions[definition].Properties
	for _, field := range present {
		if _, ok := properties[field]; !ok {
			t.Fatalf("%s schema is missing %q", definition, field)
		}
	}
	for _, field := range absent {
		if _, ok := properties[field]; ok {
			t.Fatalf("%s schema exposes removed %q", definition, field)
		}
	}
}

func assertSchemaRequired(t *testing.T, definitions map[string]schemaObject, definition string, required ...string) {
	t.Helper()
	for _, field := range required {
		if !slices.Contains(definitions[definition].Required, field) {
			t.Fatalf("%s schema does not require %q: %v", definition, field, definitions[definition].Required)
		}
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
	if selected.Source.Kind != Kind || selected.Source.Metadata.Name != "lab" || len(selected.Source.Spec.Nodes) != 2 {
		t.Fatalf("selected normalized source = %#v", selected.Source)
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
      systemDisk:
        minSizeMiB: 65536
    storage:
      disks:
        - name: data
          selector:
            disk:
              minSizeMiB: 1024
          filesystem: xfs
    access:
      ssh:
        authorizedKeys:
          - ` + testSSHKey + `
    hostConfiguration:
      fileSets:
        common-network:
          files:
            - path: /etc/systemd/network/10-common.network
              content: |
                [Match]
                Name=enp1s0

                [Network]
                DHCP=yes
  nodes:
    - name: cp-1
      controlPlane: true
      management:
        address: 10.0.0.11
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-root
      storage:
        disks:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
      kubernetes:
        labels:
          katl.dev/zone: rack-a
        taints:
          - key: node-role.kubernetes.io/control-plane
            effect: NoSchedule
    - name: worker-1
      management:
        address: 10.0.0.21
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
      storage:
        disks:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/ata-worker-data
      kubernetes:
        labels:
          katl.dev/pool: workers
`
}

func nativeFile(files []confext.NativeEtcFile, path string) *confext.NativeEtcFile {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

func kubeadmDocument(documents []map[string]any, kind string) map[string]any {
	for _, document := range documents {
		if document["kind"] == kind {
			return document
		}
	}
	return nil
}

func nestedString(document map[string]any, path ...string) string {
	var value any = document
	for _, segment := range path {
		mapping, _ := value.(map[string]any)
		value = mapping[segment]
	}
	text, _ := value.(string)
	return text
}

func kubeletNodeIP(document map[string]any) string {
	registration, _ := document["nodeRegistration"].(map[string]any)
	arguments, _ := registration["kubeletExtraArgs"].([]any)
	for _, raw := range arguments {
		argument, _ := raw.(map[string]any)
		if argument["name"] == "node-ip" {
			value, _ := argument["value"].(string)
			return value
		}
	}
	return ""
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
