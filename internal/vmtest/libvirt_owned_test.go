package vmtest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const katldevMetadata = `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-world/dev-world-1</vmtest>`

func vmtestMetadataFor(lease string) string {
	if lease == "" {
		return `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/vmtest</vmtest>`
	}
	return `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1" lease="` + lease + `" run="r">katl/vmtest</vmtest>`
}

func TestSweepReapsKilledRun(t *testing.T) {
	fake := newFakeLibvirt(t)
	leases := t.TempDir()
	live, killed := filepath.Join(leases, "live", "lease"), filepath.Join(leases, "killed", "lease")
	for _, path := range []string{live, killed} {
		mkdirForFile(t, path)
	}
	startLeaseHolder(t, live)
	holder := startLeaseHolder(t, killed)

	for _, run := range []struct{ name, lease string }{{"live", live}, {"killed", killed}} {
		fake.addDomain(t, "domain-"+run.name, vmtestMetadataFor(run.lease), "net-"+run.name)
		fake.addNetwork(t, OwnedNetwork{Name: "net-" + run.name, Owner: vmtestMetadata, Lease: run.lease, CIDR: "10.200.1.0/24", Gateway: "10.200.1.1"})
	}
	// A runner that predates run leases, and objects another tool owns.
	fake.addDomain(t, "domain-leaseless", vmtestMetadataFor(""), "default")
	fake.addDomain(t, "domain-katldev", katldevMetadata, "default")
	fake.addNetwork(t, OwnedNetwork{Name: "net-katldev", Owner: "katl/katldev-world/dev-world-1", CIDR: "10.200.3.0/24", Gateway: "10.200.3.1"})

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	if err := sweep(context.Background(), fake.virsh(), io.Discard); err != nil {
		t.Fatal(err)
	}

	if got, want := fake.objects(t, "domains"), []string{"domain-katldev", "domain-live"}; !slices.Equal(got, want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
	if got, want := fake.objects(t, "networks"), []string{"net-katldev", "net-live"}; !slices.Equal(got, want) {
		t.Fatalf("networks = %v, want %v", got, want)
	}
}

func TestSweepIgnoresMissingLease(t *testing.T) {
	// A finished run that removed its run directory leaves no lease to hold.
	fake := newFakeLibvirt(t)
	fake.addDomain(t, "gone", vmtestMetadataFor(filepath.Join(t.TempDir(), "removed", "lease")), "default")

	if err := sweep(context.Background(), fake.virsh(), io.Discard); err != nil {
		t.Fatal(err)
	}

	if got := fake.objects(t, "domains"); len(got) != 0 {
		t.Fatalf("domains = %v, want none", got)
	}
}

func mkdirForFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ownedScenarioWorld returns a world that a held run lease owns, whose
// libvirt is the fake.
func ownedScenarioWorld(t *testing.T) (World, fakeLibvirt) {
	t.Helper()
	fake := newFakeLibvirt(t)
	world := testWorld(t)
	world.Lease = filepath.Join(t.TempDir(), "lease")
	startLeaseHolder(t, world.Lease)
	world.Libvirt.URI = "qemu:///fixture"
	return world, fake
}

func TestScenarioReservesPlannedAddresses(t *testing.T) {
	world, fake := ownedScenarioWorld(t)
	scenario := world.NewScenario(t, "reserve")
	network := scenario.World.Network.Name
	if network == world.Network.Name {
		t.Fatalf("scenario shares the world network %q", network)
	}
	if got, want := fake.objects(t, "networks"), []string{network}; !slices.Equal(got, want) {
		t.Fatalf("networks = %v, want %v", got, want)
	}
	if !fake.active(network) {
		t.Fatalf("network %s is not running", network)
	}

	cp := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	worker := scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})
	if cp.Network != network || cp.VMNetwork().Name != network || cp.VMNetwork().MAC != cp.MACAddress {
		t.Fatalf("node attaches to %+v, want network %s with its MAC", cp.VMNetwork(), network)
	}
	want := map[string]string{cp.MACAddress: cp.Address, worker.MACAddress: worker.Address}
	assertReservations(t, fake, network, want)

	moved, err := scenario.ReassignAddress("worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Address == worker.Address {
		t.Fatalf("reassignment kept %s", moved.Address)
	}
	want[worker.MACAddress] = moved.Address
	assertReservations(t, fake, network, want)

	if err := scenario.ConfirmAddress("worker-1", moved.Address); err != nil {
		t.Fatal(err)
	}
	assertReservations(t, fake, network, want)

	scenario.Release()
	if got := fake.objects(t, "networks"); len(got) != 0 {
		t.Fatalf("networks after release = %v, want none", got)
	}
}

