package vmtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	run, err := planInstalledRuntimeWorldRun(world, name, repoRoot(t), spec, DefaultOptions().KVM)
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
}

func planInstalledRuntimeWorldRun(world World, name, repo string, spec NodeSpec, kvm KVMPolicy) (installedRuntimeWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return installedRuntimeWorldRun{}, err
	}
	run := installedRuntimeWorldRun{Scenario: scenario}
	node, err := AddPublishedInstalledRuntimeNode(scenario, repo, spec)
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

func firstKVM(value, fallback KVMPolicy) KVMPolicy {
	if value != "" {
		return value
	}
	return fallback
}
