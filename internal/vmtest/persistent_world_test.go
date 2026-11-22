package vmtest

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOwnedNetworkXML(t *testing.T) {
	network := OwnedNetwork{
		Name:    "katl-dev-world-1",
		Owner:   "katl/katldev-world/dev-world-1",
		CIDR:    "10.201.3.0/24",
		Gateway: "10.201.3.1",
		Hosts:   []DHCPHost{{Name: "cp-1", MACAddress: "52:54:AA:BB:CC:DD", Address: "10.201.3.2"}},
	}
	got, err := network.XML()
	if err != nil {
		t.Fatal(err)
	}
	want := `<network>
  <name>katl-dev-world-1</name>
  <metadata>
    <vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-world/dev-world-1</vmtest>
  </metadata>
  <forward mode="nat"></forward>
  <ip address="10.201.3.1" prefix="24">
    <dhcp>
      <range start="10.201.3.2" end="10.201.3.254">
        <lease expiry="0"></lease>
      </range>
      <host mac="52:54:aa:bb:cc:dd" name="cp-1" ip="10.201.3.2">
        <lease expiry="0"></lease>
      </host>
    </dhcp>
  </ip>
</network>
`
	if got != want {
		t.Fatalf("XML =\n%s\nwant\n%s", got, want)
	}

	host, err := network.HostXML(DHCPHost{Name: "worker-1", MACAddress: "52:54:00:00:00:02", Address: "10.201.3.9"})
	if err != nil || host != `<host mac="52:54:00:00:00:02" name="worker-1" ip="10.201.3.9"><lease expiry="0"></lease></host>` {
		t.Fatalf("HostXML = %s, %v", host, err)
	}
	if _, err := network.HostXML(DHCPHost{Name: "far", MACAddress: "52:54:00:00:00:03", Address: "10.201.4.9"}); err == nil {
		t.Fatal("HostXML accepted an address outside the network")
	}
}

func TestLibvirtNetworkParsing(t *testing.T) {
	// Shape of virsh net-dumpxml output for the stock default network.
	data := []byte(`<network>
  <name>default</name>
  <forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward>
  <ip address='192.168.122.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.122.2' end='192.168.122.254'/>
      <host mac='52:54:00:AA:00:01' name='cp-1' ip='192.168.122.10'/>
    </dhcp>
  </ip>
</network>`)
	subnet, err := LibvirtNetworkIPv4(data)
	if err != nil || subnet.String() != "192.168.122.0/24" {
		t.Fatalf("subnet = %v, %v", subnet, err)
	}
	hosts, err := LibvirtNetworkHosts(data)
	if err != nil || !reflect.DeepEqual(hosts, []DHCPHost{{Name: "cp-1", MACAddress: "52:54:00:aa:00:01", Address: "192.168.122.10"}}) {
		t.Fatalf("hosts = %#v, %v", hosts, err)
	}
}

func TestChooseNetworkCIDRAvoidsSubnetsInUse(t *testing.T) {
	first, gateway, err := ChooseNetworkCIDR("world-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, _ := ChooseNetworkCIDR("world-a", nil); again != first {
		t.Fatalf("choice is not stable: %s then %s", first, again)
	}
	_, chosen, _ := net.ParseCIDR(first)
	if !chosen.Contains(net.ParseIP(gateway)) || !strings.HasSuffix(gateway, ".1") {
		t.Fatalf("gateway %s for %s", gateway, first)
	}
	_, broad, _ := net.ParseCIDR("10.200.0.0/13")
	if !broad.Contains(chosen.IP) {
		t.Fatalf("%s is outside 10.200.0.0/13", first)
	}

	// A wider subnet covering the first choice forces another block.
	_, covering, _ := net.ParseCIDR(chosen.IP.String() + "/22")
	second, _, err := ChooseNetworkCIDR("world-a", []*net.IPNet{chosen, covering})
	if err != nil {
		t.Fatal(err)
	}
	_, secondNet, _ := net.ParseCIDR(second)
	if covering.Contains(secondNet.IP) {
		t.Fatalf("second choice %s overlaps %s", second, covering)
	}
}

func TestWorldScenarioPersistsAcrossLoad(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "katldev")
	cp := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	worker := scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})

	loaded, err := world.LoadScenario("katldev")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Nodes, []Node{cp, worker}) {
		t.Fatalf("loaded nodes = %#v", loaded.Nodes)
	}
	other := world
	other.RunID = "run-2"
	if _, err := other.LoadScenario("katldev"); err == nil {
		t.Fatal("another world loaded this world's scenario")
	}

	result, err := loaded.PlanNodeVM(worker, []DiskFixture{TargetDisk("root", string(DiskQCOW2), "1G")}, KVMAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != "run-1-worker-1" || result.Artifacts.InstallerSerial != filepath.Join(worker.VMDir, "installer-serial.log") {
		t.Fatalf("result = %s %s", result.RunID, result.Artifacts.InstallerSerial)
	}
	if len(result.Disks) != 1 || filepath.Dir(result.Disks[0].HostPath) != worker.DiskDir {
		t.Fatalf("disks = %#v, want under %s", result.Disks, worker.DiskDir)
	}
}