func assertReservations(t *testing.T, fake fakeLibvirt, network string, want map[string]string) {
	t.Helper()
	got := fake.reservations(t, network)
	if len(got) != len(want) {
		t.Fatalf("reservations = %v, want %v", got, want)
	}
	for mac, address := range want {
		if got[strings.ToLower(mac)] != address {
			t.Fatalf("reservations = %v, want %v", got, want)
		}
	}
}

func TestScenarioReleaseKeepsNetworkOfPreservedDomain(t *testing.T) {
	world, fake := ownedScenarioWorld(t)
	scenario := world.NewScenario(t, "preserved")
	network := scenario.World.Network.Name
	fake.addDomain(t, "debug", vmtestMetadataFor(world.Lease), network)
	fake.addDomain(t, "other-run", vmtestMetadataFor(filepath.Join(t.TempDir(), "other")), network)
	fake.addDomain(t, "elsewhere", vmtestMetadataFor(world.Lease), "default")

	scenario.Release()

	if got := fake.objects(t, "networks"); !slices.Equal(got, []string{network}) {
		t.Fatalf("networks = %v, want %s kept for the preserved domain", got, network)
	}
}

func TestScenarioNetworkRejectsForeignOwner(t *testing.T) {
	world, fake := ownedScenarioWorld(t)
	name := "katl-" + world.RunID + "-foreign"
	fake.addNetwork(t, OwnedNetwork{Name: name, Owner: vmtestMetadata, Lease: filepath.Join(t.TempDir(), "other"), CIDR: "10.200.9.0/24", Gateway: "10.200.9.1"})

	scenario, err := world.PlanScenario("foreign")
	if err == nil {
		t.Fatalf("PlanScenario() reused a network of another lease: %+v", scenario.World.Network)
	}
}

