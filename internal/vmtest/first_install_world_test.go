package vmtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	runtime := writeFixtureFile(t, filepath.Join(sourceDir, "katl-runtime-root.squashfs"), "runtime")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	manifest := writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), firstManifest())
	metadata := writeFixtureNodeMetadata(t, filepath.Join(sourceDir, "node.json"), Node{Name: "cp-1", Role: ControlPlane})

	run, err := planFirstInstallWorldRun(world, "first install world", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		RuntimeArtifact: runtime,
		RuntimeESP:      esp,
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
	if run.Config.Installer.InstallerUKI == installer || run.Config.Installer.RuntimeArtifact == runtime {
		t.Fatalf("installer inputs were not staged: %#v", run.Config.Installer)
	}
	if run.Config.Runtime.ESPArtifacts == esp || run.Config.Runtime.NodeMetadata == metadata {
		t.Fatalf("runtime inputs were not staged: %#v", run.Config.Runtime)
	}
	if run.Config.ManifestPath == manifest || !run.Config.PreseedManifest {
		t.Fatalf("install config = %#v", run.Config)
	}
	if run.Config.TargetDisk.Kind != DiskTarget || run.Config.TargetDisk.Size != "20G" {
		t.Fatalf("target disk = %#v", run.Config.TargetDisk)
	}
	scenarioManifest := readScenarioManifest(t, run.Scenario.ManifestPath)
	for _, kind := range []string{FixtureInstallerUKI, FixtureRuntimeArtifact, FixtureESPArtifacts, FixtureNodeMetadata, FixtureInstallManifest, FixtureFirstInstallDisk} {
		if !hasFixtureKind(scenarioManifest.Fixtures, kind) {
			t.Fatalf("scenario fixtures missing %s: %#v", kind, scenarioManifest.Fixtures)
		}
	}
}

func TestPlanFirstInstallWorldRunGuestHandoffMode(t *testing.T) {
	world := testWorld(t)
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")
	runtime := writeFixtureFile(t, filepath.Join(sourceDir, "katl-runtime-root.squashfs"), "runtime")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	manifest := writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), firstManifest())

	run, err := planFirstInstallWorldRun(world, "guest handoff world", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		RuntimeArtifact: runtime,
		RuntimeESP:      esp,
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

func TestPlanFirstInstallWorldRunResolvesLocalMkosiArtifacts(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	mkosiDir := filepath.Join(repo, "build", "mkosi")
	installer := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.efi"), "installer")
	runtime := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
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
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"build/mkosi/katl-installer.efi"},
    {"kind":"runtime-root","path":"build/mkosi/katl-runtime-root.squashfs"}
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
	if run.Config.Installer.RuntimeArtifact == runtime || run.Config.Installer.RuntimeArtifact == "" {
		t.Fatalf("runtime artifact was not staged: %#v", run.Config.Installer)
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
	stagedImage := filepath.Join(filepath.Dir(run.Config.ManifestPath), filepath.Base(image))
	if data, err := os.ReadFile(stagedImage); err != nil || string(data) != "katlos-image" {
		t.Fatalf("staged KatlOS image = %q, err = %v", data, err)
	}
	manifestData, err := os.ReadFile(run.Config.ManifestPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", run.Config.ManifestPath, err)
	}
	if !strings.Contains(string(manifestData), `"hostname": "cp-1"`) ||
		!strings.Contains(string(manifestData), `"localRef": "katlos-install-0.0.0-dev-x86_64.squashfs"`) ||
		!strings.Contains(string(manifestData), `"configRef": "control-plane"`) ||
		!strings.Contains(string(manifestData), `"name": "80-katl-vmtest-dhcp.network"`) ||
		!strings.Contains(string(manifestData), `DHCP=yes`) {
		t.Fatalf("generated manifest = %s", manifestData)
	}
	manifestDir := filepath.Dir(run.Config.ManifestPath)
	if data, err := os.ReadFile(filepath.Join(manifestDir, "kubeadm-configs", "control-plane.yaml")); err != nil || !strings.Contains(string(data), "configFile: kubeadm/control-plane.yaml") {
		t.Fatalf("generated KubeadmConfig = %q, err = %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(manifestDir, "kubeadm", "control-plane.yaml")); err != nil || !strings.Contains(string(data), "kind: InitConfiguration") {
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
	for _, kind := range []string{FixtureInstallerUKI, FixtureRuntimeArtifact, FixtureNodeMetadata, FixtureInstallManifest, FixtureKatlOSInstallImage, FixtureFirstInstallDisk} {
		if !hasFixtureKind(scenarioManifest.Fixtures, kind) {
			t.Fatalf("scenario fixtures missing %s: %#v", kind, scenarioManifest.Fixtures)
		}
	}
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
	mkosiDir := filepath.Join(repo, "build", "mkosi")
	installer := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-installer.efi"), "installer")
	runtime := writeFixtureFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"build/mkosi/katl-installer.efi"},
    {"kind":"runtime-root","path":"build/mkosi/katl-runtime-root.squashfs"}
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
	source := writeFixtureFile(t, filepath.Join(repo, "cmd", "katlos-install", "main.go"), "source")
	oldTime := time.Unix(1700000000, 0)
	newTime := oldTime.Add(time.Hour)
	if err := os.Chtimes(installer, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(installer) error = %v", err)
	}
	if err := os.Chtimes(runtime, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(runtime) error = %v", err)
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
	mkosiDir := filepath.Join(repo, "build", "mkosi")
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
	writeFixtureFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{
  "artifacts": [
    {"kind":"installer-uki","path":"build/mkosi/katl-installer.efi"},
    {"kind":"runtime-root","path":"build/mkosi/katl-runtime-root.squashfs"},
    {"kind":"katlos-install-image","path":"build/mkosi/katlos-install-0.0.0-dev-x86_64.squashfs","metadataPath":"build/mkosi/katlos-install-0.0.0-dev-x86_64.squashfs.json"}
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

func TestPlanFirstInstallWorldRunWritesSetupFailureForMissingSource(t *testing.T) {
	world := testWorld(t)
	sourceDir := t.TempDir()
	installer := writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")

	run, err := planFirstInstallWorldRun(world, "missing first install input", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, firstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: installer},
		RuntimeArtifact: "",
		InstallManifest: writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), firstManifest()),
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}, KVMAuto)
	if err == nil || !strings.Contains(err.Error(), "runtime-artifact source is required") {
		t.Fatalf("planFirstInstallWorldRun() error = %v, want runtime source failure", err)
	}
	if run.Scenario == nil {
		t.Fatal("planFirstInstallWorldRun() did not return scenario on setup failure")
	}
	var result scenarioResult
	readJSONForTest(t, run.Scenario.ResultPath, &result)
	if result.Status != WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, "runtime-artifact source is required") {
		t.Fatalf("result = %#v", result)
	}
}
