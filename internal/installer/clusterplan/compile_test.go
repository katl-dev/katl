package clusterplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/artifact"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/sysextcatalog"
)

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"

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
  allowDestructiveInstall: true
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
			name: "missing endpoint",
			mut: func(config *Config) {
				config.Spec.ControlPlaneEndpoint = ""
			},
			want: "control-plane endpoint is required",
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
			ControlPlaneEndpoint:    "api.katl.test:6443",
			Kubernetes:              KubernetesSelection{PayloadVersion: "v1.36.1"},
			KatlosImage:             validImage(),
			AllowDestructiveInstall: true,
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
				inventory.RoleControlPlane: {Kubernetes: KubernetesLayer{KubeadmConfigRef: "control-plane"}},
				inventory.RoleWorker:       {Kubernetes: KubernetesLayer{KubeadmConfigRef: "worker"}},
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