func TestWorldScenarioReassignAddress(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "katldev")
	cp := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	worker := scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})

	moved, err := scenario.ReassignAddress("worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Address == worker.Address || moved.Address == cp.Address || moved.MACAddress != worker.MACAddress {
		t.Fatalf("moved = %#v from %s", moved, worker.Address)
	}
	loaded, err := world.LoadScenario("katldev")
	if err != nil {
		t.Fatal(err)
	}
	if node, _ := loaded.Node("worker-1"); node.Address != moved.Address {
		t.Fatalf("manifest address = %s, want %s", node.Address, moved.Address)
	}

	for _, address := range []string{cp.Address, moved.Address, "10.77.1.5"} {
		if _, err := scenario.ReassignAddress("worker-1", address); err == nil {
			t.Fatalf("reassigned worker-1 to %s", address)
		}
	}
	if explicit, err := scenario.ReassignAddress("worker-1", "10.77.0.50"); err != nil || explicit.Address != "10.77.0.50" {
		t.Fatalf("explicit = %#v, %v", explicit, err)
	}
	if _, err := scenario.ReassignAddress("worker-9", ""); err == nil {
		t.Fatal("reassigned an unknown node")
	}
}

func TestReassignedNodeKeepsLiveAddressUntilConfirmed(t *testing.T) {
	world := testWorld(t)
	planned := world.NewScenario(t, "katldev")
	planned.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	worker := planned.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})
	scenario, err := world.LoadScenario("katldev")
	if err != nil {
		t.Fatal(err)
	}
	other, err := world.LoadScenario("katldev")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := scenario.ReassignAddress("worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Address != "10.77.0.4" || moved.PreviousAddress != worker.Address {
		t.Fatalf("moved = %s from %s, want 10.77.0.4 from %s", moved.Address, moved.PreviousAddress, worker.Address)
	}

	// Another process adds a node while worker-1's VM still holds .3.
	if added := other.NewNode(t, NodeSpec{Name: "worker-2", Role: Worker}); added.Address != "10.77.0.5" {
		t.Fatalf("worker-2 address = %s, want 10.77.0.5", added.Address)
	}

	// Answering at the old address does not release it; answering at the
	// reservation does, and the stale process keeps the other's node.
	if err := scenario.ConfirmAddress("worker-1", worker.Address); err != nil {
		t.Fatal(err)
	}
	if added := other.NewNode(t, NodeSpec{Name: "worker-3", Role: Worker}); added.Address != "10.77.0.6" {
		t.Fatalf("worker-3 address = %s, want 10.77.0.6", added.Address)
	}
	if err := scenario.ConfirmAddress("worker-1", moved.Address); err != nil {
		t.Fatal(err)
	}
	if added := other.NewNode(t, NodeSpec{Name: "worker-4", Role: Worker}); added.Address != worker.Address {
		t.Fatalf("worker-4 address = %s, want released %s", added.Address, worker.Address)
	}
	loaded, err := world.LoadScenario("katldev")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, node := range loaded.Nodes {
		names = append(names, node.Name+"@"+node.Address+"/"+node.PreviousAddress)
	}
	want := []string{"cp-1@10.77.0.2/", "worker-1@10.77.0.4/", "worker-2@10.77.0.5/", "worker-3@10.77.0.6/", "worker-4@10.77.0.3/"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("nodes = %v, want %v", names, want)
	}
}

func TestReassignBeforeRebootTracksLiveAddress(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "katldev")
	worker := scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})
	if _, err := scenario.ReassignAddress("worker-1", "10.77.0.10"); err != nil {
		t.Fatal(err)
	}

	// A second move before the VM reboots releases the unused reservation
	// but still reserves the address the VM holds.
	again, err := scenario.ReassignAddress("worker-1", "10.77.0.11")
	if err != nil || again.PreviousAddress != worker.Address {
		t.Fatalf("second move = %#v, %v; want previous %s", again, err, worker.Address)
	}
	if added := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane, Address: "10.77.0.10"}); added.Address != "10.77.0.10" {
		t.Fatalf("cp-1 address = %s", added.Address)
	}
	if _, err := scenario.AddNode(NodeSpec{Name: "cp-2", Role: ControlPlane, Address: worker.Address}); err == nil {
		t.Fatalf("cp-2 took %s, which worker-1 still holds", worker.Address)
	}

	// Moving back to the live address needs no reboot.
	back, err := scenario.ReassignAddress("worker-1", worker.Address)
	if err != nil || back.Address != worker.Address || back.PreviousAddress != "" {
		t.Fatalf("move back = %#v, %v", back, err)
	}
}

func TestScenarioAddressesFollowManifest(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "katldev")
	scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})

	// A lease file that lost this scenario's entries, as after an
	// interrupted write, does not free the manifest's addresses.
	if err := os.WriteFile(world.Network.LeaseFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if added := scenario.NewNode(t, NodeSpec{Name: "worker-2", Role: Worker}); added.Address != "10.77.0.4" {
		t.Fatalf("worker-2 address = %s, want 10.77.0.4", added.Address)
	}
}

func TestFirstAvailableAddressInOddSubnet(t *testing.T) {
	// The third octet's low bit overlaps a /24's host-bit mask arithmetic.
	world := testWorld(t)
	world.Network.CIDR = "10.202.239.0/24"
	world.Network.Gateway = "10.202.239.1"
	scenario := world.NewScenario(t, "odd")
	node := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if node.Address != "10.202.239.2" {
		t.Fatalf("address = %s, want 10.202.239.2", node.Address)
	}
}
