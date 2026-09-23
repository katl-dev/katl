package vmtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstalledRuntimeManagementRotationSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run management rotation smoke")
	}
	sshDir := t.TempDir()
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("generate SSH fixture key: %v: %s", err, output)
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	sshAuthorizedKey := strings.TrimSpace(string(publicKey))
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var plannedAddress, plannedMAC string
	var worldScenario *WorldScenario
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-management-rotation", NodeSpec{Name: "cp-1", Role: ControlPlane}, sshAuthorizedKey); ok {
		runner, runtime = worldRun.Runner, worldRun.Config
		plannedAddress, plannedMAC = worldRun.Node.Address, worldRun.Node.MACAddress
		worldScenario = worldRun.Scenario
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{Name: "installed-runtime-management-rotation"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatal(err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{Libvirt: true, OVMF: true, KVM: runner.options().KVM})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	katlctl := buildKatlctlForConfigApplySmoke(t, ctx)
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB, vm.CPUs = 2048, 2
	vm.Timeout = 10 * time.Minute
	vm.Network.MAC = first(vm.Network.MAC, plannedMAC)
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	vm.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk: runtime.Disk, DiskFormat: runtime.DiskFormat, ESPArtifacts: runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest, NodeMetadata: runtime.NodeMetadata, VM: vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("start installed node: %v", err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("management rotation smoke failed")
		} else {
			_ = node.Stop()
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	guest := NewGuestControl(node.Result, client)
	assertInstalledSSHReady(t, ctx, guest)
	endpoint := katlcEndpoint(t, node, plannedAddress)
	waitGuestFileContains(t, ctx, guest, "/var/lib/katl/install/status.json", `"finalHandoff": "waiting-for-cluster-bootstrap"`)
	enrollConfigApplyNode(t, ctx, result, katlctl, endpoint, sshAuthorizedKey)
	configPath := filepath.Join(result.RunDir, "katlctl", "enrollment", "cluster.yaml")
	runKatlctl(t, ctx, result, katlctl, "rotation-before", "node", "status", "cp-1")

	hostKey := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "rotation-host-key", "dd", "if=/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key.pub", "status=none"))
	if !strings.HasPrefix(hostKey, "ssh-ed25519 ") {
		t.Fatalf("unexpected installed SSH host key: %q", hostKey)
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(sshDir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("%s %s\n", host, hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownHosts := filepath.Join(sshDir, "unknown_hosts")
	if err := os.WriteFile(unknownHosts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(result.RunDir, "katlctl", "enrollment", "management-next.yaml")
	args := []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath, "--ssh-key", keyPath, "--ssh-known-hosts"}
	if _, err := runKatlctlOutcome(t, ctx, result, katlctl, "rotation-unknown-host", append(args, unknownHosts)...); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("unknown SSH host key was not rejected: %v", err)
	}
	runKatlctl(t, ctx, result, katlctl, "rotation-before-after-preflight", "node", "status", "cp-1")
	runKatlctl(t, ctx, result, katlctl, "rotation-switch", append(args, knownHosts)...)
	if _, err := runKatlctlOutcome(t, ctx, result, katlctl, "rotation-old-client", "node", "status", "cp-1"); err == nil {
		t.Fatal("old saved management credentials still have access")
	}
	runKatlctl(t, ctx, result, katlctl, "rotation-context-refresh", "context", "save", "--config", configPath)
	runKatlctl(t, ctx, result, katlctl, "rotation-new-client", "node", "status", "cp-1")
	runKatlctl(t, ctx, result, katlctl, "rotation-repeat", append(args, knownHosts)...)
	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	runKatlctl(t, ctx, result, katlctl, "rotation-after-reboot", "node", "status", "cp-1")
	guestCommand(t, ctx, guest, "rotation-agent-active", "systemctl", "is-active", "--quiet", "katlc-agent.service")

	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatal(err)
	}
	if worldScenario != nil {
		if err := worldScenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatal(err)
		}
	}
}
