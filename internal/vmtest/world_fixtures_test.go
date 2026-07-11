package vmtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorldFixturesWriteInstalledRuntimeManifest(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "fixture manifest")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	disk := writeFixtureFile(t, filepath.Join(t.TempDir(), "source.qcow2"), "disk-a")
	esp := writeFixtureESP(t, filepath.Join(t.TempDir(), "esp"))
	metadata := writeFixtureNodeMetadata(t, filepath.Join(t.TempDir(), "node.json"), node)

	target, err := factory.FirstInstallTargetDisk("root", DiskQCOW2, "20G")
	if err != nil {
		t.Fatalf("FirstInstallTargetDisk() error = %v", err)
	}
	if target.Kind != DiskTarget || target.Format != DiskQCOW2 || target.Size != "20G" {
		t.Fatalf("target disk = %#v", target)
	}
	fixture, err := factory.InstalledRuntime(InstalledRuntimeFixtureInput{
		Disk:         disk,
		DiskFormat:   DiskQCOW2,
		ESPArtifacts: esp,
		NodeMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("InstalledRuntime() error = %v", err)
	}

	record := readInstalledRuntimeFixtureForTest(t, fixture.ManifestPath)
	if record.NodeName != "cp-1" || record.SystemRole != "control-plane" {
		t.Fatalf("fixture identity = %#v", record)
	}
	if record.Disk.Format != "qcow2" || record.Disk.SHA256 == "" || filepath.IsAbs(record.Disk.Path) {
		t.Fatalf("fixture disk = %#v", record.Disk)
	}
	if record.ESPArtifacts.TreeSHA256 == "" || filepath.IsAbs(record.ESPArtifacts.Path) {
		t.Fatalf("fixture ESP = %#v", record.ESPArtifacts)
	}
	if record.NodeMetadata == nil || record.NodeMetadata.SHA256 == "" || filepath.IsAbs(record.NodeMetadata.Path) {
		t.Fatalf("fixture node metadata = %#v", record.NodeMetadata)
	}

	manifest := readScenarioManifest(t, scenario.ManifestPath)
	if len(manifest.Fixtures) < 2 {
		t.Fatalf("scenario fixtures = %#v", manifest.Fixtures)
	}
	if !hasFixtureKind(manifest.Fixtures, FixtureFirstInstallDisk) || !hasFixtureKind(manifest.Fixtures, FixtureInstalledRuntime) {
		t.Fatalf("scenario fixtures missing expected records: %#v", manifest.Fixtures)
	}
}

func TestWorldFixturesStageFirstInstallInputs(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "first install inputs")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	installer := writeFixtureFile(t, filepath.Join(t.TempDir(), "katl-installer.efi"), "installer")
	runtime := writeFixtureFile(t, filepath.Join(t.TempDir(), "katl-runtime-root.squashfs"), "runtime")
	manifest := writeFixtureLocalInstallManifest(t, t.TempDir())

	boot, err := factory.InstallerBoot(InstallerBootConfig{InstallerUKI: installer})
	if err != nil {
		t.Fatalf("InstallerBoot() error = %v", err)
	}
	if boot.InstallerUKI == installer || boot.InstallerUKI == "" {
		t.Fatalf("staged installer UKI = %q, source = %q", boot.InstallerUKI, installer)
	}
	runtimeRecord, err := factory.RuntimeArtifact(runtime)
	if err != nil {
		t.Fatalf("RuntimeArtifact() error = %v", err)
	}
	manifestRecord, err := factory.InstallManifest(manifest)
	if err != nil {
		t.Fatalf("InstallManifest() error = %v", err)
	}

	for _, path := range []string{boot.InstallerUKI, runtimeRecord.Path, manifestRecord.Path} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("staged fixture %s missing: %v", path, err)
		}
	}
	scenarioManifest := readScenarioManifest(t, scenario.ManifestPath)
	for _, kind := range []string{FixtureInstallerUKI, FixtureRuntimeArtifact, FixtureInstallManifest} {
		if !hasFixtureKind(scenarioManifest.Fixtures, kind) {
			t.Fatalf("scenario fixtures missing %s: %#v", kind, scenarioManifest.Fixtures)
		}
	}
}

