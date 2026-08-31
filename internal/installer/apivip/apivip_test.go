package apivip

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
)

func TestFromControlPlaneEndpointRendersVIPOwnership(t *testing.T) {
	endpoint, err := controlplaneendpoint.Normalize(controlplaneendpoint.Config{
		Host:          "api.home.example",
		Advertisement: &controlplaneendpoint.Advertisement{VIP: "10.40.0.10"},
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
	assertFile(t, plan.Files, ConfigPath, "kind: APIEndpointVIP\n")
	assertFile(t, plan.Files, OwnershipEnabledPath, "enabled\n")
	assertFile(t, plan.Files, NetworkPath, "RequiredForOnline=no\n")
	assertFile(t, plan.Files, AppDropInPath, "/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser --config "+ConfigPath)
	assertFileAbsent(t, plan.Files, NetworkPath, "Address=")
	if plan.Config.Health.Host != "127.0.0.1" || plan.Config.Health.Path != "/readyz" {
		t.Fatalf("health = %#v", plan.Config.Health)
	}
}

func TestDecodeRejectsRemovedBGPConfig(t *testing.T) {
	_, err := Decode(strings.NewReader(`apiVersion: apps.katl.dev/v1alpha1
kind: APIEndpointVIP
spec:
  endpoint:
    host: api.home.example
    vip: 10.40.0.10/32
  bgp:
    localASN: 64512
`))
	if err == nil || !strings.Contains(err.Error(), "field bgp not found") {
		t.Fatalf("Decode() error = %v", err)
	}
}

func TestRenderRejectsWorkerNodeRole(t *testing.T) {
	_, err := RenderNativeEtcFiles(RenderRequest{NodeRole: "worker", Config: minimalConfig()})
	if err == nil || !strings.Contains(err.Error(), "control-plane is required") {
		t.Fatalf("RenderNativeEtcFiles() error = %v", err)
	}
}

func TestNormalizeRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "non-host prefix", mutate: func(c *Config) { c.Endpoint.VIP = "10.40.0.0/24" }, want: "must be a /32 or /128"},
		{name: "unsafe interface", mutate: func(c *Config) { c.VIPInterface.Name = "../api" }, want: "safe Linux interface"},
		{name: "worker", mutate: func(c *Config) { c.ActivateOn.Roles = []string{"worker"} }, want: "must be control-plane"},
		{name: "remote health", mutate: func(c *Config) { c.Health.Host = "10.40.0.10" }, want: "local kube-apiserver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := minimalConfig()
			test.mutate(&config)
			_, err := Normalize(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Normalize() error = %v, want %q", err, test.want)
			}
		})
	}
}

func minimalConfig() Config {
	return Config{
		Endpoint:     Endpoint{Host: "api.home.example", VIP: "10.40.0.10/32"},
		VIPInterface: VIPInterface{Kind: "dummy", Name: "katl-api0", MTU: 1500},
	}
}

func assertFile(t *testing.T, files []confext.NativeEtcFile, path, contains string) {
	t.Helper()
	content := fileContent(t, files, path)
	if !strings.Contains(content, contains) {
		t.Fatalf("%s missing %q:\n%s", path, contains, content)
	}
}

func assertFileAbsent(t *testing.T, files []confext.NativeEtcFile, path, contains string) {
	t.Helper()
	content := fileContent(t, files, path)
	if strings.Contains(content, contains) {
		t.Fatalf("%s unexpectedly contains %q:\n%s", path, contains, content)
	}
}

func fileContent(t *testing.T, files []confext.NativeEtcFile, path string) string {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return file.Content
		}
	}
	t.Fatalf("missing file %s", path)
	return ""
}