func TestCreateNetworkConcurrentPicksDistinctSubnets(t *testing.T) {
	// Two names whose preferred subnet is the same race for one /24 unless
	// choosing and defining are serialised.
	var names []string
	seen := map[string]string{}
	for i := 0; len(names) == 0; i++ {
		name := fmt.Sprintf("katl-run-%d", i)
		cidr, _, err := ChooseNetworkCIDR(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[cidr]; ok {
			names = []string{other, name}
		}
		seen[cidr] = name
	}
	fake := newFakeLibvirt(t)
	t.Setenv("FAKE_DEFINE_DELAY", "0.2")
	lock := filepath.Join(t.TempDir(), "networks")

	networks := make([]OwnedNetwork, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			networks[i], errs[i] = createNetwork(context.Background(), fake.virsh(), lock, OwnedNetwork{Name: name, Owner: vmtestMetadata, Lease: "/l/" + name}, filepath.Join(t.TempDir(), "network.xml"))
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if networks[0].CIDR == networks[1].CIDR {
		t.Fatalf("concurrent networks share subnet %s", networks[0].CIDR)
	}
}

func TestCreateNetworkRetriesRefusedSubnet(t *testing.T) {
	fake := newFakeLibvirt(t)
	_, preferredGateway, err := ChooseNetworkCIDR("katl-run-1-retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.failStartOn(t, preferredGateway)

	network, err := createNetwork(context.Background(), fake.virsh(), filepath.Join(t.TempDir(), "networks"), OwnedNetwork{Name: "katl-run-1-retry", Owner: vmtestMetadata, Lease: "/l/retry"}, filepath.Join(t.TempDir(), "network.xml"))
	if err != nil {
		t.Fatal(err)
	}

	if network.Gateway == preferredGateway {
		t.Fatalf("network kept the subnet that libvirt refused: %s", network.CIDR)
	}
	if got := fake.objects(t, "networks"); !slices.Equal(got, []string{"katl-run-1-retry"}) || !fake.active("katl-run-1-retry") {
		t.Fatalf("networks = %v, active = %v; want one running network", got, fake.active("katl-run-1-retry"))
	}
}

func TestCreateNetworkGivesUpAfterRefusals(t *testing.T) {
	fake := newFakeLibvirt(t)
	// Every /24 the chooser can return is refused.
	for i := 0; i < 8*256; i++ {
		fake.failStartOn(t, fmt.Sprintf("10.%d.%d.1", 200+i/256, i%256))
	}

	_, err := createNetwork(context.Background(), fake.virsh(), filepath.Join(t.TempDir(), "networks"), OwnedNetwork{Name: "katl-run-1-refused", Owner: vmtestMetadata, Lease: "/l/refused"}, filepath.Join(t.TempDir(), "network.xml"))

	if err == nil || !strings.Contains(err.Error(), "tun0") {
		t.Fatalf("createNetwork() error = %v, want the libvirt refusal", err)
	}
	if got := fake.objects(t, "networks"); len(got) != 0 {
		t.Fatalf("refused network left defined: %v", got)
	}
}

// The dynamic DHCP range is the only place dnsmasq hands out addresses, so a
// guest without a reservation never receives one that Katl planned.
func TestPlannedAddressesAvoidDynamicRange(t *testing.T) {
	for _, cidr := range []string{"10.200.4.0/24", "10.200.8.0/26", "10.200.8.0/22"} {
		t.Run(cidr, func(t *testing.T) {
			_, subnet, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatal(err)
			}
			gateway := uint32ToIP(ipToUint32(subnet.IP) + 1).String()
			world := World{Network: WorldNetwork{CIDR: cidr, Gateway: gateway}}
			data, err := OwnedNetwork{Name: "n", Owner: vmtestMetadata, CIDR: cidr, Gateway: gateway}.XML()
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Ranges []struct {
					Start string `xml:"start,attr"`
					End   string `xml:"end,attr"`
				} `xml:"ip>dhcp>range"`
			}
			if err := xml.Unmarshal([]byte(data), &doc); err != nil || len(doc.Ranges) != 1 {
				t.Fatalf("rendered ranges = %+v, %v", doc.Ranges, err)
			}
			start, end := ipToUint32(net.ParseIP(doc.Ranges[0].Start)), ipToUint32(net.ParseIP(doc.Ranges[0].End))

			leased := map[string]bool{}
			planned := 0
			for {
				address, err := firstAvailableAddress(world, leased)
				if err != nil {
					break
				}
				value := ipToUint32(net.ParseIP(address))
				if (value >= start && value <= end) || address == gateway || !subnet.Contains(net.ParseIP(address)) {
					t.Fatalf("planned address %s collides with the dynamic range %s-%s, the gateway, or leaves %s", address, doc.Ranges[0].Start, doc.Ranges[0].End, cidr)
				}
				leased[address] = true
				planned++
			}
			if planned == 0 {
				t.Fatal("no address can be planned")
			}
			for address := start; address <= end; address++ {
				if err := validateRequestedAddress(world, nil, uint32ToIP(address).String()); err == nil {
					t.Fatalf("requested address %s in the dynamic range was accepted", uint32ToIP(address))
				}
			}
			if err := validateRequestedAddress(world, nil, uint32ToIP(start-1).String()); err != nil {
				t.Fatalf("address just below the dynamic range was rejected: %v", err)
			}
		})
	}
}

func TestOwnedNetworkXMLRunOwnership(t *testing.T) {
	got, err := OwnedNetwork{Name: "katl-run-1-a", Owner: "katl/vmtest", Lease: "/runs/1/lease", Run: "run-1", CIDR: "10.200.4.0/24", Gateway: "10.200.4.1"}.XML()
	if err != nil {
		t.Fatal(err)
	}
	want := `<network>
  <name>katl-run-1-a</name>
  <metadata>
    <vmtest xmlns="https://katlos.io/xmlns/vmtest/1" lease="/runs/1/lease" run="run-1">katl/vmtest</vmtest>
  </metadata>
  <forward mode="nat"></forward>
  <ip address="10.200.4.1" prefix="24">
    <dhcp>
      <range start="10.200.4.192" end="10.200.4.254">
        <lease expiry="0"></lease>
      </range>
    </dhcp>
  </ip>
  <options xmlns="http://libvirt.org/schemas/network/dnsmasq/1.0">
    <option value="log-dhcp"></option>
  </options>
</network>
`
	if got != want {
		t.Fatalf("XML =\n%s\nwant\n%s", got, want)
	}
}
