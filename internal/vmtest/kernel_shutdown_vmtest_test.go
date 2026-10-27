package vmtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestInstalledRuntimePowerButton(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to test native ACPI poweroff")
	}
	worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-power-button", NodeSpec{Name: "power-button", Role: Worker})
	if !ok {
		_ = RequireWorld(t)
	}
	result, err := worldRun.Runner.Plan(Scenario{Name: "installed-runtime-power-button"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	runtime := worldRun.Config
	runtime.VM.KVM = options.KVM
	runtime.VM.RAMMiB = 2048
	runtime.VM.CPUs = 2
	runtime.VM.Timeout = 5 * time.Minute
	runtime.VM.Agent.Timeout = 30 * time.Second
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{Name: "power-button", Runtime: runtime}, VMRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("native ACPI poweroff failed")
			_ = worldRun.Scenario.WriteResult(WorldStatusFailed, "native ACPI poweroff failed")
			return
		}
		_ = node.Stop()
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	assertInstalledRuntimeReady(t, ctx, guest, "power-button")
	guestCommand(t, ctx, guest, "native-device-database", "udevadm", "info", "--export-db")
	guestCommand(t, ctx, guest, "logind-journal", "journalctl", "-b", "-u", "systemd-logind", "--no-pager")

	plan := node.handle.Plan
	command := exec.CommandContext(ctx, plan.VirshPath, "--connect", plan.LibvirtURI, "shutdown", plan.DomainName, "--mode", "acpi")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("press ACPI power button: %v: %s", err, output)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, time.Minute)
	defer shutdownCancel()
	if err := node.WaitForPoweroff(shutdownCtx); err != nil {
		t.Fatalf("ACPI power button did not power off the runtime: %v", err)
	}
	serial, err := os.ReadFile(plan.SerialLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(serial), "reboot: Power down") {
		t.Fatal("runtime exited without the kernel poweroff marker")
	}
	if err := worldRun.Scenario.WriteResult(WorldStatusPassed, "native ACPI poweroff completed"); err != nil {
		t.Fatal(err)
	}
}
