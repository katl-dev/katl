package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
)

// fakeLibvirt is an in-memory libvirt that records mutations.
type fakeLibvirt struct {
	t         *testing.T
	domains   map[string]string // name -> owner metadata
	states    map[string]string
	networks  map[string]*fakeNetwork
	mutations [][]string
	// dropNetworkMetadata mimics a libvirt without network metadata support.
	dropNetworkMetadata bool
	// startErr fails net-start.
	startErr error
}

type fakeNetwork struct {
	owner  string
	active bool
	xml    string
}

func newFakeLibvirt(t *testing.T) *fakeLibvirt {
	return &fakeLibvirt{
		t:       t,
		domains: map[string]string{},
		states:  map[string]string{},
		networks: map[string]*fakeNetwork{"default": {
			owner:  "",
			active: true,
			xml:    "<network><name>default</name><ip address='192.168.122.1' netmask='255.255.255.0'/></network>",
		}},
	}
}

func (fake *fakeLibvirt) virsh(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "list":
		var names []string
		for name := range fake.domains {
			names = append(names, name)
		}
		return []byte(strings.Join(names, "\n") + "\n"), nil
	case "metadata":
		return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">` + fake.domains[args[1]] + `</vmtest>`), nil
	case "domstate":
		return []byte(fake.states[args[1]] + "\n"), nil
	case "dominfo":
		return []byte("Managed save:   no\n"), nil
	case "destroy":
		fake.mutations = append(fake.mutations, args)
		fake.states[args[1]] = "shut off"
		return nil, nil
	case "undefine":
		fake.mutations = append(fake.mutations, args)
		delete(fake.domains, args[1])
		return nil, nil
	case "net-list":
		var names []string
		for name := range fake.networks {
			names = append(names, name)
		}
		return []byte(strings.Join(names, "\n") + "\n"), nil
	case "net-dumpxml":
		return []byte(fake.networks[args[1]].xml), nil
	case "net-metadata":
		network, ok := fake.networks[args[1]]
		if !ok || network.owner == "" {
			return nil, errors.New("metadata not found")
		}
		return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">` + network.owner + `</vmtest>`), nil
	case "net-info":
		active := "no"
		if fake.networks[args[1]].active {
			active = "yes"
		}
		return []byte("Name: " + args[1] + "\nActive:         " + active + "\n"), nil
	case "net-define":
		fake.mutations = append(fake.mutations, []string{"net-define"})
		data, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		owner := ""
		if start := strings.Index(string(data), `">`); start >= 0 {
			rest := string(data)[start+2:]
			owner = rest[:strings.Index(rest, "<")]
		}
		name := string(data)[strings.Index(string(data), "<name>")+6 : strings.Index(string(data), "</name>")]
		if fake.dropNetworkMetadata {
			owner = ""
		}
		fake.networks[name] = &fakeNetwork{owner: owner, xml: string(data)}
		return nil, nil
	case "net-start", "net-destroy", "net-undefine":
		fake.mutations = append(fake.mutations, args)
		switch args[0] {
		case "net-start":
			if fake.startErr != nil {
				return nil, fake.startErr
			}
			fake.networks[args[1]].active = true
		case "net-destroy":
			fake.networks[args[1]].active = false
		default:
			delete(fake.networks, args[1])
		}
		return nil, nil
	case "net-update":
		fake.mutations = append(fake.mutations, args[:3])
		return nil, nil
	}
	fake.t.Fatalf("unexpected virsh %v", args)
	return nil, nil
}

func testManager(t *testing.T, fake *fakeLibvirt) installerManager {
	t.Helper()
	return installerManager{
		repoRoot: t.TempDir(),
		stdout:   io.Discard,
		stderr:   io.Discard,
		runVirsh: fake.virsh,
		guestsConfig: func() (libvirtGuestsConfig, error) {
			return libvirtGuestsConfig{Loaded: true, Enabled: true, OnShutdown: "shutdown"}, nil
		},
	}
}

