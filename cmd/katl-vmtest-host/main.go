// Command katl-vmtest-host owns the host state that concurrent
// scripts/vmtest-run invocations share: admission of runs, the sweep of
// libvirt objects that finished runs left behind, and each run's libvirt
// network.
//
// Each run holds an exclusive flock on its own lease file for as long as it
// may own libvirt objects. scripts/vmtest-run opens the lease and keeps the
// descriptor; admit locks that inherited descriptor, so the lock belongs to the
// runner's open file description and outlives this process. The kernel drops
// it when the runner exits for any reason, which frees the run's slot and
// marks its objects for the next sweep.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/vmtest"
)

// vmtestMetadata is the metadata value of libvirt objects that vmtest creates.
const vmtestMetadata = "katl/vmtest"

const metadataURI = "https://katlos.io/xmlns/vmtest/1"

// A run boots at most three 2 GiB, 2-vCPU guests at a time, because the
// runner keeps test packages serial. Memory is budgeted for that with room for
// QEMU and the host side of the test; vCPUs are overcommitted two to one.
const (
	runMemoryBytes = 8 << 30
	runCPUs        = 4
)

const leaseSuffix = ".lease"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katl-vmtest-host: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("command is required: admit, sweep, network, or release")
	}
	switch args[0] {
	case "admit":
		return runAdmit(args[1:], stderr)
	case "sweep":
		return runSweep(ctx, args[1:], stderr)
	case "network":
		return runNetwork(ctx, args[1:], stdout, stderr)
	case "release":
		return runRelease(ctx, args[1:], stderr)
	default:
		return fmt.Errorf("unsupported command %q", args[0])
	}
}

func runAdmit(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-host admit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lease := flags.String("lease", "", "this run's lease file, in the admission directory")
	fd := flags.Int("fd", -1, "inherited descriptor open on --lease")
	limit := flags.Int("max", 0, "runs the host admits at once; 0 derives it from host memory and CPUs")
	timeout := flags.Duration("timeout", 90*time.Minute, "how long to wait for a slot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *lease == "" || *fd < 0 {
		return errors.New("--lease and --fd are required")
	}
	if *limit < 0 {
		return fmt.Errorf("KATL_VMTEST_MAX_CONCURRENT must be a positive number of runs, got %d", *limit)
	}
	if *limit == 0 {
		memory, err := memTotal()
		if err != nil {
			return err
		}
		*limit = defaultLimit(memory, runtime.NumCPU())
	}
	file := os.NewFile(uintptr(*fd), *lease)
	if err := sameFile(file, *lease); err != nil {
		return err
	}
	return admit(admission{lease: *lease, file: file, limit: *limit, timeout: *timeout, poll: time.Second, stderr: stderr})
}

// defaultLimit is how many runs fit the host's memory and CPUs, and at
// least one.
func defaultLimit(memoryBytes uint64, cpus int) int {
	return max(1, min(int(memoryBytes/runMemoryBytes), cpus/runCPUs))
}

func memTotal() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse /proc/meminfo MemTotal: %w", err)
			}
			return kib << 10, nil
		}
	}
	return 0, errors.New("/proc/meminfo has no MemTotal")
}

func sameFile(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("lease descriptor: %w", err)
	}
	named, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("lease %s: %w", path, err)
	}
	if !os.SameFile(opened, named) {
		return fmt.Errorf("lease descriptor is not open on %s", path)
	}
	return nil
}

type admission struct {
	lease   string
	file    *os.File
	limit   int
	timeout time.Duration
	poll    time.Duration
	stderr  io.Writer
}

// admit waits until fewer than limit other runs hold their leases, then locks
// this run's lease. Counting and locking happen under one directory lock, so
// concurrent runs cannot both take the last slot.
func admit(a admission) error {
	deadline := time.Now().Add(a.timeout)
	waiting := false
	for {
		admitted, holders, err := tryAdmit(a)
		if err != nil {
			return err
		}
		if admitted {
			fmt.Fprintf(a.stderr, "vmtest admission: admitted, %d of %d run slot(s) on this host in use\n", len(holders)+1, a.limit)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for one of %d vmtest run slot(s) on this host, held by %s; wait for one of those runs to finish, or set KATL_VMTEST_MAX_CONCURRENT higher if the host has memory and CPUs to spare", a.timeout, a.limit, strings.Join(holders, ", "))
		}
		if !waiting {
			fmt.Fprintf(a.stderr, "vmtest admission: waiting for one of %d run slot(s) on this host, held by %s (timeout %s)\n", a.limit, strings.Join(holders, ", "), a.timeout)
			waiting = true
		}
		time.Sleep(min(a.poll, max(time.Until(deadline), 0)))
	}
}

