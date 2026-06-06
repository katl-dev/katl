package vmtest

import (
	"context"
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
		t.Fatalf("prepare installed runtime world fixture: %v", err)
	}
	run, err := planInstalledRuntimeWorldRun(world, name, repo, spec, DefaultOptions().KVM)
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
}

func ensureInstalledRuntimeWorldFixture(world World, spec NodeSpec, produce func() error) error {
	buildRoot := filepath.Join(world.RunDir, "build")
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
		filepath.Join(world.RunDir, "build"),
		filepath.Join(repo, "build"),
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
