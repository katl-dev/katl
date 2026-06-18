package vmtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type firstInstallFixtureContractRun = FirstInstallRuntimeFixtureContract
type producedInstalledRuntimeFixture = ProducedInstalledRuntimeFixture

func TestFirstInstallTargetDiskFixtureContract(t *testing.T) {
	contract := firstInstallFixtureContractRunFor(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	fixture := produceFirstInstallRuntimeFixture(t, contract)

	runner := contract.Runner
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	runtimeResult, err := runner.Plan(Scenario{Name: "first-install-packaged-runtime-agent"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	runtimeResult.start(runner.time())
	runtimeResult = RunInstalledRuntime(ctx, runtimeResult, InstalledRuntimeConfig{
		Disk:               fixture.Disk,
		DiskFormat:         DiskQCOW2,
		ESPArtifacts:       fixture.ESPArtifacts,
		FixtureManifest:    fixture.ManifestPath,
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

	handoffResult, err := runner.Plan(Scenario{Name: "first-install-packaged-runtime-waiting-for-bootstrap"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	handoffResult.start(runner.time())
	const waitingForBootstrapSignal = "katl-runtime-status state=waiting-for-cluster-bootstrap"
	handoffResult = RunInstalledRuntime(ctx, handoffResult, InstalledRuntimeConfig{
		Disk:               fixture.Disk,
		DiskFormat:         DiskQCOW2,
		ESPArtifacts:       fixture.ESPArtifacts,
		FixtureManifest:    fixture.ManifestPath,
		RequireVMTestAgent: true,
		Expect:             waitingForBootstrapSignal,
		VM: VMConfig{
			KVM:     runner.options().KVM,
			RAMMiB:  4096,
			CPUs:    2,
			Timeout: 8 * time.Minute,
			VSock: VSockConfig{
				Enabled: true,
			},
		},
	}, VMRunner{})
	if err := runner.Write(Scenario{Name: "first-install-packaged-runtime-waiting-for-bootstrap"}, handoffResult); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if handoffResult.Status != StatusPassed {
		t.Fatalf("packaged runtime handoff status = %q, failure = %q, run dir = %s", handoffResult.Status, handoffResult.FailureSummary, handoffResult.RunDir)
	}
	serial, err := os.ReadFile(handoffResult.Artifacts.RuntimeSerial)
	if err != nil {
		t.Fatalf("read runtime serial: %v", err)
	}
	if !strings.Contains(string(serial), waitingForBootstrapSignal) {
		t.Fatalf("runtime serial did not record waiting-for-bootstrap handoff: %s", serial)
	}
}

func firstInstallFixtureContractRunFor(t *testing.T, spec NodeSpec) firstInstallFixtureContractRun {
	t.Helper()
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install fixture smoke")
	}
	if strings.TrimSpace(os.Getenv(WorldManifestEnv)) != "" {
		world := RequireWorld(t)
		return firstInstallFixtureContractRunForWorld(t, world, repoRoot(t), spec)
	}
	_ = RequireWorld(t)
	return firstInstallFixtureContractRun{}
}

func firstInstallFixtureContractRunForWorld(t *testing.T, world World, repo string, spec NodeSpec) firstInstallFixtureContractRun {
	t.Helper()
	worldRun, err := planFirstInstallWorldRun(world, "first-install-installed-runtime-fixture", repo, spec, firstInstallWorldInput{
		Installer:       firstInstallInstallerBootFromEnv(),
		RuntimeArtifact: strings.TrimSpace(os.Getenv("KATL_RUNTIME_ARTIFACT")),
		InstallManifest: strings.TrimSpace(os.Getenv("KATL_INSTALL_MANIFEST")),
		UseInstalledESP: envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP"),
		TargetDiskSize:  first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "32G"),
	}, DefaultOptions().KVM)
	if err != nil {
		failWorldSetup(t, worldRun.Scenario, err)
	}
	return firstInstallFixtureContractRun{
		Runner:          worldRun.Runner,
		WorldScenario:   worldRun.Scenario,
		WorldNode:       worldRun.Node,
		InstallerBoot:   worldRun.Config.Installer,
		RuntimeArtifact: worldRun.Config.Installer.RuntimeArtifact,
		RuntimeESP:      worldRun.Config.Runtime.ESPArtifacts,
		NodeMetadata:    worldRun.Config.Runtime.NodeMetadata,
		ManifestPath:    worldRun.Config.ManifestPath,
		Repo:            worldRun.Repo,
		TargetDisk:      worldRun.Config.TargetDisk,
		UseInstalledESP: worldRun.Config.UseInstalledESP,
		Node:            spec,
	}
}

func produceFirstInstallRuntimeFixture(t *testing.T, contract firstInstallFixtureContractRun) producedInstalledRuntimeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fixture, err := ProduceFirstInstallRuntimeFixture(ctx, contract)
	if err != nil {
		t.Fatalf("produce first-install runtime fixture: %v", err)
	}
	return fixture
}

func TestFirstInstallTargetDiskSerialSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install serial smoke")
	}
	useInstalledESP := envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")
	worldRun, ok := firstInstallWorldRunFor(t, "first-install-serial-runtime", NodeSpec{Name: "cp-1", Role: ControlPlane}, useInstalledESP)
	if !ok {
		_ = RequireWorld(t)
	}
	runner := worldRun.Runner
	scenario := withTarget(Scenario{Name: "first-install-serial-runtime"}, worldRun.Config.TargetDisk)
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt:   true,
		ImageTool: true,
		OVMF:      true,
		KVM:       runner.options().KVM,
		MTools:    true,
	})
	scenario.RunID = result.RunID
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	const cleanHandoffSignal = "katl-runtime-status state=waiting-for-cluster-bootstrap"
	const bootHealthSuccessSignal = "katl-boot-health generation=0 result=success"
	result, err = RunFirstInstall(ctx, runner, scenario, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    worldRun.Config.Installer.InstallerUKI,
			InstallerKernel: worldRun.Config.Installer.InstallerKernel,
			InstallerInitrd: worldRun.Config.Installer.InstallerInitrd,
			CommandLine:     worldRun.Config.Installer.CommandLine,
			RuntimeArtifact: worldRun.Config.Installer.RuntimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: worldRun.Config.Runtime.ESPArtifacts,
			Expect:       bootHealthSuccessSignal,
			VM:           vm,
		},
		UseInstalledESP: worldRun.Config.UseInstalledESP,
		ManifestPath:    worldRun.Config.ManifestPath,
		PreseedManifest: true,
		TargetDisk:      worldRun.Config.TargetDisk,
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
	if !strings.Contains(string(serial), runtimeBootSignal) {
		t.Fatalf("runtime serial did not record runtime boot signal: %s", serial)
	}
	if !strings.Contains(string(serial), cleanHandoffSignal) {
		t.Fatalf("runtime serial did not record clean generation 0 handoff: %s", serial)
	}
	if !strings.Contains(string(serial), bootHealthSuccessSignal) {
		t.Fatalf("runtime serial did not record boot-complete health promotion: %s", serial)
	}
	for _, refused := range []string{
		"generation 0 is not clean",
		"katl-boot-health:",
		"katl-boot-health.service: Failed",
		"katl-runtime-handoff-status.service: Failed",
		"katl-boot-complete.target/start failed",
	} {
		if strings.Contains(string(serial), refused) {
			t.Fatalf("runtime serial recorded failed generation 0 handoff %q: %s", refused, serial)
		}
	}
	_ = targetDiskPath(t, result)
}

