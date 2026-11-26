package vmtest

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestInstalledRuntimeCarrierRecovery(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	const scenario = "installed-runtime-carrier-recovery"
	run, ok := installedRuntimeWorldRunFor(t, scenario, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if !ok {
		t.Fatal("run this test with a vmtest world")
	}
	reserved, err := netip.ParseAddr(run.Node.Address)
	if err != nil || !reserved.Is4() || !run.Node.Reserved {
		t.Fatalf("need a reserved IPv4 address for %s, got %q: %v", run.Node.Name, run.Node.Address, err)
	}
	result, err := run.Runner.Plan(Scenario{Name: scenario})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)

	run.Config.VM.Timeout = 10 * time.Minute
	run.Config.VM.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: run.Node.Name, Runtime: run.Config,
	}, VMRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("carrier recovery failed")
			return
		}
		_ = node.Stop()
	}()

	management := inventory.ManagementAddress{Host: inventory.HostFromAddr(reserved)}
	endpoint := management.Endpoint().AddrPort()
	enrollConfigApplyNode(t, ctx, result, katlctl, endpoint.String())
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	// Retain the old boot identity to prove the interruption hits the rebooted guest.
	defer client.Close()
	previousBoot := guestBootID(t, ctx, client)
	cursor := len(node.handle.notifyEvents())

	rebootCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
	defer stop()
	runKatlctl(t, rebootCtx, result, katlctl, "reboot", "node", "reboot", node.Name, "--no-wait")
	client, bootID, err := discoverGuestBoot(rebootCtx, &node, previousBoot, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan := node.handle.Plan
	guest := NewGuestControl(node.Result, client)
	link, err := guestInterfaceForMAC(rebootCtx, guest, plan.MACAddress)
	if err != nil {
		t.Fatal(err)
	}
	virsh := LibvirtVMExecutor{VirshPath: plan.VirshPath, URI: plan.LibvirtURI}
	prefix := netip.PrefixFrom(reserved, 32)
	if err := waitCarrierAddress(rebootCtx, client, link, prefix, "1", true); err != nil {
		t.Fatalf("new boot %s did not bring up %s: %v", bootID, link, err)
	}
	if err := virsh.virsh(rebootCtx, "domif-setlink", plan.DomainName, plan.MACAddress, "down"); err != nil {
		t.Fatal(err)
	}
	// A cached libvirt lease may survive the outage. Require the guest to
	// actually lose the address before giving it carrier again.
	if err := waitCarrierAddress(rebootCtx, client, link, prefix, "0", false); err != nil {
		t.Fatalf("carrier interruption did not remove %s from %s: %v", reserved, link, err)
	}
	if err := virsh.virsh(rebootCtx, "domif-setlink", plan.DomainName, plan.MACAddress, "up"); err != nil {
		t.Fatal(err)
	}
	if err := waitCarrierAddress(rebootCtx, client, link, prefix, "1", true); err != nil {
		t.Fatalf("guest did not reacquire %s on %s after restoring carrier: %v", reserved, link, err)
	}
	if err := WaitManagementEndpoint(rebootCtx, node.Name, endpoint, managementStartupTimeout); err != nil {
		t.Fatal(err)
	}
	runKatlctl(t, rebootCtx, result, katlctl, "carrier-restored-status", "node", "status", node.Name, "--output", "json")
	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func waitCarrierAddress(ctx context.Context, client *AgentClient, link string, reserved netip.Prefix, wantCarrier string, wantAddress bool) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		carrier, err := client.ReadFile(ctx, &vmtestpb.ReadFileRequest{
			Path: filepath.Join("/sys/class/net", link, "carrier"), MaxBytes: 128,
		})
		if err != nil {
			return err
		}
		addresses, err := client.RunCommand(ctx, &vmtestpb.RunCommandRequest{
			Argv: []string{"ip", "-4", "-o", "address", "show", "dev", link, "to", reserved.String()},
		})
		if err != nil {
			return err
		}
		if addresses.ExitStatus != 0 {
			return fmt.Errorf("ip address exited %d with %s", addresses.ExitStatus, addresses.Stderr)
		}
		gotCarrier := strings.TrimSpace(string(carrier.Content))
		hasAddress := len(addresses.Stdout) != 0
		if gotCarrier == wantCarrier && hasAddress == wantAddress {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("last observation carrier=%q, reserved address present=%t: %w", gotCarrier, hasAddress, ctx.Err())
		case <-ticker.C:
		}
	}
}
