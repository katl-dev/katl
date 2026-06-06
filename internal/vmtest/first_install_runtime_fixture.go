package vmtest

import (
	"context"
	"encoding/json"
	"errors"
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
	WorldNode       Node
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
	var errs []error
	for _, spec := range specs {
		if err := ensurePublishedFirstInstallRuntimeFixture(ctx, world, repo, spec, options, produce); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", first(strings.TrimSpace(spec.Name), FirstInstallRuntimeFixtureScenarioName(spec)), err))
		}
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
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
		WorldNode:       run.Node,
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
	var requiredTools []string
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
	if contract.WorldScenario != nil {
		return publishFirstInstallRuntimeWorldFixture(contract, installedDisk, fixtureESP)
	}
	return packageFirstInstallRuntimeFixture(contract, firstResult, installedDisk, fixtureESP)
}

func publishFirstInstallRuntimeWorldFixture(contract FirstInstallRuntimeFixtureContract, installedDisk, fixtureESP string) (ProducedInstalledRuntimeFixture, error) {
	if contract.WorldNode.Name == "" {
		return ProducedInstalledRuntimeFixture{}, errors.New("first-install runtime fixture contract is missing world node")
	}
	factory := contract.WorldScenario.NodeFixtures(contract.WorldNode)
	fixture, err := factory.InstalledRuntime(InstalledRuntimeFixtureInput{
		Disk:         installedDisk,
		DiskFormat:   DiskQCOW2,
		ESPArtifacts: fixtureESP,
		NodeMetadata: contract.NodeMetadata,
		NodeName:     contract.Node.Name,
		SystemRole:   contract.Node.Role,
	})
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	fixture.Record.Kind = FixturePublishedFirstInstall
	fixture.Record.Provenance = FixtureProvenance{
		Source:     "first-install",
		SourcePath: contract.ManifestPath,
	}
	if err := factory.replaceRecord(FixtureInstalledRuntime, fixture.Record); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	if _, err := WritePublishedFirstInstallRuntimeFixture(contract.WorldScenario.World.RunDir, FirstInstallRuntimeFixtureScenarioName(contract.Node), fixture.ManifestPath, DiskQCOW2); err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("publish first-install runtime fixture: %w", err)
	}
	return ProducedInstalledRuntimeFixture{
		ManifestPath: fixture.ManifestPath,
		Disk:         fixture.Disk,
		ESPArtifacts: fixture.ESPArtifacts,
	}, nil
}

func packageFirstInstallRuntimeFixture(contract FirstInstallRuntimeFixtureContract, firstResult Result, installedDisk, fixtureESP string) (ProducedInstalledRuntimeFixture, error) {
	fixtureDir := filepath.Join(firstResult.ManifestDir, "installed-runtime-fixture")
	fixtureManifest := filepath.Join(fixtureDir, "installed-runtime-fixture.json")
	packagedDisk := filepath.Join(fixtureDir, "installed-runtime.qcow2")
	packagedESP := filepath.Join(fixtureDir, "esp")
	packagedMetadata := ""
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	if err := copyFile(installedDisk, packagedDisk, 0o644); err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("copy installed runtime disk: %w", err)
	}
	if err := copyDir(fixtureESP, packagedESP); err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("copy installed runtime ESP artifacts: %w", err)
	}
	if err := CheckESP(packagedESP); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	if strings.TrimSpace(contract.NodeMetadata) != "" {
		packagedMetadata = filepath.Join(fixtureDir, "node.json")
		if err := copyFile(contract.NodeMetadata, packagedMetadata, 0o644); err != nil {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("copy installed runtime node metadata: %w", err)
		}
	}
	nodeName, systemRole, err := packagedFirstInstallRuntimeFixtureIdentity(contract, packagedMetadata)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	diskSHA, err := fileSHA256(packagedDisk)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("hash installed runtime disk: %w", err)
	}
	espSHA, err := espTreeSHA256(packagedESP)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("hash installed runtime ESP artifacts: %w", err)
	}
	record := installedRuntimeFixtureRecord{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "InstalledRuntimeVMTestFixture",
		NodeName:   nodeName,
		SystemRole: string(systemRole),
		Disk: installedRuntimeFixtureDisk{
			Path:   relFrom(filepath.Dir(fixtureManifest), packagedDisk),
			Format: string(DiskQCOW2),
			SHA256: diskSHA,
		},
		ESPArtifacts: installedRuntimeFixtureESP{
			Path:       relFrom(filepath.Dir(fixtureManifest), packagedESP),
			TreeSHA256: espSHA,
		},
	}
	if packagedMetadata != "" {
		metadataSHA, err := fileSHA256(packagedMetadata)
		if err != nil {
			return ProducedInstalledRuntimeFixture{}, fmt.Errorf("hash installed runtime node metadata: %w", err)
		}
		record.NodeMetadata = &installedRuntimeFixtureFile{
			Path:   relFrom(filepath.Dir(fixtureManifest), packagedMetadata),
			SHA256: metadataSHA,
		}
	}
	if err := writeJSON(fixtureManifest, record); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	if err := validateInstalledRuntimeFixture(fixtureManifest, record, InstalledRuntimeConfig{
		Disk:         packagedDisk,
		DiskFormat:   DiskQCOW2,
		ESPArtifacts: packagedESP,
	}, packagedMetadata); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	return ProducedInstalledRuntimeFixture{
		ManifestPath: fixtureManifest,
		Disk:         packagedDisk,
		ESPArtifacts: packagedESP,
	}, nil
}

func packagedFirstInstallRuntimeFixtureIdentity(contract FirstInstallRuntimeFixtureContract, nodeMetadata string) (string, NodeRole, error) {
	nodeName := strings.TrimSpace(contract.Node.Name)
	systemRole := contract.Node.Role
	if nodeMetadata != "" {
		metadata, err := readNodeMetadataIdentity(nodeMetadata)
		if err != nil {
			return "", "", err
		}
		if metadata.hostname != "" {
			if nodeName != "" && nodeName != metadata.hostname {
				return "", "", fmt.Errorf("node metadata hostname %q does not match node %q", metadata.hostname, nodeName)
			}
			nodeName = metadata.hostname
		}
		if metadata.systemRole != "" {
			metadataRole := NodeRole(metadata.systemRole)
			if systemRole != "" && systemRole != metadataRole {
				return "", "", fmt.Errorf("node metadata systemRole %q does not match node role %q", metadata.systemRole, systemRole)
			}
			systemRole = metadataRole
		}
	}
	if nodeName == "" {
		nodeName = "node-1"
	}
	if systemRole == "" {
		systemRole = ControlPlane
	}
	if systemRole != ControlPlane && systemRole != Worker {
		return "", "", fmt.Errorf("system role must be %q or %q", ControlPlane, Worker)
	}
	if nodeMetadata != "" {
		if err := validateNodeMetadata(nodeMetadata, Node{Name: nodeName, Role: systemRole}); err != nil {
			return "", "", err
		}
	}
	return nodeName, systemRole, nil
}

type nodeMetadataIdentity struct {
	hostname   string
	systemRole string
}

func readNodeMetadataIdentity(path string) (nodeMetadataIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nodeMetadataIdentity{}, err
	}
	var metadata struct {
		Identity struct {
			Hostname string `json:"hostname"`
		} `json:"identity"`
		SystemRole string `json:"systemRole"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nodeMetadataIdentity{}, fmt.Errorf("decode node metadata: %w", err)
	}
	return nodeMetadataIdentity{
		hostname:   strings.TrimSpace(metadata.Identity.Hostname),
		systemRole: strings.TrimSpace(metadata.SystemRole),
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
