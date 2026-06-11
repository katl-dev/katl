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
	Fixture  InstalledRuntimeFixture
	Config   InstalledRuntimeConfig
}

func installedRuntimeWorldRunFor(t *testing.T, name string, spec NodeSpec) (installedRuntimeWorldRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(WorldManifestEnv)) == "" {
		return installedRuntimeWorldRun{}, false
	}
	world := RequireWorld(t)
	repo := repoRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := EnsurePublishedFirstInstallRuntimeFixtures(ctx, world, repo, []NodeSpec{spec}, FirstInstallRuntimeFixtureOptions{
		Input: DefaultFirstInstallWorldInputFromEnv(FirstInstallWorldPreseed, envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")),
		KVM:   DefaultOptions().KVM,
	}); err != nil {
		failInstalledRuntimeWorldFixtureSetup(t, world, name, err)
	}
	run, err := planInstalledRuntimeWorldRun(world, name, repo, spec, DefaultOptions().KVM)
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
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
	buildRoot := filepath.Join(world.RunDir, "_build")
	if _, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{buildRoot}, spec); err == nil {
		return nil
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return err
	}
	if err := produce(); err != nil {
		return err
	}
	_, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{buildRoot}, spec)
	return err
}

func planInstalledRuntimeWorldRun(world World, name, repo string, spec NodeSpec, kvm KVMPolicy) (installedRuntimeWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return installedRuntimeWorldRun{}, err
	}
	run := installedRuntimeWorldRun{Scenario: scenario}
	node, err := AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, []string{
		filepath.Join(world.RunDir, "_build"),
	}, spec)
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
	run.Fixture = node.Fixture
	run.Config = node.Config
	return run, nil
}

func TestPlanInstalledRuntimeWorldRunUsesWorldPublishedFixture(t *testing.T) {
	world := testWorld(t)
	repo := t.TempDir()
	writePublishedInstalledRuntimeFixture(t, repo, "repo-cp", "cp-1", ControlPlane, time.Unix(10, 0))
	writePublishedInstalledRuntimeFixture(t, world.RunDir, "world-cp", "cp-1", ControlPlane, time.Unix(20, 0))

	run, err := planInstalledRuntimeWorldRun(world, "installed-runtime-kubeadm-api-smoke", repo, NodeSpec{Name: "cp-1", Role: ControlPlane}, KVMOff)
	if err != nil {
		t.Fatalf("planInstalledRuntimeWorldRun() error = %v", err)
	}
	if !pathUnder(run.Fixture.ManifestPath, world.RunDir) {
		t.Fatalf("installed runtime fixture = %q, want under world %q", run.Fixture.ManifestPath, world.RunDir)
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
	writePublishedInstalledRuntimeFixture(t, repo, "repo-cp", "cp-1", ControlPlane, time.Unix(10, 0))

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
