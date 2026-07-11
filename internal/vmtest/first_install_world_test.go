package vmtest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"gopkg.in/yaml.v3"
)

func firstInstallWorldRunFor(t *testing.T, name string, spec NodeSpec, useInstalledESP bool) (firstInstallWorldRun, bool) {
	t.Helper()
	return firstInstallWorldRunForMode(t, name, spec, useInstalledESP, firstInstallWorldPreseed)
}

func firstInstallWorldRunForMode(t *testing.T, name string, spec NodeSpec, useInstalledESP bool, mode firstInstallWorldMode) (firstInstallWorldRun, bool) {
	t.Helper()
	if loadWorldManifestPath() == "" {
		return firstInstallWorldRun{}, false
	}
	world := RequireWorld(t)
	run, err := planFirstInstallWorldRun(world, name, repoRoot(t), spec, DefaultFirstInstallWorldInputFromEnv(mode, useInstalledESP), DefaultOptions().KVM)
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
}

func TestDefaultFirstInstallWorldInputIgnoresManualFixtureEnv(t *testing.T) {
	t.Setenv("KATL_RUNTIME_ESP_ARTIFACTS", "/tmp/runtime-esp")
	t.Setenv("KATL_INSTALLED_ESP_ARTIFACTS", "/tmp/installed-esp")
	t.Setenv("KATL_RUNTIME_NODE_METADATA", "/tmp/runtime-node.json")
	t.Setenv("KATL_INSTALLED_NODE_METADATA", "/tmp/installed-node.json")

	input := DefaultFirstInstallWorldInputFromEnv(FirstInstallWorldPreseed, false)
	if input.RuntimeESP != "" || input.NodeMetadata != "" {
		t.Fatalf("manual fixture env leaked into world input: %#v", input)
	}
}

func TestPlanFirstInstallWorldRunStagesInputs(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	manifest := writeFixtureLocalInstallManifest(t, sourceDir)
	metadata := writeFixtureNodeMetadata(t, filepath.Join(sourceDir, "node.json"), Node{Name: "cp-1", Role: ControlPlane})

	run, err := planFirstInstallWorldRun(world, "first install world", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		NodeMetadata:    metadata,
		InstallManifest: manifest,
		TargetDiskSize:  "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun() error = %v", err)
	}
	if run.Repo != repo || run.Runner.options().Missing != MissingFails || run.Runner.options().Keep != KeepAlways {
		t.Fatalf("run metadata = %#v", run)
	}
	if run.Config.Installer.InstallerUKI == installer || run.Config.Installer.RuntimeArtifact != "" {
		t.Fatalf("installer inputs were not staged: %#v", run.Config.Installer)
	}
	if run.Config.Runtime.ESPArtifacts != "" || run.Config.Runtime.NodeMetadata == metadata {
		t.Fatalf("runtime inputs were not staged: %#v", run.Config.Runtime)
	}
	if run.Config.ManifestPath == manifest || !run.Config.PreseedManifest {
		t.Fatalf("install config = %#v", run.Config)
	}
	if run.Config.TargetDisk.Kind != DiskTarget || run.Config.TargetDisk.Size != "20G" {
		t.Fatalf("target disk = %#v", run.Config.TargetDisk)
	}
	scenarioManifest := readScenarioManifest(t, run.Scenario.ManifestPath)
	for _, kind := range []string{FixtureInstallerUKI, FixtureNodeMetadata, FixtureInstallManifest, FixtureFirstInstallDisk} {
		if !hasFixtureKind(scenarioManifest.Fixtures, kind) {
			t.Fatalf("scenario fixtures missing %s: %#v", kind, scenarioManifest.Fixtures)
		}
	}
}

func TestPlanFirstInstallWorldRunGuestHandoffMode(t *testing.T) {
	world := testWorld(t)
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	manifest := writeFixtureLocalInstallManifest(t, sourceDir)

	run, err := planFirstInstallWorldRun(world, "guest handoff world", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		InstallManifest: manifest,
		Mode:            firstInstallWorldGuestHandoff,
		TargetDiskSize:  "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun() error = %v", err)
	}
	if !run.Config.GuestHandoff || run.Config.PreseedManifest {
		t.Fatalf("handoff mode config = %#v", run.Config)
	}
}

func TestPlanFirstInstallWorldRunRejectsLooseComponentManifest(t *testing.T) {
	world := testWorld(t)
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	legacy := strings.Replace(firstManifest(),
		`"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}`,
		`"artifacts": {
			"runtimeRoot": {"url": "https://example.invalid/root.squashfs"},
			"uki": {"url": "https://example.invalid/katl.efi"},
			"sysexts": [{"url": "https://example.invalid/kubernetes.raw"}]
		}`, 1)
	manifest := writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), legacy)

	_, err := planFirstInstallWorldRun(world, "loose component manifest", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		InstallManifest: manifest,
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}, KVMOff)
	if err == nil || !strings.Contains(err.Error(), `loose component field "artifacts"`) {
		t.Fatalf("planFirstInstallWorldRun() error = %v, want loose artifacts rejection", err)
	}
}

