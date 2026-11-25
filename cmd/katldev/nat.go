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

// natChainPrefix marks the nat-table chains katldev owns.
const natChainPrefix = "KATLDEV-"

func natChain(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return natChainPrefix + hex.EncodeToString(sum[:4])
}

// removeStaleNATChains removes every katldev chain that no live world on the
// libvirt host owns, such as one left by a world whose ID was reset or whose
// removal failed. Liveness comes from the world networks libvirt reports, so
// no record of earlier worlds is needed. It returns nil only when none remain.
func (manager installerManager) removeStaleNATChains(ctx context.Context, own string) error {
	output, err := manager.virsh(ctx, manager.libvirtURI(), "net-list", "--all", "--name")
	if err != nil {
		return err
	}
	live := map[string]bool{own: true}
	for _, name := range strings.Fields(string(output)) {
		if runID, ok := strings.CutPrefix(name, "katl-"); ok && worldIDPattern.MatchString(runID) {
			live[natChain(runID)] = true
		}
	}
	table, err := manager.readNATTable(ctx)
	if err != nil {
		return err
	}
	for chain := range table.chains {
		if strings.HasPrefix(chain, natChainPrefix) && !live[chain] {
			if err := manager.removeNATChain(ctx, chain); err != nil {
				return fmt.Errorf("remove stale katldev nat chain %s: %w", chain, err)
			}
		}
	}
	return nil
}

// natTable is the host nat table as iptables -S reports it. Only a
// successful read says what is installed; a failed read is never evidence
// that nothing is.
type natTable struct {
	chains map[string]bool
	rules  []string
}

func (manager installerManager) readNATTable(ctx context.Context) (natTable, error) {
	output, err := manager.iptables(ctx, "-S")
	if err != nil {
		return natTable{}, err
	}
	table := natTable{chains: map[string]bool{}}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "-N", "-P":
			table.chains[fields[1]] = true
		case "-A":
			table.rules = append(table.rules, line)
		}
	}
	return table, nil
}

// jumps counts the rules in OUTPUT that jump to chain.
func (table natTable) jumps(chain string) int {
	count := 0
	for _, rule := range table.rules {
		if strings.TrimSpace(rule) == "-A OUTPUT -j "+chain {
			count++
		}
	}
	return count
}

// syncNAT converges the host on the world's recorded NAT addresses when a
// world command runs, so a readdress or a host firewall reload converges on
// the next command. A host that cannot run iptables without a password must
// not block commands that never asked for NAT, so a failure is reported and
// the command continues.
func (manager installerManager) syncNAT(ctx context.Context, world katldevWorld) error {
	nat, err := manager.readNAT()
	if err != nil || len(nat) == 0 {
		return err
	}
	if err := manager.applyNAT(ctx, world, nat); err != nil {
		names := make([]string, 0, len(nat))
		for name := range nat {
			names = append(names, name)
		}
		sort.Strings(names)
		rerun := fmt.Sprintf("katldev world nat %s --address %s", names[0], nat[names[0]])
		fmt.Fprintf(manager.stderr, "katldev world: %v\n", natFailure(err, true, rerun))
	}
	return nil
}

// applyNAT makes the host translate each NAT address in nat to its node's
// reserved address through the world's chain, and removes the chain when nat
// is empty. nat.json is the world's record of what it wants installed; it
// must outlive any failure here so that a later command can retry or clean up.
func (manager installerManager) applyNAT(ctx context.Context, world katldevWorld, nat map[string]string) error {
	chain := natChain(world.World.RunID)
	if len(nat) == 0 {
		return manager.removeNATChain(ctx, chain)
	}
	names := make([]string, 0, len(nat))
	destinations := map[string]string{}
	for name := range nat {
		node, ok := world.Scenario.Node(name)
		if !ok {
			return fmt.Errorf("%s gives NAT address %s to %s, which the world no longer has; run katldev world nat %s --remove", manager.natPath(), nat[name], name, name)
		}
		names = append(names, name)
		destinations[name] = node.Address
	}
	sort.Strings(names)

	table, err := manager.readNATTable(ctx)
	if err != nil {
		return err
	}
	if !table.chains[chain] {
		if _, err := manager.iptables(ctx, "-N", chain); err != nil {
			return err
		}
	}
	if _, err := manager.iptables(ctx, "-F", chain); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := manager.iptables(ctx, "-A", chain, "-d", nat[name]+"/32", "-j", "DNAT", "--to-destination", destinations[name]); err != nil {
			return err
		}
	}
	if table.jumps(chain) == 0 {
		if _, err := manager.iptables(ctx, "-A", "OUTPUT", "-j", chain); err != nil {
			return err
		}
	}
	return nil
}

