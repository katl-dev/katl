package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/vmtest"
)

// holdLease opens path as scripts/vmtest-run does and takes it as a run would.
// Closing the returned file ends the run.
func holdLease(t *testing.T, path string) *os.File {
	t.Helper()
	file := openLease(t, path)
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	return file
}

func openLease(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestAdmit(t *testing.T) {
	dir := t.TempDir()
	first := holdLease(t, filepath.Join(dir, "run-a.lease"))
	holdLease(t, filepath.Join(dir, "run-b.lease"))
	// A finished run leaves an unlocked lease behind; it holds no slot.
	openLease(t, filepath.Join(dir, "run-old.lease")).Close()

	third := filepath.Join(dir, "run-c.lease")
	request := admission{lease: third, file: openLease(t, third), limit: 2, poll: 10 * time.Millisecond, stderr: io.Discard}

	request.timeout = 50 * time.Millisecond
	err := admit(request)
	if err == nil || !strings.Contains(err.Error(), "held by run-a, run-b") || !strings.Contains(err.Error(), "KATL_VMTEST_MAX_CONCURRENT") {
		t.Fatalf("third run admitted past the limit or without guidance: %v", err)
	}
	if held, _ := leaseHeld(third); held {
		t.Fatal("refused run holds its lease")
	}

	request.timeout = 5 * time.Second
	done := make(chan error, 1)
	go func() { done <- admit(request) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("third run admitted while both slots are held: %v", err)
	default:
	}
	first.Close()
	if err := <-done; err != nil {
		t.Fatalf("third run not admitted after a slot was freed: %v", err)
	}
	if held, _ := leaseHeld(third); !held {
		t.Fatal("admitted run does not hold its lease")
	}
}

func TestAdmitConcurrent(t *testing.T) {
	// Runs that ask at the same moment must not both take the last slot.
	dir := t.TempDir()
	const runs, limit = 6, 2
	var wg sync.WaitGroup
	results := make([]error, runs)
	for i := range runs {
		path := filepath.Join(dir, fmt.Sprintf("run-%d.lease", i))
		request := admission{lease: path, file: openLease(t, path), limit: limit, timeout: 200 * time.Millisecond, poll: 10 * time.Millisecond, stderr: io.Discard}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = admit(request)
		}()
	}
	wg.Wait()

	admitted := 0
	for _, err := range results {
		if err == nil {
			admitted++
		}
	}
	if admitted != limit {
		t.Fatalf("admitted %d runs, want %d: %v", admitted, limit, results)
	}
}

func TestDefaultLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory uint64
		cpus   int
		want   int
	}{
		{name: "memory-bound", memory: 31 << 30, cpus: 20, want: 3},
		{name: "cpu-bound", memory: 64 << 30, cpus: 8, want: 2},
		{name: "small-runner", memory: 7 << 30, cpus: 2, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultLimit(tc.memory, tc.cpus); got != tc.want {
				t.Fatalf("defaultLimit = %d, want %d", got, tc.want)
			}
		})
	}
}

// fakeLibvirt is a virsh that keeps domains and networks as files, enough
// for the commands katl-vmtest-host runs.
type fakeLibvirt struct {
	root  string
	virsh string
}

const fakeVirsh = `#!/usr/bin/env bash
set -euo pipefail
root="$(dirname "$0")"
shift 2
printf '%s\n' "$*" >>"$root/calls.log"
command="$1"
shift
case "$command" in
  list) ls "$root/domains" ;;
  metadata) cat "$root/domains/$1" ;;
  domstate) printf 'running\n' ;;
  destroy) ;;
  undefine) rm "$root/domains/$1" ;;
  net-list) ls "$root/networks" ;;
  net-dumpxml) cat "$root/networks/$1" ;;
  net-define)
    sleep "${FAKE_DEFINE_DELAY:-0}"
    name="$(sed -n 's:.*<name>\(.*\)</name>.*:\1:p' "$1")"
    cp "$1" "$root/networks/$name"
    ;;
  net-metadata)
    [[ -f "$root/networks/$1" ]] || exit 1
    grep -o '<vmtest .*</vmtest>' "$root/networks/$1" || exit 1
    ;;
  net-info) printf 'Active: yes\n' ;;
  net-start | net-destroy) ;;
  net-undefine) rm "$root/networks/$1" ;;
  *) exit 2 ;;
esac
`

func newFakeLibvirt(t *testing.T) fakeLibvirt {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"domains", "networks"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	virsh := filepath.Join(root, "virsh")
	if err := os.WriteFile(virsh, []byte(fakeVirsh), 0o755); err != nil {
		t.Fatal(err)
	}
	return fakeLibvirt{root: root, virsh: virsh}
}

func (f fakeLibvirt) libvirt() libvirt {
	return libvirt{virsh: f.virsh, uri: "qemu:///fixture"}
}

func (f fakeLibvirt) addDomain(t *testing.T, name, metadata string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "domains", name), []byte(metadata+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fakeLibvirt) addNetwork(t *testing.T, network vmtest.OwnedNetwork) {
	t.Helper()
	data, err := network.XML()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "networks", network.Name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fakeLibvirt) objects(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, kind))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func metadataXML(lease string) string {
	if lease == "" {
		return `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/vmtest</vmtest>`
	}
	return `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1" lease="` + lease + `">katl/vmtest</vmtest>`
}