func TestFirstInstallTargetDiskLocalHandoffSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run first-install local handoff smoke")
	}
	useInstalledESP := envBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")
	worldRun, ok := firstInstallWorldRunForMode(t, "first-install-local-handoff-runtime", NodeSpec{Name: "cp-1", Role: ControlPlane}, useInstalledESP, firstInstallWorldGuestHandoff)
	if !ok {
		_ = RequireWorld(t)
	}
	runner := worldRun.Runner
	scenario := withTarget(Scenario{Name: "first-install-local-handoff-runtime"}, worldRun.Config.TargetDisk)
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt:   true,
		ImageTool: true,
		OVMF:      true,
		KVM:       runner.options().KVM,
		MTools:    true,
	})
	scenario.RunID = result.RunID
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	vm := VMConfig{
		KVM:     runner.options().KVM,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	const cleanHandoffSignal = "katl-runtime-status state=waiting-for-cluster-bootstrap"
	const bootHealthSuccessSignal = "katl-boot-health generation=0 result=success"
	result, err = RunFirstInstall(ctx, runner, scenario, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    worldRun.Config.Installer.InstallerUKI,
			InstallerKernel: worldRun.Config.Installer.InstallerKernel,
			InstallerInitrd: worldRun.Config.Installer.InstallerInitrd,
			CommandLine:     worldRun.Config.Installer.CommandLine,
			RuntimeArtifact: worldRun.Config.Installer.RuntimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: worldRun.Config.Runtime.ESPArtifacts,
			Expect:       bootHealthSuccessSignal,
			VM:           vm,
		},
		UseInstalledESP: worldRun.Config.UseInstalledESP,
		ManifestPath:    worldRun.Config.ManifestPath,
		GuestHandoff:    true,
		TargetDisk:      worldRun.Config.TargetDisk,
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
	if !strings.Contains(string(serial), runtimeBootSignal) {
		t.Fatalf("runtime serial did not record runtime boot signal: %s", serial)
	}
	if !strings.Contains(string(serial), cleanHandoffSignal) {
		t.Fatalf("runtime serial did not record clean generation 0 handoff: %s", serial)
	}
	if !strings.Contains(string(serial), bootHealthSuccessSignal) {
		t.Fatalf("runtime serial did not record boot-complete health promotion: %s", serial)
	}
	for _, refused := range []string{
		"generation 0 is not clean",
		"katl-boot-health:",
		"katl-boot-health.service: Failed",
		"katl-runtime-handoff-status.service: Failed",
		"katl-boot-complete.target/start failed",
	} {
		if strings.Contains(string(serial), refused) {
			t.Fatalf("runtime serial recorded failed generation 0 handoff %q: %s", refused, serial)
		}
	}
	_ = targetDiskPath(t, result)
}

