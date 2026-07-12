package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestRenderNodeConfigurationChange(t *testing.T) {
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{
			Node: manifest.NodeConfig{
				Identity: manifest.NodeIdentity{
					Hostname: "cp-1",
					SSH: manifest.SSHIdentity{AuthorizedKeys: []string{
						"ssh-ed25519 AAAA katl@example",
					}},
				},
				SystemRole: "control-plane",
				Networkd: manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
					Name:    "10-lan.network",
					Content: "[Network]\nDHCP=yes\n",
				}}},
				Kubernetes: manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"}},
			},
		},
		SourceID:       "lab",
		DesiredVersion: "2",
		ApplyMode:      "auto",
	})
	if err != nil {
		t.Fatalf("RenderNodeConfigurationChange() error = %v", err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v\n%s", err, data)
	}
	overlay := request.NodeOverrides["cp-1"]
	if request.SourceID != "lab" || request.DesiredVersion != "2" || request.ApplyMode != "auto" {
		t.Fatalf("rendered metadata = source %q version %q mode %q", request.SourceID, request.DesiredVersion, request.ApplyMode)
	}
	if overlay.Identity == nil || overlay.Identity.Hostname != "cp-1" || len(overlay.Identity.AuthorizedKeys) != 1 {
		t.Fatalf("rendered identity = %#v", overlay.Identity)
	}
	if overlay.Networkd == nil || len(overlay.Networkd.Files) != 1 || overlay.Networkd.Files[0].Name != "10-lan.network" {
		t.Fatalf("rendered networkd = %#v", overlay.Networkd)
	}
	if overlay.SystemRole != "" || overlay.Kubernetes != nil || strings.Contains(string(data), "systemRole:") || strings.Contains(string(data), "kubernetes:") || strings.Contains(string(data), "install:") {
		t.Fatalf("rendered change contains lifecycle or install fields:\n%s", data)
	}
}

func TestRenderNodeConfigurationChangePreservesEmptyAuthorizedKeys(t *testing.T) {
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "worker-1",
		Manifest: manifest.Manifest{Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{Hostname: "worker-1"},
		}},
		SourceID:       "lab",
		DesiredVersion: "3",
	})
	if err != nil {
		t.Fatalf("RenderNodeConfigurationChange() error = %v", err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v\n%s", err, data)
	}
	keys := request.NodeOverrides["worker-1"].Identity.AuthorizedKeys
	if keys == nil || len(keys) != 0 {
		t.Fatalf("authorized keys = %#v, want explicit empty list", keys)
	}
}

func TestRenderedNodeConfigurationDoesNotPlanUnchangedDomains(t *testing.T) {
	desired := manifest.Manifest{Node: manifest.NodeConfig{
		Identity: manifest.NodeIdentity{
			Hostname: "worker-1",
			SSH:      manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAA katl@example"}},
		},
		Networkd: manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{Name: "10-lan.network", Content: "[Network]\nDHCP=yes\n"}}},
	}}
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName:       "worker-1",
		Manifest:       desired,
		SourceID:       "lab",
		DesiredVersion: "4",
	})
	if err != nil {
		t.Fatalf("RenderNodeConfigurationChange() error = %v", err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{
		NodeName:        "worker-1",
		CurrentManifest: desired,
	})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v", err)
	}
	if _, _, _, err := mergeRuntimeConfig(request); err == nil || !strings.Contains(err.Error(), "desired state already matches") {
		t.Fatalf("mergeRuntimeConfig() error = %v, want unchanged desired state", err)
	}
}
