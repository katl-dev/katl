package clusterplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
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
	if got := plan.BootstrapInventory.Nodes[0].Labels["katl.dev/zone"]; got != "rack-a" {
		t.Fatalf("first inventory node zone = %q", got)
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
	hostname := nativeFile(cp.NativeEtcFiles, "/etc/hostname")
	if hostname == nil || hostname.Content != "cp-1-host\n" {
		t.Fatalf("hostname confext = %#v", hostname)
	}
	authorizedKeys := nativeFile(cp.NativeEtcFiles, "/etc/ssh/authorized_keys/katl")
	if authorizedKeys == nil || authorizedKeys.Mode != 0o644 || authorizedKeys.Content != sshKey+"\n" {
		t.Fatalf("authorized keys confext = %#v", authorizedKeys)
	}
	rootAuthorizedKeys := nativeFile(cp.NativeEtcFiles, "/etc/ssh/authorized_keys/root")
	if rootAuthorizedKeys == nil || rootAuthorizedKeys.Content != authorizedKeys.Content {
		t.Fatalf("root authorized keys confext = %#v", rootAuthorizedKeys)
	}
	if len(cp.InstallManifest.Node.Identity.SSH.AuthorizedKeys) != 1 {
		t.Fatalf("ssh keys were not de-duplicated: %#v", cp.InstallManifest.Node.Identity.SSH.AuthorizedKeys)
	}
	if cp.InstallManifest.Node.ControlPlaneEndpoint != nil {
		t.Fatalf("external endpoint selected managed advertisement: %#v", cp.InstallManifest.Node.ControlPlaneEndpoint)
	}
	if nativeFile(cp.NativeEtcFiles, "/etc/katl/apps/bird/bird.conf") != nil {
		t.Fatal("external endpoint generated BIRD config")
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

func TestCompileSelectsManagedEndpointOnlyForControlPlanes(t *testing.T) {
	config := validConfig()
	config.Spec.ControlPlaneEndpoint = &controlplaneendpoint.Config{
		Host: "api.katl.test",
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: "10.40.0.10",
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: "10.0.0.1", ASN: 64500}},
			},
		},
	}
	plan, err := Compile(CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")})
	if err != nil {
		t.Fatal(err)
	}
	cp := plan.Nodes[0]
	worker := plan.Nodes[1]
	if cp.InstallManifest.Node.ControlPlaneEndpoint == nil {
		t.Fatal("control-plane managed endpoint intent is nil")
	}
	if worker.InstallManifest.Node.ControlPlaneEndpoint != nil {
		t.Fatalf("worker managed endpoint intent = %#v", worker.InstallManifest.Node.ControlPlaneEndpoint)
	}
	if nativeFile(cp.NativeEtcFiles, "/etc/katl/apps/bird/bird.conf") == nil {
		t.Fatal("control-plane BIRD config is missing")
	}
	if nativeFile(worker.NativeEtcFiles, "/etc/katl/apps/bird/bird.conf") != nil {
		t.Fatal("worker received BIRD config")
	}
}

func TestCompileRejectsNativeKubeadmConflictWithManagedEndpoint(t *testing.T) {
	config := validConfig()
	config.Spec.ControlPlaneEndpoint = &controlplaneendpoint.Config{
		Host: "api.katl.test",
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: "10.40.0.10",
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: "10.0.0.1", ASN: 64500}},
			},
		},
	}
	configs := validKubeadmConfigs("v1.36.1")
	controlPlane := configs["control-plane"]
	controlPlane.Config.Content = []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    - name: bind-address\n      value: 10.0.0.11\n")
	configs["control-plane"] = controlPlane

	_, err := Compile(CompileRequest{Config: config, KubeadmConfigs: configs})
	if err == nil || !strings.Contains(err.Error(), "does not accept the managed VIP") {
		t.Fatalf("Compile() error = %v, want managed endpoint conflict", err)
	}
}

func nativeFile(files []confext.NativeEtcFile, path string) *confext.NativeEtcFile {
	for index := range files {
		if files[index].Path == path {
			return &files[index]
		}
	}
	return nil
}

