package operatorconsole

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/katl-dev/katl/internal/installer/generation"
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

func TestCollectorReadsInstalledVersionsFromBootedGeneration(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "kubernetes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "kubernetes", "kubelet.conf"), []byte("configured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "4",
		RuntimeVersion:        "2026.7.0-alpha.12",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-4.efi",
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/4/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/katl-kubernetes.raw",
			SHA256:          strings.Repeat("b", 64),
			ArtifactVersion: "v1.36.1-katl.1",
			PayloadVersion:  "v1.36.1",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/4/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katl",
				VersionID:    "2026.7.0-alpha.12",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := generation.SpecFromRecord(record)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, time.Date(2026, 7, 16, 12, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "4",
		BootedGenerationID:  "4",
		UpdatedAt:           time.Date(2026, 7, 16, 12, 5, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	collector := Collector{
		Mode:       ModeRuntime,
		Version:    "stale-binary-version",
		Root:       root,
		Hostname:   func() (string, error) { return "cp-1", nil },
		Interfaces: func() ([]net.Interface, error) { return nil, nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.Version != "2026.7.0-alpha.12" || snapshot.KubernetesVersion != "v1.36.1" || !snapshot.KubernetesBootstrapped {
		t.Fatalf("installed state = KatlOS %q, Kubernetes %q, bootstrapped=%t", snapshot.Version, snapshot.KubernetesVersion, snapshot.KubernetesBootstrapped)
	}
}

func TestCollectorReportsLiveBootstrapCandidateBeforeReboot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	booted := testConsoleGeneration(t, root, "0", "2026.7.0-alpha.16", "", now)
	candidate := testConsoleGeneration(t, root, "bootstrap-init-candidate", "2026.7.0-alpha.16", "v1.36.1", now.Add(time.Minute))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:             generation.APIVersion,
		Kind:                   generation.BootSelectionKind,
		DefaultGenerationID:    booted.GenerationID,
		BootedGenerationID:     booted.GenerationID,
		TargetBootGenerationID: candidate.GenerationID,
		UpdatedAt:              now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc", "kubernetes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "kubernetes", "kubelet.conf"), []byte("configured\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := Collector{
		Mode:       ModeRuntime,
		Version:    "binary-version",
		Root:       root,
		Hostname:   func() (string, error) { return "cp-1", nil },
		Interfaces: func() ([]net.Interface, error) { return nil, nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.Generation != "0" || snapshot.NextGeneration != "bootstrap-init-candidate" {
		t.Fatalf("generation = %q, next = %q", snapshot.Generation, snapshot.NextGeneration)
	}
	if snapshot.Version != "2026.7.0-alpha.16" || snapshot.KubernetesVersion != "v1.36.1" || !snapshot.KubernetesBootstrapped {
		t.Fatalf("software state = KatlOS %q, Kubernetes %q, bootstrapped=%t", snapshot.Version, snapshot.KubernetesVersion, snapshot.KubernetesBootstrapped)
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 20, false))
	for _, want := range []string{"Generation: 0", "Next boot:  bootstrap-init-candidate", "Version:    v1.36.1", "Bootstrapped"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func testConsoleGeneration(t *testing.T, root, id, runtimeVersion, kubernetesVersion string, now time.Time) generation.GenerationSpec {
	t.Helper()
	extensions := []generation.ExtensionRef(nil)
	if kubernetesVersion != "" {
		extensions = append(extensions, generation.ExtensionRef{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/" + id + "/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/katl-kubernetes.raw",
			SHA256:          strings.Repeat("b", 64),
			ArtifactVersion: kubernetesVersion + "-katl.1",
			PayloadVersion:  kubernetesVersion,
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		})
	}
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          id,
		RuntimeVersion:        runtimeVersion,
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-" + id + ".efi",
		Sysexts:               extensions,
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/" + id + "/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katl",
				VersionID:    runtimeVersion,
				ConfextLevel: 1,
			},
		},
		CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := generation.SpecFromRecord(record)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	return spec
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
	got := string(renderDashboard(&snapshot, journal, 80, 25, false))
	for _, want := range []string{
		"KatlOS Installer",
		"State:        Installing",
		"Network:      enp1s0: 192.0.2.10/24",
		"Media:        2026.7.0-alpha.9",
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
	got := string(renderDashboard(&snapshot, nil, 100, 18, false))
	want := "Run:          katlctl config init cluster.yaml --installer http://[2001:db8::10]:8080"
	if !strings.Contains(got, want) {
		t.Fatalf("render missing %q:\n%s", want, got)
	}
}

func TestRenderRuntimeFailure(t *testing.T) {
	snapshot := Snapshot{
		Mode:              ModeRuntime,
		Version:           "2026.7.0-alpha.9",
		KubernetesVersion: "v1.36.1",
		Hostname:          "cp-1",
		State:             installstatus.StateRuntimeFailedNeedsRepair,
		Generation:        "4",
		GenerationHealth:  "unhealthy",
		LastError:         "boot health check failed",
		RetryHint:         "inspect the previous generation",
		SSHEnabled:        true,
		Network:           []NetworkInterface{{Name: "eno1", Addresses: []string{"192.0.2.20/24"}}},
	}
	got := string(renderDashboard(&snapshot, nil, 80, 20, false))
	for _, want := range []string{"Status", "Host", "Kubernetes", "Needs repair", "Unavailable", "2026.7.0-alpha.9", "v1.36.1", "Generation: 4", "Error:", "boot health check failed", "SSH: katl@192.0.2.20"} {
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
	plain := string(renderDashboard(&snapshot, nil, 80, 15, false))
	for _, want := range []string{"KatlOS\n", "Status\n", "State:      OK", "Generation: 0", "Journal"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("plain render missing %q:\n%s", want, plain)
		}
	}
	for _, unwanted := range []string{"runtime", "installed system", "boot=", "Target disk", "Progress:     Reboot", "\x1b["} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("plain render contains %q:\n%s", unwanted, plain)
		}
	}
	colored := string(renderDashboard(&snapshot, nil, 80, 15, true))
	if !strings.Contains(colored, styleTitle) || !strings.Contains(colored, styleGood) {
		t.Fatalf("coloured render lacks semantic styles: %q", colored)
	}
}

func TestRenderRuntimeUsesNestedStatusAndJournalPanes(t *testing.T) {
	snapshot := Snapshot{
		Mode:                   ModeRuntime,
		Version:                "2026.7.0-alpha.12",
		KubernetesVersion:      "v1.36.1",
		KubernetesBootstrapped: true,
		Hostname:               "cp-1",
		State:                  installstatus.StateWaitingForClusterBootstrap,
		Generation:             "4",
		GenerationHealth:       "healthy",
	}
	got := string(renderDashboard(&snapshot, testJournal{[]byte("latest journal event")}, 80, 18, false))
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	statusRow := lineIndex(lines, "Status")
	splitRow := lineIndexContaining(lines, "Host")
	journalRow := lineIndex(lines, "Journal")
	if statusRow < 0 || splitRow <= statusRow || journalRow <= splitRow {
		t.Fatalf("pane order = status %d, split %d, journal %d:\n%s", statusRow, splitRow, journalRow, got)
	}
	if !strings.Contains(lines[splitRow], "│Kubernetes") {
		t.Fatalf("split title row = %q", lines[splitRow])
	}
	stateRow := lines[splitRow+1]
	if !strings.Contains(stateRow, "State:      OK") || !strings.Contains(stateRow, "│State:      Bootstrapped") {
		t.Fatalf("split state row = %q", stateRow)
	}
	versionRow := lines[splitRow+2]
	if !strings.Contains(versionRow, "Node:       cp-1") || !strings.Contains(versionRow, "│Version:    v1.36.1") {
		t.Fatalf("split version row = %q", versionRow)
	}
	if !strings.Contains(got, "latest journal event") {
		t.Fatalf("journal pane missing content:\n%s", got)
	}
}

func TestRenderRuntimeSubPanesWrapOverflowIndependently(t *testing.T) {
	hostname := "cp-with-a-deliberately-long-hostname.example.test"
	version := "v1.36.1-with-a-deliberately-long-build-identifier"
	generationID := "generation-with-a-deliberately-long-identifier"
	snapshot := Snapshot{
		Mode:                   ModeRuntime,
		Version:                "2026.7.0-alpha.12-with-a-long-build-identifier",
		KubernetesVersion:      version,
		KubernetesBootstrapped: true,
		Hostname:               hostname,
		State:                  installstatus.StateWaitingForClusterBootstrap,
		Generation:             generationID,
		GenerationHealth:       "healthy",
	}
	plain := string(renderDashboard(&snapshot, nil, 40, 40, false))
	colored := string(renderDashboard(&snapshot, nil, 40, 40, true))
	for name, output := range map[string]string{"plain": plain, "colored": colored} {
		if strings.Contains(output, "~") {
			t.Fatalf("%s render truncated overflow:\n%s", name, output)
		}
		for number, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if width := visibleWidth(line); width > 40 {
				t.Fatalf("%s line %d width = %d: %q", name, number+1, width, line)
			}
		}
	}

	lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	splitRow := lineIndexContaining(lines, "Host")
	networkRow := lineIndexContaining(lines, "Network:")
	journalRow := lineIndex(lines, "Journal")
	if splitRow < 0 || networkRow <= splitRow || journalRow <= networkRow {
		t.Fatalf("missing nested panes:\n%s", plain)
	}
	var left, right strings.Builder
	for _, line := range lines[splitRow:networkRow] {
		divider := strings.IndexRune(line, '│')
		if divider < 0 || len([]rune(line[:divider])) != 19 {
			t.Fatalf("unstable divider in %q", line)
		}
		left.WriteString(strings.ReplaceAll(line[:divider], " ", ""))
		right.WriteString(strings.ReplaceAll(line[divider+len("│"):], " ", ""))
	}
	for value, side := range map[string]string{hostname: left.String(), generationID: left.String(), version: right.String()} {
		if !strings.Contains(side, value) {
			t.Fatalf("wrapped panes lost %q:\n%s", value, plain)
		}
	}
}

func lineIndex(lines []string, value string) int {
	for index, line := range lines {
		if line == value {
			return index
		}
	}
	return -1
}

func lineIndexContaining(lines []string, value string) int {
	for index, line := range lines {
		if strings.Contains(line, value) {
			return index
		}
	}
	return -1
}

func TestRenderRebootHidesRedundantProgress(t *testing.T) {
	snapshot := Snapshot{
		Mode:        ModeInstaller,
		State:       installstatus.StateRebootRequested,
		CurrentStep: "Reboot",
	}
	got := string(renderDashboard(&snapshot, nil, 80, 15, false))
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
	got := string(renderDashboard(&snapshot, nil, 40, 40, false))
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

func TestTerminalRenderDoesNotScrollCompletedFrame(t *testing.T) {
	snapshot := Snapshot{
		Mode:       ModeRuntime,
		Hostname:   "cp-with-a-long-name",
		Version:    "2026.7.0-alpha.15",
		Generation: "generation-with-a-long-identifier",
		Network: []NetworkInterface{{
			Name:      "enp1s0",
			Addresses: []string{"2001:db8:1234:5678::10/64", "192.0.2.10/24"},
		}},
	}
	const width, height = 40, 30
	got := renderDashboard(&snapshot, testJournal{[]byte(strings.Repeat("j", width))}, width, height, true)
	terminal := emulateTerminal(t, got, width, height)
	if terminal.scrolls != 0 {
		t.Fatalf("completed frame scrolled %d times", terminal.scrolls)
	}
	if row := strings.TrimSpace(string(terminal.rows[0])); row != "KatlOS" {
		t.Fatalf("first terminal row = %q", row)
	}
	if row := strings.TrimSpace(string(terminal.rows[height-1])); !strings.HasPrefix(row, "Ctrl+Alt+F2: console") {
		t.Fatalf("footer terminal row = %q", row)
	}
}

type terminalState struct {
	rows    [][]rune
	row     int
	column  int
	pending bool
	scrolls int
}

func emulateTerminal(t *testing.T, data []byte, width, height int) terminalState {
	t.Helper()
	state := terminalState{rows: make([][]rune, height)}
	for index := range state.rows {
		state.rows[index] = []rune(strings.Repeat(" ", width))
	}
	for position := 0; position < len(data); {
		if data[position] == '\x1b' {
			next := skipANSIBytes(data, position)
			sequence := string(data[position:next])
			switch sequence {
			case "\x1b[H":
				state.row, state.column, state.pending = 0, 0, false
			case "\x1b[2J":
				for row := range state.rows {
					for column := range state.rows[row] {
						state.rows[row][column] = ' '
					}
				}
			}
			position = next
			continue
		}
		switch data[position] {
		case '\r':
			state.column, state.pending = 0, false
			position++
			continue
		case '\n':
			state.row++
			state.pending = false
			if state.row == height {
				copy(state.rows, state.rows[1:])
				state.rows[height-1] = []rune(strings.Repeat(" ", width))
				state.row--
				state.scrolls++
			}
			position++
			continue
		}
		value, size := utf8.DecodeRune(data[position:])
		if state.pending {
			state.row++
			state.column, state.pending = 0, false
			if state.row == height {
				copy(state.rows, state.rows[1:])
				state.rows[height-1] = []rune(strings.Repeat(" ", width))
				state.row--
				state.scrolls++
			}
		}
		state.rows[state.row][state.column] = value
		if state.column == width-1 {
			state.pending = true
		} else {
			state.column++
		}
		position += size
	}
	return state
}

func TestJournalLineWrapsAndStripsANSI(t *testing.T) {
	value := []byte("2026-07-14T00:01:02+01:00 host service[1]: " + strings.Repeat("payload", 8) + "\x1b[31m")
	writer := NewJournalWriter(NewRenderTarget(make([]byte, RenderCapacity(40, 10)), 40, 10))
	writer.WriteLine(value)
	got, rows := writer.Bytes(), writer.RowsWritten()
	if rows < 2 || strings.Contains(string(got), "\x1b") || strings.Contains(string(got), "[31m") || strings.Contains(string(got), "~") {
		t.Fatalf("JournalWriter.WriteLine() = %q, rows=%d", got, rows)
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
		Mode:                   ModeRuntime,
		State:                  installstatus.StateKubeadmReady,
		Hostname:               "cp-1",
		Version:                "2026.7.0-alpha.15",
		Generation:             "4",
		GenerationHealth:       "healthy",
		KubernetesVersion:      "v1.36.1",
		KubernetesBootstrapped: true,
		SSHEnabled:             true,
		Network: []NetworkInterface{{
			Name:      "enp1s0",
			Addresses: []string{"10.1.2.254/24", "fd00::254/64"},
		}},
	}
	journal := testJournal{[]byte("journal line")}
	for _, color := range []bool{false, true} {
		name := "plain"
		if color {
			name = "color"
		}
		t.Run(name, func(t *testing.T) {
			storage := make([]byte, RenderCapacity(80, 25))
			renderer := NewRenderer(NewRenderTarget(storage, 80, 25), color)
			buffer := renderer.Render(&snapshot, &journal)
			start := &buffer[0]

			allocations := testing.AllocsPerRun(1000, func() {
				buffer = renderer.Render(&snapshot, &journal)
			})
			if allocations != 0 {
				t.Fatalf("Renderer.Render() allocations = %v, want 0", allocations)
			}
			if &buffer[0] != start {
				t.Fatal("Renderer.Render() replaced owned storage")
			}
		})
	}
}

func TestRendererStartsAtTopLeft(t *testing.T) {
	snapshot := Snapshot{Mode: ModeRuntime}
	for _, test := range []struct {
		name   string
		color  bool
		prefix []byte
	}{
		{name: "plain", prefix: []byte("KatlOS")},
		{name: "terminal", color: true, prefix: []byte(clearScreen + styleTitle + "KatlOS")},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := bytes.Repeat([]byte{'x'}, RenderCapacity(80, 25))
			renderer := NewRenderer(NewRenderTarget(storage, 80, 25), test.color)
			if got := renderer.Render(&snapshot, nil); !bytes.HasPrefix(got, test.prefix) {
				t.Fatalf("render prefix = %q, want %q", got[:min(len(got), len(test.prefix))], test.prefix)
			}
		})
	}
}

type testJournal [][]byte

func (j testJournal) WriteTail(writer *JournalWriter) {
	rows := min(writer.RowsRemaining(), len(j))
	for _, line := range j[len(j)-rows:] {
		if !writer.WriteLine(line) {
			break
		}
	}
}

func renderDashboard(snapshot *Snapshot, journal Journal, width, height int, color bool) []byte {
	target := NewRenderTarget(make([]byte, RenderCapacity(width, height)), width, height)
	renderer := NewRenderer(target, color)
	return renderer.Render(snapshot, journal)
}
