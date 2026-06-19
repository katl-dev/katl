package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/vmtest"
)

func TestThreeNodeGeneration0InstallSmoke(t *testing.T) {
	if run, ok := threeNodeGeneration0InstallWorldRun(t); ok {
		runThreeNodeGeneration0Install(t, run)
		return
	}
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run three-node generation 0 install smoke")
	}
	_ = vmtest.RequireWorld(t)
}

type threeNodeGeneration0InstallRun struct {
	WorldScenario *vmtest.WorldScenario
	Runner        vmtest.Runner
	Scenario      vmtest.Scenario
	Result        vmtest.Result
	Inputs        threeNodeGeneration0InstallInputs
}

type threeNodeGeneration0InstallInputs struct {
	Nodes           []threeNodeGeneration0Input `json:"nodes"`
	WorldProvenance multiNodeWorldProvenancePaths
}

type threeNodeGeneration0Input struct {
	Name                 string   `json:"name"`
	Role                 string   `json:"role"`
	Disk                 string   `json:"disk"`
	DiskFormat           string   `json:"diskFormat"`
	ESPArtifacts         string   `json:"espArtifacts"`
	FixtureManifest      string   `json:"fixtureManifest"`
	NodeMetadata         string   `json:"nodeMetadata,omitempty"`
	Address              string   `json:"address,omitempty"`
	MACAddress           string   `json:"macAddress,omitempty"`
	InstallerUKI         string   `json:"installerUKI,omitempty"`
	InstallerKernel      string   `json:"installerKernel,omitempty"`
	InstallerInitrd      string   `json:"installerInitrd,omitempty"`
	InstallerCommandLine []string `json:"installerCommandLine,omitempty"`
	RuntimeArtifact      string   `json:"runtimeArtifact,omitempty"`
	InstallManifest      string   `json:"installManifest,omitempty"`
	FirstInstallMode     string   `json:"firstInstallMode,omitempty"`
}

type threeNodeGeneration0Proof struct {
	APIVersion               string                             `json:"apiVersion"`
	Kind                     string                             `json:"kind"`
	Nodes                    []threeNodeGeneration0NodeEvidence `json:"nodes"`
	WorldProvenance          multiNodeWorldProvenancePaths      `json:"worldProvenance"`
	NodeRunDirs              map[string]string                  `json:"nodeRunDirs,omitempty"`
	NodeScenarios            map[string]string                  `json:"nodeScenarios,omitempty"`
	NodeResults              map[string]string                  `json:"nodeResults,omitempty"`
	LaunchCommands           map[string]string                  `json:"launchCommands,omitempty"`
	DomainXMLs               map[string]string                  `json:"domainXMLs,omitempty"`
	RuntimeInputs            map[string]string                  `json:"installedRuntimeInputs,omitempty"`
	VSockTranscripts         map[string]string                  `json:"vsockTranscripts,omitempty"`
	LibvirtLeases            map[string]string                  `json:"libvirtLeases,omitempty"`
	SerialLogs               map[string]string                  `json:"serialLogs,omitempty"`
	FixtureProducerScenarios map[string]string                  `json:"fixtureProducerScenarios,omitempty"`
	FixtureProducerResults   map[string]string                  `json:"fixtureProducerResults,omitempty"`
	RerunResults             map[string]string                  `json:"rerunResults,omitempty"`
}

type threeNodeGeneration0NodeEvidence struct {
	Name                string `json:"name"`
	Role                string `json:"role"`
	Address             string `json:"address,omitempty"`
	Result              string `json:"result"`
	FixtureManifest     string `json:"fixtureManifest"`
	InstalledRuntime    string `json:"installedRuntimeInput"`
	GenerationID        string `json:"generationID"`
	GenerationSpec      string `json:"generationSpec"`
	GenerationStatus    string `json:"generationStatus"`
	BootSelection       string `json:"bootSelection"`
	NodeMetadata        string `json:"nodeMetadata,omitempty"`
	MachineIDPath       string `json:"machineIDPath"`
	PersistentMachineID string `json:"persistentMachineID"`
	LayoutProbe         string `json:"layoutProbe"`
	RerunProof          string `json:"rerunProof,omitempty"`
}

type generation0EvidencePaths struct {
	Spec                string
	Status              string
	Selection           string
	Metadata            string
	MachineID           string
	PersistentMachineID string
	LayoutProbe         string
}

