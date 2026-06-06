package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPlanInstalledRuntimeWorldRunWritesSetupFailureForMissingPublishedFixture(t *testing.T) {
	world := testWorld(t)
	run, err := planInstalledRuntimeWorldRun(world, "missing installed runtime", t.TempDir(), NodeSpec{Name: "cp-1", Role: ControlPlane}, KVMAuto)
	if err == nil || !strings.Contains(err.Error(), "published installed runtime fixture is missing") {
		t.Fatalf("planInstalledRuntimeWorldRun() error = %v, want missing published fixture", err)
	}
	if run.Scenario == nil {
		t.Fatal("planInstalledRuntimeWorldRun() did not return scenario on setup failure")
	}
	var result scenarioResult
	readJSONForTest(t, run.Scenario.ResultPath, &result)
	if result.Status != WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, "published installed runtime fixture is missing") {
		t.Fatalf("result = %#v", result)
	}
}

func TestPlanInstalledRuntimeWorldRunPublishesFixture(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	sourceManifest := writePublishedInstalledRuntimeFixture(t, world.RunDir, "first", "cp-1", ControlPlane, time.Unix(10, 0))

	run, err := planInstalledRuntimeWorldRun(world, "installed runtime", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, KVMOff)
	if err != nil {
		t.Fatalf("planInstalledRuntimeWorldRun() error = %v", err)
	}
	if run.Config.Disk == "" || run.Config.ESPArtifacts == "" || run.Config.FixtureManifest == "" {
		t.Fatalf("config = %#v", run.Config)
	}
	if run.Config.DiskFormat != DiskRaw {
		t.Fatalf("disk format = %q, want raw", run.Config.DiskFormat)
	}
	if run.Runner.options().StateRoot != filepath.Join(run.Scenario.Dir, "vm-runs") || run.Runner.options().KVM != KVMOff {
		t.Fatalf("runner options = %#v", run.Runner.options())
	}
	record := readInstalledRuntimeFixtureForTest(t, run.Config.FixtureManifest)
	if record.NodeName != "cp-1" || record.SystemRole != "control-plane" {
		t.Fatalf("fixture = %#v", record)
	}
	if !pathUnder(run.Fixture.ManifestPath, world.RunDir) {
		t.Fatalf("fixture = %q, want under world %q", run.Fixture.ManifestPath, world.RunDir)
	}
	if _, err := os.Stat(sourceManifest); err != nil {
		t.Fatalf("source fixture missing: %v", err)
	}
}