func TestPlanFirstInstallWorldRunKeepsInstallerArtifactsGeneric(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	sourceDir := t.TempDir()
	kernel := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.vmlinuz"), "generic split installer kernel")
	initrd := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.initrd"), "generic split installer initrd")
	uki := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "generic local handoff installer uki")
	writeFixtureKatlOSInstallImage(t, repo)

	splitInput := firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerKernel: kernel, InstallerInitrd: initrd},
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}
	cpSplit, err := planFirstInstallWorldRun(world, "split preseed cp", repo, NodeSpec{Name: "cp-generic-1", Role: ControlPlane}, splitInput, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(cp split) error = %v", err)
	}
	workerSplit, err := planFirstInstallWorldRun(world, "split preseed worker", repo, NodeSpec{Name: "worker-generic-1", Role: Worker}, splitInput, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(worker split) error = %v", err)
	}
	assertSameSHA256(t, cpSplit.Config.Installer.InstallerKernel, kernel)
	assertSameSHA256(t, workerSplit.Config.Installer.InstallerKernel, kernel)
	assertSameSHA256(t, cpSplit.Config.Installer.InstallerInitrd, initrd)
	assertSameSHA256(t, workerSplit.Config.Installer.InstallerInitrd, initrd)
	assertSameFixtureSHA256(t, cpSplit, workerSplit, FixtureInstallerKernel)
	assertSameFixtureSHA256(t, cpSplit, workerSplit, FixtureInstallerInitrd)
	if !cpSplit.Config.PreseedManifest || !workerSplit.Config.PreseedManifest {
		t.Fatalf("split runs did not use preseed manifests: cp=%#v worker=%#v", cpSplit.Config, workerSplit.Config)
	}
	assertFileContains(t, cpSplit.Config.ManifestPath, `"hostname": "cp-generic-1"`)
	assertFileContains(t, workerSplit.Config.ManifestPath, `"hostname": "worker-generic-1"`)
	assertFileContains(t, workerSplit.Config.ManifestPath, `"systemRole": "worker"`)
	assertDifferentSHA256(t, cpSplit.Config.ManifestPath, workerSplit.Config.ManifestPath)
	assertDifferentFixtureSHA256(t, cpSplit, workerSplit, FixtureInstallManifest)
	assertDifferentFixtureSHA256(t, cpSplit, workerSplit, FixtureNodeMetadata)
	assertDifferentFixtureProperty(t, cpSplit, workerSplit, FixtureInstallManifest, "kubeadmConfigFilesTreeSHA256")
	assertDifferentExternalTreeSHA256(t, cpSplit.Config.ManifestPath, workerSplit.Config.ManifestPath, installer.KubeadmConfigFilesDir)
	assertSameFixtureSHA256(t, cpSplit, workerSplit, FixtureKatlOSInstallImage)

	handoffInput := firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: uki},
		Mode:            firstInstallWorldGuestHandoff,
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}
	cpHandoff, err := planFirstInstallWorldRun(world, "local handoff cp", repo, NodeSpec{Name: "cp-local-1", Role: ControlPlane}, handoffInput, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(cp handoff) error = %v", err)
	}
	workerHandoff, err := planFirstInstallWorldRun(world, "local handoff worker", repo, NodeSpec{Name: "worker-local-1", Role: Worker}, handoffInput, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(worker handoff) error = %v", err)
	}
	assertSameSHA256(t, cpHandoff.Config.Installer.InstallerUKI, uki)
	assertSameSHA256(t, workerHandoff.Config.Installer.InstallerUKI, uki)
	assertSameFixtureSHA256(t, cpHandoff, workerHandoff, FixtureInstallerUKI)
	if !cpHandoff.Config.GuestHandoff || cpHandoff.Config.PreseedManifest ||
		!workerHandoff.Config.GuestHandoff || workerHandoff.Config.PreseedManifest {
		t.Fatalf("local handoff runs did not use handoff mode: cp=%#v worker=%#v", cpHandoff.Config, workerHandoff.Config)
	}
	assertDifferentSHA256(t, cpHandoff.Config.ManifestPath, workerHandoff.Config.ManifestPath)
	assertDifferentFixtureSHA256(t, cpHandoff, workerHandoff, FixtureInstallManifest)
	assertDifferentFixtureSHA256(t, cpHandoff, workerHandoff, FixtureNodeMetadata)
	assertDifferentFixtureProperty(t, cpHandoff, workerHandoff, FixtureInstallManifest, "kubeadmConfigFilesTreeSHA256")
	assertDifferentExternalTreeSHA256(t, cpHandoff.Config.ManifestPath, workerHandoff.Config.ManifestPath, installer.KubeadmConfigFilesDir)
	assertSameFixtureSHA256(t, cpHandoff, workerHandoff, FixtureKatlOSInstallImage)
	assertGenericArtifactOmitValues(t, []string{kernel, initrd, uki}, []string{
		"cp-generic-1",
		"worker-generic-1",
		"cp-local-1",
		"worker-local-1",
		"control-plane",
		"katl-smoke",
		"/dev/disk/by-id/virtio-katl-root",
		"10.244.0.0/16",
		"10.96.0.0/12",
	})

	leakyKernel := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer-leaky.vmlinuz"), "installer image leaked cp-leaked-1")
	_, err = planFirstInstallWorldRun(world, "split leaky cp", repo, NodeSpec{Name: "cp-leaked-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerKernel: leakyKernel, InstallerInitrd: initrd},
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}, KVMOff)
	if err == nil || !strings.Contains(err.Error(), "embeds external node config value") {
		t.Fatalf("planFirstInstallWorldRun(leaky installer) error = %v, want generic artifact scan failure", err)
	}

	leakyRoleKernel := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer-role-leaky.vmlinuz"), `installer image leaked "systemRole": "control-plane"`)
	_, err = planFirstInstallWorldRun(world, "split role leaky cp", repo, NodeSpec{Name: "cp-role-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerKernel: leakyRoleKernel, InstallerInitrd: initrd},
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}, KVMOff)
	if err == nil || !strings.Contains(err.Error(), `systemRole`) {
		t.Fatalf("planFirstInstallWorldRun(role leak) error = %v, want role scan failure", err)
	}
}