type generation0RuntimeMetadata struct {
	RuntimeVersion        string
	RuntimeInterface      string
	RuntimeArtifactSHA256 string
	Architecture          string
	UKIPath               string
}

func threeNodeGeneration0InstallWorldRun(t *testing.T) (threeNodeGeneration0InstallRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return threeNodeGeneration0InstallRun{}, false
	}
	world := vmtest.RequireWorld(t)
	repo := katlRepoRoot(t)
	specs := threeControlPlaneWorldRuntimeSpecs()
	kvm := vmtest.DefaultOptions().KVM
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, specs, kvm); err != nil {
		failWorldFixtureSetup(t, world, "three-node-generation-0-install", err)
	}
	run, err := planThreeNodeGeneration0InstallRun(world, repo, specs, kvm)
	if err != nil {
		failTwoNodeWorldSetup(t, run.WorldScenario, err)
	}
	return run, true
}

func planThreeNodeGeneration0InstallRun(world vmtest.World, repo string, specs []vmtest.NodeSpec, kvm vmtest.KVMPolicy) (threeNodeGeneration0InstallRun, error) {
	scenario, err := world.PlanScenario("three-node-generation-0-install")
	if err != nil {
		return threeNodeGeneration0InstallRun{}, err
	}
	run := threeNodeGeneration0InstallRun{WorldScenario: scenario}
	buildRoots := publishedRuntimeBuildRoots(world, repo)
	inputs := make([]threeNodeGeneration0Input, 0, len(specs))
	for _, spec := range specs {
		published, err := vmtest.FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, spec)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		if !published.HasInstallerProvenance() {
			err := fmt.Errorf("%s published first-install fixture is missing installer provenance", spec.Name)
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		node, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, spec)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		inputs = append(inputs, threeNodeGeneration0Input{
			Name:                 node.Node.Name,
			Role:                 string(node.Node.Role),
			Disk:                 node.Config.Disk,
			DiskFormat:           string(node.Config.DiskFormat),
			ESPArtifacts:         node.Config.ESPArtifacts,
			FixtureManifest:      node.Config.FixtureManifest,
			NodeMetadata:         node.Config.NodeMetadata,
			Address:              node.Node.Address,
			MACAddress:           node.Node.MACAddress,
			InstallerUKI:         published.InstallerUKI,
			InstallerKernel:      published.InstallerKernel,
			InstallerInitrd:      published.InstallerInitrd,
			InstallerCommandLine: append([]string(nil), published.InstallerCommandLine...),
			RuntimeArtifact:      published.RuntimeArtifact,
			InstallManifest:      published.InstallManifest,
			FirstInstallMode:     published.FirstInstallMode,
		})
	}
	options := vmtest.Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      vmtest.KeepFailed,
		KVM:       kvm,
		Missing:   vmtest.MissingFails,
	}
	runner := vmtest.NewRunner(options)
	vmScenario := vmtest.Scenario{Name: "three-node-generation-0-install"}
	result, err := runner.Plan(vmScenario)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	result.Started = time.Now().UTC()
	return threeNodeGeneration0InstallRun{
		WorldScenario: scenario,
		Runner:        runner,
		Scenario:      vmScenario,
		Result:        result,
		Inputs: threeNodeGeneration0InstallInputs{
			Nodes:           inputs,
			WorldProvenance: multiNodeWorldProvenanceForSpecs(world, repo, specs),
		},
	}, nil
}

