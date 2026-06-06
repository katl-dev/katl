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
	installerUKI := RequireEnv(t, "KATL_INSTALLER_UKI")
	runtimeArtifact := RequireEnv(t, "KATL_RUNTIME_ARTIFACT")
	runtimeESP := first(os.Getenv("KATL_RUNTIME_ESP_ARTIFACTS"), os.Getenv("KATL_INSTALLED_ESP_ARTIFACTS"))
	if runtimeESP == "" {
		t.Skip("set KATL_RUNTIME_ESP_ARTIFACTS or KATL_INSTALLED_ESP_ARTIFACTS to run first-install fixture smoke")
	}
	manifestPath := RequireEnv(t, "KATL_INSTALL_MANIFEST")
	repoRoot := repoRoot(t)
	for _, tool := range []string{"jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required to package installed runtime fixtures: %v", tool, err)
		}
	}

	runner := NewRunner(options)
	runner.RequireHost(t, HostRequirements{
		QEMU:    true,
		QEMUImg: true,
		OVMF:    true,
		KVM:     options.KVM,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	vm := VMConfig{
		KVM:     options.KVM,
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
			InstallerUKI:    installerUKI,
			RuntimeArtifact: runtimeArtifact,
			VM:              vm,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts:       runtimeESP,
			RequireVMTestAgent: true,
			VM:                 vm,
		},
		ManifestPath: manifestPath,
		GuestHandoff: true,
		TargetDisk:   TargetDisk("root", string(DiskQCOW2), first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "20G")),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if firstResult.Status != StatusPassed {
		t.Fatalf("first install status = %q, failure = %q, run dir = %s", firstResult.Status, firstResult.FailureSummary, firstResult.RunDir)
	}
	targetDisk := targetDiskPath(t, firstResult)
	fixtureDir := filepath.Join(firstResult.ManifestDir, "installed-runtime-fixture")
	createFixture := exec.CommandContext(ctx, filepath.Join(repoRoot, "scripts", "create-installed-runtime-fixture"),
		"--disk", targetDisk,
		"--esp-artifacts", runtimeESP,
		"--format", string(DiskQCOW2),
		"--state-dir", fixtureDir,
	)
	output, err := createFixture.CombinedOutput()
	if err != nil {
		t.Fatalf("create installed runtime fixture failed: %v\n%s", err, output)
	}

	fixtureManifest := filepath.Join(fixtureDir, "installed-runtime-fixture.json")
	packagedDisk := filepath.Join(fixtureDir, "installed-runtime.qcow2")
	packagedESP := filepath.Join(fixtureDir, "esp")
	checkFixture := exec.CommandContext(ctx, filepath.Join(repoRoot, "scripts", "resolve-installed-runtime-fixture"),
		"--disk", packagedDisk,
		"--esp-artifacts", packagedESP,
		"--fixture", fixtureManifest,
		"--format", string(DiskQCOW2),
		"--state-dir", filepath.Join(fixtureDir, "recheck"),
		"--check-only",
	)
	output, err = checkFixture.CombinedOutput()
	if err != nil {
		t.Fatalf("check installed runtime fixture failed: %v\n%s", err, output)
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
			KVM:     options.KVM,
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
				KVM:     options.KVM,
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

func TestInstalledRuntimeVMTestAgentSmoke(t *testing.T) {
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
	transcript, err := os.ReadFile(result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("read vsock transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"method":"Health"`) || !strings.Contains(string(transcript), `"status":"ok"`) {
		t.Fatalf("vsock transcript did not record successful health: %s", transcript)
	}
}

func TestInstalledRuntimeKubeadmAPISmoke(t *testing.T) {
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
