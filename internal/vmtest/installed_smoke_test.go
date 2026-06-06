package vmtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirstInstallTargetDiskFixtureContract(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install fixture smoke")
	}
	options.Missing = MissingSkips
	options.Keep = KeepAlways
	useInstalledESP := envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")
	var runner Runner
	var worldScenario *WorldScenario
	var installerBoot InstallerBootConfig
	var runtimeArtifact, runtimeESP, nodeMetadata, manifestPath, repo string
	targetDiskFixture := TargetDisk("root", string(DiskQCOW2), first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "20G"))
	if worldRun, ok := firstInstallWorldRunFor(t, "first-install-installed-runtime-fixture", NodeSpec{Name: "cp-1", Role: ControlPlane}, useInstalledESP); ok {
		runner = worldRun.Runner
		worldScenario = worldRun.Scenario
		installerBoot = worldRun.Config.Installer
		runtimeArtifact = worldRun.Config.Installer.RuntimeArtifact
		runtimeESP = worldRun.Config.Runtime.ESPArtifacts
		nodeMetadata = worldRun.Config.Runtime.NodeMetadata
		manifestPath = worldRun.Config.ManifestPath
		targetDiskFixture = worldRun.Config.TargetDisk
		repo = worldRun.Repo
	} else {
		installerBoot = firstInstallInstallerBoot(t)
		runtimeArtifact = RequireEnv(t, "KATL_RUNTIME_ARTIFACT")
		runtimeESP = first(os.Getenv("KATL_RUNTIME_ESP_ARTIFACTS"), os.Getenv("KATL_INSTALLED_ESP_ARTIFACTS"))
		if runtimeESP == "" && !useInstalledESP {
			t.Skip("set KATL_RUNTIME_ESP_ARTIFACTS or KATL_INSTALLED_ESP_ARTIFACTS to run first-install fixture smoke")
		}
		nodeMetadata = first(os.Getenv("KATL_RUNTIME_NODE_METADATA"), os.Getenv("KATL_INSTALLED_NODE_METADATA"))
		if nodeMetadata != "" {
			if _, err := os.Stat(nodeMetadata); err != nil {
				t.Skipf("node metadata %s is unavailable: %v", nodeMetadata, err)
			}
		}
		manifestPath = RequireEnv(t, "KATL_INSTALL_MANIFEST")
		repo = repoRoot(t)
		runner = NewRunner(options)
	}
	requiredTools := []string{"jq", "sha256sum"}
	if useInstalledESP {
		requiredTools = append(requiredTools, "sfdisk", "mcopy")
	}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to package installed runtime fixtures: %v", tool, err)
		}
	}

	runner.RequireHost(t, HostRequirements{
		QEMU:    true,
		QEMUImg: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
		MTools:  true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

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
	firstResult, err := RunFirstInstall(ctx, runner, Scenario{Name: "first-install-installed-runtime-fixture"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    installerBoot.InstallerUKI,
			InstallerKernel: installerBoot.InstallerKernel,
			InstallerInitrd: installerBoot.InstallerInitrd,
			CommandLine:     installerBoot.CommandLine,
			RuntimeArtifact: runtimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts:       runtimeESP,
			RequireVMTestAgent: true,
			VM:                 vm,
		},
		UseInstalledESP: useInstalledESP,
		ManifestPath:    manifestPath,
		PreseedManifest: true,
		TargetDisk:      targetDiskFixture,
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if firstResult.Status != StatusPassed {
		t.Fatalf("first install status = %q, failure = %q, run dir = %s", firstResult.Status, firstResult.FailureSummary, firstResult.RunDir)
	}
	installedDisk := targetDiskPath(t, firstResult)
	fixtureESP := runtimeESP
	if useInstalledESP {
		fixtureESP = firstResult.Artifacts.InstalledESP
		if _, err := os.Stat(fixtureESP); err != nil {
			t.Fatalf("installed ESP artifacts %s are unavailable: %v", fixtureESP, err)
		}
	}
	fixtureDir := filepath.Join(firstResult.ManifestDir, "installed-runtime-fixture")
	createFixture := createInstalledRuntimeFixtureCommand(ctx, repo, installedDisk, fixtureESP, string(DiskQCOW2), fixtureDir, nodeMetadata)
	output, err := createFixture.CombinedOutput()
	if err != nil {
		t.Fatalf("create installed runtime fixture failed: %v\n%s", err, output)
	}

	fixtureManifest := filepath.Join(fixtureDir, "installed-runtime-fixture.json")
	packagedDisk := filepath.Join(fixtureDir, "installed-runtime.qcow2")
	packagedESP := filepath.Join(fixtureDir, "esp")
	checkFixture := resolveInstalledRuntimeFixtureCommand(ctx, repo, packagedDisk, packagedESP, fixtureManifest, string(DiskQCOW2), filepath.Join(fixtureDir, "recheck"), packagedNodeMetadata(fixtureDir, nodeMetadata))
	output, err = checkFixture.CombinedOutput()
	if err != nil {
		t.Fatalf("check installed runtime fixture failed: %v\n%s", err, output)
	}
	if worldScenario != nil {
		if _, err := WritePublishedFirstInstallRuntimeFixture(worldScenario.World.RunDir, "first-install-installed-runtime-fixture", fixtureManifest, DiskQCOW2); err != nil {
			t.Fatalf("publish first-install runtime fixture: %v", err)
		}
	}

	t.Setenv("KATL_INSTALLED_FIXTURE_MANIFEST", fixtureManifest)
	runtimeResult, err := runner.Plan(Scenario{Name: "first-install-packaged-runtime-agent"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	runtimeResult.start(runner.time())
	runtimeResult = RunInstalledRuntime(ctx, runtimeResult, InstalledRuntimeConfig{
		Disk:               packagedDisk,
		DiskFormat:         DiskQCOW2,
		ESPArtifacts:       packagedESP,
		RequireVMTestAgent: true,
		VM: VMConfig{
			KVM:     runner.options().KVM,
			RAMMiB:  4096,
			CPUs:    2,
			Timeout: 8 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
			Agent: AgentControlConfig{
				RequireHealth: true,
				Timeout:       30 * time.Second,
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "first-install-packaged-runtime-agent"}, runtimeResult); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if runtimeResult.Status != StatusPassed {
		t.Fatalf("packaged runtime status = %q, failure = %q, run dir = %s", runtimeResult.Status, runtimeResult.FailureSummary, runtimeResult.RunDir)
	}
	transcript, err := os.ReadFile(runtimeResult.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("read vsock transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"method":"Health"`) || !strings.Contains(string(transcript), `"status":"ok"`) {
		t.Fatalf("vsock transcript did not record successful health: %s", transcript)
	}

	readyResult, err := runner.Plan(Scenario{Name: "first-install-packaged-runtime-ready"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	readyResult.start(runner.time())
	readyResult = RunInstalledKubeadmReadySmoke(ctx, readyResult, KubeadmReadySmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         packagedDisk,
			DiskFormat:   DiskQCOW2,
			ESPArtifacts: packagedESP,
			VM: VMConfig{
				KVM:     runner.options().KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 8 * time.Minute,
				VSock: VSockConfig{
					Enabled: true,
				},
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "first-install-packaged-runtime-ready"}, readyResult); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if readyResult.Status != StatusPassed {
		t.Fatalf("packaged runtime ready status = %q, failure = %q, run dir = %s", readyResult.Status, readyResult.FailureSummary, readyResult.RunDir)
	}
}

func TestFirstInstallTargetDiskSerialSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install serial smoke")
	}
	options.Missing = MissingSkips
	options.Keep = KeepAlways
	useInstalledESP := envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")
	var runner Runner
	var installerBoot InstallerBootConfig
	var runtimeArtifact, runtimeESP, manifestPath string
	targetDisk := TargetDisk("root", string(DiskQCOW2), first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "20G"))
	if worldRun, ok := firstInstallWorldRunFor(t, "first-install-serial-runtime", NodeSpec{Name: "cp-1", Role: ControlPlane}, useInstalledESP); ok {
		runner = worldRun.Runner
		installerBoot = worldRun.Config.Installer
		runtimeArtifact = worldRun.Config.Installer.RuntimeArtifact
		runtimeESP = worldRun.Config.Runtime.ESPArtifacts
		manifestPath = worldRun.Config.ManifestPath
		targetDisk = worldRun.Config.TargetDisk
	} else {
		installerBoot = firstInstallInstallerBoot(t)
		runtimeArtifact = RequireEnv(t, "KATL_RUNTIME_ARTIFACT")
		runtimeESP = first(os.Getenv("KATL_RUNTIME_ESP_ARTIFACTS"), os.Getenv("KATL_INSTALLED_ESP_ARTIFACTS"))
		if runtimeESP == "" && !useInstalledESP {
			t.Skip("set KATL_RUNTIME_ESP_ARTIFACTS or KATL_INSTALLED_ESP_ARTIFACTS to run first-install serial smoke")
		}
		manifestPath = RequireEnv(t, "KATL_INSTALL_MANIFEST")
		runner = NewRunner(options)
	}
	var requiredTools []string
	if useInstalledESP {
		requiredTools = append(requiredTools, "sfdisk", "mcopy")
	}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to run first-install serial smoke: %v", tool, err)
		}
	}

	runner.RequireHost(t, HostRequirements{
		QEMU:    true,
		QEMUImg: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
		MTools:  true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	result, err := RunFirstInstall(ctx, runner, Scenario{Name: "first-install-serial-runtime"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    installerBoot.InstallerUKI,
			InstallerKernel: installerBoot.InstallerKernel,
			InstallerInitrd: installerBoot.InstallerInitrd,
			CommandLine:     installerBoot.CommandLine,
			RuntimeArtifact: runtimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: runtimeESP,
			VM:           vm,
		},
		UseInstalledESP: useInstalledESP,
		ManifestPath:    manifestPath,
		PreseedManifest: true,
		TargetDisk:      targetDisk,
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("first install serial status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	serial, err := os.ReadFile(result.Artifacts.RuntimeSerial)
	if err != nil {
		t.Fatalf("read runtime serial: %v", err)
	}
	if !strings.Contains(string(serial), "Katl state projection ready") {
		t.Fatalf("runtime serial did not record state projection: %s", serial)
	}
	_ = targetDiskPath(t, result)
}

func TestFirstInstallTargetDiskLocalHandoffSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install local handoff smoke")
	}
	options.Missing = MissingSkips
	options.Keep = KeepAlways
	useInstalledESP := envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")
	var runner Runner
	var installerBoot InstallerBootConfig
	var runtimeArtifact, runtimeESP, manifestPath string
	targetDisk := TargetDisk("root", string(DiskQCOW2), first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "20G"))
	if worldRun, ok := firstInstallWorldRunForMode(t, "first-install-local-handoff-runtime", NodeSpec{Name: "cp-1", Role: ControlPlane}, useInstalledESP, firstInstallWorldGuestHandoff); ok {
		runner = worldRun.Runner
		installerBoot = worldRun.Config.Installer
		runtimeArtifact = worldRun.Config.Installer.RuntimeArtifact
		runtimeESP = worldRun.Config.Runtime.ESPArtifacts
		manifestPath = worldRun.Config.ManifestPath
		targetDisk = worldRun.Config.TargetDisk
	} else {
		installerBoot = firstInstallInstallerBoot(t)
		runtimeArtifact = RequireEnv(t, "KATL_RUNTIME_ARTIFACT")
		runtimeESP = first(os.Getenv("KATL_RUNTIME_ESP_ARTIFACTS"), os.Getenv("KATL_INSTALLED_ESP_ARTIFACTS"))
		if runtimeESP == "" && !useInstalledESP {
			t.Skip("set KATL_RUNTIME_ESP_ARTIFACTS or KATL_INSTALLED_ESP_ARTIFACTS to run first-install local handoff smoke")
		}
		manifestPath = RequireEnv(t, "KATL_INSTALL_MANIFEST")
		runner = NewRunner(options)
	}
	var requiredTools []string
	if useInstalledESP {
		requiredTools = append(requiredTools, "sfdisk", "mcopy")
	}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to run first-install local handoff smoke: %v", tool, err)
		}
	}

	runner.RequireHost(t, HostRequirements{
		QEMU:    true,
		QEMUImg: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	result, err := RunFirstInstall(ctx, runner, Scenario{Name: "first-install-local-handoff-runtime"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    installerBoot.InstallerUKI,
			InstallerKernel: installerBoot.InstallerKernel,
			InstallerInitrd: installerBoot.InstallerInitrd,
			CommandLine:     installerBoot.CommandLine,
			RuntimeArtifact: runtimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: runtimeESP,
			VM:           vm,
		},
		UseInstalledESP: useInstalledESP,
		ManifestPath:    manifestPath,
		GuestHandoff:    true,
		TargetDisk:      targetDisk,
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("first install local handoff status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	request := readLog(t, result.Artifacts.HandoffRequest)
	if request.PostURL == "" || request.Announcement == "" {
		t.Fatalf("handoff request missing guest announcement details: %#v", request)
	}
	response := readLog(t, result.Artifacts.HandoffResponse)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("handoff response = %#v", response)
	}
	serial, err := os.ReadFile(result.Artifacts.RuntimeSerial)
	if err != nil {
		t.Fatalf("read runtime serial: %v", err)
	}
	if !strings.Contains(string(serial), "Katl state projection ready") {
		t.Fatalf("runtime serial did not record state projection: %s", serial)
	}
	_ = targetDiskPath(t, result)
}

func firstInstallInstallerBoot(t *testing.T) InstallerBootConfig {
	t.Helper()
	boot := firstInstallInstallerBootFromEnv()
	if boot.InstallerKernel != "" || boot.InstallerInitrd != "" {
		if boot.InstallerKernel == "" || boot.InstallerInitrd == "" {
			t.Fatal("set both KATL_INSTALLER_KERNEL and KATL_INSTALLER_INITRD")
		}
		for name, path := range map[string]string{
			"KATL_INSTALLER_KERNEL": boot.InstallerKernel,
			"KATL_INSTALLER_INITRD": boot.InstallerInitrd,
		} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s is unavailable: %v", name, err)
			}
		}
		return boot
	}
	boot.InstallerUKI = RequireEnv(t, "KATL_INSTALLER_UKI")
	return boot
}

func createInstalledRuntimeFixtureCommand(ctx context.Context, repoRoot, disk, esp, format, stateDir, nodeMetadata string) *exec.Cmd {
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

func resolveInstalledRuntimeFixtureCommand(ctx context.Context, repoRoot, disk, esp, fixture, format, stateDir, nodeMetadata string) *exec.Cmd {
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

func TestFirstInstallFixtureCommandsCarryNodeMetadata(t *testing.T) {
	create := createInstalledRuntimeFixtureCommand(context.Background(), "/repo", "target.qcow2", "esp", "qcow2", "fixture", "node.json")
	if !hasArgPair(create.Args, "--node-metadata", "node.json") {
		t.Fatalf("create args missing node metadata: %#v", create.Args)
	}
	resolve := resolveInstalledRuntimeFixtureCommand(context.Background(), "/repo", "fixture/installed-runtime.qcow2", "fixture/esp", "fixture/installed-runtime-fixture.json", "qcow2", "fixture/recheck", packagedNodeMetadata("fixture", "node.json"))
	if !hasArgPair(resolve.Args, "--node-metadata", filepath.Join("fixture", "node.json")) {
		t.Fatalf("resolve args missing packaged node metadata: %#v", resolve.Args)
	}
	createWithoutMetadata := createInstalledRuntimeFixtureCommand(context.Background(), "/repo", "target.qcow2", "esp", "qcow2", "fixture", "")
	if hasSmokeArg(createWithoutMetadata.Args, "--node-metadata") {
		t.Fatalf("create args unexpectedly include node metadata: %#v", createWithoutMetadata.Args)
	}
	resolveWithoutMetadata := resolveInstalledRuntimeFixtureCommand(context.Background(), "/repo", "disk", "esp", "fixture.json", "qcow2", "recheck", "")
	if hasSmokeArg(resolveWithoutMetadata.Args, "--node-metadata") {
		t.Fatalf("resolve args unexpectedly include node metadata: %#v", resolveWithoutMetadata.Args)
	}
}

func hasArgPair(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasSmokeArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func TestInstalledRuntimeVMTestAgentSmoke(t *testing.T) {
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-vmtest-agent", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		worldRun.Runner.RequireHost(t, HostRequirements{
			QEMU: true,
			OVMF: true,
			KVM:  worldRun.Runner.options().KVM,
		})
		result, err := worldRun.Runner.Plan(Scenario{Name: "installed-runtime-vmtest-agent"})
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		config := worldRun.Config
		config.RequireVMTestAgent = true
		config.VM = VMConfig{
			KVM:     worldRun.Runner.options().KVM,
			Timeout: 3 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
			Agent: AgentControlConfig{
				RequireHealth: true,
				Timeout:       20 * time.Second,
			},
		}
		result = RunInstalledRuntime(ctx, result, config, VMRunner{})
		if err := worldRun.Runner.Write(Scenario{Name: "installed-runtime-vmtest-agent"}, result); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		requireInstalledRuntimeAgentHealth(t, result)
		return
	}
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime vmtest agent smoke")
	}
	options.Missing = MissingSkips
	disk, esp := requireInstalledRuntimeFixture(t, options, "installed-runtime-vmtest-agent")

	runner := NewRunner(options)
	runner.RequireHost(t, HostRequirements{
		QEMU: true,
		OVMF: true,
		KVM:  options.KVM,
	})
	result, err := runner.Plan(Scenario{
		Name: "installed-runtime-vmtest-agent",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result = RunInstalledRuntime(ctx, result, InstalledRuntimeConfig{
		Disk:               disk,
		DiskFormat:         DiskFormat(first(os.Getenv("KATL_INSTALLED_DISK_FORMAT"), string(DiskRaw))),
		ESPArtifacts:       esp,
		RequireVMTestAgent: true,
		VM: VMConfig{
			KVM:     options.KVM,
			Timeout: 3 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
			Agent: AgentControlConfig{
				RequireHealth: true,
				Timeout:       20 * time.Second,
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "installed-runtime-vmtest-agent"}, result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	requireInstalledRuntimeAgentHealth(t, result)
}

func TestInstalledRuntimeKubeadmReadySmoke(t *testing.T) {
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-kubeadm-ready", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		worldRun.Runner.RequireHost(t, HostRequirements{
			QEMU: true,
			OVMF: true,
			KVM:  worldRun.Runner.options().KVM,
		})
		result, err := worldRun.Runner.Plan(Scenario{Name: "installed-runtime-kubeadm-ready"})
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		config := worldRun.Config
		config.VM = VMConfig{
			KVM:     worldRun.Runner.options().KVM,
			RAMMiB:  4096,
			CPUs:    2,
			Timeout: 5 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
		}
		result = RunInstalledKubeadmReadySmoke(ctx, result, KubeadmReadySmokeConfig{
			Runtime: config,
		}, VMRunner{})
		if err := worldRun.Runner.Write(Scenario{Name: "installed-runtime-kubeadm-ready"}, result); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		requireInstalledRuntimeKubeadmReadyTranscript(t, result)
		return
	}
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime kubeadm-ready smoke")
	}
	options.Missing = MissingSkips
	disk, esp := requireInstalledRuntimeFixture(t, options, "installed-runtime-kubeadm-ready")

	runner := NewRunner(options)
	runner.RequireHost(t, HostRequirements{
		QEMU: true,
		OVMF: true,
		KVM:  options.KVM,
	})
	result, err := runner.Plan(Scenario{
		Name: "installed-runtime-kubeadm-ready",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	result = RunInstalledKubeadmReadySmoke(ctx, result, KubeadmReadySmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         disk,
			DiskFormat:   DiskFormat(first(os.Getenv("KATL_INSTALLED_DISK_FORMAT"), string(DiskRaw))),
			ESPArtifacts: esp,
			VM: VMConfig{
				KVM:     options.KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 5 * time.Minute,
				VSock: VSockConfig{
					Enabled: true,
				},
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "installed-runtime-kubeadm-ready"}, result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	requireInstalledRuntimeKubeadmReadyTranscript(t, result)
}

func TestInstalledRuntimeKubeadmAPISmoke(t *testing.T) {
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-kubeadm-api-smoke", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		worldRun.Runner.RequireHost(t, HostRequirements{
			QEMU: true,
			OVMF: true,
			KVM:  worldRun.Runner.options().KVM,
		})
		result, err := worldRun.Runner.Plan(Scenario{Name: "installed-runtime-kubeadm-api-smoke"})
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		config := worldRun.Config
		config.VM = VMConfig{
			KVM:     worldRun.Runner.options().KVM,
			RAMMiB:  4096,
			CPUs:    2,
			Timeout: 18 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
		}
		result = RunInstalledKubeadmAPISmoke(ctx, result, KubeadmAPISmokeConfig{
			Runtime: config,
		}, VMRunner{})
		if err := worldRun.Runner.Write(Scenario{Name: "installed-runtime-kubeadm-api-smoke"}, result); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if result.Status != StatusPassed {
			t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
		}
		return
	}
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime kubeadm API smoke")
	}
	options.Missing = MissingSkips
	disk, esp := requireInstalledRuntimeFixture(t, options, "installed-runtime-kubeadm-api-smoke")

	runner := NewRunner(options)
	runner.RequireHost(t, HostRequirements{
		QEMU: true,
		OVMF: true,
		KVM:  options.KVM,
	})
	result, err := runner.Plan(Scenario{
		Name: "installed-runtime-kubeadm-api-smoke",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result = RunInstalledKubeadmAPISmoke(ctx, result, KubeadmAPISmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         disk,
			DiskFormat:   DiskFormat(first(os.Getenv("KATL_INSTALLED_DISK_FORMAT"), string(DiskRaw))),
			ESPArtifacts: esp,
			VM: VMConfig{
				KVM:     options.KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 18 * time.Minute,
				VSock: VSockConfig{
					Enabled: true,
				},
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "installed-runtime-kubeadm-api-smoke"}, result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
}

func requireInstalledRuntimeAgentHealth(t *testing.T, result Result) {
	t.Helper()
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	transcript, err := os.ReadFile(result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("read vsock transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"method":"Health"`) || !strings.Contains(string(transcript), `"status":"ok"`) {
		t.Fatalf("vsock transcript did not record successful health: %s", transcript)
	}
}

func requireInstalledRuntimeKubeadmReadyTranscript(t *testing.T, result Result) {
	t.Helper()
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q, run dir = %s", result.Status, result.FailureSummary, result.RunDir)
	}
	transcript, err := os.ReadFile(result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("read vsock transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"method":"RunCommand"`) || !strings.Contains(string(transcript), "katl-kubeadm-ready.target") {
		t.Fatalf("vsock transcript did not record kubeadm-ready checks: %s", transcript)
	}
}

func requireInstalledRuntimeFixture(t *testing.T, options Options, scenarioName string) (string, string) {
	t.Helper()
	disk := os.Getenv("KATL_INSTALLED_DISK")
	esp := os.Getenv("KATL_INSTALLED_ESP_ARTIFACTS")
	if disk != "" && esp != "" {
		return disk, esp
	}
	var missing []string
	if disk == "" {
		missing = append(missing, "KATL_INSTALLED_DISK")
	}
	if esp == "" {
		missing = append(missing, "KATL_INSTALLED_ESP_ARTIFACTS")
	}
	message := fmt.Sprintf("set %s or run scripts/resolve-installed-runtime-fixture", strings.Join(missing, " and "))
	runner := NewRunner(options)
	result, err := runner.Plan(Scenario{Name: scenarioName})
	if err == nil {
		now := runner.time()
		result.start(now)
		result.finish(StatusSkipped, message, now)
		result.Missing = append(result.Missing, MissingPrerequisite{
			Name:   strings.Join(missing, ","),
			Detail: message,
		})
		_ = runner.Write(Scenario{Name: scenarioName}, result)
	}
	t.Skip(message)
	return "", ""
}

func targetDiskPath(t *testing.T, result Result) string {
	t.Helper()
	for _, disk := range result.Disks {
		if disk.Kind == DiskTarget {
			if _, err := os.Stat(disk.HostPath); err != nil {
				t.Fatalf("target disk %s is not available after first install: %v", disk.HostPath, err)
			}
			return disk.HostPath
		}
	}
	t.Fatalf("first install result has no target disk: %#v", result.Disks)
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(output))
}