func TestFindPublishedFirstInstallRuntimeFixtureSelectsNewestMatch(t *testing.T) {
	repo := t.TempDir()
	old := writePublishedInstalledRuntimeFixture(t, repo, "old", "cp-1", ControlPlane, time.Unix(10, 0))
	newestWorker := writePublishedInstalledRuntimeFixture(t, repo, "new-worker", "worker-1", Worker, time.Unix(30, 0))
	newestCP := writePublishedInstalledRuntimeFixture(t, repo, "new-cp", "cp-1", ControlPlane, time.Unix(20, 0))

	published, err := FindPublishedFirstInstallRuntimeFixture(repo, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if err != nil {
		t.Fatalf("FindPublishedFirstInstallRuntimeFixture() error = %v", err)
	}
	if published.FixtureManifest != newestCP {
		t.Fatalf("fixture = %q, want %q", published.FixtureManifest, newestCP)
	}
	if published.FixtureManifest == old || published.FixtureManifest == newestWorker {
		t.Fatalf("selected wrong fixture = %#v", published)
	}
}

func TestPublishedFirstInstallRuntimeFixtureUsesBuildRootPriority(t *testing.T) {
	worldRoot := t.TempDir()
	repoRoot := t.TempDir()
	worldFixture := writePublishedInstalledRuntimeFixture(t, worldRoot, "world-cp", "cp-1", ControlPlane, time.Unix(10, 0))
	repoFixture := writePublishedInstalledRuntimeFixture(t, repoRoot, "repo-cp", "cp-1", ControlPlane, time.Unix(30, 0))

	published, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{
		filepath.Join(worldRoot, "build"),
		filepath.Join(repoRoot, "build"),
	}, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if err != nil {
		t.Fatalf("FindPublishedFirstInstallRuntimeFixtureInBuildRoots() error = %v", err)
	}
	if published.FixtureManifest != worldFixture {
		t.Fatalf("fixture = %q, want world fixture %q, not repo fixture %q", published.FixtureManifest, worldFixture, repoFixture)
	}
}

func TestEnsureInstalledRuntimeWorldFixtureProducesMissingFixture(t *testing.T) {
	world := testWorld(t)
	produced := false
	err := ensureInstalledRuntimeWorldFixture(world, NodeSpec{Name: "cp-1", Role: ControlPlane}, func() error {
		produced = true
		writePublishedInstalledRuntimeFixture(t, world.RunDir, "world-cp", "cp-1", ControlPlane, time.Unix(10, 0))
		return nil
	})
	if err != nil {
		t.Fatalf("ensureInstalledRuntimeWorldFixture() error = %v", err)
	}
	if !produced {
		t.Fatal("ensureInstalledRuntimeWorldFixture() did not produce missing fixture")
	}
}

func TestEnsureInstalledRuntimeWorldFixtureUsesExistingWorldFixture(t *testing.T) {
	world := testWorld(t)
	writePublishedInstalledRuntimeFixture(t, world.RunDir, "world-cp", "cp-1", ControlPlane, time.Unix(10, 0))
	produced := false
	err := ensureInstalledRuntimeWorldFixture(world, NodeSpec{Name: "cp-1", Role: ControlPlane}, func() error {
		produced = true
		return nil
	})
	if err != nil {
		t.Fatalf("ensureInstalledRuntimeWorldFixture() error = %v", err)
	}
	if produced {
		t.Fatal("ensureInstalledRuntimeWorldFixture() produced despite existing world fixture")
	}
}

func TestEnsurePublishedFirstInstallRuntimeFixturesProducesMissingSpecs(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	input := firstInstallFixtureInputForTest(t)
	var produced []string

	err := EnsurePublishedFirstInstallRuntimeFixtures(context.Background(), world, repo, []NodeSpec{
		{Name: "cp-1", Role: ControlPlane},
		{Name: "worker-1", Role: Worker},
	}, FirstInstallRuntimeFixtureOptions{
		Input: input,
		KVM:   KVMOff,
		Produce: func(_ context.Context, contract FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
			produced = append(produced, contract.Node.Name)
			manifest := writePublishedInstalledRuntimeFixture(t, world.RunDir, FirstInstallRuntimeFixtureScenarioName(contract.Node), contract.Node.Name, contract.Node.Role, time.Unix(10, 0))
			return ProducedInstalledRuntimeFixture{ManifestPath: manifest}, nil
		},
	})
	if err != nil {
		t.Fatalf("EnsurePublishedFirstInstallRuntimeFixtures() error = %v", err)
	}
	if !reflect.DeepEqual(produced, []string{"cp-1", "worker-1"}) {
		t.Fatalf("produced = %#v", produced)
	}
	for _, spec := range []NodeSpec{{Name: "cp-1", Role: ControlPlane}, {Name: "worker-1", Role: Worker}} {
		if _, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{filepath.Join(world.RunDir, "build")}, spec); err != nil {
			t.Fatalf("FindPublishedFirstInstallRuntimeFixtureInBuildRoots(%#v) error = %v", spec, err)
		}
	}
}

