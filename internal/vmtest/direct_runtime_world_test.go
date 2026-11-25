package vmtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type directRuntimeWorldInput struct {
	RuntimeRoot string
	Kernel      string
	Initrd      string
}

type directRuntimeWorldRun struct {
	Scenario *WorldScenario
	Runner   Runner
	Config   DirectRuntimeConfig
}

func directRuntimeWorldRunFor(t *testing.T, name string) (directRuntimeWorldRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(WorldManifestEnv)) == "" {
		return directRuntimeWorldRun{}, false
	}
	world := RequireWorld(t)
	run, err := planDirectRuntimeWorldRun(world, name, repoRoot(t), DefaultOptions().KVM)
	if run.Scenario != nil {
		t.Cleanup(run.Scenario.Release)
	}
	if err != nil {
		failWorldSetup(t, run.Scenario, err)
	}
	return run, true
}

func planDirectRuntimeWorldRun(world World, name, repo string, kvm KVMPolicy) (directRuntimeWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return directRuntimeWorldRun{}, err
	}
	run := directRuntimeWorldRun{Scenario: scenario}
	input, err := resolveDirectRuntimeWorldInput(repo, directRuntimeWorldInput{
		RuntimeRoot: strings.TrimSpace(os.Getenv("KATL_RUNTIME_ARTIFACT")),
		Kernel:      strings.TrimSpace(os.Getenv("KATL_RUNTIME_KERNEL")),
		Initrd:      strings.TrimSpace(os.Getenv("KATL_RUNTIME_INITRD")),
	})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	if err := scenario.WriteManifest(); err != nil {
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
	run.Config = DirectRuntimeConfig{
		RuntimeRoot: input.RuntimeRoot,
		Kernel:      input.Kernel,
		Initrd:      input.Initrd,
		VM:          VMConfig{Network: scenario.VMNetwork()},
	}
	return run, nil
}

func resolveDirectRuntimeWorldInput(repo string, input directRuntimeWorldInput) (directRuntimeWorldInput, error) {
	index := defaultLocalMkosiArtifacts(repo)
	if indexPath := explicitMkosiArtifactIndexPath(); indexPath != "" {
		var err error
		index, err = readMkosiArtifactIndex(indexPath, repo)
		if err != nil {
			return input, err
		}
	}
	if strings.TrimSpace(input.RuntimeRoot) == "" {
		if artifact, ok := index.artifact("runtime-root"); ok {
			input.RuntimeRoot = artifact.Path
		}
	}
	if strings.TrimSpace(input.RuntimeRoot) == "" {
		return input, fmt.Errorf("direct runtime root squashfs is missing: set KATL_RUNTIME_ARTIFACT or run scripts/vmtest-run --artifact-set=runtime")
	}
	return input, nil
}
