package config

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestExternalWorkerChange(t *testing.T) {
	data, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{NodeName: "worker-1", SourceID: "external-lab", DesiredVersion: "2", Manifest: manifest.Manifest{Node: manifest.NodeConfig{Identity: manifest.NodeIdentity{Hostname: "worker-1", SSH: manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"}}}, SystemRole: "worker", Kubernetes: manifest.KubernetesConfig{External: true}, Bootstrap: &manifest.BootstrapIntent{ControlPlaneEndpoint: "api.external.test:6443"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result := ValidateNodeConfigurationChange(string(data), Options{})
	if !result.Accepted() {
		t.Fatalf("external worker host configuration rejected: %v", result.Strings())
	}
	request, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(string(data)), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	overlay := request.NodeOverrides["worker-1"]
	if overlay.ExternalEndpoint == nil || *overlay.ExternalEndpoint != "api.external.test:6443" || !overlay.Kubernetes.External {
		t.Fatal("external ownership was lost during node configuration decoding")
	}
}
