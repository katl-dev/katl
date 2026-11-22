package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/spf13/cobra"
)

const (
	worldScenario    = "katldev"
	worldOwnerPrefix = "katl/katldev-world/"
)

var worldNodeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// katldevWorld is the persistent world of one checkout: its vmtest world
// manifest, the scenario that records node addresses and MACs, and the
// libvirt network it owns.
type katldevWorld struct {
	World    vmtest.World
	Scenario *vmtest.WorldScenario
}

func (world katldevWorld) owner() string {
	return worldOwnerPrefix + world.World.RunID
}

func (world katldevWorld) network() vmtest.OwnedNetwork {
	network := vmtest.OwnedNetwork{
		Name:    world.World.Network.Name,
		Owner:   world.owner(),
		CIDR:    world.World.Network.CIDR,
		Gateway: world.World.Network.Gateway,
	}
	for _, node := range world.Scenario.Nodes {
		network.Hosts = append(network.Hosts, vmtest.DHCPHost{Name: node.Name, MACAddress: node.MACAddress, Address: node.Address})
	}
	return network
}

func (manager installerManager) worldDir() string {
	return filepath.Join(manager.repoRoot, "_build", "katldev", "world")
}

func (manager installerManager) worldManifestPath() string {
	return filepath.Join(manager.worldDir(), "world.json")
}

func (manager installerManager) worldIDPath() string {
	return filepath.Join(manager.repoRoot, "_build", "katldev", "world-id")
}

var worldIDPattern = regexp.MustCompile(`^dev-world-[0-9a-f]{8}$`)