func TestCompileRemovesManagementCredentialReference(t *testing.T) {
	config := validConfig()
	config.Spec.Nodes[0].Overrides.Bootstrap.Access = inventory.Access{
		Method:        "agent",
		CredentialRef: "file:/home/operator/.config/katl/credentials/lab/cp-1.token",
	}
	plan, err := Compile(CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if got := plan.Nodes[0].InstallManifest.Node.Bootstrap.Access.CredentialRef; got != "" {
		t.Fatalf("install credentialRef = %q", got)
	}
	if got := plan.BootstrapInventory.Nodes[0].Access.CredentialRef; got != "" {
		t.Fatalf("inventory credentialRef = %q", got)
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

func TestCompileDefaultDHCPUsesStableMACIdentity(t *testing.T) {
	config := validConfig()
	config.Spec.Defaults.Networkd.Files = nil
	config.Spec.Nodes[0].Overrides.Networkd.Files = nil
	plan, err := Compile(CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	for _, node := range plan.Nodes {
		files := node.InstallManifest.Node.Networkd.Files
		if len(files) != 1 || !strings.Contains(files[0].Content, "[DHCPv4]\nClientIdentifier=mac\nUseHostname=no") || !strings.Contains(files[0].Content, "[DHCPv6]\nUseHostname=no") {
			t.Fatalf("%s default network = %#v", node.Name, files)
		}
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
	if bootstrap == nil || bootstrap.KubernetesBundle != "" {
		t.Fatalf("install manifest bootstrap = %#v", bootstrap)
	}
	if plan.Nodes[0].KubernetesBundleRef != bundleRef {
		t.Fatalf("node material = %#v", plan.Nodes[0])
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
  controlPlaneEndpoint:
    host: api.katl.test
    port: 6443
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
  nodes:
    - name: cp-1
      systemRole: control-plane
      overrides:
        kubernetes:
          kubeadmConfigRef: control-plane
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-cp-root
            minSizeMiB: 32768
        bootstrap:
          address: 10.0.0.11
    - name: worker-1
      systemRole: worker
      overrides:
        kubernetes:
          kubeadmConfigRef: worker
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

func TestCompileAcceptsMultipleControlPlanesWithoutBootstrapOrdering(t *testing.T) {
	config := validConfig()
	config.Spec.Nodes[1].SystemRole = inventory.RoleControlPlane
	config.Spec.Nodes[1].Overrides.Kubernetes.KubeadmConfigRef = "control-plane"

	plan, err := Compile(CompileRequest{
		Config:                     config,
		KubeadmConfigs:             validKubeadmConfigs("v1.36.1"),
		KubernetesArtifactBasePath: "/var/lib/katl/artifacts",
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if got := plan.BootstrapInventory.Nodes[1].SystemRole; got != inventory.RoleControlPlane {
		t.Fatalf("second node role = %q", got)
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
			name: "missing wipe target",
			mut: func(config *Config) {
				config.Spec.WipeTarget = false
			},
			want: "spec.wipeTarget",
		},
		{
			name: "missing multi-control-plane endpoint",
			mut: func(config *Config) {
				config.Spec.ControlPlaneEndpoint = nil
				config.Spec.Nodes[1].SystemRole = inventory.RoleControlPlane
			},
			want: "spec.controlPlaneEndpoint is required",
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
				config.Spec.Defaults.Kubernetes.NodeLabels = map[string]string{"katl.dev/zone": "rack-a"}
				config.Spec.Nodes[0].Overrides.Kubernetes.NodeLabels = map[string]string{"katl.dev/zone": "different"}
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
			name: "defaults target disk identity",
			mut: func(config *Config) {
				config.Spec.Defaults.Install.TargetDisk = &manifest.DiskSelector{Serial: "shared-root"}
			},
			want: "spec.defaults.install.targetDisk is not allowed",
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

func TestCompileAllowsInstallMediaImage(t *testing.T) {
	config := validConfig()
	config.Spec.KatlosImage = manifest.KatlosImage{}
	plan, err := Compile(CompileRequest{Config: config, KubeadmConfigs: validKubeadmConfigs("v1.36.1")})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if !manifest.KatlosImageEmpty(plan.KatlosImage) {
		t.Fatalf("KatlosImage = %#v, want install-media selection", plan.KatlosImage)
	}
	for _, node := range plan.Nodes {
		if !manifest.KatlosImageEmpty(node.InstallManifest.KatlosImage) {
			t.Fatalf("node %s KatlosImage = %#v", node.Name, node.InstallManifest.KatlosImage)
		}
	}
}

func TestDecodeRejectsTemplateRangeAndMultipleClassShapes(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "multiple classes",
			yaml: `apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  nodes:
    - name: cp-1
      systemRole: control-plane
      nodeClass:
        - ms01
        - gpu
`,
			want: "field nodeClass not found",
		},
		{
			name: "node template",
			yaml: `apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  nodeTemplate:
    count: 3
`,
			want: "field nodeTemplate not found",
		},
		{
			name: "node range",
			yaml: `apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  nodeRange:
    prefix: worker
`,
			want: "field nodeRange not found",
		},
		{
			name: "auto detection",
			yaml: `apiVersion: cluster.katl.dev/v1alpha1
kind: ClusterPlan
metadata:
  name: lab
spec:
  nodeClasses:
    ms01:
      install:
        targetDiskDefaults:
          autoDetect: true
`,
			want: "field nodeClasses not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %v, want %q", err, tt.want)
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
	config.Spec.Nodes[1].Overrides.Kubernetes.KubeadmConfigRef = "control-plane"
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
			ControlPlaneEndpoint: &controlplaneendpoint.Config{Host: "api.katl.test", Port: 6443},
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
							KubeadmConfigRef: "control-plane",
							NodeLabels: map[string]string{
								"node-role.kubernetes.io/control-plane": "",
								"katl.dev/zone":                         "rack-a",
							},
							NodeTaints: []manifest.NodeTaint{{Key: "node-role.kubernetes.io/control-plane", Effect: "NoSchedule"}},
						},
						Install:   InstallLayer{TargetDisk: &targetCP},
						Bootstrap: BootstrapLayer{Address: "10.0.0.11"},
					},
				},
				{
					Name:       "worker-1",
					SystemRole: inventory.RoleWorker,
					Overrides: NodeLayer{
						Install: InstallLayer{TargetDisk: &targetWorker},
						Kubernetes: KubernetesLayer{
							KubeadmConfigRef: "worker",
							NodeLabels:       map[string]string{"katl.dev/pool": "workers"},
						},
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