func TestWorldFixturesStageInstallManifestLocalRef(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "install manifest local ref")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	sourceDir := t.TempDir()
	image := writeFixtureFile(t, filepath.Join(sourceDir, "images", "katlos.squashfs"), "katlos-image")
	writeFixtureKatlOSInstallImageRoot(t, filepath.Join(sourceDir, "images"), "2026.06.04")
	manifest := writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), strings.Replace(firstManifest(), `"url": "https://example.invalid/katlos-install.squashfs",`, `"localRef": "images/katlos.squashfs",`, 1))

	record, err := factory.InstallManifest(manifest)
	if err != nil {
		t.Fatalf("InstallManifest() error = %v", err)
	}
	stagedImage := filepath.Join(filepath.Dir(record.Path), "images", "katlos.squashfs")
	if stagedImage == image {
		t.Fatalf("localRef image was not staged")
	}
	if data, err := os.ReadFile(stagedImage); err != nil || string(data) != "katlos-image" {
		t.Fatalf("staged localRef image = %q, err = %v", data, err)
	}
	scenarioManifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(scenarioManifest.Fixtures, FixtureInstallManifest) || !hasFixtureKind(scenarioManifest.Fixtures, FixtureKatlOSInstallImage) {
		t.Fatalf("scenario fixtures = %#v", scenarioManifest.Fixtures)
	}
}

func TestWorldFixturesStageDirectKernelInstallerBoot(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "direct kernel installer")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	kernel := writeFixtureFile(t, filepath.Join(t.TempDir(), "vmlinuz"), "kernel")
	initrd := writeFixtureFile(t, filepath.Join(t.TempDir(), "initrd"), "initrd")
	boot, err := factory.InstallerBoot(InstallerBootConfig{
		InstallerUKI:    writeFixtureFile(t, filepath.Join(t.TempDir(), "ignored.efi"), "uki"),
		InstallerKernel: kernel,
		InstallerInitrd: initrd,
	})
	if err != nil {
		t.Fatalf("InstallerBoot() error = %v", err)
	}
	if boot.InstallerUKI != "" || boot.InstallerKernel == kernel || boot.InstallerInitrd == initrd {
		t.Fatalf("direct boot = %#v", boot)
	}
	scenarioManifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(scenarioManifest.Fixtures, FixtureInstallerKernel) || !hasFixtureKind(scenarioManifest.Fixtures, FixtureInstallerInitrd) {
		t.Fatalf("scenario fixtures = %#v", scenarioManifest.Fixtures)
	}
}

func TestWorldFixturesStageInstallerISO(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "installer ISO")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)
	source := writeFixtureFile(t, filepath.Join(t.TempDir(), "katl-installer.iso"), "iso")

	boot, err := factory.InstallerBoot(InstallerBootConfig{InstallerISO: source})
	if err != nil {
		t.Fatalf("InstallerBoot() error = %v", err)
	}
	if boot.InstallerISO == "" || boot.InstallerISO == source || boot.InstallerUKI != "" || boot.InstallerKernel != "" || boot.InstallerInitrd != "" {
		t.Fatalf("staged ISO boot = %#v", boot)
	}
	manifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(manifest.Fixtures, FixtureInstallerISO) {
		t.Fatalf("scenario fixtures missing ISO: %#v", manifest.Fixtures)
	}
}

func TestWorldFixturesRejectStaleCachedFile(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "stale cache")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	first := writeFixtureFile(t, filepath.Join(t.TempDir(), "mkosi-a.json"), `{"build":"a"}`)
	second := writeFixtureFile(t, filepath.Join(t.TempDir(), "mkosi-b.json"), `{"build":"b"}`)
	if _, err := factory.MkosiArtifactIndex(first); err != nil {
		t.Fatalf("MkosiArtifactIndex(first) error = %v", err)
	}
	if _, err := factory.MkosiArtifactIndex(second); err == nil || !strings.Contains(err.Error(), "cached artifact") {
		t.Fatalf("MkosiArtifactIndex(second) error = %v, want stale cache rejection", err)
	}
}