func TestWorldIDIsRandomAndSurvivesRemoval(t *testing.T) {
	fake := newFakeLibvirt(t)
	first := testManager(t, fake)
	second := testManager(t, fake)
	world, err := first.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if world.World.RunID == other.World.RunID {
		t.Fatalf("two checkouts share world ID %s", world.World.RunID)
	}

	if err := first.removeWorld(context.Background()); err != nil {
		t.Fatal(err)
	}
	recreated, err := first.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recreated.World.RunID != world.World.RunID {
		t.Fatalf("recreated world ID = %s, want %s", recreated.World.RunID, world.World.RunID)
	}
}

func TestWorldIDAdoptsRecordedWorld(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A world recorded before the ID file existed keeps its ID.
	if err := os.Remove(manager.worldIDPath()); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.loadWorld()
	if err != nil || loaded.World.RunID != world.World.RunID {
		t.Fatalf("loaded world = %v, %v; want ID %s", loaded.World.RunID, err, world.World.RunID)
	}
	id, err := manager.worldID(false)
	if err != nil || id != world.World.RunID {
		t.Fatalf("world ID = %q, %v; want %s", id, err, world.World.RunID)
	}
}

func TestRoutedSubnets(t *testing.T) {
	// /proc/net/route: default via 192.168.1.1, 10.8.0.0/16 over a VPN,
	// and 192.168.1.0/24 on the LAN.
	data := []byte(`Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
tun0	0000080A	00000000	0001	0	0	0	0000FFFF	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`)
	var got []string
	for _, subnet := range routedSubnets(data) {
		got = append(got, subnet.String())
	}
	if want := []string{"10.8.0.0/16", "192.168.1.0/24"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subnets = %v, want %v", got, want)
	}
}

