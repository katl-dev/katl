package vmtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type FirstInstallRuntimeFixtureContract struct {
	Runner          Runner
	WorldScenario   *WorldScenario
	InstallerBoot   InstallerBootConfig
	RuntimeArtifact string
	RuntimeESP      string
	NodeMetadata    string
	ManifestPath    string
	Repo            string
	TargetDisk      DiskFixture
	UseInstalledESP bool
	Node            NodeSpec
}

type ProducedInstalledRuntimeFixture struct {
	ManifestPath string
	Disk         string
	ESPArtifacts string
}

type FirstInstallRuntimeFixtureOptions struct {
	Input   FirstInstallWorldInput
	KVM     KVMPolicy
	Produce func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error)
}

func EnsurePublishedFirstInstallRuntimeFixtures(ctx context.Context, world World, repo string, specs []NodeSpec, options FirstInstallRuntimeFixtureOptions) error {
	produce := options.Produce
	if produce == nil {
		produce = ProduceFirstInstallRuntimeFixture
	}
	for _, spec := range specs {
		if err := ensurePublishedFirstInstallRuntimeFixture(ctx, world, repo, spec, options, produce); err != nil {
			return err
		}
	}
	return nil
}

func ensurePublishedFirstInstallRuntimeFixture(ctx context.Context, world World, repo string, spec NodeSpec, options FirstInstallRuntimeFixtureOptions, produce func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error)) error {
	buildRoot := filepath.Join(world.RunDir, "build")
	if _, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{buildRoot}, spec); err == nil {
		return nil
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return err
	}
	unlock, err := lockLeaseFile(filepath.Join(buildRoot, "locks", FirstInstallRuntimeFixtureScenarioName(spec)))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{buildRoot}, spec); err == nil {
		return nil
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return err
	}
	contract, err := FirstInstallRuntimeFixtureContractForWorld(world, repo, spec, options.Input, options.KVM)
	if err != nil {
		return err
	}
	if _, err := produce(ctx, contract); err != nil {
		if contract.WorldScenario != nil {
			_ = contract.WorldScenario.WriteSetupFailure(err)
		}
		return err
	}
	_, err = FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{buildRoot}, spec)
	return err
}

func FirstInstallRuntimeFixtureContractForWorld(world World, repo string, spec NodeSpec, input FirstInstallWorldInput, kvm KVMPolicy) (FirstInstallRuntimeFixtureContract, error) {
	run, err := PlanFirstInstallWorldRun(world, FirstInstallRuntimeFixtureScenarioName(spec), repo, spec, input, kvm)
	if err != nil {
		return FirstInstallRuntimeFixtureContract{}, err
	}
	return FirstInstallRuntimeFixtureContract{
		Runner:          run.Runner,
		WorldScenario:   run.Scenario,
		InstallerBoot:   run.Config.Installer,
		RuntimeArtifact: run.Config.Installer.RuntimeArtifact,
		RuntimeESP:      run.Config.Runtime.ESPArtifacts,
		NodeMetadata:    run.Config.Runtime.NodeMetadata,
		ManifestPath:    run.Config.ManifestPath,
		Repo:            run.Repo,
		TargetDisk:      run.Config.TargetDisk,
		UseInstalledESP: run.Config.UseInstalledESP,
		Node:            spec,
	}, nil
}

func FirstInstallRuntimeFixtureScenarioName(spec NodeSpec) string {
	name := clean(strings.TrimSpace(spec.Name))
	role := clean(string(spec.Role))
	return first(strings.TrimSuffix("first-install-installed-runtime-fixture-"+name+"-"+role, "-"), "first-install-installed-runtime-fixture")
}

