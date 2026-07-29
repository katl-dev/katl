package configapply

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestRenderNodeConfigurationChange(t *testing.T) {
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{
			Install: manifest.InstallConfig{Volumes: []manifest.Volume{{
				Name: "local-hostpath", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{}}, Filesystem: "xfs",
			}}},
			Node: manifest.NodeConfig{
				Identity: manifest.NodeIdentity{
					Hostname: "cp-1",
					SSH: manifest.SSHIdentity{AuthorizedKeys: []string{
						"ssh-ed25519 AAAA katl@example",
					}},
				},
				SystemRole: "control-plane",
				Kernel: manifest.KernelConfig{
					CommandLine: []string{"intel_iommu=on", "iommu=pt"},
				},
				HostConfiguration:    testHostConfiguration("lan", "/etc/systemd/network/10-lan.network", "[Network]\nDHCP=yes\n"),
				Kubernetes:           manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"}},
				ControlPlaneEndpoint: managedEndpoint("192.0.2.1"),
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
	if overlay.HostConfiguration == nil || len(overlay.HostConfiguration.Sets["lan"].Files) != 1 {
		t.Fatalf("rendered host configuration = %#v", overlay.HostConfiguration)
	}
	if overlay.SystemRole != "control-plane" || overlay.Kubernetes == nil || overlay.Kubernetes.Kubeadm.ConfigRef != "control-plane" {
		t.Fatalf("rendered operation fields = role %q kubernetes %#v", overlay.SystemRole, overlay.Kubernetes)
	}
	if overlay.Kernel == nil || !slices.Equal(overlay.Kernel.CommandLine, []string{"intel_iommu=on", "iommu=pt"}) {
		t.Fatalf("rendered kernel config = %#v", overlay.Kernel)
	}
	if !overlay.ControlPlaneEndpointSet || overlay.ControlPlaneEndpoint == nil || overlay.ControlPlaneEndpoint.Advertisement == nil {
		t.Fatalf("rendered control-plane endpoint = %#v, set=%t", overlay.ControlPlaneEndpoint, overlay.ControlPlaneEndpointSet)
	}
	if overlay.Volumes == nil || len(*overlay.Volumes) != 1 || (*overlay.Volumes)[0].Name != "local-hostpath" {
		t.Fatalf("rendered volumes = %#v", overlay.Volumes)
	}
	if strings.Contains(string(data), "install:") {
		t.Fatalf("rendered change contains install fields:\n%s", data)
	}
}

func TestRenderNodeConfigurationChangeCarriesExternalEndpointOwnership(t *testing.T) {
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{Node: manifest.NodeConfig{
			Identity:             manifest.NodeIdentity{Hostname: "cp-1"},
			ControlPlaneEndpoint: nil,
		}},
		SourceID:       "lab",
		DesiredVersion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v\n%s", err, data)
	}
	overlay := request.NodeOverrides["cp-1"]
	if !overlay.ControlPlaneEndpointSet || overlay.ControlPlaneEndpoint != nil {
		t.Fatalf("external endpoint overlay = %#v, set=%t", overlay.ControlPlaneEndpoint, overlay.ControlPlaneEndpointSet)
	}
	if !strings.Contains(string(data), "controlPlaneEndpoint:\n                managed: false") {
		t.Fatalf("rendered change omitted external endpoint ownership:\n%s", data)
	}
}

func managedEndpoint(peer string) *controlplaneendpoint.Config {
	return &controlplaneendpoint.Config{
		Host: "192.0.2.10",
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: "192.0.2.10",
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: peer, ASN: 64500}},
			},
		},
	}
}

func TestRenderNodeConfigurationChangeCarriesSelectedKubeadmInput(t *testing.T) {
	plan, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
			Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"),
		},
		{
			RenderPath: "/etc/katl/kubeadm/control-plane/patches/kube-apiserver0+merge.yaml",
			Content:    []byte("metadata:\n  labels:\n    katl.dev/profile: homelab\n"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "cp-1"},
			SystemRole: "control-plane",
			Kubernetes: manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"}},
		}},
		KubeadmConfigs: map[string]kubeadmconfig.Plan{"control-plane": plan},
		SourceID:       "lab",
		DesiredVersion: "2",
	})
	if err != nil {
		t.Fatalf("RenderNodeConfigurationChange() error = %v", err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v\n%s", err, data)
	}
	resolved := request.KubeadmConfigs["control-plane"]
	if !strings.Contains(string(resolved.Config.Content), "kubernetesVersion: v1.36.1") || len(resolved.Patches) != 1 {
		t.Fatalf("resolved kubeadm input = %#v", resolved)
	}
	if !request.NodeOverrides["cp-1"].KubeadmChanged {
		t.Fatal("rendered kubeadm input was not planned as an operation-only change")
	}
}