func TestWorldFixturesRejectDigestMismatch(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "digest mismatch")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	sourceDir := t.TempDir()
	disk := writeFixtureFile(t, filepath.Join(sourceDir, "installed-runtime.qcow2"), "disk-a")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	manifestPath := writeInstalledFixtureManifestWithESPHash(t, sourceDir, disk, esp, strings.Repeat("0", 64))

	_, err := factory.PublishInstalledRuntimeFromFirstInstall(manifestPath, DiskRaw)
	if err == nil || !strings.Contains(err.Error(), "ESP treeSHA256 does not match") {
		t.Fatalf("PublishInstalledRuntimeFromFirstInstall() error = %v, want digest mismatch", err)
	}
}

func TestWorldFixturesPublishFirstInstallRuntime(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "publish first install")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	factory := scenario.NodeFixtures(node)

	sourceDir := t.TempDir()
	disk := writeFixtureFile(t, filepath.Join(sourceDir, "installed-runtime.qcow2"), "disk-a")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	metadata := writeFixtureNodeMetadata(t, filepath.Join(sourceDir, "node.json"), node)
	sourceManifest := writeInstalledFixtureManifestWithESPHash(t, sourceDir, disk, esp, mustTreeSHA(t, esp), metadata)

	fixture, err := factory.PublishInstalledRuntimeFromFirstInstall(sourceManifest, DiskRaw)
	if err != nil {
		t.Fatalf("PublishInstalledRuntimeFromFirstInstall() error = %v", err)
	}
	if fixture.ManifestPath != filepath.Join(node.ManifestDir, "installed-runtime-fixture.json") {
		t.Fatalf("published manifest path = %q", fixture.ManifestPath)
	}
	if _, err := os.Stat(fixture.Disk); err != nil {
		t.Fatalf("published disk missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.ESPArtifacts, "loader", "entries", "katl.conf")); err != nil {
		t.Fatalf("published ESP missing: %v", err)
	}
	record := readInstalledRuntimeFixtureForTest(t, fixture.ManifestPath)
	if record.Disk.SHA256 == "" || record.ESPArtifacts.TreeSHA256 == "" || record.NodeMetadata == nil {
		t.Fatalf("published fixture record = %#v", record)
	}
	manifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(manifest.Fixtures, FixturePublishedFirstInstall) {
		t.Fatalf("scenario fixtures = %#v", manifest.Fixtures)
	}
}

func readInstalledRuntimeFixtureForTest(t *testing.T, path string) installedRuntimeFixtureRecord {
	t.Helper()
	record, err := readInstalledRuntimeFixture(path)
	if err != nil {
		t.Fatalf("readInstalledRuntimeFixture(%s) error = %v", path, err)
	}
	if record == nil {
		t.Fatalf("readInstalledRuntimeFixture(%s) returned nil", path)
	}
	return *record
}

func writeFixtureFile(t *testing.T, path string, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func writeFixtureESP(t *testing.T, path string) string {
	t.Helper()
	entry := filepath.Join(path, "loader", "entries", "katl.conf")
	content := "title Katl\noptions root=PARTUUID=11111111-2222-3333-4444-555555555555 rootfstype=squashfs katl.generation=2026.06.06 systemd.machine_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ro\n"
	writeFixtureFile(t, entry, content)
	return path
}

func writeFixtureNodeMetadata(t *testing.T, path string, node Node) string {
	t.Helper()
	content := `{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata","identity":{"hostname":"` + node.Name + `"},"systemRole":"` + string(node.Role) + `"}`
	return writeFixtureFile(t, path, content)
}

func mustTreeSHA(t *testing.T, path string) string {
	t.Helper()
	sha, err := espTreeSHA256(path)
	if err != nil {
		t.Fatalf("espTreeSHA256(%s) error = %v", path, err)
	}
	return sha
}

func hasFixtureKind(records []FixtureRecord, kind string) bool {
	for _, record := range records {
		if record.Kind == kind {
			return true
		}
	}
	return false
}
