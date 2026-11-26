package kubeadmconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestBootstrapCertSANs(t *testing.T) {
	for _, tt := range []struct {
		name, endpoint, vip string
		want                []string
		wantError           bool
	}{
		{name: "host and loopback", endpoint: "api.katl.test:6443", want: []string{"api.katl.test", "127.0.0.1"}},
		{name: "with VIP", endpoint: "api.katl.test:6443", vip: "192.0.2.10", want: []string{"api.katl.test", "127.0.0.1", "192.0.2.10"}},
		{name: "VIP is the host", endpoint: "192.0.2.10:6443", vip: "192.0.2.10", want: []string{"192.0.2.10", "127.0.0.1"}},
		{name: "no endpoint"},
		{name: "not host and port", endpoint: "api.katl.test", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got, err := bootstrapEndpointAndSANs(tt.endpoint, tt.vip)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, want error %t", err, tt.wantError)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SANs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderInitConfigAddsEndpointAndSANs(t *testing.T) {
	base := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  certSANs: [user-api.example]\n")
	const want = `apiServer:
  certSANs:
    - user-api.example
    - api.katl.test
    - 127.0.0.1
    - 192.0.2.10
apiVersion: kubeadm.k8s.io/v1beta4
controlPlaneEndpoint: api.katl.test:6443
kind: ClusterConfiguration
`
	got, err := RenderInitConfig(base, "api.katl.test:6443", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderInitConfigRejectsConflictingEndpointAndDeduplicatesSAN(t *testing.T) {
	base := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\ncontrolPlaneEndpoint: old-api.katl.test:6443\napiServer:\n  certSANs:\n    - api.katl.test\n")
	if _, err := RenderInitConfig(base, "api.katl.test:6443", ""); err == nil || !strings.Contains(err.Error(), "conflicts with cluster endpoint") {
		t.Fatalf("RenderInitConfig() error = %v, want endpoint conflict", err)
	}

	same := strings.ReplaceAll(string(base), "old-api.katl.test:6443", "api.katl.test:6443")
	got, err := RenderInitConfig([]byte(same), "api.katl.test:6443", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "- api.katl.test\n") != 1 {
		t.Fatalf("rendered config duplicated endpoint SAN:\n%s", got)
	}
}

func TestRenderInitConfigWithoutEndpointKeepsBase(t *testing.T) {
	base := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")
	got, err := RenderInitConfig(base, "", "192.0.2.10")
	if err != nil || string(got) != string(base) {
		t.Fatalf("RenderInitConfig() = %q, %v", got, err)
	}
}

func TestRenderInitConfigAcceptsEquivalentIPv6Endpoint(t *testing.T) {
	base := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\ncontrolPlaneEndpoint: '[2001:0db8::1]:6443'\n")

	got, err := RenderInitConfig(base, "[2001:db8::1]:6443", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "controlPlaneEndpoint: '[2001:db8::1]:6443'") || !strings.Contains(string(got), "- 2001:db8::1\n") {
		t.Fatalf("rendered config lacks canonical endpoint and SAN:\n%s", got)
	}
}

func TestBootstrapInitConfig(t *testing.T) {
	config := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: [addon/kube-proxy]\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"
	input, err := ForNode(testPlan(t, config), testNode("", nil))
	if err != nil {
		t.Fatal(err)
	}
	file, err := input.BootstrapInitConfig("api.katl.test:6443", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if file.Path != BootstrapInitConfigPath || file.Mode != 0o600 {
		t.Fatalf("file = %s mode %#o", file.Path, file.Mode)
	}
	for _, want := range []string{
		"controlPlaneEndpoint: api.katl.test:6443",
		"- api.katl.test",
		"- 127.0.0.1",
		"- 192.0.2.10",
		"- addon/kube-proxy",
		"- wait-control-plane",
	} {
		if !strings.Contains(file.Content, want) {
			t.Errorf("init config lacks %q:\n%s", want, file.Content)
		}
	}
	// The input itself is untouched: it still has no endpoint.
	if strings.Contains(string(input.Plan().Config.Content), "controlPlaneEndpoint") {
		t.Fatalf("input gained the endpoint:\n%s", input.Plan().Config.Content)
	}
}

func TestBootstrapInitConfigSynthesizesInitConfiguration(t *testing.T) {
	input, err := ForNode(testPlan(t, "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"), testNode("", nil))
	if err != nil {
		t.Fatal(err)
	}
	file, err := input.BootstrapInitConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file.Content, "kind: InitConfiguration") || !strings.Contains(file.Content, "- wait-control-plane") {
		t.Fatalf("init config:\n%s", file.Content)
	}
}

func TestBootstrapInitConfigRejectsMalformedSkipPhases(t *testing.T) {
	input, err := ForNode(testPlan(t, "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: 3\n"), testNode("", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.BootstrapInitConfig("", ""); err == nil || !strings.Contains(err.Error(), "skipPhases must be a list of strings") {
		t.Fatalf("BootstrapInitConfig() error = %v", err)
	}
}