func TestDirectRuntimeVMTestAgentSmoke(t *testing.T) {
	if worldRun, ok := directRuntimeWorldRunFor(t, "direct-runtime-vmtest-agent"); ok {
		scenario := Scenario{Name: "direct-runtime-vmtest-agent"}
		result, err := worldRun.Runner.Plan(scenario)
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		result = requirePlannedVMHost(t, worldRun.Runner, scenario, result, HostRequirements{
			Libvirt: true,
			KVM:     worldRun.Runner.options().KVM,
		})
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
		result = RunDirectRuntime(ctx, result, config, VMRunner{})
		if err := worldRun.Runner.Write(scenario, result); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		requireInstalledRuntimeAgentHealth(t, result)
		return
	}
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run direct runtime vmtest agent smoke")
	}
	_ = RequireWorld(t)
}

func TestInstalledRuntimeKubeadmReadySmoke(t *testing.T) {
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-kubeadm-ready", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		scenario := Scenario{Name: "installed-runtime-kubeadm-ready"}
		result, err := worldRun.Runner.Plan(scenario)
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		result = requirePlannedVMHost(t, worldRun.Runner, scenario, result, HostRequirements{
			Libvirt: true,
			OVMF:    true,
			KVM:     worldRun.Runner.options().KVM,
		})
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
			Smoke: KubeadmReadySmokePlan{
				ReadyTimeout:      20 * time.Second,
				ReadyPollInterval: time.Second,
			},
		}, VMRunner{})
		if err := worldRun.Runner.Write(scenario, result); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		requireInstalledRuntimeKubeadmReadyNotTerminal(t, result)
		return
	}
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime kubeadm-ready smoke")
	}
	_ = RequireWorld(t)
}

func TestInstalledRuntimeKubeadmAPISmoke(t *testing.T) {
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-kubeadm-api-smoke", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		scenario := Scenario{Name: "installed-runtime-kubeadm-api-smoke"}
		result, err := worldRun.Runner.Plan(scenario)
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		result = requirePlannedVMHost(t, worldRun.Runner, scenario, result, HostRequirements{
			Libvirt: true,
			OVMF:    true,
			KVM:     worldRun.Runner.options().KVM,
		})
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
		if err := worldRun.Runner.Write(scenario, result); err != nil {
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
	_ = RequireWorld(t)
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
	if !strings.Contains(string(transcript), `"method":"RunCommand"`) || !strings.Contains(string(transcript), "katl-kubeadm-ready.target") || !strings.Contains(string(transcript), "/usr/bin/katlc") {
		t.Fatalf("vsock transcript did not record kubeadm-ready checks: %s", transcript)
	}
}

func requireInstalledRuntimeKubeadmReadyNotTerminal(t *testing.T, result Result) {
	t.Helper()
	if result.Status != StatusFailed || (!strings.Contains(result.FailureSummary, "katl-kubeadm-ready.target") && !strings.Contains(result.FailureSummary, "kubeadm-config")) {
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
