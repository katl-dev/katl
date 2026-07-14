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
	if err := WriteHandoff(path, "http://192.0.2.10:8080/"); err != nil {
		t.Fatalf("WriteHandoff() error = %v", err)
	}
	record, err := ReadHandoff(path)
	if err != nil {
		t.Fatalf("ReadHandoff() error = %v", err)
	}
	if record.URL != "http://192.0.2.10:8080/v1/config-bundle" || record.UpdatedAt.IsZero() {
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
	if err := WriteHandoff(handoffPath, "http://192.0.2.10:8080"); err != nil {
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
	if snapshot.Handoff.URL != "http://192.0.2.10:8080/v1/config-bundle" {
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
		DestructiveMutation: true,
		Handoff:             Handoff{URL: "http://192.0.2.10:8080/v1/config-bundle"},
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
		"Run:          katlctl config init cluster.yaml --installer 192.0.2.10",
		"Journal",
		"installing root",
		"Ctrl+Alt+F2: console | SSH disabled",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<cluster.yaml>") || strings.Contains(got, "<base URL>") || strings.Contains(got, "<name>") {
		t.Fatalf("render contains command placeholders:\n%s", got)
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

func TestRenderInstallerCommandUsesHandoffURLWithoutIPv4(t *testing.T) {
	snapshot := Snapshot{
		Mode:    ModeInstaller,
		State:   installstatus.StateWaitingForConfig,
		Handoff: Handoff{URL: "http://[2001:db8::10]:8080/v1/config-bundle"},
		Network: []NetworkInterface{{Name: "enp1s0", Addresses: []string{"2001:db8::10/64"}}},
	}
	var renderer Renderer
	got := string(renderer.Append(make([]byte, 0, RenderCapacity(100, 18)), &snapshot, nil, 100, 18))
	want := "Run:          katlctl config init cluster.yaml --installer http://[2001:db8::10]:8080"
	if !strings.Contains(got, want) {
		t.Fatalf("render missing %q:\n%s", want, got)
	}
}

func TestRenderRuntimeFailure(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		Version:          "2026.7.0-alpha.9",
		Hostname:         "cp-1",
		State:            installstatus.StateRuntimeFailedNeedsRepair,
		Generation:       "4",
		GenerationHealth: "unhealthy",
		LastError:        "boot health check failed",
		RetryHint:        "inspect the previous generation",
		SSHEnabled:       true,
		Network:          []NetworkInterface{{Name: "eno1", Addresses: []string{"192.0.2.20/24"}}},
	}
	var renderer Renderer
	got := string(renderer.Append(make([]byte, 0, RenderCapacity(80, 20)), &snapshot, nil, 80, 20))
	for _, want := range []string{"KatlOS needs repair", "Generation:   4  health=FAILED", "Error:", "boot health check failed", "SSH: root@192.0.2.20"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderKatlOSHealthAndColour(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		State:            installstatus.StateKubeadmReady,
		Generation:       "0",
		GenerationHealth: "healthy",
		CurrentStep:      "Reboot",
	}
	var renderer Renderer
	plain := string(renderer.Append(nil, &snapshot, nil, 80, 15))
	for _, want := range []string{"KatlOS\n", "Generation:   0  health=OK", "Journal"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("plain render missing %q:\n%s", want, plain)
		}
	}
	for _, unwanted := range []string{"runtime", "installed system", "boot=", "Target disk", "Progress:     Reboot", "\x1b["} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("plain render contains %q:\n%s", unwanted, plain)
		}
	}
	colored := string(renderer.AppendColor(nil, &snapshot, nil, 80, 15))
	if !strings.Contains(colored, styleTitle) || !strings.Contains(colored, styleGood) {
		t.Fatalf("coloured render lacks semantic styles: %q", colored)
	}
}

func TestRenderRebootHidesRedundantProgress(t *testing.T) {
	snapshot := Snapshot{
		Mode:        ModeInstaller,
		State:       installstatus.StateRebootRequested,
		CurrentStep: "Reboot",
	}
	var renderer Renderer
	got := string(renderer.Append(nil, &snapshot, nil, 80, 15))
	if !strings.Contains(got, "Installation complete; rebooting") || strings.Contains(got, "Progress:") {
		t.Fatalf("reboot dashboard =\n%s", got)
	}
}

func TestRenderWrapsOperatorContentAtNarrowWidth(t *testing.T) {
	snapshot := Snapshot{
		Mode:      ModeInstaller,
		State:     installstatus.StateWaitingForConfig,
		LastError: "installer diagnostic has a deliberately-long-unbroken-value-with-tail-marker",
		Handoff:   Handoff{URL: "http://[2001:db8:1234:5678::10]:8080/v1/config-bundle"},
		Network: []NetworkInterface{{
			Name:      "enp1s0",
			Addresses: []string{"2001:db8:1234:5678::10/64", "192.0.2.10/24"},
		}},
	}
	var renderer Renderer
	got := string(renderer.Append(nil, &snapshot, nil, 40, 40))
	joined := strings.ReplaceAll(got, "\n"+strings.Repeat(" ", fieldWidth), "")
	for _, want := range []string{"tail-marker", "config-bundle", "192.0.2.10/24"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("narrow render lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "~") {
		t.Fatalf("narrow render truncated content:\n%s", got)
	}
	for number, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len([]rune(line)) > 40 {
			t.Fatalf("line %d width = %d: %q", number+1, len([]rune(line)), line)
		}
	}
}

func TestJournalLineWrapsAndStripsANSI(t *testing.T) {
	value := []byte("2026-07-14T00:01:02+01:00 host service[1]: " + strings.Repeat("payload", 8) + "\x1b[31m")
	got, rows := AppendJournalLine(nil, value, 40, 10)
	if rows < 2 || strings.Contains(string(got), "\x1b") || strings.Contains(string(got), "[31m") || strings.Contains(string(got), "~") {
		t.Fatalf("AppendJournalLine() = %q, rows=%d", got, rows)
	}
	for number, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		if len([]rune(line)) > 40 {
			t.Fatalf("line %d width = %d: %q", number+1, len([]rune(line)), line)
		}
	}
	if joined := strings.ReplaceAll(string(got), "\n", ""); joined != strings.TrimSuffix(string(value), "\x1b[31m") {
		t.Fatalf("wrapped content changed: %q", joined)
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
	written := 0
	for _, line := range j[len(j)-rows:] {
		var lineRows int
		dst, lineRows = AppendJournalLine(dst, line, width, rows-written)
		written += lineRows
	}
	return dst, written
}
