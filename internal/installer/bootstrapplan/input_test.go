package bootstrapplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
)

const (
	sharedControlPlaneInput = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"

	registeredControlPlaneInput = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.0.0.21
nodeRegistration:
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.21
    - name: node-labels
      value: example.test/tier=control
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`
)

// A bootstrap request pins the digest of the input its node held when it was
// accepted. Whatever form the source generation stored that input in, the
// selection reproduces that digest.
func TestSelectInputReproducesAcceptedDigest(t *testing.T) {
	registered, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
		RenderPath: kubeadmconfig.ConfigPath("control-plane"), Content: []byte(registeredControlPlaneInput),
	}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := registered.InputDigest()
	if err != nil {
		t.Fatal(err)
	}

	for name, stored := range map[string]string{
		"shared":     sharedControlPlaneInput,
		"registered": registeredControlPlaneInput,
		"stale":      strings.ReplaceAll(registeredControlPlaneInput, "10.0.0.21", "10.0.0.5"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			node := manifest.Manifest{
				APIVersion: manifest.APIVersion, Kind: manifest.Kind,
				Node: manifest.NodeConfig{
					Identity:   manifest.NodeIdentity{Hostname: "cp-1", SSH: manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"}}},
					SystemRole: "control-plane",
					Kubernetes: manifest.KubernetesConfig{
						Address: "10.0.0.21",
						Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"},
					},
					Bootstrap: &manifest.BootstrapIntent{Labels: map[string]string{"example.test/tier": "control"}},
				},
				Install: manifest.InstallConfig{WipeTarget: true, TargetDisk: manifest.DiskSelector{ByID: "disk/by-id/test"}},
				KatlosImage: manifest.KatlosImage{
					LocalRef: "images/katlos.raw", SHA256: strings.Repeat("a", 64), SizeBytes: 1024,
					Version: "0.1.0", Architecture: "x86_64", Role: "install",
				},
			}
			if err := configapply.WriteGenerationManifest(root, "0", node); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "var/lib/katl/generations/0/kubeadm-input/control-plane/config.yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(stored), 0o644); err != nil {
				t.Fatal(err)
			}

			selection, err := SelectInput(root, "0", installer.ClusterIntent{
				GenerationID: "0", SystemRole: "control-plane",
				Inventory: installer.ClusterIntentInventory{NodeName: "cp-1"},
			}, operation.BootstrapRequest{
				InventoryNodeName: "cp-1", SystemRole: "control-plane", BootstrapProfileRef: "control-plane",
				KubeadmInputDigest: accepted,
			})
			if err != nil {
				t.Fatalf("SelectInput() error = %v", err)
			}
			if selection.Input.Digest != accepted || selection.Input.Path != "/etc/katl/kubeadm/control-plane/config.yaml" {
				t.Fatalf("input = %+v, want digest %s", selection.Input, accepted)
			}
			if selection.Configuration.Kubeadm.Name() != "control-plane" {
				t.Fatalf("configuration input = %q", selection.Configuration.Kubeadm.Name())
			}
		})
	}
}