func TestExternalConfigLiteralsIncludeRolesNetworkdAndKubeadmSecrets(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeFixtureFile(t, filepath.Join(root, "install-manifest.json"), `{
  "apiVersion": "install.katl.dev/v1alpha1",
  "kind": "InstallManifest",
  "node": {
    "identity": {
      "hostname": "n1",
      "ssh": {
        "authorizedKeys": [
          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
        ]
      }
    },
  "systemRole": "control-plane",
    "networkd": {
      "files": [
        {"name": "80-static.network", "content": "[Network]\nAddress=192.0.2.10/24\nGateway=192.0.2.1\nDNS=192.0.2.53\n"}
      ]
    }
  },
  "install": {
    "wipeTarget": true,
    "targetDisk": {"byID": "/dev/disk/by-id/virtio-static-root", "minSizeMiB": 32}
  },
  "katlosImage": {
    "localRef": "katlos-install.squashfs",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 11,
    "version": "2026.06.04",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  }
}`)
	kubeadmDir := filepath.Join(root, "kubeadm")
	writeFixtureFile(t, filepath.Join(kubeadmDir, "join.yaml"), `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  name: n1
discovery:
  bootstrapToken:
    apiServerEndpoint: 192.0.2.60:6443
    token: abcdef.0123456789abcdef
    caCertHashes:
    - sha256:111122223333444455556666777788889999aaaabbbbccccddddeeeeffff0000
controlPlane:
  certificateKey: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	nodeMetadata := writeFixtureFile(t, filepath.Join(root, "node.json"), `{"identity":{"hostname":"n1"},"systemRole":"control-plane"}`)

	values, err := externalConfigLiterals(manifestPath, nodeMetadata)
	if err != nil {
		t.Fatalf("externalConfigLiterals() error = %v", err)
	}
	for _, want := range []string{
		`"hostname": "n1"`,
		"name: n1",
		`"systemRole": "control-plane"`,
		"192.0.2.10/24",
		"192.0.2.1",
		"192.0.2.53",
		"apiServerEndpoint: 192.0.2.60:6443",
		"/dev/disk/by-id/virtio-static-root",
		"abcdef.0123456789abcdef",
		"sha256:111122223333444455556666777788889999aaaabbbbccccddddeeeeffff0000",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if !stringSliceContains(values, want) {
			t.Fatalf("external config literals missing %q from %#v", want, values)
		}
	}
	if stringSliceContains(values, "192.0.2.60:6443") {
		t.Fatalf("external config literals included raw kubeadm endpoint in %#v", values)
	}
}

func TestScanExternalConfigAvoidsShortHostnameSubstrings(t *testing.T) {
	values := map[string]bool{}
	addHostnameLiterals(values, "cp-1")
	addYAMLKeyContextLiterals(values, "name", &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: "cp-1",
	})

	literals := make([]string, 0, len(values))
	for value := range values {
		literals = append(literals, value)
	}
	if value, ok := scanBytesForExternalConfig([]byte("encoding cp-1250 and protocol tcp-1 are generic system strings"), literals); ok {
		t.Fatalf("short hostname matched unrelated generic string %q in %#v", value, literals)
	}
	for _, data := range []string{
		`{"hostname":"cp-1"}`,
		`{"hostname": "cp-1"}`,
		"name: cp-1",
		`name: "cp-1"`,
	} {
		if _, ok := scanBytesForExternalConfig([]byte(data), literals); !ok {
			t.Fatalf("short hostname context %q was not detected by %#v", data, literals)
		}
	}
}

func TestKubeadmSubnetLiteralsIgnorePackagedDefaults(t *testing.T) {
	values := map[string]bool{}
	addYAMLKeyContextLiterals(values, "serviceSubnet", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "10.96.0.0/12"})
	addYAMLKeyContextLiterals(values, "podSubnet", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "10.244.0.0/16"})
	addYAMLKeyContextLiterals(values, "serviceSubnet", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "172.30.0.0/16"})
	addYAMLKeyContextLiterals(values, "podSubnet", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "192.168.0.0/16"})

	for _, refused := range []string{"serviceSubnet: 10.96.0.0/12", "podSubnet: 10.244.0.0/16"} {
		if values[refused] {
			t.Fatalf("generic kubeadm default %q was added to external config literals: %#v", refused, values)
		}
	}
	for _, want := range []string{"serviceSubnet: 172.30.0.0/16", "podSubnet: 192.168.0.0/16"} {
		if !values[want] {
			t.Fatalf("non-default kubeadm subnet %q missing from external config literals: %#v", want, values)
		}
	}
}

func TestScanGenericInstallerArtifactChecksGzipPayload(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("installer payload leaked cp-gzip-1")); err != nil {
		t.Fatalf("gzip write error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close error = %v", err)
	}
	path := writeFixtureFile(t, filepath.Join(t.TempDir(), "initrd.img"), compressed.String())
	err := scanGenericInstallerArtifact(path, []string{"cp-gzip-1"})
	if err == nil || !strings.Contains(err.Error(), "cp-gzip-1") {
		t.Fatalf("scanGenericInstallerArtifact() error = %v, want gzip payload leak", err)
	}
}

func TestScanGenericInstallerArtifactChecksZstdPayload(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not available")
	}
	cmd := exec.Command("zstd", "-q", "-c")
	cmd.Stdin = strings.NewReader("installer payload leaked cp-zstd-1")
	compressed, err := cmd.Output()
	if err != nil {
		t.Fatalf("zstd compress error = %v", err)
	}
	path := writeFixtureFile(t, filepath.Join(t.TempDir(), "initrd.zst"), string(compressed))
	err = scanGenericInstallerArtifact(path, []string{"cp-zstd-1"})
	if err == nil || !strings.Contains(err.Error(), "cp-zstd-1") {
		t.Fatalf("scanGenericInstallerArtifact() error = %v, want zstd payload leak", err)
	}
}

func TestScanGenericInstallerArtifactChecksSquashFSPayload(t *testing.T) {
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not available")
	}
	if _, err := exec.LookPath("unsquashfs"); err != nil {
		t.Skip("unsquashfs not available")
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "etc", "katl", "leak.txt"), "installer payload leaked cp-squash-1")
	image := filepath.Join(t.TempDir(), "katlos-install.squashfs")
	if output, err := exec.Command("mksquashfs", root, image, "-noappend", "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("mksquashfs error = %v\n%s", err, output)
	}
	err := scanGenericInstallerArtifact(image, []string{"cp-squash-1"})
	if err == nil || !strings.Contains(err.Error(), "cp-squash-1") {
		t.Fatalf("scanGenericInstallerArtifact() error = %v, want squashfs payload leak", err)
	}
}

func TestPlanFirstInstallWorldRunResolvesLocalMkosiArtifacts(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	installer := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.efi"), "installer")
	installerISO := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.iso"), "installer-iso")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-kubernetes.raw"), "kubernetes")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), `{"payloadVersion":"v1.36.0"}`)
	image := writeFixtureFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos-image")
	writeFixtureFile(t, image+".json", `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KatlOSImageArtifact",
  "imageRole": "install",
  "version": "0.0.0-dev",
  "architecture": "x86_64",
  "runtimeInterface": "katl-runtime-1",
  "sizeBytes": 11,
  "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`)
	writeFixtureKatlOSInstallImageRoot(t, mkosiDir, "0.0.0-dev")
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"_build/mkosi/katl-installer.efi"},
    {"kind":"installer-iso","path":"_build/mkosi/katl-installer.iso"},
    {"kind":"runtime-root","path":"_build/mkosi/katl-runtime-root.squashfs"}
  ]
}`)

	run, err := planFirstInstallWorldRun(world, "local mkosi first install", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		TargetDiskSize: "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun() error = %v", err)
	}
	if run.Config.Installer.InstallerUKI == installer || run.Config.Installer.InstallerUKI == "" {
		t.Fatalf("installer UKI was not staged: %#v", run.Config.Installer)
	}
	if run.Config.Installer.RuntimeArtifact != "" {
		t.Fatalf("first-install world staged loose runtime artifact: %#v", run.Config.Installer)
	}
	if !run.Config.UseInstalledESP || run.Config.Runtime.ESPArtifacts != "" {
		t.Fatalf("installed ESP fallback was not selected: %#v", run.Config)
	}
	if run.Config.Runtime.NodeMetadata == "" || strings.Contains(run.Config.Runtime.NodeMetadata, "node-metadata-source") {
		t.Fatalf("node metadata was not staged: %#v", run.Config.Runtime)
	}
	if run.Config.ManifestPath == "" || strings.Contains(run.Config.ManifestPath, "install-manifest-source") {
		t.Fatalf("install manifest was not staged: %q", run.Config.ManifestPath)
	}
	if run.Config.ConfigBundle == "" || !strings.HasSuffix(run.Config.ConfigBundle, "config.katlcfg") {
		t.Fatalf("config bundle was not staged: %#v", run.Config)
	}
	stagedImage := filepath.Join(filepath.Dir(run.Config.ManifestPath), filepath.Base(image))
	if data, err := os.ReadFile(stagedImage); err != nil || string(data) != "katlos-image" {
		t.Fatalf("staged KatlOS image = %q, err = %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(run.Config.ManifestPath), "single-image-proof.json")); err != nil {
		t.Fatalf("single-image install proof was not staged: %v", err)
	}
	manifestData, err := os.ReadFile(run.Config.ManifestPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", run.Config.ManifestPath, err)
	}
	if !strings.Contains(string(manifestData), `"hostname": "cp-1"`) ||
		!strings.Contains(string(manifestData), `"localRef": "katlos-install-0.0.0-dev-x86_64.squashfs"`) ||
		!strings.Contains(string(manifestData), `"configRef": "control-plane"`) ||
		!strings.Contains(string(manifestData), `"name": "80-katl-vmtest-dhcp.network"`) ||
		!strings.Contains(string(manifestData), `Name=en*`) ||
		!strings.Contains(string(manifestData), `DHCP=yes`) {
		t.Fatalf("generated manifest = %s", manifestData)
	}
	manifestDir := filepath.Dir(run.Config.ManifestPath)
	if data, err := os.ReadFile(filepath.Join(manifestDir, "kubeadm-configs", "control-plane.yaml")); err != nil || !strings.Contains(string(data), "configFile: kubeadm/control-plane.yaml") {
		t.Fatalf("generated KubeadmConfig = %q, err = %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(manifestDir, "kubeadm", "control-plane.yaml")); err != nil || !strings.Contains(string(data), "kind: InitConfiguration") || !strings.Contains(string(data), "kubernetesVersion: v1.36.0") {
		t.Fatalf("generated kubeadm config = %q, err = %v", data, err)
	}
	scenarioManifest := readScenarioManifest(t, run.Scenario.ManifestPath)
	metadataData, err := os.ReadFile(run.Config.Runtime.NodeMetadata)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", run.Config.Runtime.NodeMetadata, err)
	}
	if !strings.Contains(string(metadataData), `"hostname": "cp-1"`) || !strings.Contains(string(metadataData), `"systemRole": "control-plane"`) {
		t.Fatalf("generated node metadata = %s", metadataData)
	}
	for _, kind := range []string{FixtureInstallerUKI, FixtureNodeMetadata, FixtureInstallManifest, FixtureConfigBundle, FixtureKatlOSInstallImage, FixtureFirstInstallDisk} {
		if !hasFixtureKind(scenarioManifest.Fixtures, kind) {
			t.Fatalf("scenario fixtures missing %s: %#v", kind, scenarioManifest.Fixtures)
		}
	}

	handoff, err := planFirstInstallWorldRun(world, "local mkosi handoff", repo, NodeSpec{Name: "cp-2", Role: ControlPlane}, firstInstallWorldInput{
		Mode:           firstInstallWorldGuestHandoff,
		TargetDiskSize: "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(handoff) error = %v", err)
	}
	if handoff.Config.Installer.InstallerISO == "" || handoff.Config.Installer.InstallerISO == installerISO || handoff.Config.Installer.InstallerUKI != "" {
		t.Fatalf("handoff installer input = %#v", handoff.Config.Installer)
	}
	if handoff.Config.ConfigBundle == "" || handoff.Config.SelectedNode != "cp-2" {
		t.Fatalf("handoff bundle input = %#v", handoff.Config)
	}
}

func TestPlanFirstInstallWorldRunSelectsNodesFromSharedConfigBundle(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	sourceDir := t.TempDir()
	installerUKI := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	bundle := writeSharedFirstInstallConfigBundle(t, sourceDir)
	input := firstInstallWorldInput{
		Installer:      InstallerBootConfig{InstallerUKI: installerUKI},
		ConfigBundle:   bundle,
		TargetDiskSize: "20G",
	}

	cpRun, err := planFirstInstallWorldRun(world, "shared bundle cp", repo, NodeSpec{Name: "cp-shared-1", Role: ControlPlane}, input, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(cp) error = %v", err)
	}
	workerRun, err := planFirstInstallWorldRun(world, "shared bundle worker", repo, NodeSpec{Name: "worker-shared-1", Role: Worker}, input, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun(worker) error = %v", err)
	}
	if cpRun.Config.SelectedNode != "cp-shared-1" || workerRun.Config.SelectedNode != "worker-shared-1" {
		t.Fatalf("selected nodes = %q / %q", cpRun.Config.SelectedNode, workerRun.Config.SelectedNode)
	}
	assertSameSHA256(t, cpRun.Config.ConfigBundle, bundle)
	assertSameSHA256(t, workerRun.Config.ConfigBundle, bundle)
	assertSameFixtureSHA256(t, cpRun, workerRun, FixtureConfigBundle)
	assertDifferentSHA256(t, cpRun.Config.ManifestPath, workerRun.Config.ManifestPath)
	assertDifferentFixtureSHA256(t, cpRun, workerRun, FixtureInstallManifest)
	assertDifferentFixtureProperty(t, cpRun, workerRun, FixtureInstallManifest, "kubeadmConfigFilesTreeSHA256")
	assertDifferentExternalTreeSHA256(t, cpRun.Config.ManifestPath, workerRun.Config.ManifestPath, installer.KubeadmConfigFilesDir)
	assertFileContains(t, cpRun.Config.ManifestPath, `"hostname": "cp-shared-1"`)
	assertFileContains(t, cpRun.Config.ManifestPath, `"systemRole": "control-plane"`)
	assertFileContains(t, cpRun.Config.ManifestPath, `"byID": "/dev/disk/by-id/virtio-katl-control-plane-root"`)
	assertFileContains(t, workerRun.Config.ManifestPath, `"hostname": "worker-shared-1"`)
	assertFileContains(t, workerRun.Config.ManifestPath, `"systemRole": "worker"`)
	assertFileContains(t, workerRun.Config.ManifestPath, `"byID": "/dev/disk/by-id/virtio-katl-worker-root"`)
	assertFileContains(t, filepath.Join(filepath.Dir(cpRun.Config.ManifestPath), installer.KubeadmConfigFilesDir, "control-plane.yaml"), "name: cp-shared-1")
	assertFileContains(t, filepath.Join(filepath.Dir(workerRun.Config.ManifestPath), installer.KubeadmConfigFilesDir, "worker.yaml"), "name: worker-shared-1")
}

func writeFixtureKatlOSInstallImage(t *testing.T, repo string) string {
	t.Helper()
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	image := writeFixtureFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos-image")
	writeFixtureFile(t, image+".json", `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KatlOSImageArtifact",
  "imageRole": "install",
  "version": "0.0.0-dev",
  "architecture": "x86_64",
  "runtimeInterface": "katl-runtime-1",
  "sizeBytes": 11,
  "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`)
	writeFixtureKatlOSInstallImageRoot(t, mkosiDir, "0.0.0-dev")
	return image
}

