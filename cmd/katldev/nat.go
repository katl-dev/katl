package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// A NAT address stands in for a public address that a router translates to
// a node: this host reaches the node there, but no VM holds it. It lets a
// journey give a node a management.address distinct from the
// kubernetes.address the VM actually has.

func (manager installerManager) natPath() string {
	return filepath.Join(manager.worldDir(), "nat.json")
}

// readNAT returns the world's NAT addresses by node name.
func (manager installerManager) readNAT() (map[string]string, error) {
	data, err := os.ReadFile(manager.natPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	nat := map[string]string{}
	if err := json.Unmarshal(data, &nat); err != nil {
		return nil, fmt.Errorf("decode %s: %w", manager.natPath(), err)
	}
	return nat, nil
}

func (manager installerManager) writeNAT(nat map[string]string) error {
	if len(nat) == 0 {
		err := os.Remove(manager.natPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(nat, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manager.natPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(manager.natPath(), append(data, '\n'), 0o644)
}

func natChain(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "KATLDEV-" + hex.EncodeToString(sum[:4])
}

// syncNAT makes the host translate each NAT address to its node's reserved
// address. The rules live in the world's own nat-table chain, jumped to from
// OUTPUT because katlctl runs on this host; they are rewritten from nat.json
// each time, so a readdress or a host firewall reload converges on the next
// world command. With no NAT addresses it touches nothing unless the chain
// must be removed.
func (manager installerManager) syncNAT(ctx context.Context, world katldevWorld, removeEmpty bool) error {
	nat, err := manager.readNAT()
	if err != nil {
		return err
	}
	chain := natChain(world.World.RunID)
	if len(nat) == 0 {
		if !removeEmpty {
			return nil
		}
		return manager.removeNATChain(ctx, chain)
	}

	if _, err := manager.iptables(ctx, "-S", chain); err != nil {
		if _, err := manager.iptables(ctx, "-N", chain); err != nil {
			return err
		}
	}
	if _, err := manager.iptables(ctx, "-F", chain); err != nil {
		return err
	}
	names := make([]string, 0, len(nat))
	for name := range nat {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node, ok := world.Scenario.Node(name)
		if !ok {
			return fmt.Errorf("%s gives NAT address %s to %s, which the world no longer has; run katldev world nat %s --remove", manager.natPath(), nat[name], name, name)
		}
		if _, err := manager.iptables(ctx, "-A", chain, "-d", nat[name]+"/32", "-j", "DNAT", "--to-destination", node.Address); err != nil {
			return err
		}
	}
	if _, err := manager.iptables(ctx, "-C", "OUTPUT", "-j", chain); err != nil {
		if _, err := manager.iptables(ctx, "-A", "OUTPUT", "-j", chain); err != nil {
			return err
		}
	}
	return nil
}

func (manager installerManager) removeNATChain(ctx context.Context, chain string) error {
	if _, err := manager.iptables(ctx, "-S", chain); err != nil {
		return nil
	}
	// Remove every jump, including duplicates an interrupted sync left.
	for range 16 {
		if _, err := manager.iptables(ctx, "-D", "OUTPUT", "-j", chain); err != nil {
			break
		}
	}
	if _, err := manager.iptables(ctx, "-F", chain); err != nil {
		return err
	}
	_, err := manager.iptables(ctx, "-X", chain)
	return err
}

// iptables edits the host nat table, which needs root; sudo must not prompt.
func (manager installerManager) iptables(ctx context.Context, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-w", "-t", "nat"}, args...)
	if manager.runIptables != nil {
		return manager.runIptables(ctx, fullArgs...)
	}
	command := append([]string{firstNonEmpty(os.Getenv("KATL_VMTEST_SUDO"), "sudo"), "-n", "iptables"}, fullArgs...)
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(command, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func newWorldNATCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	var address string
	var remove bool
	cmd := &cobra.Command{
		Use:   "nat NODE --address IP",
		Short: "Reach a VM at a host-translated address that no VM holds, like a public NAT address",
		Long: `Reach a VM at a host-translated address that no VM holds.

The host translates the address to the VM's reserved address, as a router
translates a public address to a node behind it. Use it as the node's
management.address and the VM's own address as its kubernetes.address.
Adding or changing NAT rules runs iptables through sudo, which must not prompt.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			if remove {
				return manager.removeNAT(ctx, args[0])
			}
			return manager.setNAT(ctx, args[0], address)
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "IPv4 address outside the world network that no host interface holds, such as 198.18.0.10")
	cmd.Flags().BoolVar(&remove, "remove", false, "stop translating the node's NAT address")
	return cmd
}

func (manager installerManager) setNAT(ctx context.Context, name, address string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	node, err := manager.worldNode(world, name)
	if err != nil {
		return err
	}
	nat, err := manager.readNAT()
	if err != nil {
		return err
	}
	parsed, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || !parsed.Is4() || !parsed.IsGlobalUnicast() {
		return fmt.Errorf("--address %q must be a unicast IPv4 address, such as 198.18.0.10", address)
	}
	if _, network, err := net.ParseCIDR(world.World.Network.CIDR); err == nil && network.Contains(parsed.AsSlice()) {
		return fmt.Errorf("--address %s is inside the world network %s, where a VM could hold it; choose an address outside it, such as 198.18.0.10", parsed, world.World.Network.CIDR)
	}
	if multihome, err := manager.readMultihome(); err != nil {
		return err
	} else if prefix, err := netip.ParsePrefix(multihome.Network.CIDR); err == nil && prefix.Contains(parsed) {
		return fmt.Errorf("--address %s is inside the world's second network %s, where a VM's second NIC could hold it; choose an address outside it, such as 198.18.0.10", parsed, multihome.Network.CIDR)
	}
	if hostHoldsAddress(parsed) {
		return fmt.Errorf("--address %s is assigned to a host interface; choose an address no host interface holds", parsed)
	}
	for other, existing := range nat {
		if other != node.Name && existing == parsed.String() {
			return fmt.Errorf("--address %s already translates to %s", parsed, other)
		}
	}
	// Another world's rule for the address would win or lose by OUTPUT order,
	// so this host could reach a different VM there.
	rules, err := manager.iptables(ctx, "-S")
	if err != nil {
		return err
	}
	own := natChain(world.World.RunID)
	for _, line := range strings.Split(string(rules), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" || fields[1] == own {
			continue
		}
		if strings.Contains(line, " -d "+parsed.String()+"/32 ") && strings.Contains(line, " -j DNAT") {
			return fmt.Errorf("--address %s is already translated by host nat chain %s, such as another katldev world's; choose another address", parsed, fields[1])
		}
	}

	nat[node.Name] = parsed.String()
	if err := manager.writeNAT(nat); err != nil {
		return err
	}
	if err := manager.syncNAT(ctx, world, true); err != nil {
		return fmt.Errorf("recorded NAT address %s for %s but could not install the host rule: %w; rerun katldev world nat %s --address %s", parsed, node.Name, err, node.Name, parsed)
	}
	fmt.Fprintf(manager.stdout, "This host reaches %s at %s and translates it to %s, which the VM holds.\n", node.Name, parsed, node.Address)
	fmt.Fprintf(manager.stdout, "In %s, give %s:\n", manager.clusterConfigPath(), node.Name)
	fmt.Fprintf(manager.stdout, "  management:\n    address: %s\n  kubernetes:\n    address: %s\n", parsed, node.Address)
	return nil
}

func (manager installerManager) removeNAT(ctx context.Context, name string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	nat, err := manager.readNAT()
	if err != nil {
		return err
	}
	address, ok := nat[name]
	if !ok {
		return fmt.Errorf("%s has no NAT address", name)
	}
	delete(nat, name)
	if err := manager.writeNAT(nat); err != nil {
		return err
	}
	if err := manager.syncNAT(ctx, world, true); err != nil {
		return err
	}
	fmt.Fprintf(manager.stdout, "This host no longer translates %s to %s.\n", address, name)
	return nil
}

func hostHoldsAddress(address netip.Addr) bool {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, candidate := range addresses {
		if prefix, err := netip.ParsePrefix(candidate.String()); err == nil && prefix.Addr().Unmap() == address {
			return true
		}
	}
	return false
}
