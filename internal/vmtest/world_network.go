package vmtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// VMNetwork returns the network attachment of the node's VM: its MAC, the
// network that reserves the MAC's address, and, when the network reserves
// it, the address the VM must be seen at.
func (node Node) VMNetwork() VMNetworkConfig {
	config := VMNetworkConfig{MAC: node.MACAddress, Name: node.Network}
	if node.Reserved {
		config.Address = node.Address
	}
	return config
}

// VMNetwork returns the network attachment of a VM that is not one of the
// scenario nodes: the scenario network, without a reservation.
func (scenario *WorldScenario) VMNetwork() VMNetworkConfig {
	return VMNetworkConfig{Name: scenario.World.Network.Name}
}

var swept sync.Once

// sweepOnce removes the libvirt objects of finished runs at the first
// scenario of the process. Cleanup is best effort, so a failure is reported and
// the scenario goes on; the next process sweeps again.
func sweepOnce(ctx context.Context, virsh virshFunc) {
	swept.Do(func() {
		if err := sweep(ctx, virsh, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "vmtest cleanup: remove libvirt objects left by finished runs failed: %v\n", err)
		}
	})
}

func networkCreationLock() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("katl-vmtest-networks-%d", os.Getuid()))
}

// createNetwork gives the scenario its own libvirt network, owned by the run
// lease of its world, and points the scenario's copy of the world network at
// it. Every reader of scenario.World.Network then sees the scenario's network.
func (scenario *WorldScenario) createNetwork(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	world := scenario.World
	virsh := runVirsh(world.Libvirt.URI)
	sweepOnce(ctx, virsh)
	network, err := createNetwork(ctx, virsh, networkCreationLock(), OwnedNetwork{
		Name:  "katl-" + world.RunID + "-" + scenario.ID,
		Owner: vmtestMetadata,
		Lease: world.Lease,
		Run:   world.RunID,
	}, filepath.Join(scenario.Dir, "network.xml"))
	if err != nil {
		return err
	}
	scenario.World.Network.Name = network.Name
	scenario.World.Network.CIDR = network.CIDR
	scenario.World.Network.Gateway = network.Gateway
	scenario.owned = true
	return nil
}

// admit reserves host capacity for the scenario's declared nodes, waiting until
// the host has it.
func (scenario *WorldScenario) admit(ctx context.Context, options ...ScenarioOption) error {
	settings := scenarioOptions{nodes: 1}
	for _, option := range options {
		option(&settings)
	}
	capacity, err := HostCapacity()
	if err != nil {
		return err
	}
	reservation, err := Admission{Dir: AdmissionDir(), Capacity: capacity, Progress: os.Stderr}.Admit(ctx, scenario.World.RunID, scenario.ID, nodesDemand(settings.nodes))
	if err != nil {
		return err
	}
	scenario.admission, scenario.declaredNodes = reservation, settings.nodes
	return nil
}

// Release gives back the scenario's host capacity and removes its libvirt
// network, unless a domain preserved for debugging still uses the network. A
// scenario without a reservation or network has nothing to release.
func (scenario *WorldScenario) Release() {
	// The reservation outlives the network, so capacity frees only once the
	// scenario's VMs and network are gone.
	defer func() {
		scenario.admission.Release()
		scenario.admission = nil
	}()
	if !scenario.owned {
		return
	}
	scenario.owned = false
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := releaseNetwork(ctx, runVirsh(scenario.World.Libvirt.URI), scenario.World.Network.Name, scenario.World.Lease, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vmtest cleanup: remove libvirt network %s failed; the next run removes it: %v\n", scenario.World.Network.Name, err)
	}
}

// ownedNetwork returns the scenario's network with a reservation for every
// node at its planned address.
func (scenario *WorldScenario) ownedNetwork() OwnedNetwork {
	network := scenario.World.Network
	owned := OwnedNetwork{
		Name:    network.Name,
		Owner:   vmtestMetadata,
		Lease:   scenario.World.Lease,
		Run:     scenario.World.RunID,
		CIDR:    network.CIDR,
		Gateway: network.Gateway,
	}
	for _, node := range scenario.Nodes {
		owned.Hosts = append(owned.Hosts, DHCPHost{Name: node.Name, MACAddress: node.MACAddress, Address: node.Address})
	}
	return owned
}

// syncNetwork makes the scenario's network reserve exactly its nodes'
// planned addresses, so an address the scenario plans is the address the
// guest observes. Scenarios that did not create their network leave it alone.
func (scenario *WorldScenario) syncNetwork(ctx context.Context) error {
	if !scenario.owned {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	virsh := runVirsh(scenario.World.Libvirt.URI)
	if err := scenario.ownedNetwork().Ensure(ctx, virsh, filepath.Join(scenario.Dir, "network.xml")); err != nil {
		return fmt.Errorf("sync address reservations of libvirt network %s failed; the node is recorded, and the next change to this scenario syncs all reservations again: %w", scenario.World.Network.Name, err)
	}
	return nil
}