// removeNATChain removes chain and every jump to it. It returns nil only when
// the chain is gone: failing to read the table or to delete a rule is an
// error, so the caller keeps its record.
func (manager installerManager) removeNATChain(ctx context.Context, chain string) error {
	table, err := manager.readNATTable(ctx)
	if err != nil {
		return err
	}
	if !table.chains[chain] {
		return nil
	}
	// Remove every jump, including duplicates an interrupted sync left.
	for range table.jumps(chain) {
		if _, err := manager.iptables(ctx, "-D", "OUTPUT", "-j", chain); err != nil {
			return err
		}
	}
	if _, err := manager.iptables(ctx, "-F", chain); err != nil {
		return err
	}
	_, err = manager.iptables(ctx, "-X", chain)
	return err
}

// hostNATError marks a failure to read or change the host nat table. Only
// iptables creates it, and natFailure is the only place that explains it.
type hostNATError struct{ err error }

func (e hostNATError) Error() string { return e.err.Error() }
func (e hostNATError) Unwrap() error { return e.err }

// natFailure is the operator's message for a failed host NAT operation, so
// every path that touches the host rules gives the same guidance. recordKept
// says that nat.json still holds the address, and rerun is the command that
// retries once the cause is fixed. Errors that did not come from iptables
// pass through unchanged.
func natFailure(err error, recordKept bool, rerun string) error {
	if !errors.As(err, &hostNATError{}) {
		return err
	}
	kept := ""
	if recordKept {
		kept = "; its record is kept"
	}
	return fmt.Errorf("could not read or change the host NAT rules%s: %w; katldev needs sudo that does not prompt for iptables (check that sudo -n iptables -L works as this user), then rerun: %s", kept, err, rerun)
}

// iptables edits the host nat table, which needs root; sudo must not prompt.
func (manager installerManager) iptables(ctx context.Context, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-w", "-t", "nat"}, args...)
	if manager.runIptables != nil {
		output, err := manager.runIptables(ctx, fullArgs...)
		if err != nil {
			return nil, hostNATError{err}
		}
		return output, nil
	}
	command := append([]string{firstNonEmpty(os.Getenv("KATL_VMTEST_SUDO"), "sudo"), "-n", "iptables"}, fullArgs...)
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	if err != nil {
		return nil, hostNATError{fmt.Errorf("%s: %w: %s", strings.Join(command, " "), err, strings.TrimSpace(string(output)))}
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
	// A chain left by an earlier world would otherwise translate the address
	// to a VM that no longer exists, and make it look taken.
	rerun := fmt.Sprintf("katldev world nat %s --address %s", node.Name, parsed)
	if err := manager.removeStaleNATChains(ctx, natChain(world.World.RunID)); err != nil {
		return natFailure(err, false, rerun)
	}
	// Another world's rule for the address would win or lose by OUTPUT order,
	// so this host could reach a different VM there.
	table, err := manager.readNATTable(ctx)
	if err != nil {
		return natFailure(err, false, rerun)
	}
	own := natChain(world.World.RunID)
	for _, line := range table.rules {
		fields := strings.Fields(line)
		if fields[1] == own {
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
	if err := manager.applyNAT(ctx, world, nat); err != nil {
		return natFailure(err, true, rerun)
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
	rerun := "katldev world nat " + name + " --remove"
	if err := manager.removeStaleNATChains(ctx, natChain(world.World.RunID)); err != nil {
		return natFailure(err, true, rerun)
	}
	// The record stays until the host rules are gone, so a failed removal
	// can be retried.
	remaining := map[string]string{}
	for other, existing := range nat {
		if other != name {
			remaining[other] = existing
		}
	}
	if err := manager.applyNAT(ctx, world, remaining); err != nil {
		return natFailure(err, true, rerun)
	}
	if err := manager.writeNAT(remaining); err != nil {
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
