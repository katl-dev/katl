package vmtest

import (
	"context"
	"encoding/xml"
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
	t.Run("post-lease", func(t *testing.T) {
		testInstalledRuntimeCarrierRecovery(t, "installed-runtime-carrier-recovery", false)
	})
	t.Run("pre-lease", func(t *testing.T) {
		testInstalledRuntimeCarrierRecovery(t, "installed-runtime-carrier-recovery-pre-lease", true)
	})
}

func testInstalledRuntimeCarrierRecovery(t *testing.T, scenario string, preLease bool) {
	t.Helper()
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
	plan := node.handle.Plan
	virsh := LibvirtVMExecutor{VirshPath: plan.VirshPath, URI: plan.LibvirtURI}
	var restoreDHCP func(context.Context)
	if preLease {
		if !run.Scenario.owned || plan.LibvirtNetwork != run.Scenario.World.Network.Name {
			t.Fatal("pre-lease carrier recovery requires a scenario-owned network")
		}
		// The agent can become ready after networkd has already received a
		// lease. Pause DHCP before reboot so link-up can be observed without racing it.
		restoreDHCP = pauseScenarioDHCP(t, ctx, virsh, run.Scenario.ownedNetwork())
	}
	cursor := len(node.handle.notifyEvents())

	rebootCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
	defer stop()
	runKatlctl(t, rebootCtx, result, katlctl, "reboot", "node", "reboot", node.Name, "--no-wait")
	client, bootID, err := discoverGuestBoot(rebootCtx, &node, previousBoot, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	link, err := guestInterfaceForMAC(rebootCtx, guest, plan.MACAddress)
	if err != nil {
		t.Fatal(err)
	}
	prefix := netip.PrefixFrom(reserved, 32)
	outagePrefix := prefix
	if preLease {
		outagePrefix = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	}
	if err := waitCarrierAddress(rebootCtx, client, link, outagePrefix, "1", !preLease); err != nil {
		t.Fatalf("new boot %s did not bring up %s: %v", bootID, link, err)
	}
	// Host-side setlink tests recovery from a carrier loss that clears.
	// In #529 the guest lost carrier with host carrier up and never regained it.
	if err := virsh.virsh(rebootCtx, "domif-setlink", plan.DomainName, plan.MACAddress, "down"); err != nil {
		t.Fatal(err)
	}
	// A cached libvirt lease may survive the outage. Require the guest to
	// actually lose the address before giving it carrier again.
	if err := waitCarrierAddress(rebootCtx, client, link, outagePrefix, "0", false); err != nil {
		t.Fatalf("guest did not observe carrier down without an address from %s on %s: %v", outagePrefix, link, err)
	}
	if preLease {
		hold := time.NewTimer(3 * time.Second)
		defer hold.Stop()
		select {
		case <-rebootCtx.Done():
			t.Fatal(rebootCtx.Err())
		case <-hold.C:
		}
		// Have the server ready before restoring carrier so the first lease
		// depends on carrier recovery, with no guest-side reconfigure.
		restoreDHCP(rebootCtx)
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
	if got := guestBootID(t, rebootCtx, client); got != bootID {
		t.Fatalf("carrier recovery changed boot ID from %s to %s", bootID, got)
	}
	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func pauseScenarioDHCP(t *testing.T, ctx context.Context, virsh LibvirtVMExecutor, network OwnedNetwork) func(context.Context) {
	t.Helper()
	var live struct {
		IPs []struct {
			Ranges []struct {
				XMLName xml.Name
				networkDHCPRange
			} `xml:"dhcp>range"`
			Hosts []networkDHCPHost `xml:"dhcp>host"`
		} `xml:"ip"`
	}
	if err := readLiveXML(ctx, virsh.virshOutput, &live, "net-dumpxml", network.Name); err != nil {
		t.Fatal(err)
	}
	if len(live.IPs) != 1 || len(live.IPs[0].Ranges) != 1 || len(live.IPs[0].Hosts) != 1 || len(network.Hosts) != 1 ||
		!strings.EqualFold(live.IPs[0].Hosts[0].MAC, network.Hosts[0].MACAddress) {
		t.Fatal("expected the scenario network to have one DHCP pool and the node's reservation")
	}
	pool, err := xml.Marshal(live.IPs[0].Ranges[0])
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := network.HostXML(network.Hosts[0])
	if err != nil {
		t.Fatal(err)
	}
	update := func(ctx context.Context, action, section, element string) {
		t.Helper()
		if err := virsh.virsh(ctx, "net-update", network.Name, action, section, element, "--parent-index", "0", "--live"); err != nil {
			t.Fatal(err)
		}
	}
	// Remove both allocations so dnsmasq cannot serve a dynamic or reserved
	// address. Range updates restart dnsmasq without replacing the bridge.
	update(ctx, "delete", "ip-dhcp-host", reservation)
	update(ctx, "delete", "ip-dhcp-range", string(pool))
	return func(ctx context.Context) {
		update(ctx, "add-last", "ip-dhcp-range", string(pool))
		update(ctx, "add-last", "ip-dhcp-host", reservation)
	}
}

func waitCarrierAddress(ctx context.Context, client GuestAgentClient, link string, reserved netip.Prefix, wantCarrier string, wantAddress bool) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		carrier, err := readGuestFileWithCarrierState(ctx, client, &vmtestpb.ReadFileRequest{
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