func writeSharedFirstInstallConfigBundle(t *testing.T, dir string) string {
	t.Helper()
	image := writeFixtureFile(t, filepath.Join(dir, "katlos-install-2026.06.04-x86_64.squashfs"), "katlos-image")
	writeFixtureKatlOSInstallImageRoot(t, dir, "2026.06.04")
	source := map[string]any{
		"apiVersion": configbundle.APIVersion,
		"kind":       configbundle.Kind,
		"metadata": map[string]any{
			"name": "katl-shared",
		},
		"spec": map[string]any{
			"controlPlaneEndpoint": "api.katl.test:6443",
			"kubernetes": map[string]any{
				"version": "v1.36.1",
			},
			"katlosImage": map[string]any{
				"localRef":         filepath.Base(image),
				"sha256":           strings.Repeat("a", 64),
				"sizeBytes":        11,
				"version":          "2026.06.04",
				"architecture":     "x86_64",
				"runtimeInterface": "katl-runtime-1",
				"role":             "install",
			},
			"defaults": map[string]any{
				"install": map[string]any{
					"wipeTarget": true,
				},
				"identity": map[string]any{
					"ssh": map[string]any{
						"authorizedKeys": []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"},
					},
				},
				"networkd": map[string]any{
					"files": []map[string]any{{
						"name":    "80-katl-vmtest-dhcp.network",
						"content": "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n",
					}},
				},
				"bootstrap": map[string]any{
					"access": map[string]any{
						"method":        "agent",
						"credentialRef": "vsock:1234:10240",
					},
				},
			},
			"systemRoleDefaults": map[string]any{
				string(ControlPlane): map[string]any{
					"kubernetes": map[string]any{
						"kubeadm": map[string]any{"configRef": "control-plane"},
					},
				},
				string(Worker): map[string]any{
					"kubernetes": map[string]any{
						"kubeadm": map[string]any{"configRef": "worker"},
					},
				},
			},
			"kubeadmConfigs": map[string]any{
				"control-plane": map[string]any{"config": controlPlaneKubeadmConfig("cp-shared-1", "v1.36.1")},
				"worker":        map[string]any{"config": workerKubeadmConfig("worker-shared-1")},
			},
			"nodes": []map[string]any{
				firstInstallWorldSourceNode("cp-shared-1", ControlPlane, "/dev/disk/by-id/virtio-katl-control-plane-root"),
				firstInstallWorldSourceNode("worker-shared-1", Worker, "/dev/disk/by-id/virtio-katl-worker-root"),
			},
		},
	}
	sourceData, err := yaml.Marshal(source)
	if err != nil {
		t.Fatalf("marshal shared config bundle source: %v", err)
	}
	sourcePath := writeFixtureFile(t, filepath.Join(dir, "cluster.yaml"), string(sourceData))
	bundlePath := filepath.Join(dir, "config.katlcfg")
	if _, err := configbundle.WriteArchive(bundlePath, configbundle.BuildRequest{SourcePath: sourcePath, CreatedBy: "vmtest shared bundle"}); err != nil {
		t.Fatalf("WriteArchive() error = %v", err)
	}
	return bundlePath
}