func TestEnsurePublishedFirstInstallRuntimeFixturesWritesSetupFailureWhenProducerFails(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	input := firstInstallFixtureInputForTest(t)
	spec := NodeSpec{Name: "cp-1", Role: ControlPlane}

	err := EnsurePublishedFirstInstallRuntimeFixtures(context.Background(), world, repo, []NodeSpec{spec}, FirstInstallRuntimeFixtureOptions{
		Input: input,
		KVM:   KVMOff,
		Produce: func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
			return ProducedInstalledRuntimeFixture{}, errors.New("first install generator failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "first install generator failed") {
		t.Fatalf("EnsurePublishedFirstInstallRuntimeFixtures() error = %v, want generator failure", err)
	}
	var result scenarioResult
	resultPath := filepath.Join(world.ScenarioDir, clean(FirstInstallRuntimeFixtureScenarioName(spec)), "result.json")
	readJSONForTest(t, resultPath, &result)
	if result.Status != WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, "first install generator failed") {
		t.Fatalf("result = %#v", result)
	}
}

func TestEnsurePublishedFirstInstallRuntimeFixturesWritesSetupFailureForEachFailedSpec(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	input := firstInstallFixtureInputForTest(t)
	specs := []NodeSpec{
		{Name: "cp-1", Role: ControlPlane},
		{Name: "worker-1", Role: Worker},
	}

	err := EnsurePublishedFirstInstallRuntimeFixtures(context.Background(), world, repo, specs, FirstInstallRuntimeFixtureOptions{
		Input: input,
		KVM:   KVMOff,
		Produce: func(_ context.Context, contract FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("%s fixture generator failed", contract.Node.Name)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cp-1 fixture generator failed") || !strings.Contains(err.Error(), "worker-1 fixture generator failed") {
		t.Fatalf("EnsurePublishedFirstInstallRuntimeFixtures() error = %v, want both generator failures", err)
	}
	for _, spec := range specs {
		var result scenarioResult
		resultPath := filepath.Join(world.ScenarioDir, clean(FirstInstallRuntimeFixtureScenarioName(spec)), "result.json")
		readJSONForTest(t, resultPath, &result)
		if result.Status != WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, spec.Name+" fixture generator failed") {
			t.Fatalf("%s result = %#v", spec.Name, result)
		}
	}
}

func TestEnsurePublishedFirstInstallRuntimeFixturesReusesExistingFixture(t *testing.T) {
	world := testWorld(t)
	writePublishedInstalledRuntimeFixture(t, world.RunDir, "world-cp", "cp-1", ControlPlane, time.Unix(10, 0))

	err := EnsurePublishedFirstInstallRuntimeFixtures(context.Background(), world, t.TempDir(), []NodeSpec{
		{Name: "cp-1", Role: ControlPlane},
		{Name: "cp-1", Role: ControlPlane},
	}, FirstInstallRuntimeFixtureOptions{
		Produce: func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
			t.Fatal("producer was called for an existing fixture")
			return ProducedInstalledRuntimeFixture{}, nil
		},
	})
	if err != nil {
		t.Fatalf("EnsurePublishedFirstInstallRuntimeFixtures() error = %v", err)
	}
}

func TestEnsurePublishedFirstInstallRuntimeFixturesIsolatesSeparateWorlds(t *testing.T) {
	repo := t.TempDir()
	input := firstInstallFixtureInputForTest(t)
	spec := NodeSpec{Name: "cp-1", Role: ControlPlane}
	worldA := testWorld(t)
	worldB := testWorld(t)

	ensureWithPublishedFixture := func(world World, name string) PublishedFirstInstallRuntimeFixture {
		t.Helper()
		err := EnsurePublishedFirstInstallRuntimeFixtures(context.Background(), world, repo, []NodeSpec{spec}, FirstInstallRuntimeFixtureOptions{
			Input: input,
			KVM:   KVMOff,
			Produce: func(_ context.Context, contract FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
				manifest := writePublishedInstalledRuntimeFixture(t, contract.WorldScenario.World.RunDir, name, contract.Node.Name, contract.Node.Role, time.Unix(10, 0))
				return ProducedInstalledRuntimeFixture{ManifestPath: manifest}, nil
			},
		})
		if err != nil {
			t.Fatalf("EnsurePublishedFirstInstallRuntimeFixtures(%s) error = %v", name, err)
		}
		published, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{filepath.Join(world.RunDir, "build")}, spec)
		if err != nil {
			t.Fatalf("FindPublishedFirstInstallRuntimeFixtureInBuildRoots(%s) error = %v", name, err)
		}
		return published
	}

	publishedA := ensureWithPublishedFixture(worldA, "world-a")
	publishedB := ensureWithPublishedFixture(worldB, "world-b")
	if publishedA.FixtureManifest == publishedB.FixtureManifest {
		t.Fatalf("separate worlds reused fixture manifest %q", publishedA.FixtureManifest)
	}
	if !pathUnder(publishedA.FixtureManifest, worldA.RunDir) || !pathUnder(publishedB.FixtureManifest, worldB.RunDir) {
		t.Fatalf("published fixtures escaped their worlds: a=%q b=%q", publishedA.FixtureManifest, publishedB.FixtureManifest)
	}
}

func pathUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." && !filepath.IsAbs(rel)
}

func TestPublishFirstInstallRuntimeWorldFixtureUsesWorldFactory(t *testing.T) {
	world := testWorld(t)
	scenario, err := world.PlanScenario("first-install-installed-runtime-fixture-cp-1-control-plane")
	if err != nil {
		t.Fatalf("PlanScenario() error = %v", err)
	}
	node, err := scenario.AddNode(NodeSpec{Name: "cp-1", Role: ControlPlane})
	if err != nil {
		t.Fatalf("AddNode() error = %v", err)
	}
	sourceDir := t.TempDir()
	disk := writeFixtureFile(t, filepath.Join(sourceDir, "installed-runtime.qcow2"), "disk")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	metadata := writeFixtureNodeMetadata(t, filepath.Join(sourceDir, "node.json"), Node{Name: "cp-1", Role: ControlPlane})

	fixture, err := publishFirstInstallRuntimeWorldFixture(FirstInstallRuntimeFixtureContract{
		WorldScenario: scenario,
		WorldNode:     node,
		ManifestPath:  filepath.Join(sourceDir, "install-manifest.json"),
		NodeMetadata:  metadata,
		Node:          NodeSpec{Name: "cp-1", Role: ControlPlane},
	}, disk, esp)
	if err != nil {
		t.Fatalf("publishFirstInstallRuntimeWorldFixture() error = %v", err)
	}
	if !strings.HasPrefix(fixture.ManifestPath, node.ManifestDir) || !strings.HasPrefix(fixture.Disk, node.DiskDir) || !strings.HasPrefix(fixture.ESPArtifacts, node.ArtifactDir) {
		t.Fatalf("fixture was not staged under node dirs: %#v node=%#v", fixture, node)
	}
	published, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{filepath.Join(world.RunDir, "build")}, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if err != nil {
		t.Fatalf("FindPublishedFirstInstallRuntimeFixtureInBuildRoots() error = %v", err)
	}
	if published.FixtureManifest != fixture.ManifestPath {
		t.Fatalf("published fixture = %q, want %q", published.FixtureManifest, fixture.ManifestPath)
	}
	manifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(manifest.Fixtures, FixturePublishedFirstInstall) {
		t.Fatalf("scenario fixtures missing published first-install runtime: %#v", manifest.Fixtures)
	}
}