func TestSweep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hostLock  bool
		remaining []string
	}{
		// While a runner without leases holds the host lock, its domains
		// stay; otherwise they are left over and go.
		{name: "host-lock-held", hostLock: true, remaining: []string{"katldev", "live", "ownerless"}},
		{name: "host-lock-free", remaining: []string{"katldev", "live"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := t.TempDir()
			live := filepath.Join(leases, "live.lease")
			holdLease(t, live)
			finished := filepath.Join(leases, "finished.lease")
			openLease(t, finished).Close()
			hostLock := filepath.Join(leases, "host.lock")
			if tc.hostLock {
				holdLease(t, hostLock)
			}

			fake := newFakeLibvirt(t)
			fake.addDomain(t, "live", metadataXML(live))
			fake.addDomain(t, "finished", metadataXML(finished))
			fake.addDomain(t, "vanished", metadataXML(filepath.Join(leases, "vanished.lease")))
			fake.addDomain(t, "ownerless", metadataXML(""))
			fake.addDomain(t, "katldev", `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-world/dev-world-1</vmtest>`)
			fake.addNetwork(t, vmtest.OwnedNetwork{Name: "live-net", Owner: vmtestMetadata, Lease: live, CIDR: "10.200.1.0/24", Gateway: "10.200.1.1"})
			fake.addNetwork(t, vmtest.OwnedNetwork{Name: "finished-net", Owner: vmtestMetadata, Lease: finished, CIDR: "10.200.2.0/24", Gateway: "10.200.2.1"})
			fake.addNetwork(t, vmtest.OwnedNetwork{Name: "katldev-net", Owner: "katl/katldev-world/dev-world-1", CIDR: "10.200.3.0/24", Gateway: "10.200.3.1"})

			if err := sweep(context.Background(), fake.libvirt(), hostLock, io.Discard); err != nil {
				t.Fatal(err)
			}

			if got := fake.objects(t, "domains"); !slices.Equal(got, tc.remaining) {
				t.Fatalf("domains = %v, want %v", got, tc.remaining)
			}
			if got, want := fake.objects(t, "networks"), []string{"katldev-net", "live-net"}; !slices.Equal(got, want) {
				t.Fatalf("networks = %v, want %v", got, want)
			}
		})
	}
}

func TestCreateNetworkConcurrent(t *testing.T) {
	// Two names whose preferred subnet is the same make concurrent runs race
	// for one /24 unless choosing and defining are serialised.
	var names []string
	seen := map[string]string{}
	for i := 0; len(names) == 0; i++ {
		name := fmt.Sprintf("katl-vmtest-run-%d", i)
		cidr, _, err := vmtest.ChooseNetworkCIDR(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[cidr]; ok {
			names = []string{other, name}
		}
		seen[cidr] = name
	}

	fake := newFakeLibvirt(t)
	t.Setenv("FAKE_DEFINE_DELAY", "0.2")
	leases := t.TempDir()
	var wg sync.WaitGroup
	networks := make([]vmtest.OwnedNetwork, len(names))
	errs := make([]error, len(names))
	for i, name := range names {
		lease := filepath.Join(leases, name+".lease")
		holdLease(t, lease)
		wg.Add(1)
		go func() {
			defer wg.Done()
			networks[i], errs[i] = createNetwork(context.Background(), fake.libvirt(), name, lease, filepath.Join(t.TempDir(), "network.xml"))
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if networks[0].CIDR == networks[1].CIDR {
		t.Fatalf("concurrent runs share subnet %s", networks[0].CIDR)
	}
	for _, network := range networks {
		metadata, ok := fake.libvirt().networkMetadata(context.Background(), network.Name)
		if !ok || metadata.Lease != network.Lease {
			t.Fatalf("network %s metadata = %+v, %v; want lease %s", network.Name, metadata, ok, network.Lease)
		}
	}
}

func TestRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preserved bool
		remaining []string
	}{
		{name: "finished", remaining: nil},
		// A domain preserved for debugging keeps using the network.
		{name: "preserved", preserved: true, remaining: []string{"run-net"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := filepath.Join(t.TempDir(), "run.lease")
			holdLease(t, lease)
			fake := newFakeLibvirt(t)
			fake.addNetwork(t, vmtest.OwnedNetwork{Name: "run-net", Owner: vmtestMetadata, Lease: lease, CIDR: "10.200.1.0/24", Gateway: "10.200.1.1"})
			fake.addDomain(t, "other", metadataXML(filepath.Join(t.TempDir(), "other.lease")))
			if tc.preserved {
				fake.addDomain(t, "debug", metadataXML(lease))
			}

			if err := release(context.Background(), fake.libvirt(), "run-net", lease, io.Discard); err != nil {
				t.Fatal(err)
			}

			if got := fake.objects(t, "networks"); !slices.Equal(got, tc.remaining) {
				t.Fatalf("networks = %v, want %v", got, tc.remaining)
			}
		})
	}
}