func TestWorldPersistsNodesAcrossSessions(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, subnet, err := net.ParseCIDR(world.World.Network.CIDR)
	if err != nil || subnet.Contains(net.ParseIP("192.168.122.1")) {
		t.Fatalf("world subnet %s overlaps the default network: %v", world.World.Network.CIDR, err)
	}
	for _, add := range []struct {
		name         string
		controlPlane bool
	}{{"", true}, {"", false}, {"", true}, {"", false}} {
		if _, err := addWorldNode(world, add.name, add.controlPlane); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := addWorldNode(world, "cp-1", false); err == nil {
		t.Fatal("added a second cp-1")
	}
	if _, err := addWorldNode(world, "CP_3", false); err == nil {
		t.Fatal("accepted an invalid node name")
	}

	reloaded, err := manager.loadWorld()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	addresses := map[string]bool{}
	macs := map[string]bool{}
	for _, node := range reloaded.Scenario.Nodes {
		names = append(names, node.Name+"/"+string(node.Role))
		addresses[node.Address] = true
		macs[node.MACAddress] = true
		if !subnet.Contains(net.ParseIP(node.Address)) {
			t.Fatalf("%s address %s is outside %s", node.Name, node.Address, subnet)
		}
	}
	want := []string{"cp-1/control-plane", "worker-1/worker", "cp-2/control-plane", "worker-2/worker"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("nodes = %v, want %v", names, want)
	}
	if len(addresses) != 4 || len(macs) != 4 {
		t.Fatalf("nodes share addresses or MACs: %v %v", addresses, macs)
	}
}

func TestWorldNodeSpecs(t *testing.T) {
	existing := katldevWorld{Scenario: &vmtest.WorldScenario{Nodes: []vmtest.Node{
		{Name: "cp-1", Role: vmtest.ControlPlane},
		{Name: "worker-1", Role: vmtest.Worker},
		{Name: "cp-2", Role: vmtest.ControlPlane},
	}}}
	kept := []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}, {Name: "worker-1", Role: vmtest.Worker}, {Name: "cp-2", Role: vmtest.ControlPlane}}
	counted := []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}, {Name: "worker-1", Role: vmtest.Worker}}
	for _, test := range []struct {
		name   string
		exists bool
		count  int
		want   []vmtest.NodeSpec
	}{
		{name: "new default", want: counted[:1]},
		{name: "new counted", count: 2, want: counted},
		{name: "reset keeps VMs", exists: true, want: kept},
		{name: "reset same count keeps VMs", exists: true, count: 3, want: kept},
		{name: "reset new count", exists: true, count: 2, want: counted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := worldNodeSpecs(existing, test.exists, test.count); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("specs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWorldNodeStateIsNodeScoped(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cp1 := mustAddWorldNode(t, world, true)
	worker1 := mustAddWorldNode(t, world, false)
	if err := manager.writeState(cp1, installerState{
		APIVersion: installerStateAPIVersion,
		Kind:       installerStateKind,
		RepoRoot:   manager.repoRoot,
		World:      world.World.RunID,
		Node:       cp1.Name,
		DomainName: "katl-" + world.World.RunID + "-cp-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.readState(world, cp1); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.readState(world, worker1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker-1 state error = %v, want not exist", err)
	}

	data, err := os.ReadFile(manager.statePath(cp1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(worker1), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.readState(world, worker1); err == nil {
		t.Fatal("worker-1 adopted cp-1's VM state")
	}
}

func TestWaitReachableReportsStoppedVM(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	node := mustAddWorldNode(t, world, false)
	domain := world.World.Libvirt.DomainPrefix + node.Name
	fake.domains[domain], fake.states[domain] = world.owner(), "shut off"
	if err := manager.writeState(node, installerState{
		APIVersion: installerStateAPIVersion,
		Kind:       installerStateKind,
		RepoRoot:   manager.repoRoot,
		World:      world.World.RunID,
		Node:       node.Name,
		DomainName: domain,
	}); err != nil {
		t.Fatal(err)
	}

	observation, err := manager.waitReachable(context.Background(), world, node, time.Minute)
	if err != nil || observation.summary() != "shut off" {
		t.Fatalf("observation = %q, %v; want shut off", observation.summary(), err)
	}
}

func mustAddWorldNode(t *testing.T, world katldevWorld, controlPlane bool) vmtest.Node {
	t.Helper()
	node, err := addWorldNode(world, "", controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func TestSyncNetworkConvergesReservations(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cp1 := mustAddWorldNode(t, world, true)

	if err := manager.syncNetwork(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	network := fake.networks[world.World.Network.Name]
	if network == nil || !network.active || network.owner != world.owner() {
		t.Fatalf("network = %#v, want active and owned by %s", network, world.owner())
	}
	if !strings.Contains(network.xml, "mac=\""+cp1.MACAddress+"\"") || !strings.Contains(network.xml, "ip=\""+cp1.Address+"\"") {
		t.Fatalf("network definition lacks cp-1 reservation:\n%s", network.xml)
	}

	// Live state: cp-1 at a stale address, plus a reservation nobody owns.
	worker1 := mustAddWorldNode(t, world, false)
	moved, err := world.Scenario.ReassignAddress("cp-1", "")
	if err != nil {
		t.Fatal(err)
	}
	network.xml = fmt.Sprintf(`<network><ip address='%s' prefix='24'><dhcp>
<host mac='%s' name='cp-1' ip='%s'/><host mac='52:54:00:00:00:01' name='gone' ip='%s'/>
</dhcp></ip></network>`, world.World.Network.Gateway, cp1.MACAddress, cp1.Address, worker1.Address)
	fake.mutations = nil
	if err := manager.syncNetwork(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"net-update", world.World.Network.Name, "modify"},
		{"net-update", world.World.Network.Name, "add-last"},
		{"net-update", world.World.Network.Name, "delete"},
	}
	if !reflect.DeepEqual(fake.mutations, want) {
		t.Fatalf("mutations = %v, want %v", fake.mutations, want)
	}
	if moved.Address == cp1.Address {
		t.Fatalf("reassignment kept address %s", moved.Address)
	}
}

func TestSyncNetworkRefusesForeignNetwork(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fake.networks[world.World.Network.Name] = &fakeNetwork{owner: "katl/someone-else", xml: "<network/>"}
	if err := manager.syncNetwork(context.Background(), world); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("sync error = %v, want refusal", err)
	}
	if len(fake.mutations) != 0 {
		t.Fatalf("mutated a foreign network: %v", fake.mutations)
	}
}

func TestSyncNetworkUndefinesNetworkWithoutMetadata(t *testing.T) {
	fake := newFakeLibvirt(t)
	fake.dropNetworkMetadata = true
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = manager.syncNetwork(context.Background(), world)
	if err == nil || !strings.Contains(err.Error(), "libvirt 9.7") {
		t.Fatalf("sync error = %v, want libvirt version guidance", err)
	}
	if _, ok := fake.networks[world.World.Network.Name]; ok {
		t.Fatal("network without ownership metadata was left defined")
	}
}

func TestSyncNetworkStartFailureNamesRecovery(t *testing.T) {
	fake := newFakeLibvirt(t)
	fake.startErr = errors.New("internal error: Network is already in use by interface tun0")
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = manager.syncNetwork(context.Background(), world)
	if err == nil || !strings.Contains(err.Error(), world.World.Network.CIDR) || !strings.Contains(err.Error(), "katldev world reset") || !strings.Contains(err.Error(), "tun0") {
		t.Fatalf("sync error = %v, want subnet, cause, and reset guidance", err)
	}
}

func TestRemoveWorldRemovesOnlyItsObjects(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.syncNetwork(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	own := world.World.Libvirt.DomainPrefix + "cp-1"
	fake.domains[own], fake.states[own] = world.owner(), "running"
	fake.domains["katl-dev-world-other-cp-1"], fake.states["katl-dev-world-other-cp-1"] = worldOwnerPrefix+"dev-world-other", "running"
	fake.domains["katl-vmtest-node"], fake.states["katl-vmtest-node"] = "katl/vmtest", "running"
	legacy := "katl-dev-installer-legacy"
	fake.domains[legacy], fake.states[legacy] = legacyDomainMetadata, "shut off"
	if err := writeJSONFile(filepath.Join(manager.legacyStateRoot(), "state.json"), installerState{
		Kind: legacyStateKind, RepoRoot: manager.repoRoot, DomainName: legacy, LibvirtURI: "qemu:///system",
	}); err != nil {
		t.Fatal(err)
	}
	fake.mutations = nil

	if err := manager.removeWorld(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.domains[own]; ok {
		t.Fatal("world domain survived removal")
	}
	if _, ok := fake.domains[legacy]; ok {
		t.Fatal("legacy installer survived removal")
	}
	for _, other := range []string{"katl-dev-world-other-cp-1", "katl-vmtest-node"} {
		if _, ok := fake.domains[other]; !ok || fake.states[other] != "running" {
			t.Fatalf("removal touched %s", other)
		}
	}
	if _, ok := fake.networks[world.World.Network.Name]; ok {
		t.Fatal("world network survived removal")
	}
	if _, ok := fake.networks["default"]; !ok {
		t.Fatal("removal deleted the default network")
	}
	if _, err := os.Stat(manager.worldDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("world directory remains: %v", err)
	}

	// A prefixed domain that another owner claims stops removal untouched.
	fake.domains[own], fake.states[own] = "katl/vmtest", "running"
	if err := manager.removeWorld(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("remove error = %v, want refusal", err)
	}
	if fake.states[own] != "running" {
		t.Fatal("removal powered off a domain it does not own")
	}
}

func TestStartRefusesLegacyStateWithGuidance(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	if err := writeJSONFile(filepath.Join(manager.legacyStateRoot(), "state.json"), installerState{
		Kind: legacyStateKind, RepoRoot: manager.repoRoot, DomainName: "katl-dev-installer-legacy",
	}); err != nil {
		t.Fatal(err)
	}
	err := manager.startWorld(context.Background(), worldStartOptions{installerOptions: defaultInstallerOptions()}, false)
	if err == nil || !strings.Contains(err.Error(), "katldev world reset") {
		t.Fatalf("start error = %v, want reset guidance", err)
	}
	if len(fake.mutations) != 0 {
		t.Fatalf("start mutated libvirt: %v", fake.mutations)
	}
}

func waitingObservation(name, address string, role vmtest.NodeRole) nodeObservation {
	return nodeObservation{
		Node:      vmtest.Node{Name: name, Role: role, Address: address},
		State:     installerState{IPAddress: address, Endpoint: "http://" + address + ":8080"},
		Exists:    true,
		Installer: "waiting-for-config",
	}
}

func TestEnsureClusterConfig(t *testing.T) {
	cp1 := waitingObservation("cp-1", "192.0.2.41", vmtest.ControlPlane)
	worker1 := waitingObservation("worker-1", "192.0.2.42", vmtest.Worker)
	for _, test := range []struct {
		name     string
		existing string
		force    bool
		want     string
		wantArgs []string
		wantErr  string
	}{
		{name: "created", want: "created", wantArgs: []string{"--installer", cp1.State.Endpoint, "--installer", worker1.State.Endpoint}},
		{name: "lists a VM", existing: "192.0.2.41", want: "existing"},
		{name: "lists no VM", existing: "192.0.2.99", wantErr: "--force-config"},
		{name: "forced", existing: "192.0.2.99", force: true, want: "created", wantArgs: []string{"--installer", cp1.State.Endpoint, "--installer", worker1.State.Endpoint, "--force"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotArgs []string
			manager := installerManager{repoRoot: t.TempDir(), stdout: io.Discard, stderr: io.Discard}
			manager.run = func(_ context.Context, _, _ string, args, _ []string, _, _ io.Writer) error {
				gotArgs = args[5:]
				writeKatldevClusterConfig(t, manager.clusterConfigPath(), cp1.Node.Address)
				return nil
			}
			if test.existing != "" {
				writeKatldevClusterConfig(t, manager.clusterConfigPath(), test.existing)
			}
			got, err := manager.ensureClusterConfig(context.Background(), []nodeObservation{cp1, worker1}, test.force)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want || !reflect.DeepEqual(gotArgs, test.wantArgs) {
				t.Fatalf("status=%q err=%v args=%v, want %q %v", got, err, gotArgs, test.want, test.wantArgs)
			}
		})
	}
}

func TestAddNodeGuidanceNamesConfigAddNode(t *testing.T) {
	for _, test := range []struct {
		name string
		obs  nodeObservation
		want string
	}{
		{"control plane", waitingObservation("cp-2", "192.0.2.43", vmtest.ControlPlane), "katlctl config add-node /repo/_build/katldev/cluster.yaml --installer 192.0.2.43 --name cp-2 --role control-plane\n"},
		{"worker", waitingObservation("worker-2", "192.0.2.44", vmtest.Worker), "katlctl config add-node /repo/_build/katldev/cluster.yaml --installer 192.0.2.44 --name worker-2 --role worker\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			manager := installerManager{repoRoot: "/repo", stdout: &stdout}
			manager.printAddNode(test.obs)
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("guidance missing %q:\n%s", test.want, stdout.String())
			}
			if strings.Contains(stdout.String(), "byID") {
				t.Fatalf("guidance asks for a hand-written node entry:\n%s", stdout.String())
			}
		})
	}
}

func TestMatchingConfigAcceptsWorkerWithExternalControlPlane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	writeKatldevClusterConfig(t, path, "192.0.2.42")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), "      controlPlane: true\n", "", 1)
	source = strings.Replace(source, "    version: v1.36.1\n", "    version: v1.36.1\n    external:\n      endpoint: https://api.external.test:6443\n  defaults:\n    kubernetes:\n      kubelet:\n        configFile: kubelet.yaml\n", 1)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, err := matchingKatldevConfig(path, "192.0.2.42"); err != nil || name != "cp-1" {
		t.Fatalf("match = %q, %v", name, err)
	}
	if _, err := matchingKatldevConfig(path, "192.0.2.43"); !errors.Is(err, errNodeUnlisted) {
		t.Fatalf("unlisted address error = %v", err)
	}
}

func TestStopDomain(t *testing.T) {
	const owner = worldOwnerPrefix + "dev-world-test"
	for _, test := range []struct {
		name    string
		initial string
		owner   string
		force   bool
		want    []string
		wantErr string
	}{
		{name: "graceful", initial: "running", owner: owner, want: []string{"shutdown", "katl-dev-test", "--mode", "acpi"}},
		{name: "already off", initial: "shut off", owner: owner},
		{name: "already shutting down", initial: "in shutdown", owner: owner},
		{name: "force", initial: "running", owner: owner, force: true, want: []string{"destroy", "katl-dev-test"}},
		{name: "paused", initial: "paused", owner: owner, wantErr: "resume"},
		{name: "unowned", initial: "running", owner: "katl/vmtest", wantErr: "refusing"},
		{name: "unowned force", initial: "running", owner: "katl/vmtest", force: true, wantErr: "refusing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := test.initial
			var mutation []string
			var stdout bytes.Buffer
			manager := installerManager{stdout: &stdout, stderr: io.Discard}
			manager.runVirsh = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte("katl-dev-test\n"), nil
				case "metadata":
					return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">` + test.owner + `</vmtest>`), nil
				case "domstate":
					observed := state
					if state == "in shutdown" {
						state = "shut off"
					}
					return []byte(observed), nil
				case "shutdown", "destroy":
					mutation = append([]string(nil), args...)
					state = "in shutdown"
					if args[0] == "destroy" {
						state = "shut off"
					}
					return nil, nil
				}
				t.Fatalf("unexpected virsh %v", args)
				return nil, nil
			}
			err := manager.stopDomain(context.Background(), installerState{DomainName: "katl-dev-test", LibvirtURI: "qemu:///system"}, owner, 5*time.Second, test.force)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || mutation != nil {
					t.Fatalf("error = %v, mutation = %v; want %q and no mutation", err, mutation, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if state != "shut off" || !reflect.DeepEqual(mutation, test.want) || stdout.Len() == 0 {
				t.Fatalf("state=%s mutation=%v output=%q", state, mutation, stdout.String())
			}
		})
	}
}

func TestStopDomainTimesOut(t *testing.T) {
	manager := installerManager{stdout: io.Discard, stderr: io.Discard}
	manager.runVirsh = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte("katl-dev-test\n"), nil
		case "metadata":
			return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">owner</vmtest>`), nil
		case "domstate":
			return []byte("running"), nil
		}
		return nil, nil
	}
	err := manager.stopDomain(context.Background(), installerState{DomainName: "katl-dev-test"}, "owner", 10*time.Millisecond, false)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("error = %v, want deadline with --timeout guidance", err)
	}
}