// worldID returns the checkout's world ID, which names and owns the world's
// libvirt objects. It is random, not derived from the checkout path, so
// checkouts at the same path in different containers sharing one libvirt
// never own each other's worlds. It is kept beside cluster.yaml rather than
// in the world directory, so a reset or remove keeps the world's names, MACs,
// and preferred subnet. A world recorded before the ID file existed keeps its
// recorded ID. When create is false and no ID exists, worldID returns "".
func (manager installerManager) worldID(create bool) (string, error) {
	path := manager.worldIDPath()
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !worldIDPattern.MatchString(id) {
			return "", fmt.Errorf("%s holds %q, not a katldev world ID; run katldev world remove, delete the file, and start again", path, id)
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id := ""
	if world, err := vmtest.LoadWorld(manager.worldManifestPath()); err == nil && worldIDPattern.MatchString(world.RunID) {
		id = world.RunID
	} else if !create {
		return "", nil
	} else {
		random := make([]byte, 4)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		id = "dev-world-" + hex.EncodeToString(random)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// O_EXCL lets a concurrent first start win; the loser adopts its ID.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return manager.worldID(create)
	}
	if err != nil {
		return "", err
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	return id, file.Close()
}

func (manager installerManager) libvirtURI() string {
	return firstNonEmpty(os.Getenv("KATL_VMTEST_LIBVIRT_URI"), "qemu:///system")
}

func (manager installerManager) planWorld(runID, cidr, gateway string) vmtest.World {
	dir := manager.worldDir()
	return vmtest.World{
		APIVersion:  vmtest.WorldAPIVersion,
		Kind:        vmtest.WorldKind,
		RunID:       runID,
		RunDir:      dir,
		CacheDir:    vmtest.DefaultVMTestCacheDir(manager.repoRoot),
		ArtifactDir: filepath.Join(manager.repoRoot, "_build", "mkosi"),
		ScenarioDir: filepath.Join(dir, "scenarios"),
		Libvirt: vmtest.WorldLibvirt{
			URI:          manager.libvirtURI(),
			Network:      "katl-" + runID,
			StoragePath:  dir,
			DomainPrefix: "katl-" + runID + "-",
		},
		Network: vmtest.WorldNetwork{
			Backend:   vmtest.NetworkLibvirt,
			Name:      "katl-" + runID,
			CIDR:      cidr,
			Gateway:   gateway,
			LeaseFile: filepath.Join(dir, "network", "leases.json"),
		},
		Capabilities: map[string]vmtest.WorldStatus{
			"libvirt":         vmtest.WorldStatusPassed,
			"libvirt-network": vmtest.WorldStatusPassed,
		},
	}
}

func (manager installerManager) loadWorld() (katldevWorld, error) {
	world, err := vmtest.LoadWorld(manager.worldManifestPath())
	if err != nil {
		return katldevWorld{}, err
	}
	id, err := manager.worldID(false)
	if err != nil {
		return katldevWorld{}, err
	}
	if world.RunID != id || world.RunDir != manager.worldDir() {
		return katldevWorld{}, fmt.Errorf("%s describes another checkout's world; run katldev world remove", manager.worldManifestPath())
	}
	scenario, err := world.LoadScenario(worldScenario)
	if err != nil {
		return katldevWorld{}, err
	}
	return katldevWorld{World: world, Scenario: scenario}, nil
}

// createWorld records a new world on a subnet that no libvirt network or host
// interface uses. It does not create libvirt objects.
func (manager installerManager) createWorld(ctx context.Context) (katldevWorld, error) {
	id, err := manager.worldID(true)
	if err != nil {
		return katldevWorld{}, err
	}
	inUse, err := manager.networksInUse(ctx, manager.libvirtURI())
	if err != nil {
		return katldevWorld{}, err
	}
	cidr, gateway, err := vmtest.ChooseNetworkCIDR(id, inUse)
	if err != nil {
		return katldevWorld{}, err
	}
	world := manager.planWorld(id, cidr, gateway)
	if err := vmtest.ValidateWorld(world); err != nil {
		return katldevWorld{}, fmt.Errorf("plan katldev world: %w", err)
	}
	if err := os.MkdirAll(world.RunDir, 0o755); err != nil {
		return katldevWorld{}, err
	}
	scenario, err := world.PlanScenario(worldScenario)
	if err != nil {
		return katldevWorld{}, err
	}
	if err := scenario.WriteManifest(); err != nil {
		return katldevWorld{}, err
	}
	if err := writeJSONFile(manager.worldManifestPath(), world); err != nil {
		return katldevWorld{}, err
	}
	// Reopen the scenario so that it reloads nodes other katldev processes add.
	if scenario, err = world.LoadScenario(worldScenario); err != nil {
		return katldevWorld{}, err
	}
	return katldevWorld{World: world, Scenario: scenario}, nil
}

func (manager installerManager) networksInUse(ctx context.Context, uri string) ([]*net.IPNet, error) {
	var inUse []*net.IPNet
	output, err := manager.virsh(ctx, uri, "net-list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Fields(string(output)) {
		data, err := manager.virsh(ctx, uri, "net-dumpxml", name)
		if err != nil {
			return nil, err
		}
		subnet, err := vmtest.LibvirtNetworkIPv4(data)
		if err != nil {
			return nil, fmt.Errorf("read libvirt network %s: %w", name, err)
		}
		inUse = append(inUse, subnet)
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if subnet, ok := address.(*net.IPNet); ok && subnet.IP.To4() != nil {
			inUse = append(inUse, &net.IPNet{IP: subnet.IP.Mask(subnet.Mask), Mask: subnet.Mask})
		}
	}
	// Routes cover networks without a local address, such as VPN routes.
	if data, err := os.ReadFile("/proc/net/route"); err == nil {
		inUse = append(inUse, routedSubnets(data)...)
	}
	return inUse, nil
}

// routedSubnets returns the IPv4 destinations of /proc/net/route, except the
// default route, which overlaps every subnet.
func routedSubnets(data []byte) []*net.IPNet {
	var subnets []*net.IPNet
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		destination, err := strconv.ParseUint(fields[1], 16, 32)
		if err != nil {
			continue
		}
		mask, err := strconv.ParseUint(fields[7], 16, 32)
		if err != nil || mask == 0 {
			continue
		}
		// The kernel prints the network-order address as a host-order word.
		ip, netmask := make(net.IP, 4), make(net.IPMask, 4)
		binary.NativeEndian.PutUint32(ip, uint32(destination))
		binary.NativeEndian.PutUint32(netmask, uint32(mask))
		subnets = append(subnets, &net.IPNet{IP: ip.Mask(netmask), Mask: netmask})
	}
	return subnets
}

// addWorldNode reserves the next address and MAC for a node. The name defaults
// to the next cp-N or worker-N, matching katlctl config init.
func addWorldNode(world katldevWorld, name string, controlPlane bool) (vmtest.Node, error) {
	role := vmtest.Worker
	prefix := "worker-"
	if controlPlane {
		role = vmtest.ControlPlane
		prefix = "cp-"
	}
	if name == "" {
		for i := 1; ; i++ {
			candidate := prefix + strconv.Itoa(i)
			if _, exists := world.Scenario.Node(candidate); !exists {
				name = candidate
				break
			}
		}
	}
	if !worldNodeName.MatchString(name) {
		return vmtest.Node{}, fmt.Errorf("node name %q must be a lowercase DNS label of at most 32 characters", name)
	}
	if _, exists := world.Scenario.Node(name); exists {
		return vmtest.Node{}, fmt.Errorf("world already has node %s", name)
	}
	return world.Scenario.AddNode(vmtest.NodeSpec{Name: name, Role: role})
}

// syncNetwork makes the world's libvirt network exist, run, and reserve
// exactly the scenario's node addresses. The scenario is the source of truth,
// so an interrupted readdress or add-node converges on the next command.
func (manager installerManager) syncNetwork(ctx context.Context, world katldevWorld) error {
	uri := world.World.Libvirt.URI
	desired := world.network()
	exists, err := manager.networkExists(ctx, uri, desired.Name)
	if err != nil {
		return err
	}
	defined := false
	if !exists {
		definition, err := desired.XML()
		if err != nil {
			return err
		}
		path := filepath.Join(world.World.RunDir, "network", "network.xml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(definition), 0o644); err != nil {
			return err
		}
		if _, err := manager.virsh(ctx, uri, "net-define", path); err != nil {
			return err
		}
		defined = true
	}
	if err := manager.requireOwnedNetwork(ctx, uri, desired.Name, desired.Owner); err != nil {
		if !defined {
			return err
		}
		// A libvirt without network metadata accepts the definition but drops
		// the ownership marker, which would leave a network katldev refuses to
		// manage or remove.
		if _, undefineErr := manager.virsh(ctx, uri, "net-undefine", desired.Name); undefineErr != nil {
			return fmt.Errorf("libvirt network %s lost its ownership metadata and could not be undefined: %w; remove it with virsh net-undefine %s", desired.Name, undefineErr, desired.Name)
		}
		return fmt.Errorf("libvirt did not keep the ownership metadata of network %s, so it was undefined again; katldev world needs libvirt 9.7 or newer for network metadata: %w", desired.Name, err)
	}
	active, err := manager.networkActive(ctx, uri, desired.Name)
	if err != nil {
		return err
	}
	if !active {
		if _, err := manager.virsh(ctx, uri, "net-start", desired.Name); err != nil {
			return fmt.Errorf("start libvirt network %s on %s: %w; if another network, VPN, or route now uses that subnet, run katldev world reset to recreate the world on a free subnet", desired.Name, desired.CIDR, err)
		}
	}

	data, err := manager.virsh(ctx, uri, "net-dumpxml", desired.Name, "--inactive")
	if err != nil {
		return err
	}
	current, err := vmtest.LibvirtNetworkHosts(data)
	if err != nil {
		return fmt.Errorf("read libvirt network %s reservations: %w", desired.Name, err)
	}
	byMAC := map[string]vmtest.DHCPHost{}
	for _, host := range current {
		byMAC[strings.ToLower(host.MACAddress)] = host
	}
	update := func(command string, host vmtest.DHCPHost) error {
		element, err := desired.HostXML(host)
		if err != nil {
			return err
		}
		_, err = manager.virsh(ctx, uri, "net-update", desired.Name, command, "ip-dhcp-host", element, "--live", "--config")
		return err
	}
	for _, host := range desired.Hosts {
		mac := strings.ToLower(host.MACAddress)
		existing, ok := byMAC[mac]
		delete(byMAC, mac)
		switch {
		case !ok:
			err = update("add-last", host)
		case existing.Address != host.Address || existing.Name != host.Name:
			err = update("modify", host)
		}
		if err != nil {
			return err
		}
	}
	for _, stale := range byMAC {
		if err := update("delete", stale); err != nil {
			return err
		}
	}
	return nil
}

func (manager installerManager) networkExists(ctx context.Context, uri, name string) (bool, error) {
	output, err := manager.virsh(ctx, uri, "net-list", "--all", "--name")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Fields(string(output)) {
		if line == name {
			return true, nil
		}
	}
	return false, nil
}

func (manager installerManager) networkActive(ctx context.Context, uri, name string) (bool, error) {
	output, err := manager.virsh(ctx, uri, "net-info", name)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Active") {
			return strings.EqualFold(strings.TrimSpace(value), "yes"), nil
		}
	}
	return false, fmt.Errorf("virsh net-info %s did not report whether it is active", name)
}

