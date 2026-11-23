package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/spf13/cobra"
)

// A multihomed VM has a second NIC on an isolated network that the world owns
// beside its primary network. The host reaches the VM on both, so a journey
// can give the node a management.address on the second NIC and keep its
// primary address, which has the default route, as its kubernetes.address.

type multihomeState struct {
	Network vmtest.WorldNetwork           `json:"network"`
	Nodes   map[string]multihomeInterface `json:"nodes"`
}

type multihomeInterface struct {
	MACAddress string `json:"macAddress"`
	Address    string `json:"address"`
}

func multihomeNetworkName(runID string) string {
	return "katl-" + runID + "-multihome"
}

func (manager installerManager) multihomePath() string {
	return filepath.Join(manager.worldDir(), "multihome.json")
}

func (manager installerManager) readMultihome() (multihomeState, error) {
	state := multihomeState{Nodes: map[string]multihomeInterface{}}
	data, err := os.ReadFile(manager.multihomePath())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return multihomeState{}, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return multihomeState{}, fmt.Errorf("decode %s: %w", manager.multihomePath(), err)
	}
	if state.Nodes == nil {
		state.Nodes = map[string]multihomeInterface{}
	}
	return state, nil
}

func (manager installerManager) writeMultihome(state multihomeState) error {
	if len(state.Nodes) == 0 {
		err := os.Remove(manager.multihomePath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeJSONFile(manager.multihomePath(), state)
}

func (state multihomeState) network(world katldevWorld) vmtest.OwnedNetwork {
	network := vmtest.OwnedNetwork{
		Name: state.Network.Name, Owner: world.owner(), CIDR: state.Network.CIDR, Gateway: state.Network.Gateway, Isolated: true,
	}
	names := make([]string, 0, len(state.Nodes))
	for name := range state.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		nic := state.Nodes[name]
		network.Hosts = append(network.Hosts, vmtest.DHCPHost{Name: name, MACAddress: nic.MACAddress, Address: nic.Address})
	}
	return network
}

// syncMultihome converges the second network on multihome.json. With no
// multihomed VMs it touches nothing unless removeEmpty asks it to remove the
// network.
func (manager installerManager) syncMultihome(ctx context.Context, world katldevWorld, removeEmpty bool) error {
	state, err := manager.readMultihome()
	if err != nil {
		return err
	}
	uri := world.World.Libvirt.URI
	if len(state.Nodes) == 0 {
		if !removeEmpty {
			return nil
		}
		return manager.removeOwnedNetwork(ctx, uri, multihomeNetworkName(world.World.RunID), world.owner())
	}
	for name := range state.Nodes {
		if _, ok := world.Scenario.Node(name); !ok {
			return fmt.Errorf("%s gives a second NIC to %s, which the world no longer has; run katldev world multihome %s --remove", manager.multihomePath(), name, name)
		}
	}
	return manager.syncOwnedNetwork(ctx, uri, state.network(world), filepath.Join(world.World.RunDir, "network", "multihome.xml"))
}

// attachMultihome gives an existing domain its second NIC, persistently and,
// when it runs, live. Each definition that already has the recorded MAC is
// left alone, so a rerun after a partial attach converges.
func (manager installerManager) attachMultihome(ctx context.Context, uri, domain, network string, nic multihomeInterface) error {
	hasNIC := func(args ...string) (bool, error) {
		output, err := manager.virsh(ctx, uri, append([]string{"domiflist", domain}, args...)...)
		return strings.Contains(strings.ToLower(string(output)), strings.ToLower(nic.MACAddress)), err
	}
	persistent, err := hasNIC("--inactive")
	if err != nil {
		return err
	}
	state, err := manager.domainState(ctx, uri, domain)
	if err != nil {
		return err
	}
	live := true
	if state == "running" {
		if live, err = hasNIC(); err != nil {
			return err
		}
	}
	args := []string{"attach-interface", domain, "network", network, "--mac", nic.MACAddress, "--model", "virtio"}
	if !persistent {
		args = append(args, "--config")
	}
	if !live {
		args = append(args, "--live")
	}
	if persistent && live {
		return nil
	}
	_, err = manager.virsh(ctx, uri, args...)
	return err
}

func newWorldMultihomeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "multihome NODE",
		Short: "Give a VM a second NIC on an isolated network that this host also reaches",
		Long: `Give a VM a second NIC on an isolated network that this host also reaches.

The world creates the network on a free subnet and reserves the NIC's address.
Use the second address as the node's management.address and the VM's primary
address, which keeps the default route, as its kubernetes.address. A running VM
gets the NIC live, and a VM that katldev recreates keeps it; katldev world reset
and remove drop it with the world.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			if remove {
				return manager.removeMultihome(ctx, args[0])
			}
			return manager.setMultihome(ctx, args[0])
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "detach the VM's second NIC")
	return cmd
}

func (manager installerManager) setMultihome(ctx context.Context, name string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	node, err := manager.worldNode(world, name)
	if err != nil {
		return err
	}
	vm, err := manager.readState(world, node)
	if err != nil {
		return fmt.Errorf("read VM state for %s: %w; run katldev world start", node.Name, err)
	}
	if err := manager.requireOwnedDomain(ctx, vm.LibvirtURI, vm.DomainName, world.owner()); err != nil {
		return err
	}
	state, err := manager.readMultihome()
	if err != nil {
		return err
	}

	if state.Network.Name == "" {
		inUse, err := manager.networksInUse(ctx, world.World.Libvirt.URI)
		if err != nil {
			return err
		}
		name := multihomeNetworkName(world.World.RunID)
		cidr, gateway, err := vmtest.ChooseNetworkCIDR(name, inUse)
		if err != nil {
			return err
		}
		state.Network = vmtest.WorldNetwork{Backend: vmtest.NetworkLibvirt, Name: name, CIDR: cidr, Gateway: gateway}
	}
	nic, ok := state.Nodes[node.Name]
	if !ok {
		if nic, err = multihomeInterfaceFor(node, state.Network.CIDR); err != nil {
			return err
		}
		state.Nodes[node.Name] = nic
	}
	if err := manager.writeMultihome(state); err != nil {
		return err
	}
	if err := manager.syncMultihome(ctx, world, true); err != nil {
		return err
	}
	if err := manager.attachMultihome(ctx, vm.LibvirtURI, vm.DomainName, state.Network.Name, nic); err != nil {
		return fmt.Errorf("recorded a second NIC for %s but could not attach it: %w; rerun katldev world multihome %s", node.Name, err, node.Name)
	}
	fmt.Fprintf(manager.stdout, "%s has a second NIC at %s on %s (%s); this host reaches it at both addresses.\n", node.Name, nic.Address, state.Network.Name, state.Network.CIDR)
	fmt.Fprintf(manager.stdout, "In %s, give %s:\n", manager.clusterConfigPath(), node.Name)
	fmt.Fprintf(manager.stdout, "  management:\n    address: %s\n  kubernetes:\n    address: %s\n", nic.Address, node.Address)
	return nil
}

// multihomeInterfaceFor keeps the node's primary host number and MAC suffix
// on the second network, so each node's second NIC is distinct and readable.
func multihomeInterfaceFor(node vmtest.Node, cidr string) (multihomeInterface, error) {
	primary, err := netip.ParseAddr(node.Address)
	if err != nil || !primary.Is4() {
		return multihomeInterface{}, fmt.Errorf("%s address %q is not IPv4", node.Name, node.Address)
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || prefix.Bits() != 24 {
		return multihomeInterface{}, fmt.Errorf("second network %q is not an IPv4 /24", cidr)
	}
	network := prefix.Masked().Addr().As4()
	network[3] = primary.As4()[3]
	mac, err := net.ParseMAC(node.MACAddress)
	if err != nil || len(mac) != 6 {
		return multihomeInterface{}, fmt.Errorf("%s MAC %q is invalid", node.Name, node.MACAddress)
	}
	// The first octet keeps the locally administered bit, and the NIC sits on
	// its own network, so only its sibling NIC must differ.
	mac[2] ^= 0x01
	return multihomeInterface{MACAddress: mac.String(), Address: netip.AddrFrom4(network).String()}, nil
}

func (manager installerManager) removeMultihome(ctx context.Context, name string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	state, err := manager.readMultihome()
	if err != nil {
		return err
	}
	nic, ok := state.Nodes[name]
	if !ok {
		return fmt.Errorf("%s has no second NIC", name)
	}
	if node, ok := world.Scenario.Node(name); ok {
		if vm, err := manager.readState(world, node); err == nil {
			if exists, err := manager.domainExists(ctx, vm.LibvirtURI, vm.DomainName); err == nil && exists {
				args := []string{"detach-interface", vm.DomainName, "network", "--mac", nic.MACAddress, "--config"}
				if state, err := manager.domainState(ctx, vm.LibvirtURI, vm.DomainName); err == nil && state == "running" {
					args = append(args, "--live")
				}
				if _, err := manager.virsh(ctx, vm.LibvirtURI, args...); err != nil {
					return err
				}
			}
		}
	}
	delete(state.Nodes, name)
	if err := manager.writeMultihome(state); err != nil {
		return err
	}
	if err := manager.syncMultihome(ctx, world, true); err != nil {
		return err
	}
	fmt.Fprintf(manager.stdout, "%s no longer has a second NIC at %s.\n", name, nic.Address)
	return nil
}