func TestLibvirtGuestsWarning(t *testing.T) {
	files := map[string]string{"/var/lib/sysconfig/libvirt-guests": "# comment\nON_SHUTDOWN=shutdown\n"}
	read := func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, os.ErrNotExist
	}
	for _, test := range []struct {
		name string
		show string
		warn string
	}{
		{
			name: "suspend in environment",
			show: "LoadState=loaded\nUnitFileState=enabled\nEnvironment=\"LOCALE_ARCHIVE=/x y\" ON_BOOT=start ON_SHUTDOWN=suspend\nEnvironmentFiles=/missing (ignore_errors=yes)\n",
			warn: "ON_SHUTDOWN=suspend",
		},
		{
			name: "unset defaults to suspend",
			show: "LoadState=loaded\nUnitFileState=enabled\nEnvironment=ON_BOOT=start\n",
			warn: "ON_SHUTDOWN=suspend",
		},
		{
			name: "file overrides environment",
			show: "LoadState=loaded\nUnitFileState=enabled\nEnvironment=ON_SHUTDOWN=suspend\nEnvironmentFiles=/var/lib/sysconfig/libvirt-guests (ignore_errors=yes)\n",
		},
		{
			name: "disabled",
			show: "LoadState=loaded\nUnitFileState=disabled\nEnvironment=ON_SHUTDOWN=shutdown\n",
			warn: "not enabled",
		},
		{
			name: "not installed",
			show: "LoadState=not-found\nUnitFileState=\n",
			warn: "not enabled",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			warning := libvirtGuestsWarning(parseLibvirtGuestsConfig([]byte(test.show), read))
			if test.warn == "" && warning != "" || test.warn != "" && !strings.Contains(warning, test.warn) {
				t.Fatalf("warning = %q, want %q", warning, test.warn)
			}
		})
	}

	var stderr bytes.Buffer
	manager := installerManager{stderr: &stderr, guestsConfig: func() (libvirtGuestsConfig, error) {
		return libvirtGuestsConfig{}, errors.New("no systemd")
	}}
	manager.warnHostShutdown()
	if stderr.Len() != 0 {
		t.Fatalf("unreadable configuration produced output: %s", stderr.String())
	}
}