func TestWritePublishedFirstInstallRuntimeFixture(t *testing.T) {
	root := t.TempDir()
	sourceDir := t.TempDir()
	disk := writeFixtureFile(t, filepath.Join(sourceDir, "installed-runtime.qcow2"), "disk")
	esp := writeFixtureESP(t, filepath.Join(sourceDir, "esp"))
	metadata := writeFixtureNodeMetadata(t, filepath.Join(sourceDir, "node.json"), Node{Name: "cp-1", Role: ControlPlane})
	fixtureManifest := writeInstalledFixtureManifestWithESPHash(t, sourceDir, disk, esp, mustTreeSHA(t, esp), metadata)

	publishedPath, err := WritePublishedFirstInstallRuntimeFixture(root, "fixture contract", fixtureManifest, DiskQCOW2)
	if err != nil {
		t.Fatalf("WritePublishedFirstInstallRuntimeFixture() error = %v", err)
	}
	published, err := ReadPublishedFirstInstallRuntimeFixture(publishedPath)
	if err != nil {
		t.Fatalf("ReadPublishedFirstInstallRuntimeFixture() error = %v", err)
	}
	if published.NodeName != "node-1" || published.SystemRole != string(ControlPlane) || published.DiskFormat != string(DiskQCOW2) {
		t.Fatalf("published fixture = %#v", published)
	}
	if published.FixtureManifest != fixtureManifest {
		t.Fatalf("published manifest = %q, want %q", published.FixtureManifest, fixtureManifest)
	}
	if !strings.HasPrefix(publishedPath, filepath.Join(root, "build", "published-first-install-runtime")) {
		t.Fatalf("published path = %q", publishedPath)
	}
}

func writePublishedInstalledRuntimeFixture(t *testing.T, repo, name, nodeName string, role NodeRole, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(repo, "build", "published", name)
	disk := writeFixtureFile(t, filepath.Join(dir, "installed-runtime.raw"), "disk-"+name)
	esp := writeFixtureESP(t, filepath.Join(dir, "esp"))
	metadata := writeFixtureNodeMetadata(t, filepath.Join(dir, "node.json"), Node{Name: nodeName, Role: role})
	fixtureManifest := writeInstalledFixtureManifestWithESPHash(t, dir, disk, esp, mustTreeSHA(t, esp), metadata)
	fixtureRecord := readInstalledRuntimeFixtureForTest(t, fixtureManifest)
	fixtureRecord.NodeName = nodeName
	fixtureRecord.SystemRole = string(role)
	fixtureContent, err := json.MarshalIndent(fixtureRecord, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	if err := os.WriteFile(fixtureManifest, append(fixtureContent, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", fixtureManifest, err)
	}
	publishedManifest := filepath.Join(dir, "published-first-install-runtime-fixture.json")
	content := `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "PublishedFirstInstallRuntimeFixture",
  "nodeName": "` + nodeName + `",
  "systemRole": "` + string(role) + `",
  "fixtureManifest": "installed-runtime-fixture.json",
  "diskFormat": "raw"
}
`
	if err := os.WriteFile(publishedManifest, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", publishedManifest, err)
	}
	if err := os.Chtimes(publishedManifest, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(%s) error = %v", publishedManifest, err)
	}
	return fixtureManifest
}

func firstInstallFixtureInputForTest(t *testing.T) FirstInstallWorldInput {
	t.Helper()
	sourceDir := t.TempDir()
	return FirstInstallWorldInput{
		Installer:       InstallerBootConfig{InstallerUKI: writeFixtureFile(t, filepath.Join(sourceDir, "katl-installer.efi"), "installer")},
		RuntimeArtifact: writeFixtureFile(t, filepath.Join(sourceDir, "katl-runtime-root.squashfs"), "runtime"),
		InstallManifest: writeFixtureFile(t, filepath.Join(sourceDir, "install-manifest.json"), firstManifest()),
		UseInstalledESP: true,
		TargetDiskSize:  "20G",
	}
}