func runThreeNodeGeneration0Install(t *testing.T, run threeNodeGeneration0InstallRun) {
	t.Helper()
	result := run.Result
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	nodes := make([]vmtest.RunningInstalledRuntimeNode, 0, len(run.Inputs.Nodes))
	defer func() {
		for _, node := range nodes {
			stopNode(t, node)
		}
	}()
	planned := make([]vmtest.RunningInstalledRuntimeNode, 0, len(run.Inputs.Nodes))
	for _, input := range run.Inputs.Nodes {
		nodeResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, input.Name)
		if err != nil {
			t.Fatal(err)
		}
		planned = append(planned, vmtest.RunningInstalledRuntimeNode{Name: input.Name, Result: nodeResult})
		if err := writeThreeNodeGeneration0Proof(result, run.Inputs, planned, nil); err != nil {
			t.Fatal(err)
		}
		node, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
			Name: input.Name,
			Runtime: vmtest.InstalledRuntimeConfig{
				Disk:               input.Disk,
				DiskFormat:         vmtest.DiskFormat(input.DiskFormat),
				ESPArtifacts:       input.ESPArtifacts,
				FixtureManifest:    input.FixtureManifest,
				NodeMetadata:       input.NodeMetadata,
				RequireVMTestAgent: true,
				VM: vmtest.VMConfig{
					KVM:    run.Runner.Options.KVM,
					RAMMiB: 4096,
					CPUs:   2,
					Network: vmtest.VMNetworkConfig{
						MAC: input.MACAddress,
					},
					Timeout: 8 * time.Minute,
					VSock: vmtest.VSockConfig{
						Enabled: true,
					},
					Agent: vmtest.AgentControlConfig{
						RequireHealth: true,
						Timeout:       30 * time.Second,
					},
				},
			},
		}, vmtest.VMRunner{})
		if err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, run.Runner, run.Scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("start %s VM: %v", input.Name, err)
		}
		nodes = append(nodes, node)
	}
	evidence, err := collectThreeNodeGeneration0Evidence(ctx, result, run.Inputs, nodes)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, run.Runner, run.Scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	for _, node := range nodes {
		stopNode(t, node)
	}
	nodes = nil
	rerunResults, err := runThreeNodeGeneration0RerunProofs(ctx, result, run.Inputs, run.Runner.Options.KVM)
	if err != nil {
		finishTwoNodeResult(t, run.Runner, run.Scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	if err := writeThreeNodeGeneration0Proof(result, run.Inputs, evidence, rerunResults); err != nil {
		finishTwoNodeResult(t, run.Runner, run.Scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	finishTwoNodeResult(t, run.Runner, run.Scenario, result, vmtest.StatusPassed, "")
}

func collectThreeNodeGeneration0Evidence(ctx context.Context, result vmtest.Result, inputs threeNodeGeneration0InstallInputs, nodes []vmtest.RunningInstalledRuntimeNode) ([]vmtest.RunningInstalledRuntimeNode, error) {
	var expected *generation0RuntimeMetadata
	for _, node := range nodes {
		input, ok := threeNodeGeneration0InputFor(inputs.Nodes, node.Name)
		if !ok {
			return nil, os.ErrNotExist
		}
		evidenceDir := filepath.Join(node.Result.Artifacts.GuestDir, "generation-0")
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			return nil, err
		}
		paths, err := collectGeneration0NodeEvidence(ctx, node, evidenceDir)
		if err != nil {
			return nil, err
		}
		if err := assertNoRunIdentity(ctx, node); err != nil {
			return nil, err
		}
		runtime, err := assertGeneration0NodeEvidence(paths, input)
		if err != nil {
			return nil, err
		}
		if expected == nil {
			expected = &runtime
			continue
		}
		if runtime != *expected {
			return nil, fmt.Errorf("%s runtime metadata = %#v, want %#v", node.Name, runtime, *expected)
		}
	}
	return nodes, nil
}

func runThreeNodeGeneration0RerunProofs(ctx context.Context, parent vmtest.Result, inputs threeNodeGeneration0InstallInputs, kvm vmtest.KVMPolicy) (map[string]string, error) {
	runner := vmtest.NewRunner(vmtest.Options{
		Enabled:   true,
		StateRoot: filepath.Join(parent.RunDir, "installer-rerun-vm-runs"),
		Keep:      vmtest.KeepFailed,
		KVM:       kvm,
		Missing:   vmtest.MissingFails,
	})
	results := make(map[string]string, len(inputs.Nodes))
	for _, input := range inputs.Nodes {
		resultPath, err := runThreeNodeGeneration0RerunProof(ctx, runner, input)
		if err != nil {
			return nil, err
		}
		results[input.Name] = resultPath
	}
	return results, nil
}

func runThreeNodeGeneration0RerunProof(ctx context.Context, runner vmtest.Runner, input threeNodeGeneration0Input) (string, error) {
	if strings.TrimSpace(input.InstallManifest) == "" {
		return "", fmt.Errorf("%s installer rerun proof is missing install manifest provenance", input.Name)
	}
	scenario := vmtest.Scenario{
		Name: "three-node-generation-0-install-rerun-" + input.Name,
		Disks: []vmtest.DiskFixture{
			vmtest.SnapshotDisk("root", input.Disk, vmtest.DiskFormat(input.DiskFormat)),
		},
	}
	config := vmtest.FirstInstallConfig{
		Installer: vmtest.InstallerBootConfig{
			InstallerUKI:    input.InstallerUKI,
			InstallerKernel: input.InstallerKernel,
			InstallerInitrd: input.InstallerInitrd,
			CommandLine:     append([]string(nil), input.InstallerCommandLine...),
			RuntimeArtifact: input.RuntimeArtifact,
			VM: vmtest.VMConfig{
				KVM:     runner.Options.KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 8 * time.Minute,
			},
		},
		Runtime: vmtest.InstalledRuntimeConfig{
			ESPArtifacts:       input.ESPArtifacts,
			RequireVMTestAgent: true,
			VM: vmtest.VMConfig{
				KVM:     runner.Options.KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 8 * time.Minute,
				Agent: vmtest.AgentControlConfig{
					RequireHealth: true,
					Timeout:       30 * time.Second,
				},
			},
		},
		ManifestPath:    input.InstallManifest,
		UseInstalledESP: true,
	}
	switch vmtest.FirstInstallWorldMode(input.FirstInstallMode) {
	case vmtest.FirstInstallWorldPreseed:
		config.PreseedManifest = true
	case vmtest.FirstInstallWorldGuestHandoff:
		config.GuestHandoff = true
	default:
		return "", fmt.Errorf("%s installer rerun proof has unsupported first-install mode %q", input.Name, input.FirstInstallMode)
	}
	result, err := vmtest.RunFirstInstall(ctx, runner, scenario, config)
	if err != nil {
		return "", err
	}
	if result.Status == vmtest.StatusPassed {
		return result.Artifacts.Result, nil
	}
	if installerRerunRefused(result) {
		return result.Artifacts.Result, nil
	}
	return "", fmt.Errorf("%s installer rerun status = %q, failure = %q, run dir = %s", input.Name, result.Status, result.FailureSummary, result.RunDir)
}

func installerRerunRefused(result vmtest.Result) bool {
	text := strings.ToLower(result.FailureSummary)
	if data, err := os.ReadFile(result.Artifacts.InstallerSerial); err == nil {
		text += "\n" + strings.ToLower(string(data))
	}
	for _, signal := range []string{
		"install refused",
		"install-refused",
		"already installed",
		"already-installed",
		"already exists",
		"generation spec already exists",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func assertNoRunIdentity(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) error {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := vmtest.DialAgent(opCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		return err
	}
	defer client.Close()
	guest := vmtest.NewGuestControl(node.Result, client)
	record, err := guest.RunCommand(opCtx, vmtest.GuestCommandRequest{
		Name: "no-run-identity",
		Argv: []string{
			"find",
			"/run/katl",
			"-xdev",
			"(",
			"-path",
			"/run/katl/identity",
			"-o",
			"-name",
			"machine-id",
			"-o",
			"-name",
			"node.json",
			")",
			"-print",
			"-quit",
		},
		StdoutLimit:  16 << 10,
		StderrLimit:  16 << 10,
		AllowFailure: true,
	})
	if err != nil {
		return fmt.Errorf("%s /run identity probe failed: %w", node.Name, err)
	}
	stdout, err := os.ReadFile(record.Stdout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(stdout)) != "" {
		return fmt.Errorf("%s stores persistent identity under /run: %s", node.Name, strings.TrimSpace(string(stdout)))
	}
	return nil
}

func collectGeneration0NodeEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string) (generation0EvidencePaths, error) {
	healthyStatus, err := waitForGeneration0BootHealth(ctx, node, 2*time.Minute)
	if err != nil {
		return generation0EvidencePaths{}, err
	}
	files := []struct {
		guestPath string
		name      string
		maxBytes  uint32
	}{
		{"/var/lib/katl/generations/0/spec.json", "generation-spec.json", 256 << 10},
		{"/var/lib/katl/generations/0/status.json", "generation-status.json", 256 << 10},
		{"/var/lib/katl/boot/selection.json", "boot-selection.json", 64 << 10},
		{"/var/lib/katl/generations/0/confext/etc/katl/node.json", "node-metadata.json", 64 << 10},
		{"/etc/machine-id", "machine-id.txt", 4 << 10},
		{"/var/lib/katl/identity/machine-id", "persistent-machine-id.txt", 4 << 10},
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		data := healthyStatus
		if file.guestPath != "/var/lib/katl/generations/0/status.json" {
			var err error
			data, err = readNodeFileWithRetry(ctx, node, file.guestPath, file.maxBytes, 2*time.Minute)
			if err != nil {
				return generation0EvidencePaths{}, err
			}
		}
		hostPath := filepath.Join(evidenceDir, file.name)
		if err := os.WriteFile(hostPath, data, 0o600); err != nil {
			return generation0EvidencePaths{}, err
		}
		paths = append(paths, hostPath)
	}
	layoutProbe, err := collectGeneration0LayoutProbe(ctx, node, evidenceDir)
	if err != nil {
		return generation0EvidencePaths{}, err
	}
	return generation0EvidencePaths{
		Spec:                paths[0],
		Status:              paths[1],
		Selection:           paths[2],
		Metadata:            paths[3],
		MachineID:           paths[4],
		PersistentMachineID: paths[5],
		LayoutProbe:         layoutProbe,
	}, nil
}

func waitForGeneration0BootHealth(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		data, err := readNodeFile(ctx, node, "/var/lib/katl/generations/0/status.json", 256<<10)
		if err == nil {
			var status generation.GenerationStatus
			if err := json.Unmarshal(data, &status); err != nil {
				lastErr = err
			} else if status.CommitState == generation.CommitStateCommitted && status.BootState == generation.BootStateGood && status.HealthState == generation.HealthStateHealthy {
				return data, nil
			} else {
				lastErr = fmt.Errorf("generation 0 status = commit:%q boot:%q health:%q", status.CommitState, status.BootState, status.HealthState)
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func collectGeneration0LayoutProbe(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string) (string, error) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := vmtest.DialAgent(opCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		return "", err
	}
	defer client.Close()
	guest := vmtest.NewGuestControl(node.Result, client)

	var probe strings.Builder
	cmdline, err := guest.ReadFile(opCtx, vmtest.GuestFileRequest{
		Name:         "proc-cmdline",
		Path:         "/proc/cmdline",
		MaxBytes:     64 << 10,
		StoreContent: true,
	})
	if err != nil {
		return "", fmt.Errorf("%s generation 0 cmdline probe failed: %w", node.Name, err)
	}
	cmdlineData, err := os.ReadFile(cmdline.Artifact)
	if err != nil {
		return "", err
	}
	probe.WriteString("[cmdline]\n")
	probe.Write(cmdlineData)
	if !strings.HasSuffix(string(cmdlineData), "\n") {
		probe.WriteByte('\n')
	}

	probe.WriteString("\n[findmnt]\n")
	for _, target := range []string{
		"/",
		"/var",
		"/var/lib",
		"/var/lib/katl",
		"/etc/machine-id",
	} {
		findmnt, err := guest.RunCommand(opCtx, vmtest.GuestCommandRequest{
			Name: "generation-0-findmnt-" + strings.Trim(strings.ReplaceAll(target, "/", "-"), "-"),
			Argv: []string{
				"findmnt",
				"--target",
				target,
				"-n",
				"-o",
				"TARGET,SOURCE,FSTYPE,OPTIONS",
			},
			StdoutLimit: 64 << 10,
			StderrLimit: 32 << 10,
		})
		if err != nil {
			return "", fmt.Errorf("%s generation 0 mount probe for %s failed: %w", node.Name, target, err)
		}
		for _, path := range []string{findmnt.Stdout, findmnt.Stderr} {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			probe.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				probe.WriteByte('\n')
			}
		}
	}

	probe.WriteString("\n[paths]\n")
	for _, path := range []string{
		"/var/lib/katl/generations/0",
		"/var/lib/katl/generations/0/spec.json",
		"/var/lib/katl/generations/0/status.json",
		"/var/lib/katl/generations/0/confext",
		"/var/lib/katl/generations/0/confext/etc/katl/node.json",
		"/var/lib/katl/boot/selection.json",
		"/var/lib/katl/identity/machine-id",
	} {
		if _, err := guest.RunCommand(opCtx, vmtest.GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "-e", path}}); err != nil {
			return "", fmt.Errorf("%s generation 0 path %s missing: %w", node.Name, path, err)
		}
		probe.WriteString("exists ")
		probe.WriteString(path)
		probe.WriteByte('\n')
	}
	if _, err := guest.RunCommand(opCtx, vmtest.GuestCommandRequest{Name: "run-identity", Argv: []string{"test", "!", "-e", "/run/katl/identity"}}); err != nil {
		return "", fmt.Errorf("%s has /run/katl/identity: %w", node.Name, err)
	}
	probe.WriteString("absent /run/katl/identity\n")

	path := filepath.Join(evidenceDir, "layout-probe.txt")
	if err := os.WriteFile(path, []byte(probe.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func assertGeneration0NodeEvidence(paths generation0EvidencePaths, input threeNodeGeneration0Input) (generation0RuntimeMetadata, error) {
	var spec generation.GenerationSpec
	if err := readJSONFile(paths.Spec, &spec); err != nil {
		return generation0RuntimeMetadata{}, err
	}
	var status generation.GenerationStatus
	if err := readJSONFile(paths.Status, &status); err != nil {
		return generation0RuntimeMetadata{}, err
	}
	var selection generation.BootSelectionRecord
	if err := readJSONFile(paths.Selection, &selection); err != nil {
		return generation0RuntimeMetadata{}, err
	}
	if spec.APIVersion != generation.APIVersion || spec.Kind != generation.SpecKind {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation spec identity = %q/%q", spec.APIVersion, spec.Kind)
	}
	if status.APIVersion != generation.APIVersion || status.Kind != generation.StatusKind {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation status identity = %q/%q", status.APIVersion, status.Kind)
	}
	if selection.APIVersion != generation.APIVersion || selection.Kind != generation.BootSelectionKind {
		return generation0RuntimeMetadata{}, fmt.Errorf("boot selection identity = %q/%q", selection.APIVersion, selection.Kind)
	}
	if spec.GenerationID != "0" || status.GenerationID != "0" || selection.DefaultGenerationID != "0" || strings.TrimSpace(selection.BootedGenerationID) != "0" {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation identity mismatch: spec=%q status=%q default=%q booted=%q", spec.GenerationID, status.GenerationID, selection.DefaultGenerationID, selection.BootedGenerationID)
	}
	if selection.Generation0FallbackID != "0" {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation 0 fallback = %q, want 0", selection.Generation0FallbackID)
	}
	if selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" || selection.PendingHealthValidation || selection.PendingTransactionID != "" {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation 0 has pending boot transaction: %#v", selection)
	}
	if selection.PersistentDefaultPromotion != "" && selection.PersistentDefaultPromotion != generation.DefaultPromotionDone {
		return generation0RuntimeMetadata{}, fmt.Errorf("persistent default promotion = %q", selection.PersistentDefaultPromotion)
	}
	if strings.TrimSpace(spec.Root.RuntimeArtifactSHA256) == "" || strings.TrimSpace(spec.Root.RuntimeInterface) == "" {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation 0 root metadata is incomplete")
	}
	if strings.TrimSpace(spec.RuntimeVersion) == "" || strings.TrimSpace(spec.Root.Architecture) == "" || strings.TrimSpace(spec.Boot.UKIPath) == "" {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation 0 runtime version, architecture, or UKI path is incomplete")
	}
	if status.CommitState != generation.CommitStateCommitted || status.BootState != generation.BootStateGood || status.HealthState != generation.HealthStateHealthy {
		return generation0RuntimeMetadata{}, fmt.Errorf("generation 0 status = commit:%q boot:%q health:%q", status.CommitState, status.BootState, status.HealthState)
	}
	var metadata struct {
		Identity struct {
			Hostname string `json:"hostname"`
		} `json:"identity"`
		SystemRole string `json:"systemRole"`
	}
	if err := readJSONFile(paths.Metadata, &metadata); err != nil {
		return generation0RuntimeMetadata{}, err
	}
	if metadata.Identity.Hostname != input.Name || metadata.SystemRole != input.Role {
		return generation0RuntimeMetadata{}, fmt.Errorf("node metadata = %q/%q, want %q/%q", metadata.Identity.Hostname, metadata.SystemRole, input.Name, input.Role)
	}
	machineID, err := os.ReadFile(paths.MachineID)
	if err != nil {
		return generation0RuntimeMetadata{}, err
	}
	if strings.TrimSpace(string(machineID)) == "" {
		return generation0RuntimeMetadata{}, fmt.Errorf("machine-id is empty")
	}
	persistentMachineID, err := os.ReadFile(paths.PersistentMachineID)
	if err != nil {
		return generation0RuntimeMetadata{}, err
	}
	if strings.TrimSpace(string(persistentMachineID)) == "" {
		return generation0RuntimeMetadata{}, fmt.Errorf("persistent machine-id is empty")
	}
	if strings.TrimSpace(string(machineID)) != strings.TrimSpace(string(persistentMachineID)) {
		return generation0RuntimeMetadata{}, fmt.Errorf("/etc/machine-id does not match /var/lib/katl/identity/machine-id")
	}
	return generation0RuntimeMetadata{
		RuntimeVersion:        spec.RuntimeVersion,
		RuntimeInterface:      spec.Root.RuntimeInterface,
		RuntimeArtifactSHA256: spec.Root.RuntimeArtifactSHA256,
		Architecture:          spec.Root.Architecture,
		UKIPath:               spec.Boot.UKIPath,
	}, nil
}

func writeThreeNodeGeneration0Proof(result vmtest.Result, inputs threeNodeGeneration0InstallInputs, nodes []vmtest.RunningInstalledRuntimeNode, rerunResults map[string]string) error {
	proof := threeNodeGeneration0Proof{
		APIVersion:               vmtest.WorldAPIVersion,
		Kind:                     "ThreeNodeGeneration0InstallProof",
		WorldProvenance:          inputs.WorldProvenance,
		NodeRunDirs:              nodeRunDirs(nodes),
		NodeScenarios:            nodeScenarioPaths(nodes),
		NodeResults:              nodeResultPaths(nodes),
		LaunchCommands:           launchCommandPaths(nodes),
		DomainXMLs:               domainXMLPaths(nodes),
		RuntimeInputs:            installedRuntimeInputPaths(nodes),
		VSockTranscripts:         vsockTranscriptPaths(nodes),
		LibvirtLeases:            libvirtLeasePaths(nodes),
		SerialLogs:               serialLogPaths(nodes),
		FixtureProducerScenarios: inputs.WorldProvenance.FixtureProducerScenarios,
		FixtureProducerResults:   inputs.WorldProvenance.FixtureProducerResults,
		RerunResults:             rerunResults,
	}
	for _, node := range nodes {
		input, _ := threeNodeGeneration0InputFor(inputs.Nodes, node.Name)
		evidenceDir := filepath.Join(node.Result.Artifacts.GuestDir, "generation-0")
		proof.Nodes = append(proof.Nodes, threeNodeGeneration0NodeEvidence{
			Name:                node.Name,
			Role:                input.Role,
			Address:             firstString(node.Result.IPAddress, input.Address),
			Result:              node.Result.Artifacts.Result,
			FixtureManifest:     input.FixtureManifest,
			InstalledRuntime:    node.Result.Artifacts.InstalledRuntime,
			GenerationID:        "0",
			GenerationSpec:      filepath.Join(evidenceDir, "generation-spec.json"),
			GenerationStatus:    filepath.Join(evidenceDir, "generation-status.json"),
			BootSelection:       filepath.Join(evidenceDir, "boot-selection.json"),
			NodeMetadata:        filepath.Join(evidenceDir, "node-metadata.json"),
			MachineIDPath:       filepath.Join(evidenceDir, "machine-id.txt"),
			PersistentMachineID: filepath.Join(evidenceDir, "persistent-machine-id.txt"),
			LayoutProbe:         filepath.Join(evidenceDir, "layout-probe.txt"),
			RerunProof:          rerunResults[node.Name],
		})
	}
	return writeTwoNodeDiagnosticJSON(filepath.Join(result.ManifestDir, "three-node-generation-0-install-proof.json"), proof)
}

func threeNodeGeneration0InputFor(inputs []threeNodeGeneration0Input, name string) (threeNodeGeneration0Input, bool) {
	for _, input := range inputs {
		if input.Name == name {
			return input, true
		}
	}
	return threeNodeGeneration0Input{}, false
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
