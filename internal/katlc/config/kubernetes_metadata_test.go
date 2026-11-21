package config

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestValidateNodeConfigurationChangeAcceptsRenderedKubernetesMetadata(t *testing.T) {
	data, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{Hostname: "cp-1", SSH: manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"}}},
			Bootstrap: &manifest.BootstrapIntent{
				Labels: map[string]string{"example.com/pool": "a"},
				Taints: []manifest.NodeTaint{{Key: "dedicated", Effect: "NoSchedule"}},
			},
		}},
		SourceID:       "lab",
		DesiredVersion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "kubernetesMetadata:") {
		t.Fatalf("rendered change omitted Kubernetes metadata:\n%s", data)
	}

	result := ValidateNodeConfigurationChange(string(data), Options{})
	if !result.Accepted() {
		t.Fatalf("diagnostics = %#v, want accepted", result.Strings())
	}
}
