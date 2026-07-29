package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Mode            FirstInstallWorldMode
}

type ProducedInstalledRuntimeFixture struct {
	ManifestPath string
	Disk         string
	ESPArtifacts string
}

type FirstInstallRuntimeFixtureOptions struct {
	Input                      FirstInstallWorldInput
	KVM                        KVMPolicy
	RequireInstallerProvenance bool
	Produce                    func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error)
}

type firstInstallRuntimeFixtureInputIdentity struct {
	InstallerUKISHA256    string   `json:"installerUKISHA256,omitempty"`
	InstallerKernelSHA256 string   `json:"installerKernelSHA256,omitempty"`
	InstallerInitrdSHA256 string   `json:"installerInitrdSHA256,omitempty"`
	InstallerCommandLine  []string `json:"installerCommandLine,omitempty"`
	RuntimeRootSHA256     string   `json:"runtimeRootSHA256"`
	RuntimeESPTreeSHA256  string   `json:"runtimeESPTreeSHA256,omitempty"`
	InstallManifestSHA256 string   `json:"installManifestSHA256"`
	NodeMetadataSHA256    string   `json:"nodeMetadataSHA256,omitempty"`
	Mode                  string   `json:"mode,omitempty"`
	TargetDiskFormat      string   `json:"targetDiskFormat,omitempty"`
	TargetDiskSize        string   `json:"targetDiskSize,omitempty"`
	UseInstalledESP       bool     `json:"useInstalledESP"`
}

func EnsurePublishedFirstInstallRuntimeFixtures(ctx context.Context, world World, repo string, specs []NodeSpec, options FirstInstallRuntimeFixtureOptions) error {
	produce := options.Produce
	if produce == nil {
		produce = ProduceFirstInstallRuntimeFixture
	}
	var errs []error
	for _, spec := range specs {
		if _, err := ensurePublishedFirstInstallRuntimeFixture(ctx, world, repo, spec, options, produce); err != nil {
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

func ensurePublishedFirstInstallRuntimeFixture(ctx context.Context, world World, repo string, spec NodeSpec, options FirstInstallRuntimeFixtureOptions, produce func(context.Context, FirstInstallRuntimeFixtureContract) (ProducedInstalledRuntimeFixture, error)) (string, error) {
	cacheDir := WorldFixtureCacheDir(world)
	contract, err := FirstInstallRuntimeFixtureContractForWorld(world, repo, spec, options.Input, options.KVM)
	if err != nil {
		if resetErr := resetFirstInstallRuntimeFixtureScenario(world, spec); resetErr != nil {
			return "", errors.Join(err, resetErr)
		}
		contract, err = FirstInstallRuntimeFixtureContractForWorld(world, repo, spec, options.Input, options.KVM)
		if err != nil {
			return "", err
		}
	}
	inputDigest, err := firstInstallRuntimeFixtureInputDigest(contract)
	if err != nil {
		return "", err
	}
	if published, err := findPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{cacheDir}, spec, inputDigest); err == nil {
		if !options.RequireInstallerProvenance || published.HasInstallerProvenance() {
			return inputDigest, nil
		}
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return "", err
	}
	unlock, err := lockLeaseFile(filepath.Join(cacheDir, "locks", FirstInstallRuntimeFixtureScenarioName(spec)))
	if err != nil {
		return "", err
	}
	defer unlock()
	if published, err := findPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{cacheDir}, spec, inputDigest); err == nil {
		if !options.RequireInstallerProvenance || published.HasInstallerProvenance() {
			return inputDigest, nil
		}
	} else if !isMissingPublishedFirstInstallRuntimeFixture(err) {
		return "", err
	}
	if _, err := produce(ctx, contract); err != nil {
		if contract.WorldScenario != nil {
			_ = contract.WorldScenario.WriteSetupFailure(err)
		}
		return "", err
	}
	published, err := findPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{cacheDir}, spec, inputDigest)
	if err != nil {
		return "", err
	}
	if options.RequireInstallerProvenance && !published.HasInstallerProvenance() {
		return "", errors.New("published installed runtime fixture is missing installer provenance")
	}
	return inputDigest, nil
}