func (manager installerManager) requireOwnedNetwork(ctx context.Context, uri, name, owner string) error {
	output, err := manager.virsh(ctx, uri, "net-metadata", name, vmtestMetadataURI)
	if err != nil {
		return fmt.Errorf("refusing to manage libvirt network %s without katldev world ownership metadata: %w", name, err)
	}
	got, err := domainOwner(output)
	if err != nil {
		return fmt.Errorf("decode libvirt network %s ownership metadata: %w", name, err)
	}
	if got != owner {
		return fmt.Errorf("refusing to manage libvirt network %s owned by %q", name, got)
	}
	return nil
}

// removeWorld deletes exactly the libvirt domains and network that carry
// this checkout's world ownership, then the world's files. A legacy single-VM
// installer is removed the same way under its own ownership marker.
// _build/katldev/cluster.yaml is operator input and is kept.
func (manager installerManager) removeWorld(ctx context.Context) error {
	runID, err := manager.worldID(false)
	if err != nil {
		return err
	}
	if runID != "" {
		if err := manager.removeWorldObjects(ctx, runID); err != nil {
			return err
		}
	}

	if legacy, err := manager.readLegacyState(); err == nil {
		if err := manager.removeDomainIfPresent(ctx, legacy.LibvirtURI, legacy.DomainName, legacyDomainMetadata); err != nil {
			return err
		}
		fmt.Fprintf(manager.stderr, "katldev world: removed earlier single installer VM %s\n", legacy.DomainName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(manager.legacyStateRoot()); err != nil {
		return err
	}
	return os.RemoveAll(manager.worldDir())
}

func (manager installerManager) removeWorldObjects(ctx context.Context, runID string) error {
	uri := manager.libvirtURI()
	owner := worldOwnerPrefix + runID
	prefix := "katl-" + runID + "-"
	output, err := manager.virsh(ctx, uri, "list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, domain := range strings.Fields(string(output)) {
		if !strings.HasPrefix(domain, prefix) {
			continue
		}
		if err := manager.removeDomain(ctx, uri, domain, owner); err != nil {
			return err
		}
		fmt.Fprintf(manager.stderr, "katldev world: removed VM %s\n", domain)
	}

	network := "katl-" + runID
	exists, err := manager.networkExists(ctx, uri, network)
	if err != nil {
		return err
	}
	if exists {
		if err := manager.requireOwnedNetwork(ctx, uri, network, owner); err != nil {
			return err
		}
		active, err := manager.networkActive(ctx, uri, network)
		if err != nil {
			return err
		}
		if active {
			if _, err := manager.virsh(ctx, uri, "net-destroy", network); err != nil {
				return err
			}
		}
		if _, err := manager.virsh(ctx, uri, "net-undefine", network); err != nil {
			return err
		}
		fmt.Fprintf(manager.stderr, "katldev world: removed network %s\n", network)
	}
	return nil
}

type worldStartOptions struct {
	installerOptions
	Nodes int
}

func newWorldCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "world",
		Short: "Manage a persistent multi-node KatlOS VM world for manual journeys",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newWorldStartCommand(ctx, stdout, stderr, "start", "Create or resume the world and its installer VMs"))
	cmd.AddCommand(newWorldStartCommand(ctx, stdout, stderr, "reset", "Remove the world and boot fresh installer VMs"))
	cmd.AddCommand(newWorldAddNodeCommand(ctx, stdout, stderr))
	cmd.AddCommand(newWorldStatusCommand(ctx, stdout, stderr))
	cmd.AddCommand(newWorldStopCommand(ctx, stdout, stderr, "stop [NODE...]"))
	cmd.AddCommand(newWorldConsoleCommand(ctx, stdout, stderr, "console [NODE]"))
	cmd.AddCommand(newWorldReaddressCommand(ctx, stdout, stderr))
	cmd.AddCommand(&cobra.Command{
		Use:   "remove",
		Short: "Delete the world's VMs, disks, and network",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			if err := manager.removeWorld(ctx); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Removed the katldev world. _build/katldev/cluster.yaml is kept.")
			return nil
		},
	})
	return cmd
}