func tryAdmit(a admission) (bool, []string, error) {
	dir := filepath.Dir(a.lease)
	unlock, err := lockPath(filepath.Join(dir, "admission.lock"))
	if err != nil {
		return false, nil, err
	}
	defer unlock()

	holders, err := leaseHolders(dir, a.lease)
	if err != nil {
		return false, nil, err
	}
	if len(holders) >= a.limit {
		return false, holders, nil
	}
	if err := lockLease(a.file, a.lease); err != nil {
		return false, nil, err
	}
	return true, holders, nil
}

// leaseHolders returns the run IDs of the leases in dir, other than own,
// that a live run holds.
func leaseHolders(dir, own string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*"+leaseSuffix))
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, path := range paths {
		if path == own {
			continue
		}
		held, err := leaseHeld(path)
		if err != nil {
			return nil, err
		}
		if held {
			holders = append(holders, strings.TrimSuffix(filepath.Base(path), leaseSuffix))
		}
	}
	slices.Sort(holders)
	return holders, nil
}

// lockLease takes this run's lease. A sweep probing the lease holds a shared
// lock for an instant, so contention is retried briefly; a lease that stays
// locked belongs to another live run with the same run ID.
func lockLease(file *os.File, path string) error {
	for range 50 {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock lease %s: %w", path, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("another vmtest run holds lease %s; give each run a distinct KATL_VMTEST_RUN_ID", path)
}

// leaseHeld reports whether a live run holds the lease at path. A lease that
// cannot be read belongs to another user's run and counts as held, so this
// run never removes what it cannot judge.
func leaseHeld(path string) (bool, error) {
	file, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case errors.Is(err, fs.ErrPermission):
		return true, nil
	case err != nil:
		return false, err
	}
	defer file.Close()
	err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe lease %s: %w", path, err)
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return false, nil
}

// lockPath takes an exclusive flock on path, waiting as long as it takes. It
// guards short critical sections only.
func lockPath(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = file.Close() }, nil
}

type libvirt struct {
	virsh string
	uri   string
}

func (l libvirt) run(ctx context.Context, args ...string) ([]byte, error) {
	args = append([]string{"-c", l.uri}, args...)
	cmd := exec.CommandContext(ctx, l.virsh, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (l libvirt) names(ctx context.Context, args ...string) ([]string, error) {
	output, err := l.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(output)), nil
}

// metadata returns the vmtest metadata of a domain or network, and false for
// objects that vmtest did not create.
func (l libvirt) metadata(ctx context.Context, args ...string) (vmtest.LibvirtMetadata, bool) {
	output, err := l.run(ctx, args...)
	if err != nil {
		return vmtest.LibvirtMetadata{}, false
	}
	metadata, err := vmtest.ParseLibvirtMetadata(output)
	if err != nil || metadata.Value != vmtestMetadata {
		return vmtest.LibvirtMetadata{}, false
	}
	return metadata, true
}

func (l libvirt) domainMetadata(ctx context.Context, domain string) (vmtest.LibvirtMetadata, bool) {
	return l.metadata(ctx, "metadata", domain, "--uri", metadataURI)
}

func (l libvirt) networkMetadata(ctx context.Context, network string) (vmtest.LibvirtMetadata, bool) {
	return l.metadata(ctx, "net-metadata", network, metadataURI)
}

func libvirtFlags(flags *flag.FlagSet) *libvirt {
	l := &libvirt{}
	flags.StringVar(&l.virsh, "virsh", "virsh", "virsh command")
	flags.StringVar(&l.uri, "uri", "qemu:///system", "libvirt connection URI")
	return l
}

func runSweep(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-host sweep", flag.ContinueOnError)
	flags.SetOutput(stderr)
	l := libvirtFlags(flags)
	hostLock := flags.String("host-lock", "", "lock held by runners that create domains without a lease")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return sweep(ctx, *l, *hostLock, stderr)
}

// sweep removes the vmtest domains and networks whose run has ended. An
// object belongs to the run whose lease its metadata names. Domains without a
// lease come from runners that hold hostLock for their whole run instead, so
// they are removed only while no such runner holds it.
func sweep(ctx context.Context, l libvirt, hostLock string, stderr io.Writer) error {
	domains, err := l.names(ctx, "list", "--all", "--name")
	if err != nil {
		return err
	}
	var stale []string
	for _, domain := range domains {
		metadata, ok := l.domainMetadata(ctx, domain)
		if !ok {
			continue
		}
		owner := metadata.Lease
		if owner == "" {
			if hostLock == "" {
				continue
			}
			owner = hostLock
		}
		held, err := leaseHeld(owner)
		if err != nil {
			return err
		}
		if !held {
			stale = append(stale, domain)
		}
	}
	if len(stale) > 0 {
		fmt.Fprintf(stderr, "vmtest cleanup: removing %d libvirt domain(s) left by finished runs\n", len(stale))
	}
	for _, domain := range stale {
		if err := removeDomain(ctx, l, domain); err != nil {
			return err
		}
	}

	networks, err := l.names(ctx, "net-list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, network := range networks {
		metadata, ok := l.networkMetadata(ctx, network)
		if !ok || metadata.Lease == "" {
			continue
		}
		held, err := leaseHeld(metadata.Lease)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		fmt.Fprintf(stderr, "vmtest cleanup: removing libvirt network %s left by a finished run\n", network)
		if err := removeNetwork(ctx, l, network); err != nil {
			return err
		}
	}
	return nil
}

func removeDomain(ctx context.Context, l libvirt, domain string) error {
	state, _ := l.run(ctx, "domstate", domain)
	switch strings.TrimSpace(string(state)) {
	case "shut off", "", "no state":
	default:
		if _, err := l.run(ctx, "destroy", domain); err != nil {
			return fmt.Errorf("destroy vmtest domain %s: %w", domain, err)
		}
	}
	if _, err := l.run(ctx, "undefine", domain, "--nvram"); err != nil {
		return fmt.Errorf("undefine vmtest domain %s: %w", domain, err)
	}
	return nil
}

func removeNetwork(ctx context.Context, l libvirt, network string) error {
	info, err := l.run(ctx, "net-info", network)
	if err != nil {
		return err
	}
	if networkActive(info) {
		if _, err := l.run(ctx, "net-destroy", network); err != nil {
			return err
		}
	}
	_, err = l.run(ctx, "net-undefine", network)
	return err
}

func networkActive(info []byte) bool {
	for _, line := range strings.Split(string(info), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Active") {
			return strings.EqualFold(strings.TrimSpace(value), "yes")
		}
	}
	return false
}