func resetFirstInstallRuntimeFixtureScenario(world World, spec NodeSpec) error {
	scenarioDir := strings.TrimSpace(world.ScenarioDir)
	if scenarioDir == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(scenarioDir, clean(FirstInstallRuntimeFixtureScenarioName(spec))))
}

func firstInstallRuntimeFixtureInputDigest(contract FirstInstallRuntimeFixtureContract) (string, error) {
	identity := firstInstallRuntimeFixtureInputIdentity{
		InstallerCommandLine: append([]string(nil), contract.InstallerBoot.CommandLine...),
		Mode:                 string(firstInstallModeForContract(contract)),
		TargetDiskFormat:     string(contract.TargetDisk.Format),
		TargetDiskSize:       strings.TrimSpace(contract.TargetDisk.Size),
		UseInstalledESP:      contract.UseInstalledESP,
	}
	if strings.TrimSpace(contract.InstallerBoot.InstallerUKI) != "" {
		sum, err := fileSHA256(contract.InstallerBoot.InstallerUKI)
		if err != nil {
			return "", fmt.Errorf("hash installer UKI: %w", err)
		}
		identity.InstallerUKISHA256 = sum
	}
	if strings.TrimSpace(contract.InstallerBoot.InstallerKernel) != "" {
		sum, err := fileSHA256(contract.InstallerBoot.InstallerKernel)
		if err != nil {
			return "", fmt.Errorf("hash installer kernel: %w", err)
		}
		identity.InstallerKernelSHA256 = sum
	}
	if strings.TrimSpace(contract.InstallerBoot.InstallerInitrd) != "" {
		sum, err := fileSHA256(contract.InstallerBoot.InstallerInitrd)
		if err != nil {
			return "", fmt.Errorf("hash installer initrd: %w", err)
		}
		identity.InstallerInitrdSHA256 = sum
	}
	runtimeRootSHA, err := firstInstallContractRuntimeSHA(contract)
	if err != nil {
		return "", err
	}
	identity.RuntimeRootSHA256 = runtimeRootSHA
	if strings.TrimSpace(contract.RuntimeESP) != "" {
		sum, err := espTreeSHA256(contract.RuntimeESP)
		if err != nil {
			return "", fmt.Errorf("hash runtime ESP artifacts: %w", err)
		}
		identity.RuntimeESPTreeSHA256 = sum
	}
	manifestSHA, err := fileSHA256(contract.ManifestPath)
	if err != nil {
		return "", fmt.Errorf("hash install manifest: %w", err)
	}
	identity.InstallManifestSHA256 = manifestSHA
	if strings.TrimSpace(contract.NodeMetadata) != "" {
		sum, err := fileSHA256(contract.NodeMetadata)
		if err != nil {
			return "", fmt.Errorf("hash node metadata: %w", err)
		}
		identity.NodeMetadataSHA256 = sum
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func firstInstallContractRuntimeSHA(contract FirstInstallRuntimeFixtureContract) (string, error) {
	if strings.TrimSpace(contract.RuntimeArtifact) != "" {
		sum, err := fileSHA256(contract.RuntimeArtifact)
		if err != nil {
			return "", fmt.Errorf("hash runtime root artifact: %w", err)
		}
		return sum, nil
	}
	imagePath, err := installManifestLocalImagePath(contract.ManifestPath)
	if err != nil {
		return "", err
	}
	sum, err := fileSHA256(imagePath)
	if err != nil {
		return "", fmt.Errorf("hash KatlOS install image: %w", err)
	}
	return sum, nil
}

func installManifestLocalImagePath(manifestPath string) (string, error) {
	localRef, err := installManifestLocalImageRef(manifestPath)
	if err != nil {
		return "", err
	}
	if localRef == "" {
		return "", fmt.Errorf("install manifest katlosImage.localRef is required when no loose runtime artifact is present")
	}
	return filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(localRef)), nil
}

func installManifestLocalImageRef(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read install manifest: %w", err)
	}
	var manifest struct {
		KatlosImage struct {
			LocalRef string `json:"localRef"`
		} `json:"katlosImage"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("decode install manifest: %w", err)
	}
	localRef := strings.TrimSpace(manifest.KatlosImage.LocalRef)
	if localRef == "" {
		return "", nil
	}
	if filepath.IsAbs(localRef) || filepath.Clean(localRef) != localRef || localRef == "." || strings.HasPrefix(localRef, "../") || strings.Contains(localRef, "/../") {
		return "", fmt.Errorf("install manifest katlosImage.localRef %q must be a clean relative path", localRef)
	}
	return localRef, nil
}

func firstInstallModeForContract(contract FirstInstallRuntimeFixtureContract) FirstInstallWorldMode {
	if contract.Mode != "" {
		return contract.Mode
	}
	return FirstInstallWorldPreseed
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
		Mode:            run.Mode,
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
		Libvirt:   true,
		ImageTool: true,
		OVMF:      true,
		KVM:       runner.options().KVM,
		MTools:    true,
	}); err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  2048,
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
	installerVM, runtimeVM := firstInstallFixtureVMConfigs(vm)
	config := FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    contract.InstallerBoot.InstallerUKI,
			InstallerKernel: contract.InstallerBoot.InstallerKernel,
			InstallerInitrd: contract.InstallerBoot.InstallerInitrd,
			CommandLine:     contract.InstallerBoot.CommandLine,
			RuntimeArtifact: contract.RuntimeArtifact,
			VM:              installerVM,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts:       contract.RuntimeESP,
			RequireVMTestAgent: true,
			VM:                 runtimeVM,
		},
		UseInstalledESP: contract.UseInstalledESP,
		ManifestPath:    contract.ManifestPath,
		TargetDisk:      contract.TargetDisk,
	}
	switch firstInstallModeForContract(contract) {
	case FirstInstallWorldPreseed:
		config.PreseedManifest = true
	case FirstInstallWorldGuestHandoff:
		config.GuestHandoff = true
	default:
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("unsupported first-install fixture mode: %s", contract.Mode)
	}
	firstResult, err := RunFirstInstall(ctx, runner, Scenario{Name: FirstInstallRuntimeFixtureScenarioName(contract.Node)}, config)
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

func firstInstallFixtureVMConfigs(base VMConfig) (VMConfig, VMConfig) {
	installer := base
	installer.VSock = VSockConfig{}
	installer.Agent = AgentControlConfig{}
	return installer, base
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
	inputDigest, err := firstInstallRuntimeFixtureInputDigest(contract)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, err
	}
	cached, err := packageInstalledRuntimeFixture(contract, filepath.Join(WorldFixtureCacheDir(contract.WorldScenario.World), "fixtures", FirstInstallRuntimeFixtureScenarioName(contract.Node)), installedDisk, fixtureESP)
	if err != nil {
		return ProducedInstalledRuntimeFixture{}, fmt.Errorf("cache first-install runtime fixture: %w", err)
	}
	if _, err := writePublishedFirstInstallRuntimeFixtureForContract(WorldFixtureCacheDir(contract.WorldScenario.World), FirstInstallRuntimeFixtureScenarioName(contract.Node), cached.ManifestPath, DiskQCOW2, inputDigest, contract); err != nil {
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
	return packageInstalledRuntimeFixture(contract, fixtureDir, installedDisk, fixtureESP)
}

func packageInstalledRuntimeFixture(contract FirstInstallRuntimeFixtureContract, fixtureDir, installedDisk, fixtureESP string) (ProducedInstalledRuntimeFixture, error) {
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
	return errors.Is(err, errPublishedFirstInstallRuntimeFixtureMissing)
}
