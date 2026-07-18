package operatorconsole

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestReadHandoffRejectsSemanticallyCorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(path, []byte(`{"url":"file:///tmp/config"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandoff(path); err == nil || !strings.Contains(err.Error(), "absolute HTTP or HTTPS") {
		t.Fatalf("ReadHandoff() error = %v", err)
	}
}

func TestValidateHandoffRejectsHostlessURL(t *testing.T) {
	record := Handoff{
		URL:       "http://:8080/v1/config-bundle",
		UpdatedAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	}
	if err := validateHandoff(record); err == nil || !strings.Contains(err.Error(), "absolute HTTP or HTTPS") {
		t.Fatalf("validateHandoff() error = %v", err)
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
		DefaultRouteInterface: func() (string, error) { return "enp1s0", nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.State != installstatus.StateRunning || snapshot.CurrentStep != "InstallRootSlot" || !snapshot.DestructiveMutation {
		t.Fatalf("snapshot status = %#v", snapshot)
	}
	if snapshot.ManagementAddress != "192.0.2.10" || len(snapshot.DisplayInterfaces) != 1 || strings.Join(snapshot.DisplayInterfaces[0].Addresses, ",") != "192.0.2.10/24" {
		t.Fatalf("snapshot network = %#v", snapshot.DisplayInterfaces)
	}
	if snapshot.Handoff.URL != "http://192.0.2.10:8080/v1/config-bundle" {
		t.Fatalf("snapshot handoff = %#v", snapshot)
	}
	network := &snapshot.DisplayInterfaces[0]
	address := &snapshot.DisplayInterfaces[0].Addresses[0]
	collector.Collect(&snapshot)
	if &snapshot.DisplayInterfaces[0] != network || &snapshot.DisplayInterfaces[0].Addresses[0] != address {
		t.Fatal("Collect() did not reuse snapshot network storage")
	}
}

func TestCollectorCuratesManagementNetwork(t *testing.T) {
	interfaces := []net.Interface{
		{Name: "cilium_host", Flags: net.FlagUp},
		{Name: "veth1234", Flags: net.FlagUp},
		{Name: "eno4", Flags: net.FlagUp},
		{Name: "eno2", Flags: net.FlagUp},
		{Name: "eno1", Flags: net.FlagUp},
		{Name: "ens3", Flags: net.FlagUp},
		{Name: "eno3", Flags: net.FlagUp},
		{Name: "unused0", Flags: net.FlagUp},
	}
	addresses := map[string][]net.Addr{
		"cilium_host": {testAddr("10.0.0.1/32")},
		"veth1234":    {testAddr("10.0.0.2/32")},
		"ens3":        {testAddr("192.0.2.10/24"), testAddr("2001:db8::10/64"), testAddr("2001:db8::11/64")},
		"eno1":        {testAddr("192.0.2.11/24")},
		"eno2":        {testAddr("192.0.2.12/24")},
		"eno3":        {testAddr("192.0.2.13/24")},
		"eno4":        {testAddr("192.0.2.14/24")},
	}
	collector := Collector{
		Mode:       ModeRuntime,
		Interfaces: func() ([]net.Interface, error) { return interfaces, nil },
		Addrs:      func(iface net.Interface) ([]net.Addr, error) { return addresses[iface.Name], nil },
		DefaultRouteInterface: func() (string, error) {
			return "ens3", nil
		},
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.ManagementAddress != "192.0.2.10" {
		t.Fatalf("management address = %q", snapshot.ManagementAddress)
	}
	if got := interfaceNames(snapshot.DisplayInterfaces); strings.Join(got, ",") != "ens3,eno1,eno2" {
		t.Fatalf("display interfaces = %v", got)
	}
	if snapshot.AdditionalInterfaces != 2 || snapshot.DisplayInterfaces[0].AdditionalAddresses != 1 {
		t.Fatalf("network omissions = interfaces %d, addresses %#v", snapshot.AdditionalInterfaces, snapshot.DisplayInterfaces[0])
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 24, false))
	for _, want := range []string{"SSH:katl@192.0.2.10", "+1address", "+2interfaces"} {
		if !containsIgnoringLayout(rendered, want) {
			t.Fatalf("network render missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "cilium") || strings.Contains(rendered, "veth") {
		t.Fatalf("network render exposed virtual interfaces:\n%s", rendered)
	}
}

func TestCollectorPrefersKnownManagementAddress(t *testing.T) {
	collector := Collector{
		Mode:              ModeRuntime,
		ManagementAddress: "192.0.2.99/24",
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Name: "ens3", Flags: net.FlagUp}, {Name: "eno2", Flags: net.FlagUp}}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			if iface.Name == "eno2" {
				return []net.Addr{testAddr("192.0.2.99/24")}, nil
			}
			return []net.Addr{testAddr("192.0.2.10/24")}, nil
		},
		DefaultRouteInterface: func() (string, error) { return "ens3", nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.ManagementAddress != "192.0.2.99" || snapshot.DisplayInterfaces[0].Name != "eno2" {
		t.Fatalf("known management selection = %#v", snapshot)
	}
}

func TestReadDefaultRouteInterface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route")
	data := "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n" +
		"eno2 00000000 010200C0 0003 0 0 200 00000000 0 0 0\n" +
		"ens3 00000000 010200C0 0003 0 0 100 00000000 0 0 0\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readDefaultRouteInterface(path); err != nil || got != "ens3" {
		t.Fatalf("default route = %q, %v", got, err)
	}
}

func TestCollectorSurfacesCorruptHandoff(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "handoff.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := Collector{
		Mode:        ModeInstaller,
		Root:        root,
		HandoffPath: path,
		Interfaces:  func() ([]net.Interface, error) { return nil, nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.HandoffError == "" || snapshot.Handoff.URL != "" {
		t.Fatalf("corrupt handoff snapshot = %#v", snapshot)
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 18, false))
	if !containsIgnoringLayout(rendered, "Handoffread:installerhandoffisunreadable") {
		t.Fatalf("corrupt handoff not visible:\n%s", rendered)
	}
}

func TestCollectorDoesNotPresentBinaryVersionForCorruptGeneration(t *testing.T) {
	root := t.TempDir()
	path, err := generation.BootSelectionPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := Collector{
		Mode:       ModeRuntime,
		Version:    "console-binary-version",
		Root:       root,
		Interfaces: func() ([]net.Interface, error) { return nil, nil },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if snapshot.GenerationError == "" || snapshot.Version != "" || snapshot.CurrentSoftware.KatlOSVersion != "" {
		t.Fatalf("corrupt generation snapshot = %#v", snapshot)
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 20, false))
	if strings.Contains(rendered, "console-binary-version") || !containsIgnoringLayout(rendered, "Generationread:generationmetadataisunavailable") || !strings.Contains(rendered, "Unknown") {
		t.Fatalf("corrupt generation presentation:\n%s", rendered)
	}
}

func TestCollectorMarksStaleInstallingState(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	statusPath := filepath.Join(root, "status.json")
	if err := installstatus.WriteFile(statusPath, installstatus.New(installstatus.StateRunning, now.Add(-11*time.Minute))); err != nil {
		t.Fatal(err)
	}
	collector := Collector{
		Mode:        ModeInstaller,
		Root:        root,
		StatusPath:  statusPath,
		HandoffPath: filepath.Join(root, "missing-handoff.json"),
		Interfaces:  func() ([]net.Interface, error) { return nil, nil },
		Now:         func() time.Time { return now },
	}
	var snapshot Snapshot
	collector.Collect(&snapshot)
	if !snapshot.StatusStale {
		t.Fatalf("stale snapshot = %#v", snapshot)
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 18, false))
	if strings.Contains(rendered, "Installing") || !containsIgnoringLayout(rendered, "Unknown(stalestatus)") || !containsIgnoringLayout(rendered, "Statusstale:") {
		t.Fatalf("stale state presentation:\n%s", rendered)
	}
}

func TestRuntimePresentationStatesFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		state  string
		health string
		stale  bool
		want   PresentationState
		style  Style
	}{
		{name: "healthy", state: installstatus.StateKubeadmReady, health: generation.HealthStateHealthy, want: PresentationHealthy, style: styleGood},
		{name: "deferred", state: installstatus.StateKubeadmReady, health: generation.HealthStateDeferred, want: PresentationProgressing, style: styleWarn},
		{name: "unknown health", state: installstatus.StateKubeadmReady, health: generation.HealthStateUnknown, want: PresentationUnknown, style: styleDim},
		{name: "unfamiliar health", state: installstatus.StateKubeadmReady, health: "mystery", want: PresentationUnknown, style: styleDim},
		{name: "unfamiliar state", state: "future-runtime-state", health: generation.HealthStateHealthy, want: PresentationUnknown, style: styleDim},
		{name: "stale", state: installstatus.StateKubeadmReady, health: generation.HealthStateHealthy, stale: true, want: PresentationUnknown, style: styleDim},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			presentation := NewDashboardModel(&Snapshot{Mode: ModeRuntime, State: test.state, GenerationHealth: test.health, StatusStale: test.stale}).Host
			if presentation.State != test.want {
				t.Fatalf("presentation = %#v, want %q", presentation, test.want)
			}
			if got := presentationStyle(presentation.State); got != test.style {
				t.Fatalf("presentation style = %q, want %q", got, test.style)
			}
		})
	}
}

func TestKubernetesConfigurationIsNotPresentedAsHealthy(t *testing.T) {
	snapshot := Snapshot{
		Mode:                 ModeRuntime,
		State:                installstatus.StateWaitingForClusterBootstrap,
		LiveSoftware:         Software{Generation: "4", KubernetesVersion: "v1.36.1"},
		KubernetesConfigured: true,
	}
	presentation := NewDashboardModel(&snapshot).Kubernetes
	if presentation.State != PresentationProgressing || presentation.Label != "Configured" {
		t.Fatalf("Kubernetes presentation = %#v", presentation)
	}
}

func TestPendingGenerationHealthIsWarningNotFailure(t *testing.T) {
	for _, health := range []string{generation.HealthStateUnknown, generation.HealthStateDeferred} {
		label := healthLabel(health)
		if style := healthStyle(label); style != styleWarn {
			t.Fatalf("health %q label %q style = %q", health, label, style)
		}
	}
}

func interfaceNames(interfaces []NetworkInterface) []string {
	names := make([]string, len(interfaces))
	for index := range interfaces {
		names[index] = interfaces[index].Name
	}
	return names
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
				ID:           "katlos",
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
	if snapshot.CurrentSoftware.KatlOSVersion != "2026.7.0-alpha.12" || snapshot.LiveSoftware.KubernetesVersion != "v1.36.1" || !snapshot.KubernetesConfigured {
		t.Fatalf("installed state = current %#v, live %#v, configured=%t", snapshot.CurrentSoftware, snapshot.LiveSoftware, snapshot.KubernetesConfigured)
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
	if snapshot.CurrentSoftware.Generation != "0" || snapshot.NextBootSoftware.Generation != "bootstrap-init-candidate" || snapshot.LiveSoftware.Generation != "bootstrap-init-candidate" {
		t.Fatalf("software provenance = current %#v, next %#v, live %#v", snapshot.CurrentSoftware, snapshot.NextBootSoftware, snapshot.LiveSoftware)
	}
	if snapshot.CurrentSoftware.KatlOSVersion != "2026.7.0-alpha.16" || snapshot.LiveSoftware.KubernetesVersion != "v1.36.1" || !snapshot.KubernetesConfigured {
		t.Fatalf("software state = current %#v, live %#v, configured=%t", snapshot.CurrentSoftware, snapshot.LiveSoftware, snapshot.KubernetesConfigured)
	}
	rendered := string(renderDashboard(&snapshot, nil, 80, 20, false))
	for _, want := range []string{"Current:generation0", "Nextboot:generationbootstrap-init-candidate", "Liveselected:generationbootstrap-init-candidate", "Version:v1.36.1", "Configured"} {
		if !containsIgnoringLayout(rendered, want) {
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
				ID:           "katlos",
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
		ManagementAddress:   "192.0.2.99",
		DisplayInterfaces:   []NetworkInterface{{Name: "enp1s0", Addresses: []string{"192.0.2.10/24"}}},
	}
	journal := testJournal{[]byte("old line"), []byte("installing root\x1b[31m"), []byte("latest line")}
	got := string(renderDashboard(&snapshot, journal, 80, 25, false))
	for _, want := range []string{
		"KatlOS Installer",
		"State:Installing",
		"Network:enp1s0:192.0.2.10/24",
		"Media:2026.7.0-alpha.9",
		"Diskchanges:started-donotpoweroff",
		"Configure:http://192.0.2.10:8080/v1/config-bundle",
		"Run:katlctlconfiginitcluster.yaml--installer192.0.2.10",
		"Journal",
		"installing root",
		"Ctrl+Alt+F2:console|SSHdisabled",
	} {
		if !containsIgnoringLayout(got, want) {
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
		Mode:              ModeInstaller,
		State:             installstatus.StateWaitingForConfig,
		Handoff:           Handoff{URL: "http://[2001:db8::10]:8080/v1/config-bundle"},
		DisplayInterfaces: []NetworkInterface{{Name: "enp1s0", Addresses: []string{"2001:db8::10/64"}}},
	}
	got := string(renderDashboard(&snapshot, nil, 100, 18, false))
	want := "Run:katlctlconfiginitcluster.yaml--installerhttp://[2001:db8::10]:8080"
	if !containsIgnoringLayout(got, want) {
		t.Fatalf("render missing %q:\n%s", want, got)
	}
}

func TestRenderInstallerCommandPreservesCanonicalEndpoint(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "https custom port", url: "https://192.0.2.50:8443/v1/config-bundle", want: "https://192.0.2.50:8443"},
		{name: "http custom port", url: "http://192.0.2.50:9090/v1/config-bundle", want: "http://192.0.2.50:9090"},
		{name: "hostname", url: "http://installer.example.test:8080/v1/config-bundle", want: "http://installer.example.test:8080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Snapshot{
				Mode:              ModeInstaller,
				State:             installstatus.StateWaitingForConfig,
				ManagementAddress: "192.0.2.10",
				Handoff:           Handoff{URL: test.url},
			}
			got := string(renderDashboard(&snapshot, nil, 100, 18, false))
			want := "Run:katlctlconfiginitcluster.yaml--installer" + test.want
			if !containsIgnoringLayout(got, want) {
				t.Fatalf("render missing %q:\n%s", want, got)
			}
		})
	}
}

func TestRenderRuntimeFailure(t *testing.T) {
	snapshot := Snapshot{
		Mode:                 ModeRuntime,
		Hostname:             "cp-1",
		State:                installstatus.StateRuntimeFailedNeedsRepair,
		CurrentSoftware:      Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.9"},
		NextBootSoftware:     Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.9"},
		LiveSoftware:         Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.9", KubernetesVersion: "v1.36.1"},
		GenerationHealth:     "unhealthy",
		KubernetesConfigured: true,
		LastError:            "boot health check failed",
		RetryHint:            "inspect the previous generation",
		SSHEnabled:           true,
		ManagementAddress:    "192.0.2.20",
		DisplayInterfaces:    []NetworkInterface{{Name: "eno1", Addresses: []string{"192.0.2.20/24"}}},
	}
	got := string(renderDashboard(&snapshot, nil, 80, 20, false))
	for _, want := range []string{"Status", "Host", "Kubernetes", "Needsrepair", "Unavailable", "2026.7.0-alpha.9", "v1.36.1", "Current:generation4", "Error:", "boothealthcheckfailed", "SSH:katl@192.0.2.20"} {
		if !containsIgnoringLayout(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderReservesFailureAndRecoveryAtShortHeight(t *testing.T) {
	interfaces := make([]NetworkInterface, 20)
	for index := range interfaces {
		interfaces[index] = NetworkInterface{
			Name:      "interface-" + strconv.Itoa(index),
			Addresses: []string{"192.0.2." + strconv.Itoa(index+1) + "/24"},
		}
	}
	snapshot := Snapshot{
		Mode:              ModeInstaller,
		State:             installstatus.StateFailedAfterMutation,
		Version:           strings.Repeat("version", 20),
		DisplayInterfaces: interfaces,
		Handoff:           Handoff{URL: "https://192.0.2.10:8443/v1/config-bundle"},
		LastError:         "disk write failed",
		RetryHint:         "boot recovery media",
	}
	got := string(renderDashboard(&snapshot, testJournal{[]byte("journal noise")}, 40, minimumHeight, false))
	for _, want := range []string{"Installationfailed", "Error:diskwritefailed", "Nextaction:bootrecoverymedia", "…"} {
		if !containsIgnoringLayout(got, want) {
			t.Fatalf("short failure render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderBudgetsEachActiveAlert(t *testing.T) {
	snapshot := Snapshot{
		Mode:        ModeRuntime,
		State:       installstatus.StateRuntimeFailedNeedsRepair,
		LastError:   strings.Repeat("failure ", 80),
		RetryHint:   strings.Repeat("recover ", 80),
		StatusError: strings.Repeat("corrupt ", 80),
	}
	got := string(renderDashboard(&snapshot, nil, 40, minimumHeight, false))
	for _, label := range []string{"Error:", "Nextaction:", "Statusread:"} {
		if !containsIgnoringLayout(got, label) {
			t.Fatalf("budgeted alerts missing %q:\n%s", label, got)
		}
	}
}

func TestRenderKatlOSHealthAndColour(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		State:            installstatus.StateKubeadmReady,
		CurrentSoftware:  Software{Generation: "0"},
		NextBootSoftware: Software{Generation: "0"},
		LiveSoftware:     Software{Generation: "0"},
		GenerationHealth: "healthy",
		CurrentStep:      "Reboot",
	}
	plain := string(renderDashboard(&snapshot, nil, 80, 15, false))
	for _, want := range []string{"KatlOS", "Status", "State:Healthy", "Current:generation0", "Journal"} {
		if !containsIgnoringLayout(plain, want) {
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
		Mode:                 ModeRuntime,
		CurrentSoftware:      Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.12"},
		NextBootSoftware:     Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.12"},
		LiveSoftware:         Software{Generation: "4", KatlOSVersion: "2026.7.0-alpha.12", KubernetesVersion: "v1.36.1"},
		KubernetesConfigured: true,
		Hostname:             "cp-1",
		State:                installstatus.StateWaitingForClusterBootstrap,
		GenerationHealth:     "healthy",
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
	if !containsIgnoringLayout(stateRow, "State:Healthy") || !containsIgnoringLayout(stateRow, "│State:Configured") {
		t.Fatalf("split state row = %q", stateRow)
	}
	versionRow := lines[splitRow+2]
	if !containsIgnoringLayout(versionRow, "Node:cp-1") || !containsIgnoringLayout(versionRow, "│Version:v1.36.1") {
		t.Fatalf("split version row = %q", versionRow)
	}
	if !strings.Contains(got, "latest journal event") {
		t.Fatalf("journal pane missing content:\n%s", got)
	}
}

func TestRenderRuntimeStacksPanesAtNarrowWidths(t *testing.T) {
	hostname := "cp-with-a-deliberately-long-hostname.example.test"
	version := "v1.36.1-with-a-deliberately-long-build-identifier"
	generationID := "generation-with-a-deliberately-long-identifier"
	snapshot := Snapshot{
		Mode:                 ModeRuntime,
		CurrentSoftware:      Software{Generation: generationID, KatlOSVersion: "2026.7.0-alpha.12-with-a-long-build-identifier"},
		NextBootSoftware:     Software{Generation: generationID, KatlOSVersion: "2026.7.0-alpha.12-with-a-long-build-identifier"},
		LiveSoftware:         Software{Generation: generationID, KatlOSVersion: "2026.7.0-alpha.12-with-a-long-build-identifier", KubernetesVersion: version},
		KubernetesConfigured: true,
		Hostname:             hostname,
		State:                installstatus.StateWaitingForClusterBootstrap,
		GenerationHealth:     "healthy",
	}
	plain := string(renderDashboard(&snapshot, nil, 40, 60, false))
	colored := string(renderDashboard(&snapshot, nil, 40, 60, true))
	for name, output := range map[string]string{"plain": plain, "colored": colored} {
		if strings.Contains(output, "│") || strings.Contains(output, "…") {
			t.Fatalf("%s narrow render used split panes or truncated content:\n%s", name, output)
		}
		for number, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if width := visibleWidth(line); width > 40 {
				t.Fatalf("%s line %d width = %d: %q", name, number+1, width, line)
			}
		}
	}

	hostRow := strings.Index(plain, "Host")
	kubernetesRow := strings.Index(plain, "Kubernetes")
	networkRow := strings.Index(plain, "Network:")
	journalRow := strings.Index(plain, "Journal")
	if hostRow < 0 || kubernetesRow <= hostRow || networkRow <= kubernetesRow || journalRow <= networkRow {
		t.Fatalf("unexpected stacked pane order:\n%s", plain)
	}
	for _, value := range []string{hostname, generationID, version} {
		if !containsIgnoringLayout(plain, value) {
			t.Fatalf("wrapped panes lost %q:\n%s", value, plain)
		}
	}
}

func TestRenderRuntimeUsesSplitPanesOnlyAtWideBreakpoint(t *testing.T) {
	snapshot := Snapshot{Mode: ModeRuntime, State: installstatus.StateKubeadmReady}
	if narrow := string(renderDashboard(&snapshot, nil, wideLayoutWidth-1, 24, false)); strings.Contains(narrow, "│") {
		t.Fatalf("narrow render contains divider:\n%s", narrow)
	}
	if wide := string(renderDashboard(&snapshot, nil, wideLayoutWidth, 24, false)); !strings.Contains(wide, "│") {
		t.Fatalf("wide render lacks divider:\n%s", wide)
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

func containsIgnoringLayout(value, want string) bool {
	normalize := func(input string) string {
		replacer := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "│", "")
		return replacer.Replace(input)
	}
	return strings.Contains(normalize(value), normalize(want))
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
		DisplayInterfaces: []NetworkInterface{{
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

func TestRenderUsesActualUndersizedTerminal(t *testing.T) {
	snapshot := Snapshot{
		Mode:              ModeInstaller,
		State:             "starting-installer",
		ManagementAddress: "192.0.2.10",
		DisplayInterfaces: []NetworkInterface{{
			Name:      "enp1s0",
			Addresses: []string{"192.0.2.10/24"},
		}},
	}
	const width, height = 20, 8
	got := string(renderDashboard(&snapshot, nil, width, height, false))
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != height {
		t.Fatalf("rendered rows = %d, want %d:\n%s", len(lines), height, got)
	}
	for number, line := range lines {
		if visibleWidth(line) > width {
			t.Fatalf("line %d exceeds actual width: %q", number+1, line)
		}
	}
	for _, want := range []string{"KatlOS", "Starting installer", "192.0.2.10", "F2: console"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compact render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderCompactPrioritizesRecoveryGuidance(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
	}{
		{name: "below minimum width", width: minimumWidth - 1, height: minimumHeight},
		{name: "below minimum height", width: minimumWidth, height: minimumHeight - 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Snapshot{
				Mode:            ModeRuntime,
				LastError:       "runtime failed",
				RetryHint:       "boot recovery media",
				StatusError:     "status unreadable",
				StatusStale:     true,
				HandoffError:    "handoff unreadable",
				GenerationError: "generation unreadable",
			}
			got := string(renderDashboard(&snapshot, nil, test.width, test.height, false))
			for _, want := range []string{
				"Error:runtime failed",
				"Next action:boot recovery media",
				"Status read:status unreadable",
				"Status stale:state has not updated",
				"Handoff read:handoff unreadable",
				"Generation read:generation unreadable",
			} {
				if !containsIgnoringLayout(got, want) {
					t.Fatalf("compact render missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestRenderVeryNarrowCompactShowsRecoveryText(t *testing.T) {
	snapshot := Snapshot{
		Mode:      ModeInstaller,
		LastError: "disk write failed",
		RetryHint: "boot recovery media",
	}
	got := string(renderDashboard(&snapshot, nil, 20, 8, false))
	for _, want := range []string{"Error:disk write failed", "Next action:boot recovery media"} {
		if !containsIgnoringLayout(got, want) {
			t.Fatalf("very narrow compact render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderDimensionsAreCapped(t *testing.T) {
	if got, want := RenderCapacity(maximumWidth+1, maximumHeight+1), RenderCapacity(maximumWidth, maximumHeight); got != want {
		t.Fatalf("oversized render capacity = %d, want %d", got, want)
	}
	if got, want := RenderCapacity(20, 8), 8*(20*utf8.UTFMax+32); got != want {
		t.Fatalf("undersized render capacity = %d, want %d", got, want)
	}
}

func TestTerminalRenderDoesNotScrollCompletedFrame(t *testing.T) {
	snapshot := Snapshot{
		Mode:             ModeRuntime,
		Hostname:         "cp-with-a-long-name",
		CurrentSoftware:  Software{Generation: "generation-with-a-long-identifier", KatlOSVersion: "2026.7.0-alpha.15"},
		NextBootSoftware: Software{Generation: "generation-with-a-long-identifier", KatlOSVersion: "2026.7.0-alpha.15"},
		LiveSoftware:     Software{Generation: "generation-with-a-long-identifier", KatlOSVersion: "2026.7.0-alpha.15"},
		DisplayInterfaces: []NetworkInterface{{
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
			next := position + skipTerminalSequence(string(data[position:]))
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
		if width := visibleWidth(line); width > 40 {
			t.Fatalf("line %d width = %d: %q", number+1, width, line)
		}
	}
	if joined := strings.ReplaceAll(string(got), "\n", ""); joined != strings.TrimSuffix(string(value), "\x1b[31m") {
		t.Fatalf("wrapped content changed: %q", joined)
	}
}

func TestViewportMeasuresDisplayGraphemes(t *testing.T) {
	value := "界e\u0301👩‍💻" + string([]byte{0xff})
	if got, want := displayWidth(value), 6; got != want {
		t.Fatalf("display width = %d, want %d", got, want)
	}
	frame := newFrame(5, 2)
	viewport := NewViewport(&frame, Rect{Width: 5, Height: 2})
	result := viewport.Write(value, WrapOptions{})
	if result.Truncated || result.Rows != 2 {
		t.Fatalf("write result = %#v", result)
	}
	if got := frameText(&frame, Rect{Width: 5, Height: 2}); got != "界é👩‍💻�" {
		t.Fatalf("painted graphemes = %q", got)
	}
	assertGraphemesFit(t, &frame, Rect{Width: 5, Height: 2})
}

func TestViewportPreservesLongValuesAndMarksVerticalOverflow(t *testing.T) {
	value := strings.Repeat("abcdefghij", 8)
	frame := newFrame(7, 20)
	viewport := NewViewport(&frame, Rect{Width: 7, Height: 20})
	if result := viewport.Write(value, WrapOptions{WordWrap: true}); result.Truncated {
		t.Fatalf("value unexpectedly truncated: %#v", result)
	}
	if got := frameText(&frame, Rect{Width: 7, Height: 20}); got != value {
		t.Fatalf("painted value changed: %q", got)
	}

	shortFrame := newFrame(4, 1)
	short := NewViewport(&shortFrame, Rect{Width: 4, Height: 1})
	if result := short.Write("abcdefgh", WrapOptions{}); !result.Truncated {
		t.Fatalf("overflow result = %#v", result)
	}
	if got := shortFrame.Cells[3].Glyph; got != "…" {
		t.Fatalf("truncation cell = %q", got)
	}
}

func TestFieldsPanesAndJournalShareCellWrapping(t *testing.T) {
	value := strings.Repeat("界e\u0301👩‍💻", 12)
	paintField := func() string {
		frame := newFrame(12, 24)
		viewport := NewViewport(&frame, Rect{Width: 12, Height: 24})
		writeField(&viewport, "", value, "")
		return frameText(&frame, Rect{Width: 12, Height: 24})
	}
	paintPane := func() string {
		frame := newFrame(12, 24)
		viewport := NewViewport(&frame, Rect{Width: 12, Height: 24})
		writePane(&viewport, "", []paneField{{value: value}})
		return frameText(&frame, Rect{Width: 12, Height: 24})
	}
	paintJournal := func() string {
		frame := newFrame(12, 24)
		viewport := NewViewport(&frame, Rect{Width: 12, Height: 24})
		writer := newJournalWriter(viewport)
		writer.WriteLine([]byte(value))
		return frameText(&frame, Rect{Width: 12, Height: 24})
	}
	if field, pane, journal := paintField(), paintPane(), paintJournal(); field != value || pane != field || journal != field {
		t.Fatalf("shared wrapping changed content: field=%q pane=%q journal=%q", field, pane, journal)
	}
}

func TestWidePaneDividerIsPaintedAfterBoundedContent(t *testing.T) {
	renderer := NewRenderer(NewRenderTarget(make([]byte, RenderCapacity(80, 40)), 80, 40), false)
	content := NewViewport(&renderer.frame, Rect{Width: 80, Height: 38})
	snapshot := Snapshot{
		Mode:     ModeRuntime,
		Hostname: strings.Repeat("host界\x1b[31m", 30),
		CurrentSoftware: Software{
			Generation:    strings.Repeat("generation", 30),
			KatlOSVersion: strings.Repeat("version", 30),
		},
		NextBootSoftware: Software{
			Generation:    strings.Repeat("generation", 30),
			KatlOSVersion: strings.Repeat("version", 30),
		},
		LiveSoftware: Software{
			Generation:        strings.Repeat("generation", 30),
			KatlOSVersion:     strings.Repeat("version", 30),
			KubernetesVersion: strings.Repeat("v1.36.1👩‍💻", 30),
		},
	}
	renderer.writeRuntimeStatus(&content, &snapshot)
	divider := (content.bounds.Width - 1) / 2
	for row := 0; row < content.rowsUsed(); row++ {
		if got := renderer.frame.Cells[row*renderer.frame.Width+divider].Glyph; got != "│" {
			t.Fatalf("divider row %d = %q", row, got)
		}
	}
}

func FuzzViewportBounds(f *testing.F) {
	seeds := []string{
		"plain text",
		"界e\u0301👩‍💻",
		"before\x1b[31mred\x1b[0mafter",
		string([]byte{'a', 0xff, 'b'}),
		strings.Repeat("x", 256),
	}
	for index, seed := range seeds {
		for _, width := range []uint8{0, 39, 70, 71, 119} {
			f.Add(seed, width, uint8(1+index))
		}
	}
	f.Fuzz(func(t *testing.T, value string, widthSeed, heightSeed uint8) {
		width := int(widthSeed%120) + 1
		height := int(heightSeed%32) + 1
		frame := newFrame(width+2, height+2)
		for index := range frame.Cells {
			frame.Cells[index] = Cell{Glyph: "#", span: 1}
		}
		bounds := Rect{X: 1, Y: 1, Width: width, Height: height}
		viewport := NewViewport(&frame, bounds)
		viewport.Write(value, WrapOptions{WordWrap: true})
		for y := range frame.Height {
			for x := range frame.Width {
				inside := x >= bounds.X && x < bounds.X+bounds.Width && y >= bounds.Y && y < bounds.Y+bounds.Height
				if !inside && frame.Cells[y*frame.Width+x].Glyph != "#" {
					t.Fatalf("viewport modified sentinel at %d,%d", x, y)
				}
			}
		}
		assertGraphemesFit(t, &frame, bounds)
	})
}

func frameText(frame *Frame, bounds Rect) string {
	var text strings.Builder
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		for x := bounds.X; x < bounds.X+bounds.Width; x++ {
			cell := frame.Cells[y*frame.Width+x]
			if !cell.continuation {
				text.WriteString(cell.Glyph)
			}
		}
	}
	return text.String()
}

func assertGraphemesFit(t *testing.T, frame *Frame, bounds Rect) {
	t.Helper()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		for x := bounds.X; x < bounds.X+bounds.Width; x++ {
			cell := frame.Cells[y*frame.Width+x]
			if cell.Glyph == "" || cell.continuation {
				continue
			}
			if cell.span < 1 || x+cell.span > bounds.X+bounds.Width {
				t.Fatalf("grapheme %q at %d,%d spans outside viewport", cell.Glyph, x, y)
			}
		}
	}
}

func TestRendererBoundsArbitraryContentAtSupportedWidths(t *testing.T) {
	snapshot := Snapshot{
		Mode:                 ModeRuntime,
		State:                installstatus.StateKubeadmReady,
		Hostname:             "控制面-e\u0301-👩‍💻\x1b[31m-node",
		CurrentSoftware:      Software{Generation: string([]byte{'g', 'e', 'n', 0xff}), KatlOSVersion: strings.Repeat("version", 20)},
		NextBootSoftware:     Software{Generation: string([]byte{'g', 'e', 'n', 0xff}), KatlOSVersion: strings.Repeat("version", 20)},
		LiveSoftware:         Software{Generation: string([]byte{'g', 'e', 'n', 0xff}), KatlOSVersion: strings.Repeat("version", 20), KubernetesVersion: strings.Repeat("v1.36.1", 20)},
		GenerationHealth:     "healthy",
		KubernetesConfigured: true,
		SSHEnabled:           true,
		ManagementAddress:    "10.1.2.254",
		DisplayInterfaces: []NetworkInterface{{
			Name:      strings.Repeat("interface", 12),
			Addresses: []string{"10.1.2.254/24", "fd00::254/64"},
		}},
	}
	journal := testJournal{[]byte("日志 e\u0301 👩‍💻 \x1b[31m" + strings.Repeat("payload", 20))}
	for width := 1; width <= 120; width++ {
		got := string(renderDashboard(&snapshot, journal, width, 32, false))
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		if len(lines) != 32 {
			t.Fatalf("width %d rendered %d rows", width, len(lines))
		}
		for row, line := range lines {
			if gotWidth := visibleWidth(line); gotWidth > width {
				t.Fatalf("width %d row %d occupies %d cells: %q", width, row, gotWidth, line)
			}
		}
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
