package vmtest

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// vmtestMetadata is the metadata value of the libvirt objects that
// scripts/vmtest-run worlds create. katldev objects carry other values and are
// never swept.
const vmtestMetadata = "katl/vmtest"

const vmtestMetadataURI = "https://katlos.io/xmlns/vmtest/1"

// virshFunc runs a virsh command on the libvirt connection that owns the
// objects it manages.
type virshFunc = func(ctx context.Context, args ...string) ([]byte, error)

// ErrNetworkStart marks a failure to start a libvirt network, which usually
// means another network, interface or route already uses its subnet.
var ErrNetworkStart = errors.New("start libvirt network")

// Ensure makes the network exist, run and reserve exactly Hosts. A network
// that exists must carry this network's ownership metadata, so Ensure never
// manages one it did not create. definition is where the network's XML is
// written before it is defined.
func (network OwnedNetwork) Ensure(ctx context.Context, virsh virshFunc, definition string) error {
	exists, err := networkExists(ctx, virsh, network.Name)
	if err != nil {
		return err
	}
	defined := false
	if !exists {
		data, err := network.XML()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(definition), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(definition, []byte(data), 0o644); err != nil {
			return err
		}
		if _, err := virsh(ctx, "net-define", definition); err != nil {
			return err
		}
		defined = true
	}
	if err := network.requireOwned(ctx, virsh); err != nil {
		if !defined {
			return err
		}
		// A libvirt without network metadata accepts the definition but drops
		// the ownership marker, which would leave a network nothing may manage
		// or remove.
		if _, undefineErr := virsh(ctx, "net-undefine", network.Name); undefineErr != nil {
			return fmt.Errorf("libvirt network %s lost its ownership metadata and could not be undefined: %w; remove it with virsh net-undefine %s", network.Name, undefineErr, network.Name)
		}
		return fmt.Errorf("libvirt did not keep the ownership metadata of network %s, so it was undefined again; vmtest needs libvirt 9.7 or newer for network metadata: %w", network.Name, err)
	}
	active, err := networkActive(ctx, virsh, network.Name)
	if err != nil {
		return err
	}
	if !active {
		if _, err := virsh(ctx, "net-start", network.Name); err != nil {
			return fmt.Errorf("%w %s on %s: %w", ErrNetworkStart, network.Name, network.CIDR, err)
		}
	}
	return network.reconcileHosts(ctx, virsh)
}