func writeFixtureLocalInstallManifest(t *testing.T, dir string) string {
	t.Helper()
	image := writeFixtureFile(t, filepath.Join(dir, "katlos-install.squashfs"), "katlos-image")
	writeFixtureKatlOSInstallImageRoot(t, dir, "2026.06.04")
	manifest := strings.Replace(firstManifest(), `"url": "https://example.invalid/katlos-install.squashfs",`, `"localRef": "`+filepath.Base(image)+`",`, 1)
	return writeFixtureFile(t, filepath.Join(dir, "install-manifest.json"), manifest)
}

func writeFixtureKatlOSInstallImageRoot(t *testing.T, mkosiDir, version string) {
	t.Helper()
	root := filepath.Join(mkosiDir, "katlos-install-root")
	components := map[string][]byte{
		"components/runtime/root.squashfs": []byte("runtime root"),
		"components/boot/katl.efi":         []byte("runtime uki"),
	}
	digests := make(map[string]string, len(components))
	sizes := make(map[string]int64, len(components))
	for rel, data := range components {
		writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(rel)), string(data))
		digests[rel] = sha256Hex(data)
		sizes[rel] = int64(len(data))
	}
	index := map[string]any{
		"apiVersion":       "katl.dev/v1alpha1",
		"kind":             "KatlOSImage",
		"imageRole":        "install",
		"format":           "squashfs",
		"version":          version,
		"buildID":          "test-build",
		"architecture":     "x86_64",
		"runtimeInterface": "katl-runtime-1",
		"createdAt":        "2026-06-17T12:00:00Z",
		"components": []map[string]any{
			{
				"name":         "runtime-root",
				"role":         "runtime-root",
				"path":         "components/runtime/root.squashfs",
				"format":       "squashfs",
				"sizeBytes":    sizes["components/runtime/root.squashfs"],
				"sha256":       digests["components/runtime/root.squashfs"],
				"version":      version,
				"architecture": "x86_64",
				"compatibility": map[string]any{
					"runtimeInterface": "katl-runtime-1",
				},
				"installTarget": map[string]any{"kind": "root-slot", "filesystem": "squashfs"},
			},
			{
				"name":         "runtime-uki",
				"role":         "runtime-uki",
				"path":         "components/boot/katl.efi",
				"format":       "uki",
				"sizeBytes":    sizes["components/boot/katl.efi"],
				"sha256":       digests["components/boot/katl.efi"],
				"version":      version,
				"architecture": "x86_64",
				"compatibility": map[string]any{
					"runtimeInterface": "katl-runtime-1",
					"runtimeRoot": map[string]any{
						"interface":      "katl-runtime-1",
						"artifactPath":   "components/runtime/root.squashfs",
						"artifactSHA256": digests["components/runtime/root.squashfs"],
					},
					"kernelCommandLine": []string{"quiet"},
				},
				"installTarget": map[string]any{"kind": "esp-or-xbootldr", "filename": "katl.efi"},
			},
		},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture KatlOS image index: %v", err)
	}
	writeFixtureFile(t, filepath.Join(root, "katlos", "image.json"), string(append(data, '\n')))
}