func addInstallerFlags(cmd *cobra.Command, opts *installerOptions) {
	cmd.Flags().IntVar(&opts.MemoryMiB, "memory", opts.MemoryMiB, "guest memory in MiB")
	cmd.Flags().IntVar(&opts.CPUs, "cpus", opts.CPUs, "guest virtual CPUs")
	cmd.Flags().StringVar(&opts.DiskSize, "disk-size", opts.DiskSize, "fresh target disk size")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "installer DHCP and readiness timeout")
	cmd.Flags().BoolVar(&opts.NoBuild, "no-build", false, "use the existing installer ISO without checking the current checkout")
	cmd.Flags().BoolVar(&opts.ForceConfig, "force-config", false, "replace the generated katldev ClusterConfig")
}

func defaultInstallerOptions() installerOptions {
	return installerOptions{MemoryMiB: 4096, CPUs: 2, DiskSize: "32G", Timeout: 3 * time.Minute}
}

func newWorldStartCommand(ctx context.Context, stdout, stderr io.Writer, name, short string) *cobra.Command {
	opts := worldStartOptions{installerOptions: defaultInstallerOptions()}
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("nodes") {
				opts.Nodes = 0
			}
			return manager.startWorld(ctx, opts, name == "reset")
		},
	}
	addInstallerFlags(cmd, &opts.installerOptions)
	cmd.Flags().IntVar(&opts.Nodes, "nodes", 1, "installer VMs in a new world: cp-1, then worker-1 and onward")
	return cmd
}

func newWorldAddNodeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := defaultInstallerOptions()
	var controlPlane bool
	cmd := &cobra.Command{
		Use:   "add-node [NAME]",
		Short: "Boot another installer VM in the world",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return manager.addNode(ctx, opts, name, controlPlane)
		},
	}
	addInstallerFlags(cmd, &opts)
	cmd.Flags().BoolVar(&controlPlane, "control-plane", false, "name the VM cp-N and suggest a control-plane cluster.yaml entry")
	return cmd
}

func newWorldStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the world's network, VMs, addresses, and installer state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.status(ctx)
		},
	}
}

func newWorldStopCommand(ctx context.Context, stdout, stderr io.Writer, use string) *cobra.Command {
	var force bool
	timeout := 3 * time.Minute
	cmd := &cobra.Command{
		Use:   use,
		Short: "Request guest shutdown and wait for the VMs to power off",
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.stop(ctx, args, timeout, force)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "guest shutdown timeout per VM")
	cmd.Flags().BoolVar(&force, "force", false, "cut power immediately, bypassing guest shutdown")
	return cmd
}

func newWorldConsoleCommand(ctx context.Context, stdout, stderr io.Writer, use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Attach to a VM serial console (defaults to the first VM)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return manager.console(ctx, name)
		},
	}
}

func newWorldReaddressCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "readdress NODE",
		Short: "Move a VM's DHCP reservation; the VM takes the address at its next boot",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.readdress(ctx, args[0], address)
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "new address inside the world network; defaults to the first free address")
	return cmd
}

func (manager installerManager) requireWorld() (katldevWorld, error) {
	world, err := manager.loadWorld()
	if errors.Is(err, os.ErrNotExist) {
		return katldevWorld{}, errors.New("katldev world does not exist; run katldev world start")
	}
	return world, err
}

func (manager installerManager) startWorld(ctx context.Context, opts worldStartOptions, reset bool) error {
	if err := validateInstallerOptions(opts.installerOptions); err != nil {
		return err
	}
	if opts.Nodes < 0 {
		return errors.New("--nodes must be positive")
	}

	world, err := manager.loadWorld()
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if !reset {
			return fmt.Errorf("%w; run katldev world reset to recreate the world", err)
		}
	}
	if !exists && !reset {
		if legacy, err := manager.readLegacyState(); err == nil {
			return fmt.Errorf("this checkout still has the single installer VM %s from an earlier katldev; run katldev world reset to replace it with a world (this erases that VM's disk and keeps _build/katldev/cluster.yaml)", legacy.DomainName)
		}
	}
	if exists && !reset {
		if opts.Nodes != 0 && opts.Nodes != len(world.Scenario.Nodes) {
			return fmt.Errorf("the world already has %d VMs; use katldev world add-node, or katldev world reset --nodes %d", len(world.Scenario.Nodes), opts.Nodes)
		}
		return manager.resumeWorld(ctx, world, opts.installerOptions)
	}

	specs := worldNodeSpecs(world, exists, opts.Nodes)
	iso, digest, err := manager.prepareInstaller(ctx, opts.NoBuild)
	if err != nil {
		return err
	}
	if err := manager.removeWorld(ctx); err != nil {
		return err
	}
	world, err = manager.createWorld(ctx)
	if err != nil {
		return err
	}
	var created []vmtest.Node
	for _, spec := range specs {
		node, err := addWorldNode(world, spec.Name, spec.Role == vmtest.ControlPlane)
		if err != nil {
			return err
		}
		created = append(created, node)
	}
	if err := manager.syncNetwork(ctx, world); err != nil {
		return err
	}
	for _, node := range created {
		if _, err := manager.createNode(ctx, world, node, opts.installerOptions, iso, digest); err != nil {
			return err
		}
	}
	return manager.report(ctx, world, opts.installerOptions, nil)
}

// worldNodeSpecs chooses the VMs of a new or reset world. A reset keeps the
// existing names and roles unless --nodes asks for a different count; a
// counted world is cp-1 followed by workers, matching katlctl config init.
func worldNodeSpecs(world katldevWorld, exists bool, count int) []vmtest.NodeSpec {
	if exists && (count == 0 || count == len(world.Scenario.Nodes)) && len(world.Scenario.Nodes) > 0 {
		specs := make([]vmtest.NodeSpec, 0, len(world.Scenario.Nodes))
		for _, node := range world.Scenario.Nodes {
			specs = append(specs, vmtest.NodeSpec{Name: node.Name, Role: node.Role})
		}
		return specs
	}
	if count == 0 {
		count = 1
	}
	specs := []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}}
	for i := 1; i < count; i++ {
		specs = append(specs, vmtest.NodeSpec{Name: fmt.Sprintf("worker-%d", i), Role: vmtest.Worker})
	}
	return specs
}

