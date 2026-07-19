package bgpapivip

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
)

func TestFromControlPlaneEndpointRendersManagedPolicy(t *testing.T) {
	length := 32
	endpoint, err := controlplaneendpoint.Normalize(controlplaneendpoint.Config{
		Host: "api.home.example",
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: "10.40.0.10",
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: "10.0.0.1", ASN: 64500}},
				RouteExchange: []controlplaneendpoint.RouteExchange{{
					Name:       "cilium",
					ListenPort: 179,
					PeerASN:    64512,
					ExportToFabric: []controlplaneendpoint.PrefixEnvelope{{
						CIDR: "10.50.0.0/16", PrefixLength: &length,
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := FromControlPlaneEndpoint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "control-plane", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	bird := fileContent(t, plan.Files, BirdConfigPath)
	for _, want := range []string{
		"router id from \"*\";",
		"protocol static katl_api {\n  disabled;",
		"local 127.0.0.1 port 179 as 64512;",
		"neighbor 127.0.0.1 as 64512;",
		"if net ~ [ 10.50.0.0/16{32,32} ] then accept;",
		"import none;",
	} {
		if !strings.Contains(bird, want) {
			t.Fatalf("bird config missing %q:\n%s", want, bird)
		}
	}
}

func TestGeneratedConfigParsesWithPackagedBird(t *testing.T) {
	bird := strings.TrimSpace(os.Getenv("KATL_TEST_BIRD_BINARY"))
	if bird == "" {
		t.Skip("KATL_TEST_BIRD_BINARY is not set")
	}
	loader := strings.TrimSpace(os.Getenv("KATL_TEST_BIRD_LOADER"))
	libraryPath := strings.TrimSpace(os.Getenv("KATL_TEST_BIRD_LIBRARY_PATH"))

	endpoint, err := controlplaneendpoint.Normalize(controlplaneendpoint.Config{
		Host: "api.home.example",
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: "10.40.0.10",
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: "10.0.0.1", ASN: 64500}},
				RouteExchange: []controlplaneendpoint.RouteExchange{{
					Name: "cilium", ListenPort: 179, PeerASN: 64512,
					ExportToFabric: []controlplaneendpoint.PrefixEnvelope{{CIDR: "10.50.0.0/16"}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := FromControlPlaneEndpoint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "control-plane", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "bird.conf")
	if err := os.WriteFile(configPath, []byte(fileContent(t, plan.Files, BirdConfigPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	command := bird
	args := []string{"-p", "-c", configPath}
	if loader != "" {
		command = loader
		args = append([]string{"--library-path", libraryPath, bird}, args...)
	}
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("packaged BIRD rejected generated config: %v\n%s", err, output)
	}
}

func TestRenderNativeEtcFilesMinimalIPv4DummyVIP(t *testing.T) {
	plan, err := RenderNativeEtcFiles(RenderRequest{
		NodeRole: "control-plane",
		Config:   minimalConfig(),
	})
	if err != nil {
		t.Fatalf("RenderNativeEtcFiles() error = %v", err)
	}
	assertFile(t, plan.Files, ConfigPath, "kind: BGPAPIEndpoint\n")
	assertFile(t, plan.Files, DummyNetDevPath, "Name=katl-api0\nKind=dummy\n")
	assertFile(t, plan.Files, NetworkPath, "Address=10.40.0.10/32\n")
	if filepath.Base(NetworkPath) >= "10-lan.network" {
		t.Fatalf("managed VIP network file %q must take precedence over Katl's default DHCP match", NetworkPath)
	}
	assertFile(t, plan.Files, BirdConfigPath, "router id 10.0.0.11;\n")
	assertFile(t, plan.Files, BirdConfigPath, "neighbor 10.0.0.1 as 64500;\n")
	assertFile(t, plan.Files, BirdConfigPath, "source address 10.0.0.11;\n")
	assertFile(t, plan.Files, AppDropInPath, "KATL_BGP_API_VIP_STATUS="+LiveStatusPath+"\n")
	assertFile(t, plan.Files, ConfigPath, "liveStatusPath: "+LiveStatusPath+"\n")
	assertFile(t, plan.Files, ConfigPath, "operationStatusPath: /var/lib/katl/operations/<operation-id>/apps/bgp-api-vip/status.json\n")
	if plan.Config.Endpoint.Port != 6443 || plan.Config.Endpoint.AddressFamily != "ipv4" {
		t.Fatalf("normalized endpoint = %#v", plan.Config.Endpoint)
	}
	if plan.Config.Health.Path != "/readyz" || plan.Config.Health.TLSServerName != "api.home.example" {
		t.Fatalf("normalized health = %#v", plan.Config.Health)
	}
}

func TestRenderMatchesMinimalIPv4Golden(t *testing.T) {
	plan, err := RenderNativeEtcFiles(RenderRequest{
		NodeRole: "control-plane",
		Config:   minimalConfig(),
	})
	if err != nil {
		t.Fatalf("RenderNativeEtcFiles() error = %v", err)
	}
	got := formatFiles(plan.Files)
	wantBytes, err := os.ReadFile(filepath.Join("testdata", "minimal-ipv4-dummy.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(wantBytes) {
		t.Fatalf("rendered files did not match golden\n--- got ---\n%s\n--- want ---\n%s", got, wantBytes)
	}
}

func TestRenderStartsWithdrawnByDefault(t *testing.T) {
	plan, err := RenderNativeEtcFiles(RenderRequest{
		NodeRole: "control-plane",
		Config:   minimalConfig(),
	})
	if err != nil {
		t.Fatalf("RenderNativeEtcFiles() error = %v", err)
	}
	bird := fileContent(t, plan.Files, BirdConfigPath)
	if !strings.Contains(bird, "protocol static katl_api {\n  disabled;\n") {
		t.Fatalf("bird.conf did not start withdrawn:\n%s", bird)
	}
	if !*plan.Config.Advertisement.StartWithdrawn || !*plan.Config.Advertisement.AdvertiseAfterHealthy || !*plan.Config.Advertisement.WithdrawOnFailure {
		t.Fatalf("advertisement defaults = %#v", plan.Config.Advertisement)
	}
}

func TestRenderIPv6LoopbackVIP(t *testing.T) {
	config := minimalConfig()
	config.Endpoint.VIP = "2001:db8:40::10/128"
	config.Endpoint.AddressFamily = "ipv6"
	config.VIPInterface = VIPInterface{Kind: "loopback", Name: "lo"}
	config.Routing.RouterID = "10.0.0.12"
	config.Routing.SourceAddress = "2001:db8:1::12"
	config.FabricPeers[0].Address = "2001:db8:1::1"
	config.FabricPeers[0].AllowedExportPrefixes = []string{"2001:db8:40::10/128"}
	plan, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "control-plane", Config: config})
	if err != nil {
		t.Fatalf("RenderNativeEtcFiles() error = %v", err)
	}
	assertNoFile(t, plan.Files, DummyNetDevPath)
	assertFile(t, plan.Files, NetworkPath, "Name=lo\n")
	assertFile(t, plan.Files, BirdConfigPath, "  ipv6 {\n")
}

func TestRenderAllowsMultipleControlPlanesToAdvertiseSameVIP(t *testing.T) {
	cp1 := minimalConfig()
	cp2 := minimalConfig()
	cp2.Routing.RouterID = "10.0.0.12"
	cp2.Routing.SourceAddress = "10.0.0.12"
	for _, config := range []Config{cp1, cp2} {
		plan, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "control-plane", Config: config})
		if err != nil {
			t.Fatalf("RenderNativeEtcFiles() error = %v", err)
		}
		if plan.Config.Endpoint.VIP != "10.40.0.10/32" || plan.Config.AdvertiseOn.Roles[0] != "control-plane" {
			t.Fatalf("plan = %#v", plan.Config)
		}
	}
}

func TestRenderValidatesSourceAddressAgainstInterfaceInventory(t *testing.T) {
	_, err := RenderNativeEtcFiles(RenderRequest{
		NodeRole: "control-plane",
		Config:   minimalConfig(),
		LocalInterfaceAddresses: map[string][]string{
			"enp1s0": []string{"10.0.0.12/24"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `sourceAddress "10.0.0.11" is not assigned to sourceInterface "enp1s0"`) {
		t.Fatalf("RenderNativeEtcFiles() error = %v, want source/interface mismatch", err)
	}
}

func TestDecodeRejectsArbitraryBIRDConfig(t *testing.T) {
	_, err := Decode(strings.NewReader(`apiVersion: apps.katl.dev/v1alpha1
kind: BGPAPIEndpoint
spec:
  endpoint:
    host: api.home.example
    vip: 10.40.0.10/32
  vipInterface:
    kind: dummy
    name: katl-api0
  routing:
    routerID: 10.0.0.11
    localASN: 64512
    birdConfig: "protocol static unsafe {}"
`))
	if err == nil || !strings.Contains(err.Error(), "field birdConfig not found") {
		t.Fatalf("Decode() error = %v, want unknown field", err)
	}
}

func TestNormalizeRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "non host prefix",
			mutate: func(config *Config) {
				config.Endpoint.VIP = "10.40.0.0/24"
				config.FabricPeers[0].AllowedExportPrefixes = []string{"10.40.0.0/24"}
			},
			want: "endpoint.vip must be a /32 or /128",
		},
		{
			name: "vip outside export prefixes",
			mutate: func(config *Config) {
				config.FabricPeers[0].AllowedExportPrefixes = []string{"10.40.0.11/32"}
			},
			want: "allowedExportPrefixes[0] must be endpoint.vip",
		},
		{
			name: "unsupported daemon",
			mutate: func(config *Config) {
				config.Routing.Daemon = "frr"
			},
			want: "routing.daemon must be bird",
		},
		{
			name: "unsupported protocol",
			mutate: func(config *Config) {
				config.Routing.ProtocolBoundary = "bgpToOspf"
			},
			want: "routing.protocolBoundary must be bgp",
		},
		{
			name: "unsafe interface path",
			mutate: func(config *Config) {
				config.VIPInterface.Name = "../api0"
			},
			want: "vipInterface.name",
		},
		{
			name: "cilium provenance",
			mutate: func(config *Config) {
				config.Endpoint.Provenance = "cilium"
			},
			want: "endpoint.provenance must be platform-host",
		},
		{
			name: "worker advertisement",
			mutate: func(config *Config) {
				config.AdvertiseOn.Roles = []string{"worker"}
			},
			want: "advertiseOn.roles[0] must be control-plane",
		},
		{
			name: "vip as source",
			mutate: func(config *Config) {
				config.Routing.SourceAddress = "10.40.0.10"
			},
			want: "routing.sourceAddress must not be endpoint.vip",
		},
		{
			name: "inline auth",
			mutate: func(config *Config) {
				config.FabricPeers[0].AuthRef = "hunter2"
			},
			want: "authRef must be secret/<name>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := minimalConfig()
			tt.mutate(&config)
			_, err := Normalize(config)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Normalize() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRenderRejectsWorkerNodeRole(t *testing.T) {
	_, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "worker", Config: minimalConfig()})
	if err == nil || !strings.Contains(err.Error(), "cannot advertise BGP API VIP") {
		t.Fatalf("RenderNativeEtcFiles() error = %v, want role rejection", err)
	}
}

func minimalConfig() Config {
	return Config{
		Endpoint: Endpoint{
			Host: "api.home.example",
			VIP:  "10.40.0.10/32",
		},
		VIPInterface: VIPInterface{
			Kind: "dummy",
			Name: "katl-api0",
			MTU:  1500,
		},
		Routing: Routing{
			RouterID:        "10.0.0.11",
			LocalASN:        64512,
			SourceAddress:   "10.0.0.11",
			SourceInterface: "enp1s0",
			ExportPolicy: ExportPolicy{
				Communities:     []string{"64512:100"},
				LocalPreference: 100,
				MED:             0,
			},
		},
		FabricPeers: []Peer{{
			Name:                  "router-a",
			Address:               "10.0.0.1",
			ASN:                   64500,
			AllowedExportPrefixes: []string{"10.40.0.10/32"},
		}},
	}
}

func assertFile(t *testing.T, files []confext.NativeEtcFile, path, contains string) {
	t.Helper()
	content := fileContent(t, files, path)
	if !strings.Contains(content, contains) {
		t.Fatalf("%s missing %q:\n%s", path, contains, content)
	}
}

func assertNoFile(t *testing.T, files []confext.NativeEtcFile, path string) {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			t.Fatalf("unexpected file %s", path)
		}
	}
}

func fileContent(t *testing.T, files []confext.NativeEtcFile, path string) string {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return file.Content
		}
	}
	t.Fatalf("missing file %s in %#v", path, files)
	return ""
}

func formatFiles(files []confext.NativeEtcFile) string {
	var b bytes.Buffer
	for _, file := range files {
		b.WriteString("--- " + file.Path + "\n")
		b.WriteString(file.Content)
		if !strings.HasSuffix(file.Content, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
