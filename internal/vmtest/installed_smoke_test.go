package vmtest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInstalledRuntimeVMTestAgentSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime vmtest agent smoke")
	}
	disk := RequireEnv(t, "KATL_INSTALLED_DISK")
	esp := RequireEnv(t, "KATL_INSTALLED_ESP_ARTIFACTS")

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