// reconcileHosts makes the network's DHCP reservations exactly Hosts, live and
// in the persistent definition.
func (network OwnedNetwork) reconcileHosts(ctx context.Context, virsh virshFunc) error {
	data, err := virsh(ctx, "net-dumpxml", network.Name, "--inactive")
	if err != nil {
		return err
	}
	current, err := LibvirtNetworkHosts(data)
	if err != nil {
		return fmt.Errorf("read libvirt network %s reservations: %w", network.Name, err)
	}
	byMAC := map[string]DHCPHost{}
	for _, host := range current {
		byMAC[strings.ToLower(host.MACAddress)] = host
	}
	update := func(command string, host DHCPHost) error {
		element, err := network.HostXML(host)
		if err != nil {
			return err
		}
		_, err = virsh(ctx, "net-update", network.Name, command, "ip-dhcp-host", element, "--live", "--config")
		return err
	}
	for _, host := range network.Hosts {
		mac := strings.ToLower(host.MACAddress)
		existing, ok := byMAC[mac]
		delete(byMAC, mac)
		switch {
		case !ok:
			err = update("add-last", host)
		case existing.Address != host.Address || existing.Name != host.Name:
			err = update("modify", host)
		default:
			continue
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

// requireOwned fails unless the existing network carries this network's owner
// and, when it has one, its lease.
func (network OwnedNetwork) requireOwned(ctx context.Context, virsh virshFunc) error {
	output, err := virsh(ctx, "net-metadata", network.Name, vmtestMetadataURI)
	if err != nil {
		return fmt.Errorf("refusing to manage libvirt network %s without ownership metadata: %w", network.Name, err)
	}
	metadata, err := ParseLibvirtMetadata(output)
	if err != nil {
		return fmt.Errorf("decode libvirt network %s ownership metadata: %w", network.Name, err)
	}
	if metadata.Value != network.Owner || (network.Lease != "" && metadata.Lease != network.Lease) {
		return fmt.Errorf("refusing to manage libvirt network %s owned by %q with lease %q", network.Name, metadata.Value, metadata.Lease)
	}
	return nil
}

func networkExists(ctx context.Context, virsh virshFunc, name string) (bool, error) {
	output, err := virsh(ctx, "net-list", "--all", "--name")
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

func networkActive(ctx context.Context, virsh virshFunc, name string) (bool, error) {
	output, err := virsh(ctx, "net-info", name)
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

// runVirsh returns a virshFunc for the libvirt connection uri, using
// KATL_VMTEST_VIRSH as the virsh command.
func runVirsh(uri string) virshFunc {
	virsh := first(os.Getenv("KATL_VMTEST_VIRSH"), "virsh")
	return func(ctx context.Context, args ...string) ([]byte, error) {
		args = append([]string{"-c", uri}, args...)
		cmd := exec.CommandContext(ctx, virsh, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			return output, fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return output, nil
	}
}

// leaseHeld reports whether a live run holds the lease at path. Only a lease
// file that exists in runLeaseDir and that no process locks is free. Any lease
// this sandbox cannot judge counts as held, so a sweep leaks what it cannot
// judge rather than removing a live run's objects: a missing lease may belong
// to a run under another private /tmp, which writes it at the same path out of
// this sandbox's sight; a lease outside runLeaseDir may be invisible for the
// same reason, as the leases of runs from older checkouts are; and a lease
// that cannot be read belongs to another user's run.
func leaseHeld(path string) (bool, error) {
	if filepath.Dir(filepath.Clean(path)) != runLeaseDir() {
		return true, nil
	}
	// O_NONBLOCK keeps a FIFO from blocking the open and O_NOFOLLOW keeps a
	// symlink from redirecting the probe. The descriptor is a separate open
	// file description, so the probe conflicts with the holder even inside the
	// holder's own process.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, unix.ELOOP):
		return true, nil
	case err != nil:
		return false, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, fmt.Errorf("probe lease %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		// Not a lease file a run created, so it is not ours to judge.
		return true, nil
	}
	err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe lease %s: %w", path, err)
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false, nil
}

func domainMetadataOf(ctx context.Context, virsh virshFunc, domain string) (LibvirtMetadata, bool) {
	return vmtestMetadataOf(ctx, virsh, "metadata", domain, "--uri", vmtestMetadataURI)
}

func networkMetadataOf(ctx context.Context, virsh virshFunc, network string) (LibvirtMetadata, bool) {
	return vmtestMetadataOf(ctx, virsh, "net-metadata", network, vmtestMetadataURI)
}

// vmtestMetadataOf returns the metadata of an object that scripts/vmtest-run
// worlds created, and false for any other object.
func vmtestMetadataOf(ctx context.Context, virsh virshFunc, args ...string) (LibvirtMetadata, bool) {
	output, err := virsh(ctx, args...)
	if err != nil {
		return LibvirtMetadata{}, false
	}
	metadata, err := ParseLibvirtMetadata(output)
	if err != nil || metadata.Value != vmtestMetadata {
		return LibvirtMetadata{}, false
	}
	return metadata, true
}

// sweep removes the vmtest domains and networks whose run has ended. An object
// belongs to the run whose lease its metadata names, and is removed once that
// lease is free. An object without a lease has no owner to ask, so it stays.
// Runs leave their lease file behind, so the sweep then deletes the free lease
// files that no remaining object names.
func sweep(ctx context.Context, virsh virshFunc, stderr io.Writer) error {
	// Removal is best effort: a finished run's leftovers must not fail the
	// scenario that sweeps them, and the next sweep retries them.
	fail := func(kind, name string, err error) {
		fmt.Fprintf(stderr, "vmtest cleanup: remove libvirt %s %s failed; a later run retries: %v\n", kind, name, err)
	}
	// The leases that objects still name after this sweep.
	named := map[string]bool{}
	reap := func(kind, name string, metadata LibvirtMetadata, remove func() error) {
		if metadata.Lease == "" {
			return
		}
		held, err := leaseHeld(metadata.Lease)
		if err != nil || held {
			named[filepath.Clean(metadata.Lease)] = true
			if err != nil {
				fail(kind, name, err)
			}
			return
		}
		fmt.Fprintf(stderr, "vmtest cleanup: removing libvirt %s %s left by a finished run\n", kind, name)
		if err := remove(); err != nil {
			named[filepath.Clean(metadata.Lease)] = true
			fail(kind, name, err)
		}
	}

	output, err := virsh(ctx, "list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, domain := range strings.Fields(string(output)) {
		if metadata, ok := domainMetadataOf(ctx, virsh, domain); ok {
			reap("domain", domain, metadata, func() error { return RemoveDomain(ctx, virsh, domain) })
		}
	}

	output, err = virsh(ctx, "net-list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, network := range strings.Fields(string(output)) {
		if metadata, ok := networkMetadataOf(ctx, virsh, network); ok {
			reap("network", network, metadata, func() error { return removeNetwork(ctx, virsh, network) })
		}
	}

	removeFreeLeases(named, stderr)
	return nil
}

// removeFreeLeases deletes the lease files in runLeaseDir that no process
// locks and that no name in named refers to. Deletion is best effort; a lease
// left behind only means a later sweep looks at it again.
func removeFreeLeases(named map[string]bool, stderr io.Writer) {
	dir := runLeaseDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "vmtest cleanup: read run leases failed: %v\n", err)
		}
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if !entry.Type().IsRegular() || named[path] {
			continue
		}
		if err := removeFreeLease(path); err != nil {
			fmt.Fprintf(stderr, "vmtest cleanup: remove run lease %s failed: %v\n", path, err)
		}
	}
}

// removeFreeLease deletes the lease file at path unless a process locks it.
// The exclusive lock keeps a run from taking the lease while it is deleted,
// and the identity check keeps the deletion from hitting a file that replaced
// it. A run that opened the lease just before the deletion and locks it after
// holds a lease no sweep can see, which counts as held, so its objects leak
// rather than being removed.
func removeFreeLease(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	var locked, current unix.Stat_t
	if err := unix.Fstat(fd, &locked); err != nil {
		return err
	}
	if err := unix.Lstat(path, &current); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if locked.Dev != current.Dev || locked.Ino != current.Ino || locked.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	if err := unix.Unlink(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// notFound reports whether a virsh error says the domain or network is gone.
func notFound(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, text := range []string{"failed to get domain", "Domain not found", "failed to get network", "Network not found", "no domain with matching", "no network with matching"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

// RemoveDomain stops and undefines a domain, including its managed save image
// and NVRAM. A domain that is already gone counts as removed, so concurrent
// removers do not fail each other. Callers confirm ownership first.
func RemoveDomain(ctx context.Context, virsh func(ctx context.Context, args ...string) ([]byte, error), domain string) error {
	state, err := virsh(ctx, "domstate", domain)
	if notFound(err) {
		return nil
	}
	switch strings.TrimSpace(string(state)) {
	case "shut off", "crashed", "", "no state":
	default:
		if _, err := virsh(ctx, "destroy", domain); err != nil && !notFound(err) {
			return fmt.Errorf("destroy domain %s: %w", domain, err)
		}
	}
	if _, err := virsh(ctx, "undefine", domain, "--managed-save", "--nvram"); err != nil && !notFound(err) {
		return fmt.Errorf("undefine domain %s: %w", domain, err)
	}
	return nil
}

// removeNetwork stops and undefines a network; one that is already gone
// counts as removed.
func removeNetwork(ctx context.Context, virsh virshFunc, network string) error {
	active, err := networkActive(ctx, virsh, network)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if active {
		if _, err := virsh(ctx, "net-destroy", network); err != nil && !notFound(err) {
			return err
		}
	}
	if _, err := virsh(ctx, "net-undefine", network); err != nil && !notFound(err) {
		return err
	}
	return nil
}

// networkCreationAttempts bounds how often createNetwork retries on another
// subnet after libvirt refuses to start the network on one.
const networkCreationAttempts = 4

// ErrScenarioPlanned reports a scenario ID that its run already planned.
var ErrScenarioPlanned = errors.New("scenario already planned in this run")

// createNetwork defines and starts network on a /24 that nothing else on the
// host uses, and refuses a network name that already exists. Choosing the
// subnet and starting the network happen under one lock,
// so concurrent processes cannot choose the same subnet; a subnet that libvirt
// still refuses, for example because a network outside libvirt's view uses it,
// is skipped on the next attempt. The returned network has its CIDR and
// gateway set.
func createNetwork(ctx context.Context, virsh virshFunc, lockPath string, network OwnedNetwork, definition string) (OwnedNetwork, error) {
	unlock, err := flockFile(ctx, lockPath)
	if err != nil {
		return OwnedNetwork{}, err
	}
	defer unlock()

	exists, err := networkExists(ctx, virsh, network.Name)
	if err != nil {
		return OwnedNetwork{}, err
	}
	if exists {
		if err := network.requireOwned(ctx, virsh); err != nil {
			return OwnedNetwork{}, err
		}
		// A scenario's network name derives from its run and ID, so an existing
		// one means the ID is already planned in this run, or that a domain
		// preserved for debugging still holds the network's addresses. Planning
		// again would give a new node the MAC and address of that earlier VM.
		return OwnedNetwork{}, fmt.Errorf("%w: libvirt network %s already exists; give each scenario a distinct name, and remove any domain preserved for debugging that still holds it", ErrScenarioPlanned, network.Name)
	}

	var refused []*net.IPNet
	var startErr error
	for range networkCreationAttempts {
		inUse, err := SubnetsInUse(ctx, virsh)
		if err != nil {
			return OwnedNetwork{}, err
		}
		cidr, gateway, err := ChooseNetworkCIDR(network.Name, append(inUse, refused...))
		if err != nil {
			return OwnedNetwork{}, err
		}
		network.CIDR, network.Gateway = cidr, gateway
		err = network.Ensure(ctx, virsh, definition)
		if err == nil {
			return network, nil
		}
		if !errors.Is(err, ErrNetworkStart) {
			return OwnedNetwork{}, err
		}
		startErr = err
		if _, undefineErr := virsh(ctx, "net-undefine", network.Name); undefineErr != nil {
			return OwnedNetwork{}, fmt.Errorf("%w; undefine it with virsh net-undefine %s: %v", err, network.Name, undefineErr)
		}
		_, subnet, _ := net.ParseCIDR(cidr)
		refused = append(refused, subnet)
	}
	return OwnedNetwork{}, fmt.Errorf("no subnet accepted after %d attempts: %w", networkCreationAttempts, startErr)
}

// releaseNetwork removes the network unless a domain that this lease owns
// still attaches to it, as a domain preserved for debugging does. A later
// sweep removes such a network once the lease is free.
func releaseNetwork(ctx context.Context, virsh virshFunc, name, lease string, stderr io.Writer) error {
	if metadata, ok := networkMetadataOf(ctx, virsh, name); !ok || metadata.Lease != lease {
		return nil
	}
	output, err := virsh(ctx, "list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, domain := range strings.Fields(string(output)) {
		if metadata, ok := domainMetadataOf(ctx, virsh, domain); !ok || metadata.Lease != lease {
			continue
		}
		data, err := virsh(ctx, "dumpxml", domain)
		if err != nil {
			return err
		}
		attached, err := attachesToNetwork(data, name)
		if err != nil {
			return fmt.Errorf("read libvirt domain %s: %w", domain, err)
		}
		if attached {
			fmt.Fprintf(stderr, "vmtest cleanup: keeping libvirt network %s for preserved domain %s\n", name, domain)
			return nil
		}
	}
	return removeNetwork(ctx, virsh, name)
}

// ReleaseRunNetworks removes the networks that a run's lease owns once nothing
// of that lease attaches to them any more, as a scenario does when it ends. A
// network that is not a leased vmtest network is left alone, so a caller can
// name networks from a record without risking the host's others.
func ReleaseRunNetworks(ctx context.Context, uri string, networks []string, stderr io.Writer) error {
	virsh := runVirsh(uri)
	var errs []error
	for _, network := range networks {
		if err := releaseRunNetwork(ctx, virsh, network, stderr); err != nil {
			errs = append(errs, fmt.Errorf("release libvirt network %s: %w", network, err))
		}
	}
	return errors.Join(errs...)
}

// releaseRunNetwork releases a network on behalf of the lease its metadata
// names.
func releaseRunNetwork(ctx context.Context, virsh virshFunc, name string, stderr io.Writer) error {
	metadata, ok := networkMetadataOf(ctx, virsh, name)
	if !ok || metadata.Lease == "" {
		return nil
	}
	return releaseNetwork(ctx, virsh, name, metadata.Lease, stderr)
}

// attachesToNetwork reports whether a domain definition has an interface on
// the libvirt network.
func attachesToNetwork(domain []byte, network string) (bool, error) {
	var doc struct {
		Interfaces []struct {
			Source struct {
				Network string `xml:"network,attr"`
			} `xml:"source"`
		} `xml:"devices>interface"`
	}
	if err := xml.Unmarshal(domain, &doc); err != nil {
		return false, err
	}
	for _, iface := range doc.Interfaces {
		if iface.Source.Network == network {
			return true, nil
		}
	}
	return false, nil
}

// Remove stops and undefines the network if it exists and carries this
// network's owner, and reports whether it removed anything. A network owned by
// anything else is an error, so Remove never deletes what it did not create.
func (network OwnedNetwork) Remove(ctx context.Context, virsh virshFunc) (bool, error) {
	exists, err := networkExists(ctx, virsh, network.Name)
	if err != nil || !exists {
		return false, err
	}
	if err := network.requireOwned(ctx, virsh); err != nil {
		return false, err
	}
	if err := removeNetwork(ctx, virsh, network.Name); err != nil {
		return false, err
	}
	return true, nil
}
