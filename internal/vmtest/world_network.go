package vmtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// sweepFinishedRuns removes the libvirt objects of finished runs. Cleanup is best
// effort, so a failure is reported and the caller goes on; the next sweep
// retries.
func sweepFinishedRuns(ctx context.Context, virsh virshFunc) {
	if err := sweep(ctx, virsh, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vmtest cleanup: remove libvirt objects left by finished runs failed: %v\n", err)
	}
}

// scripts/vmtest-run owns where runs coordinate and passes the resolved paths
// of the network creation lock and the run lease directory, as it does the
// admission ledger's.
const (
	networkLockEnv = "KATL_VMTEST_NETWORK_LOCK"
	runLeaseDirEnv = "KATL_VMTEST_RUN_LEASE_DIR"
)

// networkCreationLock is the lock file that serializes choosing a subnet and
// creating a network across runs.
func networkCreationLock() string {
	if path := os.Getenv(networkLockEnv); path != "" {
		return path
	}
	return filepath.Join(hostStateDir(), fmt.Sprintf("katl-vmtest-networks-%d.lock", os.Getuid()))
}

// runLeaseDir holds the lease of every run of this user on the host, so a
// sweep can judge every run's objects from inside its own sandbox.
func runLeaseDir() string {
	if dir := os.Getenv(runLeaseDirEnv); dir != "" {
		return dir
	}
	return filepath.Join(hostStateDir(), fmt.Sprintf("katl-vmtest-leases-%d", os.Getuid()))
}

// hostStateDir is where the coordination entries live when the runner did not
// pass their paths, as when go test runs without scripts/vmtest-run. Its
// default keeps the names that runs from older checkouts use, so such a test
// still coordinates with them.
func hostStateDir() string {
	if dir := os.Getenv("KATL_VMTEST_HOST_STATE_DIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// createNetwork gives the scenario its own libvirt network, owned by the run
// lease of its world, and points the scenario's copy of the world network at
// it. Every reader of scenario.World.Network then sees the scenario's network.
func (scenario *WorldScenario) createNetwork(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	world := scenario.World
	virsh := runVirsh(world.Libvirt.URI)
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
// the host has it. Waiting ends with an error that names the holders once the
// admission timeout passes, which stays below the go test timeout of a run.
func (scenario *WorldScenario) admit(ctx context.Context, options ...ScenarioOption) error {
	settings := scenarioOptions{nodes: 1}
	for _, option := range options {
		option(&settings)
	}
	capacity, err := HostCapacity()
	if err != nil {
		return err
	}
	timeout, err := admissionTimeout()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	virsh := runVirsh(scenario.World.Libvirt.URI)
	admission := Admission{
		Dir:                AdmissionDir(),
		Capacity:           capacity,
		Progress:           os.Stderr,
		Reap:               func(ctx context.Context) { sweepFinishedRuns(ctx, virsh) },
		AvailableMemoryMiB: availableMemoryMiB,
	}
	reservation, err := admission.Admit(ctx, scenario.World.RunID, scenario.ID, nodesDemand(settings.nodes))
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
