package vmtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type installedRuntimeWorldRun struct {
	Scenario *WorldScenario
	Runner   Runner
	Node     Node
	Fixture  InstalledRuntimeFixture
	Config   InstalledRuntimeConfig
}

func installedRuntimeWorldRunFor(t *testing.T, name string, spec NodeSpec) (installedRuntimeWorldRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(WorldManifestEnv)) == "" {
		return installedRuntimeWorldRun{}, false
	}
	world := RequireWorld(t)
	if err := validateInstalledRuntimeArtifactSet(world); err != nil {
		failInstalledRuntimeWorldFixtureSetup(t, world, name, err)
	}
	repo := repoRoot(t)
	options := DefaultOptions()
	input := DefaultFirstInstallWorldInputFromEnv(FirstInstallWorldPreseed, envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	inputDigest, err := ensurePublishedFirstInstallRuntimeFixture(ctx, world, repo, spec, FirstInstallRuntimeFixtureOptions{
		Input: input,
		KVM:   options.KVM,
	}, ProduceFirstInstallRuntimeFixture)
	if err != nil {
		failInstalledRuntimeWorldFixtureSetup(t, world, name, err)
	}
	run, err := planInstalledRuntimeWorldRun(world, name, repo, spec, options.KVM, inputDigest)
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
}

func TestInstalledRuntimeArtifactSetRejectsRuntimeOnly(t *testing.T) {
	err := validateInstalledRuntimeArtifactSet(World{ArtifactSet: "runtime"})
	if err == nil || !strings.Contains(err.Error(), "only for direct-runtime tests") {
		t.Fatalf("validateInstalledRuntimeArtifactSet() error = %v", err)
	}
	if err := validateInstalledRuntimeArtifactSet(World{ArtifactSet: "install"}); err != nil {
		t.Fatalf("validateInstalledRuntimeArtifactSet(install) error = %v", err)
	}
}

func failInstalledRuntimeWorldFixtureSetup(t *testing.T, world World, name string, err error) {
	t.Helper()
	scenario, scenarioErr := writeInstalledRuntimeWorldFixtureSetupFailure(world, name, err)
	if scenarioErr != nil {
		t.Fatalf("write installed runtime world setup failure: %v; original error: %v", scenarioErr, err)
	}
	t.Fatalf("prepare installed runtime world fixture: %v\nworld scenario dir: %s", err, scenario.Dir)
}

func writeInstalledRuntimeWorldFixtureSetupFailure(world World, name string, err error) (*WorldScenario, error) {
	scenario, scenarioErr := world.PlanScenario(name)
	if scenarioErr != nil {
		return nil, scenarioErr
	}
	if writeErr := scenario.WriteSetupFailure(fmt.Errorf("prepare installed runtime world fixture: %w", err)); writeErr != nil {
		return scenario, writeErr
	}
	return scenario, nil
}

func ensureInstalledRuntimeWorldFixture(world World, spec NodeSpec, produce func() error) error {
	cacheDir := WorldFixtureCacheDir(world)
	if _, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{cacheDir}, spec); err == nil {
		return nil
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return err
	}
	if err := produce(); err != nil {
		return err
	}
	_, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{cacheDir}, spec)
	return err
}

func planInstalledRuntimeWorldRun(world World, name, repo string, spec NodeSpec, kvm KVMPolicy, inputDigest ...string) (installedRuntimeWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return installedRuntimeWorldRun{}, err
	}
	run := installedRuntimeWorldRun{Scenario: scenario}
	node, err := addPublishedInstalledRuntimeNodeFromBuildRoots(scenario, []string{
		WorldFixtureCacheDir(world),
	}, spec, first(inputDigest...))
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	options := Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      KeepFailed,
		KVM:       firstKVM(kvm, KVMAuto),
		Missing:   MissingFails,
	}
	run.Runner = NewRunner(options)
	run.Node = node.Node
	run.Fixture = node.Fixture
	run.Config = node.Config
	return run, nil
}

func TestPlanInstalledRuntimeWorldRunUsesWorldPublishedFixture(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	writePublishedInstalledRuntimeFixture(t, DefaultVMTestCacheDir(repo), "repo-cp", "cp-1", ControlPlane, time.Unix(10, 0))
	writePublishedInstalledRuntimeFixture(t, world.CacheDir, "world-cp", "cp-1", ControlPlane, time.Unix(20, 0))

	run, err := planInstalledRuntimeWorldRun(world, "installed-runtime-kubeadm-api-smoke", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, KVMOff)
	if err != nil {
		t.Fatalf("planInstalledRuntimeWorldRun() error = %v", err)
	}
	if !pathUnder(run.Fixture.Record.Provenance.SourcePath, world.CacheDir) {
		t.Fatalf("installed runtime fixture source = %q, want under world cache %q", run.Fixture.Record.Provenance.SourcePath, world.CacheDir)
	}
	if !pathUnder(run.Config.FixtureManifest, run.Scenario.Dir) {
		t.Fatalf("staged fixture manifest = %q, want under scenario %q", run.Config.FixtureManifest, run.Scenario.Dir)
	}
	if run.Runner.options().Missing != MissingFails {
		t.Fatalf("runner missing policy = %v, want MissingFails", run.Runner.options().Missing)
	}
}

func TestPlanInstalledRuntimeWorldRunRejectsRepoOnlyPublishedFixture(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	writePublishedInstalledRuntimeFixture(t, DefaultVMTestCacheDir(repo), "repo-cp", "cp-1", ControlPlane, time.Unix(10, 0))

	run, err := planInstalledRuntimeWorldRun(world, "installed-runtime-kubeadm-api-smoke", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, KVMOff)
	if err == nil || !strings.Contains(err.Error(), "published installed runtime fixture is missing") {
		t.Fatalf("planInstalledRuntimeWorldRun() error = %v, want missing world fixture", err)
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

func TestWriteInstalledRuntimeWorldFixtureSetupFailure(t *testing.T) {
	world := testWorld(t)
	scenario, err := writeInstalledRuntimeWorldFixtureSetupFailure(world, "installed-runtime-vmtest-agent", fmt.Errorf("missing fresh installer artifacts"))
	if err != nil {
		t.Fatalf("writeInstalledRuntimeWorldFixtureSetupFailure() error = %v", err)
	}
	if scenario.Name != "installed-runtime-vmtest-agent" {
		t.Fatalf("scenario name = %q", scenario.Name)
	}
	var result scenarioResult
	readJSONForTest(t, scenario.ResultPath, &result)
	if result.Status != WorldStatusSetupFailed {
		t.Fatalf("status = %q, want %q", result.Status, WorldStatusSetupFailed)
	}
	if !strings.Contains(result.FailureSummary, "prepare installed runtime world fixture: missing fresh installer artifacts") {
		t.Fatalf("failure summary = %q", result.FailureSummary)
	}
}
