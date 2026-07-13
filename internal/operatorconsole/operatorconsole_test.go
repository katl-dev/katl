package operatorconsole

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type testAddr string

func (a testAddr) Network() string { return "ip" }
func (a testAddr) String() string  { return string(a) }

func TestWriteAndReadHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console", "handoff.json")
	if err := WriteHandoff(path, "http://192.0.2.10:8080/", "test-token"); err != nil {
		t.Fatalf("WriteHandoff() error = %v", err)
	}
	record, err := ReadHandoff(path)
	if err != nil {
		t.Fatalf("ReadHandoff() error = %v", err)
	}
	if record.URL != "http://192.0.2.10:8080/v1/config-bundle" || record.Token != "test-token" || record.UpdatedAt.IsZero() {
		t.Fatalf("handoff = %#v", record)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("handoff permissions = %o", info.Mode().Perm())
	}
}

func TestCollectorReadsInstallerStateAndNetwork(t *testing.T) {
	root := t.TempDir()
	statusPath := filepath.Join(root, "status.json")
	record := installstatus.New(installstatus.StateRunning, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC))
	record.CurrentStep = "InstallRootSlot"
	record.TargetDiskStableID = "/dev/disk/by-id/virtio-root"
	record.DestructiveMutation = true
	if err := installstatus.WriteFile(statusPath, record); err != nil {
		t.Fatal(err)
	}
	handoffPath := filepath.Join(root, "handoff.json")
	if err := WriteHandoff(handoffPath, "http://192.0.2.10:8080", "token"); err != nil {
		t.Fatal(err)
	}
	collector := Collector{
		Mode:        ModeInstaller,
		Version:     "2026.7.0-alpha.9",
		Root:        root,
		StatusPath:  statusPath,
		HandoffPath: handoffPath,
		Hostname:    func() (string, error) { return "katl-installer", nil },
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback}, {Index: 2, Name: "enp1s0", Flags: net.FlagUp}}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			if iface.Name == "enp1s0" {
				return []net.Addr{testAddr("fe80::1/64"), testAddr("192.0.2.10/24")}, nil
			}
			return nil, nil
		},
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.State != installstatus.StateRunning || snapshot.CurrentStep != "InstallRootSlot" || !snapshot.DestructiveMutation {
		t.Fatalf("snapshot status = %#v", snapshot)
	}
	if len(snapshot.Network) != 1 || strings.Join(snapshot.Network[0].Addresses, ",") != "192.0.2.10/24" {
		t.Fatalf("snapshot network = %#v", snapshot.Network)
	}
	if snapshot.Handoff.Token != "token" {
		t.Fatalf("snapshot handoff = %#v", snapshot)
	}
	network := &snapshot.Network[0]
	address := &snapshot.Network[0].Addresses[0]
	collector.Collect(&snapshot)
	if &snapshot.Network[0] != network || &snapshot.Network[0].Addresses[0] != address {
		t.Fatal("Collect() did not reuse snapshot network storage")
	}
}

func TestRenderInstallerDashboard(t *testing.T) {
	snapshot := Snapshot{
		Mode:                ModeInstaller,
		Version:             "2026.7.0-alpha.9",
		Hostname:            "katl-installer",
		State:               installstatus.StateRunning,
		CurrentStep:         "InstallRootSlot",
		TargetDisk:          "/dev/disk/by-id/virtio-root",
		DestructiveMutation: true,
		Handoff:             Handoff{URL: "http://192.0.2.10:8080/v1/config-bundle", Token: "secret-token"},
		Network:             []NetworkInterface{{Name: "enp1s0", Addresses: []string{"192.0.2.10/24"}}},
	}
	journal := testJournal{[]byte("old line"), []byte("installing root\x1b[31m"), []byte("latest line")}
	var renderer Renderer
	got := string(renderer.Append(make([]byte, 0, RenderCapacity(80, 25)), &snapshot, journal, 80, 25))
	for _, want := range []string{
		"KatlOS Installer  2026.7.0-alpha.9",
		"State:        Installing",
		"Network:      enp1s0: 192.0.2.10/24",
		"Disk changes: started - do not power off",
		"Configure:    http://192.0.2.10:8080/v1/config-bundle",
		"Token:        secret-token",
		"Journal (live)",
		"installing root[31m",
		"Ctrl+Alt+F2: local console | SSH disabled by installer config",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 25 {
		t.Fatalf("rendered rows = %d, want 25", len(lines))
	}
	for number, line := range lines {
		if len([]rune(line)) > 80 {
			t.Fatalf("line %d width = %d", number+1, len([]rune(line)))
		}
	}
}

func TestRenderRuntimeFailure(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		Version:          "2026.7.0-alpha.9",
		Hostname:         "cp-1",
		State:            installstatus.StateRuntimeFailedNeedsRepair,
		Generation:       "4",
		GenerationBoot:   "failed",
		GenerationHealth: "unhealthy",
		LastError:        "boot health check failed",
		RetryHint:        "inspect the previous generation",
		SSHEnabled:       true,
		Network:          []NetworkInterface{{Name: "eno1", Addresses: []string{"192.0.2.20/24"}}},
	}
	var renderer Renderer
	got := string(renderer.Append(make([]byte, 0, RenderCapacity(80, 20)), &snapshot, nil, 80, 20))
	for _, want := range []string{"Installed system needs repair", "Generation:   4  boot=failed health=unhealthy", "Error:", "boot health check failed", "SSH: ssh root@192.0.2.20"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

func TestRendererReusesBuffer(t *testing.T) {
	snapshot := Snapshot{
		Mode:       ModeRuntime,
		State:      installstatus.StateKubeadmReady,
		SSHEnabled: true,
	}
	journal := testJournal{[]byte("journal line")}
	var renderer Renderer
	buffer := make([]byte, 0, RenderCapacity(80, 25))
	buffer = renderer.Append(buffer, &snapshot, &journal, 80, 25)
	start := &buffer[0]

	allocations := testing.AllocsPerRun(1000, func() {
		buffer = renderer.Append(buffer[:0], &snapshot, &journal, 80, 25)
	})
	if allocations != 0 {
		t.Fatalf("Renderer.Append() allocations = %v, want 0", allocations)
	}
	if &buffer[0] != start {
		t.Fatal("Renderer.Append() replaced caller-owned buffer")
	}
}

type testJournal [][]byte

func (j testJournal) AppendTail(dst []byte, rows, width int) ([]byte, int) {
	rows = min(rows, len(j))
	for _, line := range j[len(j)-rows:] {
		dst = AppendJournalLine(dst, line, width)
	}
	return dst, rows
}
