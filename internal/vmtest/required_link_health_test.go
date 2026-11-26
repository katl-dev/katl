package vmtest

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/retry"
)

func TestInstalledRuntimeRequiredLinkHealth(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	const scenario = "installed-runtime-required-link-health"
	run, ok := installedRuntimeWorldRunFor(t, scenario, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if !ok {
		t.Fatal("run this test with a vmtest world")
	}
	result, err := run.Runner.Plan(Scenario{Name: scenario})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)

	run.Config.VM.Timeout = 10 * time.Minute
	run.Config.VM.HotplugPCISlots = 1
	run.Config.VM.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: run.Node.Name, Runtime: run.Config,
	}, VMRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("required link health failed")
		} else if err := node.Stop(); err != nil && err != context.Canceled {
			t.Errorf("stop installed node: %v", err)
		}
	}()

	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	current := currentGenerationFromGuest(t, ctx, guest)
	waitGenerationPromotion(t, ctx, guest, current)
	enrollConfigApplyNode(t, ctx, result, katlctl, katlcEndpoint(t, node, run.Node.Address))

	// Keep the enrolled endpoint on the primary NIC. The second required
	// link has a static address without a gateway, so its outage is observable
	// through the public management interface.
	const mac = "52:54:00:08:23:01"
	const address = "192.0.2.10/32"
	const networkFile = "05-vmtest-required-link.network"
	const tempFile = "/var/lib/katl/test-artifacts/" + networkFile
	writeGuestFile(t, ctx, guest, tempFile, []byte(`[Match]
MACAddress=`+mac+`

[Link]
RequiredForOnline=routable

[Network]
Address=`+address+`
LinkLocalAddressing=no
`), 0o644)
	guestCommand(t, ctx, guest, "install-required-link-config", "install", "-D", "-m", "0644", tempFile, "/run/systemd/network/"+networkFile)
	guestCommand(t, ctx, guest, "load-required-link-config", "networkctl", "reload")

	plan := node.handle.Plan
	virsh := LibvirtVMExecutor{VirshPath: plan.VirshPath, URI: plan.LibvirtURI}
	if err := virsh.virsh(ctx, "attach-interface", plan.DomainName, "network", plan.LibvirtNetwork, "--mac", mac, "--model", "virtio", "--live"); err != nil {
		t.Fatal(err)
	}
	link := waitRequiredLinkCarrier(t, ctx, guest, mac, true)
	waitOnline := func() {
		t.Helper()
		guestCommand(t, ctx, guest, "required-link-online", "systemd-run", "--quiet", "--wait", "--collect", "--pipe",
			"/usr/lib/systemd/systemd-networkd-wait-online", "--interface="+link+":routable", "--timeout=30")
	}
	waitOnline()

	waitStatus := func(phase, want string) string {
		t.Helper()
		type nodeStatus struct {
			Health               string `json:"health"`
			Generation           string `json:"generation"`
			BootHealthDiagnostic string `json:"bootHealthDiagnostic"`
		}
		statusCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		backoff := retry.NewBackoff(250*time.Millisecond, time.Second)
		got, err := retry.Await(statusCtx, &backoff, func(ctx context.Context) (nodeStatus, bool, error) {
			output, err := runKatlctlOutcome(t, ctx, result, katlctl, phase, "node", "status", node.Name, "--output", "json")
			if err != nil {
				return nodeStatus{}, false, err
			}
			var got nodeStatus
			err = json.Unmarshal(output, &got)
			return got, got.Health == want, err
		}, nil)
		if err != nil || got.Generation != current {
			t.Fatalf("%s: node status = %+v, want health %s on %s: %v", phase, got, want, current, err)
		}
		if want == "unhealthy" && !containsAll(got.BootHealthDiagnostic, link, "Check the cable", "katlctl node status") {
			t.Fatalf("%s: diagnostic = %q, want required link %s and repair guidance", phase, got.BootHealthDiagnostic, link)
		}
		return got.BootHealthDiagnostic
	}
	waitStatus("carrier-up", "OK")

	bootID := guestBootID(t, ctx, client)
	const selectionPath = "/var/lib/katl/boot/selection.json"
	selection := readGuestFile(t, ctx, guest, selectionPath)
	if fs := guestCommandOutput(t, ctx, guest, "efivarfs-mounted",
		"findmnt", "-n", "-o", "FSTYPE", "--mountpoint", "/sys/firmware/efi/efivars"); strings.TrimSpace(fs) != "efivarfs" {
		t.Fatalf("EFI variables are not mounted as efivarfs: %q", fs)
	}
	efiTargets := func(phase string) string {
		t.Helper()
		return guestCommandOutput(t, ctx, guest, phase, "systemd-run", "--quiet", "--wait", "--collect", "--pipe",
			"/usr/bin/sh", "-ec", `
for name in LoaderEntryDefault LoaderEntryOneShot; do
    path="/sys/firmware/efi/efivars/${name}-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"
    printf '%s\n' "$name"
    if [ -e "$path" ]; then
        od -An -v -tx1 "$path"
    fi
done
bootctl --no-pager list --json=short`)
	}
	targets := efiTargets("efi-before-loss")
	assertBootUnchanged := func(phase string) {
		t.Helper()
		if got := guestBootID(t, ctx, client); got != bootID {
			t.Fatalf("%s: boot ID changed from %s to %s", phase, bootID, got)
		}
		if efiTargets(phase) != targets {
			t.Fatalf("%s: EFI targets changed", phase)
		}
		if readGuestFile(t, ctx, guest, selectionPath) != selection {
			t.Fatalf("%s: boot selection changed", phase)
		}
	}
	if err := virsh.virsh(ctx, "domif-setlink", plan.DomainName, mac, "down"); err != nil {
		t.Fatal(err)
	}
	waitRequiredLinkCarrier(t, ctx, guest, mac, false)
	waitStatus("carrier-lost", "unhealthy")
	assertBootUnchanged("efi-during-loss")

	if err := virsh.virsh(ctx, "domif-setlink", plan.DomainName, mac, "up"); err != nil {
		t.Fatal(err)
	}
	waitOnline()
	waitStatus("carrier-restored", "OK")
	assertBootUnchanged("efi-after-recovery")

	guestCommand(t, ctx, guest, "remove-required-link-address", "ip", "address", "del", address, "dev", link)
	waitRequiredLinkCarrier(t, ctx, guest, mac, true)
	if diagnostic := waitStatus("address-lost", "unhealthy"); !strings.Contains(diagnostic, "required links have no usable address: "+link) {
		t.Fatalf("address loss diagnostic = %q, want missing address on %s", diagnostic, link)
	}
	assertBootUnchanged("efi-during-address-loss")

	guestCommand(t, ctx, guest, "restore-required-link-address", "networkctl", "reconfigure", link)
	waitOnline()
	waitStatus("address-restored", "OK")
	assertBootUnchanged("efi-after-address-recovery")

	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func waitRequiredLinkCarrier(t *testing.T, ctx context.Context, guest *GuestControl, mac string, up bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	backoff := retry.NewBackoff(250*time.Millisecond, time.Second)
	link, err := retry.Await(ctx, &backoff, func(ctx context.Context) (string, bool, error) {
		data := guestCommandOutput(t, ctx, guest, "required-link-carrier", "ip", "-j", "link", "show")
		var links []struct {
			Name    string   `json:"ifname"`
			Address string   `json:"address"`
			Flags   []string `json:"flags"`
		}
		if err := json.Unmarshal([]byte(data), &links); err != nil {
			return "", false, err
		}
		for _, link := range links {
			if strings.EqualFold(link.Address, mac) {
				return link.Name, slices.Contains(link.Flags, "LOWER_UP") == up, nil
			}
		}
		return "", false, nil
	}, nil)
	if err != nil {
		t.Fatalf("wait for interface %s (%s) carrier up=%t: %v", link, mac, up, err)
	}
	return link
}