func assertSameSHA256(t *testing.T, gotPath, wantPath string) {
	t.Helper()
	got, err := fileSHA256(gotPath)
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v", gotPath, err)
	}
	want, err := fileSHA256(wantPath)
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v", wantPath, err)
	}
	if got != want {
		t.Fatalf("%s sha256 = %s, want %s from %s", gotPath, got, want, wantPath)
	}
}

func assertGenericArtifactOmitValues(t *testing.T, paths []string, forbidden []string) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		for _, value := range forbidden {
			if strings.Contains(string(data), value) {
				t.Fatalf("%s embeds node-specific value %q", path, value)
			}
		}
	}
}

func assertDifferentSHA256(t *testing.T, firstPath, secondPath string) {
	t.Helper()
	first, err := fileSHA256(firstPath)
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v", firstPath, err)
	}
	second, err := fileSHA256(secondPath)
	if err != nil {
		t.Fatalf("fileSHA256(%s) error = %v", secondPath, err)
	}
	if first == second {
		t.Fatalf("%s and %s both have sha256 %s, want different external config digests", firstPath, secondPath, first)
	}
}

func assertSameFixtureSHA256(t *testing.T, first, second firstInstallWorldRun, kind string) {
	t.Helper()
	firstSHA := fixtureSHA256(t, first.Scenario.ManifestPath, first.Node.Name, kind)
	secondSHA := fixtureSHA256(t, second.Scenario.ManifestPath, second.Node.Name, kind)
	if firstSHA != secondSHA {
		t.Fatalf("%s fixture sha256 changed across node configs: %s=%s %s=%s", kind, first.Node.Name, firstSHA, second.Node.Name, secondSHA)
	}
}