func ProduceFirstInstallRuntimeFixture(ctx context.Context, contract FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error) {
	requiredTools := []string{"jq", "sha256sum"}
	if contract.UseInstalledESP {
		requiredTools = append(requiredTools, "sfdisk", "mcopy")
	}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("%s is required to package installed runtime fixtures: %w", tool, err)
		}
	}
	runner := contract.Runner
	if err := runner.CheckHost(HostRequirements{
		QEMU:    true,
		QEMUImg: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
		MTools:  true,
	}); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 12 * time.Minute,
		VSock: VSockConfig{
			Enabled: true,
		},
		Agent: AgentControlConfig{
			RequireHealth: true,
			Timeout:       30 * time.Second,
		},
	}
	firstResult, err := RunFirstInstall(ctx, runner, Scenario{Name: FirstInstallRuntimeFixtureScenarioName(contract.Node)}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    contract.InstallerBoot.InstallerUKI,
			InstallerKernel: contract.InstallerBoot.InstallerKernel,
			InstallerInitrd: contract.InstallerBoot.InstallerInitrd,
			CommandLine:     contract.InstallerBoot.CommandLine,
			RuntimeArtifact: contract.RuntimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts:       contract.RuntimeESP,
			RequireVMTestAgent: true,
			VM:                 vm,
		},
		UseInstalledESP: contract.UseInstalledESP,
		ManifestPath:    contract.ManifestPath,
		PreseedManifest: true,
		TargetDisk:      contract.TargetDisk,
	})
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	if firstResult.Status != StatusPassed {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("first install status = %q, failure = %q, run dir = %s", firstResult.Status, firstResult.FailureSummary, firstResult.RunDir)
	}
	installedDisk, err := targetDiskPathFromResult(firstResult)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	fixtureESP := contract.RuntimeESP
	if contract.UseInstalledESP {
		fixtureESP = firstResult.Artifacts.InstalledESP
		if _, err := os.Stat(fixtureESP); err != nil {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("installed ESP artifacts %s are unavailable: %w", fixtureESP, err)
		}
	}
	fixtureDir := filepath.Join(firstResult.ManifestDir, "installed-runtime-fixture")
	output, err := createInstalledRuntimeFixtureCmd(ctx, contract.Repo, installedDisk, fixtureESP, string(DiskQCOW2), fixtureDir, contract.NodeMetadata).CombinedOutput()
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("create installed runtime fixture failed: %w\n%s", err, output)
	}

	fixtureManifest := filepath.Join(fixtureDir, "installed-runtime-fixture.json")
	packagedDisk := filepath.Join(fixtureDir, "installed-runtime.qcow2")
	packagedESP := filepath.Join(fixtureDir, "esp")
	output, err = resolveInstalledRuntimeFixtureCmd(ctx, contract.Repo, packagedDisk, packagedESP, fixtureManifest, string(DiskQCOW2), filepath.Join(fixtureDir, "recheck"), packagedNodeMetadata(fixtureDir, contract.NodeMetadata)).CombinedOutput()
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("check installed runtime fixture failed: %w\n%s", err, output)
	}
	if contract.WorldScenario != nil {
		if _, err := WritePublishedFirstInstallRuntimeFixture(contract.WorldScenario.World.RunDir, FirstInstallRuntimeFixtureScenarioName(contract.Node), fixtureManifest, DiskQCOW2); err != nil {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("publish first-install runtime fixture: %w", err)
		}
	}
	return ProducedInstalledRuntimeFixture{
		ManifestPath: fixtureManifest,
		Disk:         packagedDisk,
		ESPArtifacts: packagedESP,
	}, nil
}

func targetDiskPathFromResult(result Result) (string, error) {
	for _, disk := range result.Disks {
		if disk.Kind == DiskTarget {
			if _, err := os.Stat(disk.HostPath); err != nil {
				return "", fmt.Errorf("target disk %s is not available after first install: %w", disk.HostPath, err)
			}
			return disk.HostPath, nil
		}
	}
	return "", fmt.Errorf("first install result has no target disk: %#v", result.Disks)
}

func isMissingPublishedFirstInstallRuntimeFixture(err error) bool {
	return err != nil && strings.Contains(err.Error(), "published installed runtime fixture is missing")
}

func createInstalledRuntimeFixtureCmd(ctx context.Context, repoRoot, disk, esp, format, stateDir, nodeMetadata string) *exec.Cmd {
	args := []string{
		"--disk", disk,
		"--esp-artifacts", esp,
		"--format", format,
		"--state-dir", stateDir,
	}
	if nodeMetadata != "" {
		args = append(args, "--node-metadata", nodeMetadata)
	}
	return exec.CommandContext(ctx, filepath.Join(repoRoot, "scripts", "create-installed-runtime-fixture"), args...)
}

func resolveInstalledRuntimeFixtureCmd(ctx context.Context, repoRoot, disk, esp, fixture, format, stateDir, nodeMetadata string) *exec.Cmd {
	args := []string{
		"--disk", disk,
		"--esp-artifacts", esp,
		"--fixture", fixture,
		"--format", format,
		"--state-dir", stateDir,
		"--check-only",
	}
	if nodeMetadata != "" {
		args = append(args, "--node-metadata", nodeMetadata)
	}
	return exec.CommandContext(ctx, filepath.Join(repoRoot, "scripts", "resolve-installed-runtime-fixture"), args...)
}

func packagedNodeMetadata(fixtureDir, nodeMetadata string) string {
	if nodeMetadata == "" {
		return ""
	}
	return filepath.Join(fixtureDir, "node.json")
}
