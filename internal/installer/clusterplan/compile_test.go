package clusterplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/artifact"
	"github.com/zariel/katl/internal/installer/bgpapivip"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/platformendpoint"
	"github.com/zariel/katl/internal/installer/sysextcatalog"
)

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func TestCompileClusterPlan(t *testing.T) {
	plan, err := Compile(CompileRequest{
		Config:         validConfig(),
		KubeadmConfigs: validKubeadmConfigs("v1.36.1"),
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if got := plan.BootstrapInventory.Nodes[0].Name; got != "cp-1" {
		t.Fatalf("first inventory node = %q", got)
	}
	cp := plan.Nodes[0]
	if cp.Name != "cp-1" || cp.SystemRole != inventory.RoleControlPlane || cp.BootstrapAddress != "10.0.0.11" {
		t.Fatalf("control-plane material = %#v", cp)
	}
	if cp.InstallManifest.Node.Identity.Hostname != "cp-1-host" {
		t.Fatalf("hostname = %q", cp.InstallManifest.Node.Identity.Hostname)
	}
	if cp.InstallManifest.Node.Kubernetes.Kubeadm.ConfigRef != "control-plane" || cp.KubeadmConfig.Path != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("kubeadm config = manifest %#v material %#v", cp.InstallManifest.Node.Kubernetes.Kubeadm, cp.KubeadmConfig)
	}
	if cp.InstallManifest.Node.Bootstrap == nil || cp.InstallManifest.Node.Bootstrap.InventoryNodeName != "cp-1" || cp.InstallManifest.Node.Bootstrap.ControlPlaneEndpoint != "api.katl.test:6443" {
		t.Fatalf("bootstrap intent = %#v", cp.InstallManifest.Node.Bootstrap)
	}
	if cp.NodeLabels["katl.dev/zone"] != "rack-a" || cp.InstallManifest.Node.Bootstrap.Labels["katl.dev/zone"] != "rack-a" {
		t.Fatalf("node labels = material %#v manifest %#v", cp.NodeLabels, cp.InstallManifest.Node.Bootstrap.Labels)
	}
	if len(cp.NodeTaints) != 1 || cp.NodeTaints[0].Effect != "NoSchedule" {
		t.Fatalf("node taints = %#v", cp.NodeTaints)
	}
	if len(cp.InstallManifest.Node.Networkd.Files) != 2 {
		t.Fatalf("networkd files = %#v", cp.InstallManifest.Node.Networkd.Files)
	}
	if len(cp.NativeEtcFiles) != 3 {
		t.Fatalf("native /etc files = %#v", cp.NativeEtcFiles)
	}
	if len(cp.InstallManifest.Node.Identity.SSH.AuthorizedKeys) != 1 {
		t.Fatalf("ssh keys were not de-duplicated: %#v", cp.InstallManifest.Node.Identity.SSH.AuthorizedKeys)
	}
	worker := plan.Nodes[1]
	if worker.Name != "worker-1" || worker.KubeadmConfig.Intent != inventory.IntentWorker {
		t.Fatalf("worker material = %#v", worker)
	}

	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	golden := filepath.Join("testdata", "basic.golden.json")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(data) != string(want) {
		t.Fatalf("compiled plan mismatch\nwant:\n%s\ngot:\n%s", want, data)
	}
}

func TestCompileAllowsMissingAddressAndAppliesOverride(t *testing.T) {
	config := validConfig()
	config.Spec.Nodes[1].Overrides.Bootstrap.Address = ""
	plan, err := Compile(CompileRequest{
		Config:         config,
		KubeadmConfigs: validKubeadmConfigs("v1.36.1"),
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if got := plan.Nodes[1].BootstrapAddress; got != "" {
		t.Fatalf("worker bootstrap address = %q, want empty", got)
	}
	if got := plan.BootstrapInventory.Nodes[1].Address; got != "" {
		t.Fatalf("worker inventory address = %q, want empty", got)
	}

	plan, err = Compile(CompileRequest{
		Config:           config,
		KubeadmConfigs:   validKubeadmConfigs("v1.36.1"),
		AddressOverrides: map[string]string{"worker-1": "10.0.0.99"},
	})
	if err != nil {
		t.Fatalf("Compile() with address override error = %v", err)
	}
	if got := plan.Nodes[1].BootstrapAddress; got != "10.0.0.99" {
		t.Fatalf("worker bootstrap address = %q", got)
	}
	if got := plan.Nodes[1].InstallManifest.Node.Bootstrap.NodeAddress; got != "10.0.0.99" {
		t.Fatalf("worker install manifest bootstrap address = %q", got)
	}
	if len(plan.AddressOverrides) != 1 || plan.AddressOverrides[0].Node != "worker-1" || plan.AddressOverrides[0].Address != "10.0.0.99" {
		t.Fatalf("address overrides = %#v", plan.AddressOverrides)
	}
}

func TestCompileSelectsCatalogRef(t *testing.T) {
	config := validConfig()
	config.Spec.Kubernetes = KubernetesSelection{CatalogRef: "default"}
	plan, err := Compile(CompileRequest{
		Config:                     config,
		KubeadmConfigs:             validKubeadmConfigs("v1.36.1"),
		KubernetesCatalog:          validSysextCatalog(),
		KubernetesArtifactBasePath: "payload/sysext",
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.KubernetesVersion != "v1.36.1" || plan.KubernetesCatalogRef != "default" {
		t.Fatalf("selection = version %q catalogRef %q", plan.KubernetesVersion, plan.KubernetesCatalogRef)
	}
	if plan.KubernetesSysext == nil {
		t.Fatalf("KubernetesSysext is nil")
	}
	if plan.KubernetesSysext.Path != "payload/sysext/katl-kubernetes.raw" {
		t.Fatalf("sysext path = %q", plan.KubernetesSysext.Path)
	}
	if got := plan.Nodes[0].KubernetesSysext; got == nil || got.PayloadVersion != "v1.36.1" {
		t.Fatalf("node sysext = %#v", got)
	}
}

func TestCompileSelectsKubernetesBundleRef(t *testing.T) {
	config := validConfig()
	bundleRef := "v1.36.1@sha256:" + strings.Repeat("a", 64)
	config.Spec.Kubernetes = KubernetesSelection{
		BundleSource: "https://artifacts.example.test/kubernetes",
		BundleRef:    bundleRef,
	}
	plan, err := Compile(CompileRequest{
		Config:         config,
		KubeadmConfigs: validKubeadmConfigs("v1.36.1"),
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.KubernetesVersion != "v1.36.1" || plan.KubernetesBundleSource != "https://artifacts.example.test/kubernetes" || plan.KubernetesBundleRef != bundleRef {
		t.Fatalf("selection = %#v", plan)
	}
	if plan.BootstrapInventory.KubernetesBundleSource != plan.KubernetesBundleSource || plan.BootstrapInventory.KubernetesBundleRef != bundleRef {
		t.Fatalf("bootstrap inventory = %#v", plan.BootstrapInventory)
	}
	bootstrap := plan.Nodes[0].InstallManifest.Node.Bootstrap
	if bootstrap == nil || bootstrap.KubernetesBundleSource != plan.KubernetesBundleSource || bootstrap.KubernetesBundleRef != bundleRef {
		t.Fatalf("install manifest bootstrap = %#v", bootstrap)
	}
	if plan.Nodes[0].KubernetesBundleRef != bundleRef {
		t.Fatalf("node material = %#v", plan.Nodes[0])
	}
}

func TestCompileComposesHostAdvertisedBGPAPIEndpoint(t *testing.T) {
	config := validConfig()
	config.Spec.ControlPlaneEndpoint = ""
	config.Spec.PlatformAPIEndpoint = &platformendpoint.Config{
		Mode:           platformendpoint.ModeHostAdvertisedBGP,
		BGPAPIEndpoint: ptrBGPConfig(clusterBGPConfig()),
	}
	plan, err := Compile(CompileRequest{
		Config:         config,
		KubeadmConfigs: validKubeadmConfigs("v1.36.1"),
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.ControlPlaneEndpoint != "api.home.example:6443" {
		t.Fatalf("control plane endpoint = %q", plan.ControlPlaneEndpoint)
	}
	if plan.PlatformAPIEndpoint == nil || plan.PlatformAPIEndpoint.HelperStatus == nil || plan.PlatformAPIEndpoint.HelperStatus.AppID != bgpapivip.AppID {
		t.Fatalf("platform endpoint plan = %#v", plan.PlatformAPIEndpoint)
	}
	if plan.BootstrapInventory.ControlPlaneEndpoint != "api.home.example:6443" {
		t.Fatalf("bootstrap inventory endpoint = %q", plan.BootstrapInventory.ControlPlaneEndpoint)
	}
	if plan.BootstrapInventory.Bootstrap == nil || plan.BootstrapInventory.Bootstrap.StableEndpoint != "api.home.example:6443" || !plan.BootstrapInventory.Bootstrap.StableEndpointBeforeManifests {
		t.Fatalf("bootstrap stable endpoint = %#v", plan.BootstrapInventory.Bootstrap)
	}
	if got := plan.Nodes[0].InstallManifest.Node.Bootstrap.ControlPlaneEndpoint; got != "api.home.example:6443" {
		t.Fatalf("control-plane install manifest endpoint = %q", got)
	}
	if got := plan.Nodes[1].InstallManifest.Node.Bootstrap.ControlPlaneEndpoint; got != "api.home.example:6443" {
		t.Fatalf("worker install manifest endpoint = %q", got)
	}
	assertNodeNativeFile(t, plan.Nodes[0], bgpapivip.ConfigPath, "kind: BGPAPIEndpoint\n")
	assertNodeNativeFile(t, plan.Nodes[0], bgpapivip.BirdConfigPath, "neighbor 10.0.0.1 as 64500;\n")
	assertNodeNoNativeFile(t, plan.Nodes[1], bgpapivip.ConfigPath)
}

func TestCompileComposesExternalPlatformAPIEndpoint(t *testing.T) {
	config := validConfig()
	config.Spec.ControlPlaneEndpoint = ""
	config.Spec.PlatformAPIEndpoint = &platformendpoint.Config{
		Mode:     platformendpoint.ModeExternal,
		Endpoint: platformendpoint.Endpoint{Host: "api.external.test", Port: 7443},
	}
	plan, err := Compile(CompileRequest{
		Config:         config,
		KubeadmConfigs: validKubeadmConfigs("v1.36.1"),
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.ControlPlaneEndpoint != "api.external.test:7443" {
		t.Fatalf("control plane endpoint = %q", plan.ControlPlaneEndpoint)
	}
	if plan.PlatformAPIEndpoint == nil || plan.PlatformAPIEndpoint.HelperStatus != nil || len(plan.PlatformAPIEndpoint.NativeEtcFiles) != 0 {
		t.Fatalf("platform endpoint plan = %#v", plan.PlatformAPIEndpoint)
	}
	if plan.BootstrapInventory.Bootstrap == nil || plan.BootstrapInventory.Bootstrap.StableEndpoint != "api.external.test:7443" {
		t.Fatalf("bootstrap stable endpoint = %#v", plan.BootstrapInventory.Bootstrap)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode(strings.NewReader(`apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  unknown: true
`))
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("Decode() error = %v, want unknown field rejection", err)
	}
}

func TestDecodeAcceptsClusterPlanYAML(t *testing.T) {
	config, err := Decode(strings.NewReader(`apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  controlPlaneEndpoint: api.katl.test:6443
  kubernetes:
    payloadVersion: v1.36.1
  katlosImage:
    url: https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs
    sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    sizeBytes: 1073741824
    version: 2026.06.04
    architecture: x86_64
    runtimeInterface: katl-runtime-1
    role: install
  wipeTarget: true
  defaults:
    ssh:
      authorizedKeys:
        - ` + sshKey + `
    bootstrap:
      access:
        method: agent
        credentialRef: vsock:1234:10240
  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadmConfigRef: control-plane
    worker:
      kubernetes:
        kubeadmConfigRef: worker
  nodes:
    - name: cp-1
      systemRole: control-plane
      overrides:
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-cp-root
            minSizeMiB: 32768
        bootstrap:
          address: 10.0.0.11
    - name: worker-1
      systemRole: worker
      overrides:
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-worker-root
            minSizeMiB: 32768
        bootstrap:
          address: 10.0.0.21
`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got := config.Spec.Nodes[0].Overrides.Install.TargetDisk.ByID; got != "/dev/disk/by-id/ata-cp-root" {
		t.Fatalf("target disk byID = %q", got)
	}
	if got := config.Spec.Defaults.Bootstrap.Access.CredentialRef; got != "vsock:1234:10240" {
		t.Fatalf("credentialRef = %q", got)
	}
}

func TestCompileRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{
			name: "duplicate node name",
			mut: func(config *Config) {
				config.Spec.Nodes[1].Name = "cp-1"
			},
			want: "duplicate node name",
		},
		{
			name: "missing system role",
			mut: func(config *Config) {
				config.Spec.Nodes[0].SystemRole = ""
			},
			want: "systemRole",
		},
		{
			name: "missing image",
			mut: func(config *Config) {
				config.Spec.KatlosImage = manifest.KatlosImage{}
			},
			want: "katlosImage",
		},
		{
			name: "missing wipe target",
			mut: func(config *Config) {
				config.Spec.WipeTarget = false
			},
			want: "spec.wipeTarget",
		},
		{
			name: "missing endpoint",
			mut: func(config *Config) {
				config.Spec.ControlPlaneEndpoint = ""
			},
			want: "control-plane endpoint is required",
		},
		{
			name: "conflicting platform endpoint",
			mut: func(config *Config) {
				config.Spec.PlatformAPIEndpoint = &platformendpoint.Config{
					Mode:     platformendpoint.ModeExternal,
					Endpoint: platformendpoint.Endpoint{Host: "other.katl.test"},
				}
			},
			want: "does not match selected platformAPIEndpoint",
		},
		{
			name: "cilium platform endpoint",
			mut: func(config *Config) {
				config.Spec.ControlPlaneEndpoint = ""
				config.Spec.PlatformAPIEndpoint = &platformendpoint.Config{
					Mode:     platformendpoint.ModeCilium,
					Endpoint: platformendpoint.Endpoint{Host: "api.katl.test"},
				}
			},
			want: "post-Cilium",
		},
		{
			name: "platform endpoint generated file collision",
			mut: func(config *Config) {
				config.Spec.ControlPlaneEndpoint = ""
				config.Spec.PlatformAPIEndpoint = &platformendpoint.Config{
					Mode:           platformendpoint.ModeHostAdvertisedBGP,
					BGPAPIEndpoint: ptrBGPConfig(clusterBGPConfig()),
				}
				config.Spec.Nodes[0].Overrides.Networkd.Files = append(config.Spec.Nodes[0].Overrides.Networkd.Files, manifest.NetworkdFile{
					Name:    "20-katl-bgp-api-vip.network",
					Content: "[Match]\nName=enp2s0\n",
				})
			},
			want: "native /etc files",
		},
		{
			name: "unknown address override",
			mut: func(config *Config) {
				config.Spec.Nodes[1].Name = "renamed-worker"
			},
			want: "address override references unknown node",
		},
		{
			name: "conflicting networkd file",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Networkd.Files = append(config.Spec.Nodes[0].Overrides.Networkd.Files, manifest.NetworkdFile{
					Name:    "10-common.network",
					Content: "[Match]\nName=enp9s0\n",
				})
			},
			want: "networkd file",
		},
		{
			name: "conflicting extra disk",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Install.ExtraDisks = append(config.Spec.Nodes[0].Overrides.Install.ExtraDisks, manifest.ExtraDisk{
					Name:       "data",
					Selector:   manifest.DiskSelector{Serial: "different"},
					Filesystem: "xfs",
					Mount:      manifest.ExtraMount{Path: "/srv/data"},
				})
			},
			want: "extra disk",
		},
		{
			name: "conflicting node label",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Kubernetes.NodeLabels = map[string]string{"node-role.kubernetes.io/control-plane": "different"}
			},
			want: "node label",
		},
		{
			name: "invalid node label",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Kubernetes.NodeLabels = map[string]string{"bad key": "value"}
			},
			want: "node.bootstrap.labels key",
		},
		{
			name: "invalid node taint",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Kubernetes.NodeTaints = []manifest.NodeTaint{{Key: "katl.dev/bad", Effect: "Sometimes"}}
			},
			want: "node.bootstrap.taints",
		},
		{
			name: "host specific path",
			mut: func(config *Config) {
				config.Spec.Nodes[0].Overrides.Networkd.Files = append(config.Spec.Nodes[0].Overrides.Networkd.Files, manifest.NetworkdFile{
					Name:    "20-host.network",
					Content: "[Network]\nDescription=/nix/store/host-specific\n",
				})
			},
			want: "host-specific path",
		},
		{
			name: "catalog ref unresolved",
			mut: func(config *Config) {
				config.Spec.Kubernetes.PayloadVersion = ""
				config.Spec.Kubernetes.CatalogRef = "default"
			},
			want: "invalid Kubernetes sysext catalog",
		},
		{
			name: "bad payload version",
			mut: func(config *Config) {
				config.Spec.Kubernetes.PayloadVersion = "v1.36"
			},
			want: "must be vMAJOR.MINOR.PATCH",
		},
		{
			name: "bad system role default",
			mut: func(config *Config) {
				config.Spec.SystemRoleDefaults["controlplane"] = NodeLayer{}
			},
			want: "systemRoleDefaults key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfig()
			tt.mut(&config)
			request := CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")}
			if tt.name == "unknown address override" {
				request.AddressOverrides = map[string]string{"worker-1": "10.0.0.99"}
			}
			_, err := Compile(request)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Compile() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func assertNodeNativeFile(t *testing.T, node NodeMaterial, path string, content string) {
	t.Helper()
	for _, file := range node.NativeEtcFiles {
		if file.Path == path {
			if !strings.Contains(file.Content, content) {
				t.Fatalf("%s did not contain %q:\n%s", path, content, file.Content)
			}
			return
		}
	}
	t.Fatalf("node %s missing native file %s", node.Name, path)
}

func assertNodeNoNativeFile(t *testing.T, node NodeMaterial, path string) {
	t.Helper()
	for _, file := range node.NativeEtcFiles {
		if file.Path == path {
			t.Fatalf("node %s unexpectedly rendered native file %s", node.Name, path)
		}
	}
}

func clusterBGPConfig() bgpapivip.Config {
	return bgpapivip.Config{
		Endpoint: bgpapivip.Endpoint{
			Host: "api.home.example",
			VIP:  "10.40.0.10/32",
		},
		VIPInterface: bgpapivip.VIPInterface{
			Kind: "dummy",
			Name: "katl-api0",
		},
		Routing: bgpapivip.Routing{
			RouterID:        "10.0.0.11",
			LocalASN:        64512,
			SourceAddress:   "10.0.0.11",
			SourceInterface: "enp1s0",
		},
		FabricPeers: []bgpapivip.Peer{{
			Name:                  "router-a",
			Address:               "10.0.0.1",
			ASN:                   64500,
			AllowedExportPrefixes: []string{"10.40.0.10/32"},
		}},
	}
}

func ptrBGPConfig(config bgpapivip.Config) *bgpapivip.Config {
	return &config
}

func TestCompileRejectsHostSpecificKubeadmMaterial(t *testing.T) {
	configs := validKubeadmConfigs("v1.36.1")
	controlPlane := configs["control-plane"]
	controlPlane.Config.Content = []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nlocal: /nix/store/host-specific\n")
	configs["control-plane"] = controlPlane

	_, err := Compile(CompileRequest{Config: validConfig(), KubeadmConfigs: configs})
	if err == nil || !strings.Contains(err.Error(), "host-specific path") {
		t.Fatalf("Compile() error = %v, want host-specific path rejection", err)
	}
}

func TestCompileRejectsKubeadmIntentAndVersionMismatch(t *testing.T) {
	config := validConfig()
	config.Spec.SystemRoleDefaults[inventory.RoleWorker] = NodeLayer{
		Kubernetes: KubernetesLayer{KubeadmConfigRef: "control-plane"},
	}
	_, err := Compile(CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")})
	if err == nil || !strings.Contains(err.Error(), "requires kubeadm intent") {
		t.Fatalf("Compile() error = %v, want intent mismatch", err)
	}

	_, err = Compile(CompileRequest{Config: validConfig(), KubeadmConfigs: validKubeadmConfigs("v1.35.9")})
	if err == nil || !strings.Contains(err.Error(), "does not match selected Kubernetes payload version") {
		t.Fatalf("Compile() error = %v, want version mismatch", err)
	}
}

func validConfig() Config {
	targetCP := manifest.DiskSelector{ByID: "/dev/disk/by-id/ata-cp-root", MinSizeMiB: 32768}
	targetWorker := manifest.DiskSelector{ByID: "/dev/disk/by-id/ata-worker-root", MinSizeMiB: 32768}
	return Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "lab"},
		Spec: Spec{
			ControlPlaneEndpoint: "api.katl.test:6443",
			Kubernetes:           KubernetesSelection{PayloadVersion: "v1.36.1"},
			KatlosImage:          validImage(),
			WipeTarget:           true,
			Defaults: NodeLayer{
				SSH: manifest.SSHIdentity{AuthorizedKeys: []string{sshKey}},
				Networkd: manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
					Name:    "10-common.network",
					Content: "[Match]\nName=enp1s0\n\n[Network]\nDHCP=yes\n",
				}}},
				Install: InstallLayer{ExtraDisks: []manifest.ExtraDisk{{
					Name:       "data",
					Selector:   manifest.DiskSelector{ByID: "/dev/disk/by-id/ata-data"},
					Filesystem: "xfs",
					Mount:      manifest.ExtraMount{Path: "/srv/data"},
				}}},
				Bootstrap: BootstrapLayer{Access: inventory.Access{Method: "agent", CredentialRef: "vsock:1234:10240"}},
			},
			SystemRoleDefaults: map[inventory.SystemRole]NodeLayer{
				inventory.RoleControlPlane: {Kubernetes: KubernetesLayer{
					KubeadmConfigRef: "control-plane",
					NodeLabels:       map[string]string{"node-role.kubernetes.io/control-plane": ""},
					NodeTaints:       []manifest.NodeTaint{{Key: "node-role.kubernetes.io/control-plane", Effect: "NoSchedule"}},
				}},
				inventory.RoleWorker: {Kubernetes: KubernetesLayer{
					KubeadmConfigRef: "worker",
					NodeLabels:       map[string]string{"katl.dev/pool": "workers"},
				}},
			},
			Nodes: []Node{
				{
					Name:       "cp-1",
					SystemRole: inventory.RoleControlPlane,
					Overrides: NodeLayer{
						Hostname: "cp-1-host",
						SSH:      manifest.SSHIdentity{AuthorizedKeys: []string{sshKey}},
						Networkd: manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
							Name:    "20-cp.network",
							Content: "[Match]\nName=enp2s0\n",
						}}},
						Kubernetes: KubernetesLayer{
							NodeLabels: map[string]string{"katl.dev/zone": "rack-a"},
						},
						Install:   InstallLayer{TargetDisk: &targetCP},
						Bootstrap: BootstrapLayer{Address: "10.0.0.11"},
					},
				},
				{
					Name:       "worker-1",
					SystemRole: inventory.RoleWorker,
					Overrides: NodeLayer{
						Install:   InstallLayer{TargetDisk: &targetWorker},
						Bootstrap: BootstrapLayer{Address: "10.0.0.21", Access: inventory.Access{CredentialRef: "vsock:1235:10240"}},
					},
				},
			},
		},
	}
}

func validSysextCatalog() sysextcatalog.Catalog {
	return sysextcatalog.Catalog{
		APIVersion: sysextcatalog.APIVersion,
		Kind:       sysextcatalog.Kind,
		Entries: []sysextcatalog.Entry{{
			Name:            sysextcatalog.KubernetesName,
			ArtifactVersion: "6db181a573ef",
			PayloadVersion:  "v1.36.1",
			KubernetesMinor: "v1.36",
			Architecture:    "x86_64",
			SHA256:          "b6d80cca75983945d7a89339562f0c93edf006aaa0c1aee57b77e173071cddde",
			SizeBytes:       301191168,
			SourceRepo: artifact.SourceRepo{
				ID:      "kubernetes",
				BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
				Minor:   "v1.36",
			},
			LocalPath: "katl-kubernetes.raw",
			RuntimeInterfaces: []string{
				"katl-runtime-1",
			},
		}},
	}
}

func validImage() manifest.KatlosImage {
	return manifest.KatlosImage{
		URL:              "https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1073741824,
		Version:          "2026.06.04",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
}

func validKubeadmConfigs(version string) map[string]kubeadmconfig.Plan {
	return map[string]kubeadmconfig.Plan{
		"control-plane": {
			Name: "control-plane",
			Config: kubeadmconfig.File{
				RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
				Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"),
				Mode:       0o644,
			},
			Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration", KubernetesVersion: version}},
		},
		"worker": {
			Name: "worker",
			Config: kubeadmconfig.File{
				RenderPath: "/etc/katl/kubeadm/worker/config.yaml",
				Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"),
				Mode:       0o644,
			},
			Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration"}},
		},
	}
}
