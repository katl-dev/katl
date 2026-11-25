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

func installedRuntimeWorldRunFor(t *testing.T, name string, spec NodeSpec, sshAuthorizedKey ...string) (installedRuntimeWorldRun, bool) {
	t.Helper()
	runs, ok := installedRuntimeWorldRunsFor(t, name, []NodeSpec{spec}, sshAuthorizedKey...)
	if !ok {
		return installedRuntimeWorldRun{}, false
	}
	return runs[0], true
}

// installedRuntimeWorldRunsFor plans one installed-runtime node per spec in a
// single scenario, so the nodes share the scenario's network.
func installedRuntimeWorldRunsFor(t *testing.T, name string, specs []NodeSpec, sshAuthorizedKey ...string) ([]installedRuntimeWorldRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(WorldManifestEnv)) == "" {
		return nil, false
	}
	world := RequireWorld(t)
	if err := validateInstalledRuntimeArtifactSet(world); err != nil {
		failInstalledRuntimeWorldFixtureSetup(t, world, name, err)
	}
	repo := repoRoot(t)
	options := DefaultOptions()
	input := DefaultFirstInstallWorldInputFromEnv(FirstInstallWorldPreseed, envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP"))
	input.SSHAuthorizedKey = first(sshAuthorizedKey...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	digests := make([]string, len(specs))
	for i, spec := range specs {
		inputDigest, err := ensurePublishedFirstInstallRuntimeFixture(ctx, world, repo, spec, FirstInstallRuntimeFixtureOptions{
			Input: input,
			KVM:   options.KVM,
		}, ProduceFirstInstallRuntimeFixture)
		if err != nil {
			failInstalledRuntimeWorldFixtureSetup(t, world, name, err)
		}
		digests[i] = inputDigest
	}
	scenario, runs, err := planInstalledRuntimeWorldRuns(world, name, specs, options.KVM, digests)
	if scenario != nil {
		t.Cleanup(scenario.Release)
	}
	if err != nil {
		failWorldSetup(t, scenario, err)
	}
	return runs, true
}

// planInstalledRuntimeWorldRuns plans one scenario that declares a node per
// spec. The scenario is returned with any error that follows its planning, so
// the caller can release it.
func planInstalledRuntimeWorldRuns(world World, name string, specs []NodeSpec, kvm KVMPolicy, digests []string) (*WorldScenario, []installedRuntimeWorldRun, error) {
	scenario, err := world.PlanScenario(name, Nodes(len(specs)))
	if err != nil {
		return nil, nil, fmt.Errorf("plan VM test scenario: %w", err)
	}
	runs := make([]installedRuntimeWorldRun, len(specs))
	for i, spec := range specs {
		runs[i], err = addInstalledRuntimeWorldRun(scenario, spec, kvm, digests[i])
		if err != nil {
			return scenario, nil, err
		}
	}
	return scenario, runs, nil
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
	return world.RecordSetupFailure(name, fmt.Errorf("prepare installed runtime world fixture: %w", err))
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
	return addInstalledRuntimeWorldRun(scenario, spec, kvm, inputDigest...)
}

func addInstalledRuntimeWorldRun(scenario *WorldScenario, spec NodeSpec, kvm KVMPolicy, inputDigest ...string) (installedRuntimeWorldRun, error) {
	run := installedRuntimeWorldRun{Scenario: scenario}
	node, err := addPublishedInstalledRuntimeNodeFromBuildRoots(scenario, []string{
		WorldFixtureCacheDir(scenario.World),
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
