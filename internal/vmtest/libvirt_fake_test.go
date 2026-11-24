package vmtest

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeVirsh is a stateful virsh that keeps domains and networks as files under
// $KATL_FAKE_LIBVIRT, enough for the commands that vmtest runs. Network
// definitions, activity and DHCP reservations are kept apart, so tests read
// the reservations that net-update left rather than asking vmtest.
const fakeVirsh = `#!/usr/bin/env bash
set -euo pipefail
root="$KATL_FAKE_LIBVIRT"
shift 2
printf '%s\n' "$*" >>"$root/calls.log"
command="$1"
shift
network_name() { sed -n 's:.*<name>\(.*\)</name>.*:\1:p' "$1" | head -n1; }
case "$command" in
  list) for f in "$root"/domains/*.meta; do [[ -e $f ]] && basename "$f" .meta; done || true ;;
  metadata) [[ -f "$root/domains/$1.meta" ]] || exit 1; cat "$root/domains/$1.meta" ;;
  dumpxml) cat "$root/domains/$1.xml" ;;
  domstate) printf 'running\n' ;;
  destroy) ;;
  undefine) rm -f "$root/domains/$1.meta" "$root/domains/$1.xml" ;;
  net-list) for f in "$root"/networks/*.xml; do [[ -e $f ]] && basename "$f" .xml; done || true ;;
  net-define)
    sleep "${FAKE_DEFINE_DELAY:-0}"
    name="$(network_name "$1")"
    cp "$1" "$root/networks/$name.xml"
    ;;
  net-dumpxml)
    read -r gateway prefix < <(sed -n 's:.*<ip address="\([^"]*\)" prefix="\([0-9]*\)".*:\1 \2:p' "$root/networks/$1.xml")
    printf "<network><ip address='%s' prefix='%s'><dhcp>" "$gateway" "$prefix"
    if [[ -f "$root/networks/$1.hosts" ]]; then
      while read -r mac ip name; do printf "<host mac='%s' name='%s' ip='%s'/>" "$mac" "$name" "$ip"; done <"$root/networks/$1.hosts"
    fi
    printf '</dhcp></ip></network>\n'
    ;;
  net-metadata)
    [[ -f "$root/networks/$1.xml" ]] || exit 1
    # Real libvirt prints the element without its namespace declaration.
    grep -o '<vmtest[^>]*>[^<]*</vmtest>' "$root/networks/$1.xml" | sed 's: xmlns="[^"]*"::' || exit 1
    ;;
  net-info)
    if [[ -f "$root/networks/$1.active" ]]; then printf 'Active: yes\n'; else printf 'Active: no\n'; fi
    ;;
  net-start)
    gateway="$(sed -n 's:.*<ip address="\([^"]*\)".*:\1:p' "$root/networks/$1.xml")"
    if [[ -f "$root/fail-start" ]] && grep -qx "$gateway" "$root/fail-start"; then
      echo "error: internal error: Network is already in use by interface tun0" >&2
      exit 1
    fi
    touch "$root/networks/$1.active"
    ;;
  net-destroy) rm -f "$root/networks/$1.active" ;;
  net-undefine) rm -f "$root/networks/$1.xml" "$root/networks/$1.hosts" "$root/networks/$1.active" ;;
  net-update)
    name="$1"; action="$2"; element="$4"
    mac="$(sed -n "s:.*mac=[\"']\([^\"']*\)[\"'].*:\1:p" <<<"$element")"
    ip="$(sed -n "s:.* ip=[\"']\([^\"']*\)[\"'].*:\1:p" <<<"$element")"
    host="$(sed -n "s:.* name=[\"']\([^\"']*\)[\"'].*:\1:p" <<<"$element")"
    hosts="$root/networks/$name.hosts"
    touch "$hosts"
    grep -v "^$mac " "$hosts" >"$hosts.new" || true
    case "$action" in
      add-last | modify) printf '%s %s %s\n' "$mac" "$ip" "$host" >>"$hosts.new" ;;
      delete) ;;
      *) exit 2 ;;
    esac
    mv "$hosts.new" "$hosts"
    ;;
  *) exit 2 ;;
esac
`

type fakeLibvirt struct {
	root string
}

// newFakeLibvirt makes vmtest's virsh the fake, for the duration of the test.
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
	t.Setenv("KATL_FAKE_LIBVIRT", root)
	t.Setenv("KATL_VMTEST_VIRSH", virsh)
	return fakeLibvirt{root: root}
}

func (f fakeLibvirt) virsh() virshFunc {
	return runVirsh("qemu:///fixture")
}

// addDomain defines a domain with the given metadata element and, when
// network is not empty, an interface on that network.
func (f fakeLibvirt) addDomain(t *testing.T, name, metadata, network string) {
	t.Helper()
	xml := "<domain><devices><interface type='network'><source network='" + network + "'/></interface></devices></domain>\n"
	for suffix, content := range map[string]string{".meta": metadata + "\n", ".xml": xml} {
		if err := os.WriteFile(filepath.Join(f.root, "domains", name+suffix), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f fakeLibvirt) addNetwork(t *testing.T, network OwnedNetwork) {
	t.Helper()
	data, err := network.XML()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "networks", network.Name+".xml"), []byte(data), 0o644); err != nil {
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
		if name, ok := strings.CutSuffix(entry.Name(), ".meta"); ok && kind == "domains" {
			names = append(names, name)
		}
		if name, ok := strings.CutSuffix(entry.Name(), ".xml"); ok && kind == "networks" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// reservations returns the MAC to address reservations of a network, as the
// fake's net-update calls left them.
func (f fakeLibvirt) reservations(t *testing.T, network string) map[string]string {
	t.Helper()
	file, err := os.Open(filepath.Join(f.root, "networks", network+".hosts"))
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hosts := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) >= 2 {
			hosts[fields[0]] = fields[1]
		}
	}
	return hosts
}

func (f fakeLibvirt) active(network string) bool {
	_, err := os.Stat(filepath.Join(f.root, "networks", network+".active"))
	return err == nil
}

func (f fakeLibvirt) failStartOn(t *testing.T, gateway string) {
	t.Helper()
	path := filepath.Join(f.root, "fail-start")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(gateway + "\n"); err != nil {
		t.Fatal(err)
	}
}

// startLeaseHolder runs a process that holds an exclusive flock on path, as a
// scripts/vmtest-run invocation does for its run. Killing it releases the
// lease, whatever else the test process does.
func startLeaseHolder(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestLeaseHolderProcess$")
	cmd.Env = append(os.Environ(), "KATL_LEASE_HOLDER="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// The holder prints once it holds the lease.
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("lease holder for %s did not start: %v", path, err)
	}
	return cmd
}

func TestLeaseHolderProcess(t *testing.T) {
	path := os.Getenv("KATL_LEASE_HOLDER")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		panic(err)
	}
	os.Stdout.WriteString("held\n")
	// Hold until killed.
	_, _ = os.Stdin.Read(make([]byte, 1))
	os.Exit(0)
}