func (manager installerManager) resumeWorld(ctx context.Context, world katldevWorld, opts installerOptions) error {
	if err := manager.syncNetwork(ctx, world); err != nil {
		return err
	}
	var iso, digest string
	for _, node := range world.Scenario.Nodes {
		state, err := manager.readState(world, node)
		exists := false
		if err == nil {
			exists, err = manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if exists {
			if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName, world.owner()); err != nil {
				return err
			}
			if err := manager.resume(ctx, state); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(manager.stderr, "katldev world: VM for %s is missing; recreating it with a fresh disk\n", node.Name)
		if iso == "" {
			if iso, digest, err = manager.prepareInstaller(ctx, opts.NoBuild); err != nil {
				return err
			}
		}
		if _, err := manager.createNode(ctx, world, node, opts, iso, digest); err != nil {
			return err
		}
	}
	return manager.report(ctx, world, opts, nil)
}

func (manager installerManager) addNode(ctx context.Context, opts installerOptions, name string, controlPlane bool) error {
	if err := validateInstallerOptions(opts); err != nil {
		return err
	}
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	iso, digest, err := manager.prepareInstaller(ctx, opts.NoBuild)
	if err != nil {
		return err
	}
	node, err := addWorldNode(world, name, controlPlane)
	if err != nil {
		return err
	}
	if err := manager.syncNetwork(ctx, world); err != nil {
		return err
	}
	if _, err := manager.createNode(ctx, world, node, opts, iso, digest); err != nil {
		return err
	}
	return manager.report(ctx, world, opts, []string{node.Name})
}

func (manager installerManager) worldNode(world katldevWorld, name string) (vmtest.Node, error) {
	if name == "" {
		if len(world.Scenario.Nodes) == 0 {
			return vmtest.Node{}, errors.New("the world has no VMs; run katldev world add-node")
		}
		return world.Scenario.Nodes[0], nil
	}
	node, ok := world.Scenario.Node(name)
	if !ok {
		var names []string
		for _, node := range world.Scenario.Nodes {
			names = append(names, node.Name)
		}
		return vmtest.Node{}, fmt.Errorf("the world has no VM %s; VMs: %s", name, strings.Join(names, ", "))
	}
	return node, nil
}

func (manager installerManager) readdress(ctx context.Context, name, address string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	node, err := manager.worldNode(world, name)
	if err != nil {
		return err
	}
	// Observe first so that a VM that already rebooted onto an earlier
	// readdress is recorded at that address before it moves again.
	if _, err := manager.observe(ctx, world, node); err != nil {
		return err
	}
	node, err = world.Scenario.ReassignAddress(node.Name, address)
	if err != nil {
		return err
	}
	if err := manager.syncNetwork(ctx, world); err != nil {
		return fmt.Errorf("reserved %s for %s but could not update libvirt network %s: %w; rerun katldev world start to apply the reservation", node.Address, node.Name, world.World.Network.Name, err)
	}
	if node.PreviousAddress == "" {
		fmt.Fprintf(manager.stdout, "Reserved %s for %s, the address its VM already holds.\n", node.Address, node.Name)
		return nil
	}
	previous := node.PreviousAddress
	fmt.Fprintf(manager.stdout, "Reserved %s for %s (currently %s).\n", node.Address, node.Name, previous)
	fmt.Fprintf(manager.stdout, "The VM keeps %s, which stays reserved for it, until it next boots.\n", previous)
	configName, err := matchingKatldevConfig(manager.clusterConfigPath(), previous)
	if err != nil {
		return nil
	}
	config := manager.clusterConfigPath()
	fmt.Fprintln(manager.stdout)
	fmt.Fprintln(manager.stdout, "Re-address the worker (docs/operations/change-cluster-nodes.md#re-address-a-worker):")
	fmt.Fprintf(manager.stdout, "  set %s management.address to %s in %s\n", configName, node.Address, config)
	fmt.Fprintf(manager.stdout, "  katlctl cluster apply --config %s --node %s --endpoint %s\n", config, configName, previous)
	fmt.Fprintf(manager.stdout, "  katlctl node reboot %s --config %s --endpoint %s\n", configName, config, previous)
	fmt.Fprintf(manager.stdout, "  katlctl cluster apply --config %s --node %s\n", config, configName)
	return nil
}