func assertDifferentFixtureSHA256(t *testing.T, first, second firstInstallWorldRun, kind string) {
	t.Helper()
	firstSHA := fixtureSHA256(t, first.Scenario.ManifestPath, first.Node.Name, kind)
	secondSHA := fixtureSHA256(t, second.Scenario.ManifestPath, second.Node.Name, kind)
	if firstSHA == secondSHA {
		t.Fatalf("%s fixture sha256 did not change across node configs: %s=%s %s=%s", kind, first.Node.Name, firstSHA, second.Node.Name, secondSHA)
	}
}

func assertDifferentFixtureProperty(t *testing.T, first, second firstInstallWorldRun, kind, key string) {
	t.Helper()
	firstValue := fixtureProperty(t, first.Scenario.ManifestPath, first.Node.Name, kind, key)
	secondValue := fixtureProperty(t, second.Scenario.ManifestPath, second.Node.Name, kind, key)
	if firstValue == secondValue {
		t.Fatalf("%s fixture property %s did not change across node configs: %s", kind, key, firstValue)
	}
}

func fixtureProperty(t *testing.T, manifestPath, nodeName, kind, key string) string {
	t.Helper()
	manifest := readScenarioManifest(t, manifestPath)
	for _, record := range manifest.Fixtures {
		if record.Node == nodeName && record.Kind == kind {
			if record.Properties == nil || record.Properties[key] == "" {
				t.Fatalf("%s fixture for %s missing property %s: %#v", kind, nodeName, key, record)
			}
			return record.Properties[key]
		}
	}
	t.Fatalf("%s fixture for node %s not found in %#v", kind, nodeName, manifest.Fixtures)
	return ""
}

func assertDifferentExternalTreeSHA256(t *testing.T, firstManifest, secondManifest, rel string) {
	t.Helper()
	firstSHA, err := espTreeSHA256(filepath.Join(filepath.Dir(firstManifest), rel))
	if err != nil {
		t.Fatalf("espTreeSHA256(%s) error = %v", filepath.Join(filepath.Dir(firstManifest), rel), err)
	}
	secondSHA, err := espTreeSHA256(filepath.Join(filepath.Dir(secondManifest), rel))
	if err != nil {
		t.Fatalf("espTreeSHA256(%s) error = %v", filepath.Join(filepath.Dir(secondManifest), rel), err)
	}
	if firstSHA == secondSHA {
		t.Fatalf("%s tree sha256 did not change across external configs: %s", rel, firstSHA)
	}
}