func TestRenderNodeConfigurationChangeCanSelectOnlyKubeadmInput(t *testing.T) {
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName: "cp-1",
		Manifest: manifest.Manifest{Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "cp-1"},
			Kubernetes: manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"}},
		}},
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": {Name: "control-plane", Config: kubeadmconfig.File{Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")}},
		},
		SourceID: "lab", DesiredVersion: "2", ApplyMode: generation.ApplyModeAuto, KubeadmOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, absent := range []string{"identity:", "networkd:", "controlPlaneEndpoint:"} {
		if strings.Contains(text, absent) {
			t.Fatalf("kubeadm-only change contains %q:\n%s", absent, text)
		}
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(text), TrustedBundleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	overlay := request.NodeOverrides["cp-1"]
	if overlay.Kubernetes == nil || overlay.Kubernetes.Kubeadm.ConfigRef != "control-plane" || len(request.KubeadmConfigs) != 1 {
		t.Fatalf("decoded request = %#v", request)
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
		HostConfiguration: testHostConfiguration("lan", "/etc/systemd/network/10-lan.network", "[Network]\nDHCP=yes\n"),
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
	if _, _, _, err := mergeRuntimeConfig(request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("mergeRuntimeConfig() error = %v, want unchanged desired state", err)
	}
}

func TestRenderedNodeConfigurationClearsDesiredDomains(t *testing.T) {
	current := manifest.Manifest{Node: manifest.NodeConfig{
		Identity: manifest.NodeIdentity{
			Hostname: "worker-1",
			SSH:      manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAA katl@example"}},
		},
		Kernel:            manifest.KernelConfig{CommandLine: []string{"intel_iommu=on"}},
		HostConfiguration: testHostConfiguration("lan", "/etc/systemd/network/10-lan.network", "[Network]\nDHCP=yes\n"),
		SystemExtensions:  []manifest.SystemExtension{{Name: "tools"}},
	}, Install: manifest.InstallConfig{Volumes: []manifest.Volume{{
		Name:       "data",
		Selector:   manifest.VolumeSelector{Partition: &manifest.PartitionSelector{}},
		Filesystem: "xfs",
	}}}}
	desired := current
	desired.Node.Kernel = manifest.KernelConfig{}
	desired.Node.HostConfiguration = manifest.HostConfiguration{}
	desired.Node.SystemExtensions = []manifest.SystemExtension{}
	desired.Install.Volumes = []manifest.Volume{}

	data, err := RenderNodeConfigurationChange(RenderNodeRequest{
		NodeName:       "worker-1",
		Manifest:       desired,
		SourceID:       "lab",
		DesiredVersion: "5",
	})
	if err != nil {
		t.Fatalf("RenderNodeConfigurationChange() error = %v", err)
	}
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{
		NodeName:        "worker-1",
		CurrentManifest: current,
	})
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v", err)
	}
	merged, changes, _, err := mergeRuntimeConfig(request)
	if err != nil {
		t.Fatalf("mergeRuntimeConfig() error = %v", err)
	}
	if len(merged.Node.Kernel.CommandLine) != 0 ||
		!merged.Node.HostConfiguration.IsZero() ||
		len(merged.Node.SystemExtensions) != 0 ||
		len(merged.Install.Volumes) != 0 {
		t.Fatalf("cleared manifest = %#v", merged)
	}
	domains := map[string]bool{}
	for _, change := range changes {
		domains[change.Domain] = true
	}
	for _, domain := range []string{DomainKernelCommandLine, DomainHostConfiguration, DomainSystemExtensions, DomainVolumes} {
		if !domains[domain] {
			t.Fatalf("changed domains = %#v, missing %s", domains, domain)
		}
	}
}