func runNetwork(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-host network", flag.ContinueOnError)
	flags.SetOutput(stderr)
	l := libvirtFlags(flags)
	name := flags.String("name", "", "network name")
	lease := flags.String("lease", "", "lease of the run that owns the network")
	definition := flags.String("xml", "", "path to write the network definition to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || *lease == "" || *definition == "" {
		return errors.New("--name, --lease, and --xml are required")
	}
	network, err := createNetwork(ctx, *l, *name, *lease, *definition)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s %s\n", network.Name, network.CIDR, network.Gateway)
	return nil
}

// createNetwork defines and starts a NAT network that the run holding lease
// owns, on a /24 that nothing else on the host uses. Choosing the subnet and
// starting the network happen under one host lock, so concurrent runs cannot
// choose the same subnet.
func createNetwork(ctx context.Context, l libvirt, name, lease, definition string) (vmtest.OwnedNetwork, error) {
	unlock, err := lockPath(filepath.Join(filepath.Dir(lease), "network.lock"))
	if err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	defer unlock()

	inUse, err := vmtest.SubnetsInUse(ctx, l.run)
	if err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	cidr, gateway, err := vmtest.ChooseNetworkCIDR(name, inUse)
	if err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	network := vmtest.OwnedNetwork{Name: name, Owner: vmtestMetadata, Lease: lease, CIDR: cidr, Gateway: gateway}
	data, err := network.XML()
	if err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	if err := os.MkdirAll(filepath.Dir(definition), 0o755); err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	if err := os.WriteFile(definition, []byte(data), 0o644); err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	if _, err := l.run(ctx, "net-define", definition); err != nil {
		return vmtest.OwnedNetwork{}, err
	}
	// Without its lease the network would never be swept.
	if metadata, ok := l.networkMetadata(ctx, name); !ok || metadata.Lease != lease {
		_, _ = l.run(ctx, "net-undefine", name)
		return vmtest.OwnedNetwork{}, fmt.Errorf("libvirt did not keep the ownership metadata of network %s; vmtest needs libvirt 9.7 or newer, or set KATL_VMTEST_LIBVIRT_NETWORK to an existing network", name)
	}
	if _, err := l.run(ctx, "net-start", name); err != nil {
		_, _ = l.run(ctx, "net-undefine", name)
		return vmtest.OwnedNetwork{}, fmt.Errorf("start vmtest network %s on %s: %w", name, cidr, err)
	}
	return network, nil
}

func runRelease(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-host release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	l := libvirtFlags(flags)
	name := flags.String("name", "", "network name")
	lease := flags.String("lease", "", "lease of the run that owns the network")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || *lease == "" {
		return errors.New("--name and --lease are required")
	}
	return release(ctx, *l, *name, *lease, stderr)
}

// release removes the run's network unless the run preserved domains for
// debugging, which keep using it until a later sweep.
func release(ctx context.Context, l libvirt, name, lease string, stderr io.Writer) error {
	if metadata, ok := l.networkMetadata(ctx, name); !ok || metadata.Lease != lease {
		return nil
	}
	domains, err := l.names(ctx, "list", "--all", "--name")
	if err != nil {
		return err
	}
	for _, domain := range domains {
		if metadata, ok := l.domainMetadata(ctx, domain); ok && metadata.Lease == lease {
			fmt.Fprintf(stderr, "vmtest cleanup: keeping libvirt network %s for preserved domain %s\n", name, domain)
			return nil
		}
	}
	return removeNetwork(ctx, l, name)
}