func fixtureSHA256(t *testing.T, manifestPath, nodeName, kind string) string {
	t.Helper()
	manifest := readScenarioManifest(t, manifestPath)
	for _, record := range manifest.Fixtures {
		if record.Node == nodeName && record.Kind == kind {
			if record.SHA256 == "" {
				t.Fatalf("%s fixture for %s has empty sha256: %#v", kind, nodeName, record)
			}
			return record.SHA256
		}
	}
	t.Fatalf("%s fixture for node %s not found in %#v", kind, nodeName, manifest.Fixtures)
	return ""
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestWorkerKubeadmConfigSetsNodeName(t *testing.T) {
	config := workerKubeadmConfig("worker-1")
	if !strings.Contains(config, "kind: JoinConfiguration") || !strings.Contains(config, "name: worker-1") {
		t.Fatalf("worker kubeadm config = %s", config)
	}
}

func TestPlanFirstInstallWorldRunAcceptsResolvedInstallerArtifact(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	installer := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.efi"), "installer")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"_build/mkosi/katl-installer.efi"},
    {"kind":"runtime-root","path":"_build/mkosi/katl-runtime-root.squashfs"}
  ]
}`)
	writeFixtureFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos-image")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs.json"), `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KatlOSImageArtifact",
  "imageRole": "install",
  "version": "0.0.0-dev",
  "architecture": "x86_64",
  "runtimeInterface": "katl-runtime-1",
  "sizeBytes": 11,
  "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`)
	writeFixtureKatlOSInstallImageRoot(t, mkosiDir, "0.0.0-dev")
	source := writeFixtureFile(t, filepath.Join(repo, "cmd", "katlos-install", "main.go"), "source")
	oldTime := time.Unix(1700000000, 0)
	newTime := oldTime.Add(time.Hour)
	if err := os.Chtimes(installer, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(installer) error = %v", err)
	}
	if err := os.Chtimes(source, newTime, newTime); err != nil {
		t.Fatalf("Chtimes(source) error = %v", err)
	}

	run, err := planFirstInstallWorldRun(world, "stale installer world", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		TargetDiskSize: "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun() error = %v", err)
	}
	if run.Config.Installer.InstallerUKI == "" {
		t.Fatalf("installer was not staged: %#v", run.Config.Installer)
	}
}

func TestPlanFirstInstallWorldRunAcceptsResolvedKatlOSInstallImage(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.efi"), "installer")
	writeFixtureFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	image := writeFixtureFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos-image")
	metadata := writeFixtureFile(t, image+".json", `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "KatlOSImageArtifact",
  "imageRole": "install",
  "version": "0.0.0-dev",
  "architecture": "x86_64",
  "runtimeInterface": "katl-runtime-1",
  "sizeBytes": 11,
  "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`)
	writeFixtureKatlOSInstallImageRoot(t, mkosiDir, "0.0.0-dev")
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"_build/mkosi/katl-installer.efi"},
    {"kind":"runtime-root","path":"_build/mkosi/katl-runtime-root.squashfs"},
    {"kind":"katlos-install-image","path":"_build/mkosi/katlos-install-0.0.0-dev-x86_64.squashfs","metadataPath":"_build/mkosi/katlos-install-0.0.0-dev-x86_64.squashfs.json"}
  ]
}`)
	source := writeFixtureFile(t, filepath.Join(repo, "scripts", "build-katlos-install-image"), "source")
	oldTime := time.Unix(1700000000, 0)
	newTime := oldTime.Add(time.Hour)
	for _, path := range []string{image, metadata} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", path, err)
		}
	}
	if err := os.Chtimes(source, newTime, newTime); err != nil {
		t.Fatalf("Chtimes(source) error = %v", err)
	}

	run, err := planFirstInstallWorldRun(world, "stale katlos image world", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		TargetDiskSize: "20G",
	}, KVMOff)
	if err != nil {
		t.Fatalf("planFirstInstallWorldRun() error = %v", err)
	}
	if run.Config.ManifestPath == "" {
		t.Fatalf("install manifest was not staged: %#v", run.Config)
	}
}

func TestPlanFirstInstallWorldRunWritesSetupFailureForLooseRuntimeInput(t *testing.T) {
	world := testWorld(t)
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	runtime := writeFixtureFile(t, filepath.Join(sourceDir, "katl-runtime-root.squashfs"), "runtime")

	run, err := planFirstInstallWorldRun(world, "loose runtime first install input", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		RuntimeArtifact: runtime,
		InstallManifest: writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), firstManifest()),
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}, KVMAuto)
	if err == nil || !strings.Contains(err.Error(), "loose runtime artifact input is not supported") {
		t.Fatalf("planFirstInstallWorldRun() error = %v, want loose runtime source failure", err)
	}
	if run.Scenario == nil {
		t.Fatal("planFirstInstallWorldRun() did not return scenario on setup failure")
	}
	var result scenarioResult
	readJSONForTest(t, run.Scenario.ResultPath, &result)
	if result.Status != WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, "loose runtime artifact input is not supported") {
		t.Fatalf("result = %#v", result)
	}
}
