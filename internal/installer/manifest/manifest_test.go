package manifest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/disk"
)

func TestDecodeAcceptsMinimal(t *testing.T) {
	manifest, err := Decode(strings.NewReader(validManifest()))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.Node.Identity.Hostname != "lab-node-01" {
		t.Fatalf("hostname = %q", manifest.Node.Identity.Hostname)
	}
	if len(manifest.Node.Identity.SSH.AuthorizedKeys) != 1 {
		t.Fatalf("authorized keys = %#v", manifest.Node.Identity.SSH.AuthorizedKeys)
	}
	if manifest.KatlosImage.URL == "" || manifest.KatlosImage.Role != "install" {
		t.Fatalf("katlos image = %#v", manifest.KatlosImage)
	}
}

func TestDecodeUsesDefaultKatlosImage(t *testing.T) {
	input := `apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: lab-node-01
    ssh:
      authorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  systemRole: control-plane
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/ata-root
`
	defaultImage := KatlosImage{
		LocalRef: "images/katlos-install.squashfs", SHA256: strings.Repeat("a", 64),
		SizeBytes: 1024, Version: "2026.7.0", Architecture: "x86_64",
		RuntimeInterface: "katl-runtime-1", Role: "install",
	}
	got, defaulted, err := DecodeWithDefaultImage(strings.NewReader(input), defaultImage)
	if err != nil {
		t.Fatalf("DecodeWithDefaultImage() error = %v", err)
	}
	if !defaulted || got.KatlosImage != defaultImage {
		t.Fatalf("defaulted = %v, image = %#v", defaulted, got.KatlosImage)
	}
	if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "katlosImage") {
		t.Fatalf("Decode() error = %v, want missing image rejection", err)
	}
}

func TestDecodeAcceptsYAML(t *testing.T) {
	manifest, err := Decode(strings.NewReader(`apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: lab-node-01
    ssh:
      authorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  systemRole: control-plane
  networkd:
    files:
      - name: 10-lan.network
        content: |
          [Match]
          Name=enp1s0

          [Network]
          DHCP=yes
  sysctl:
    settings:
      net.ipv4.ip_forward: "1"
  kubernetes:
    kubeadm:
      configRef: control-plane
  bootstrap:
    clusterName: lab
    inventoryNodeName: cp-1
    nodeAddress: 10.0.0.11
    controlPlaneEndpoint: api.katl.test:6443
    bootstrapProfileRef: control-plane
    kubernetesCatalogRef: v1.36.1
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/ata-root
    minSizeMiB: 32768
katlosImage:
  url: https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs
  sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  sizeBytes: 1073741824
  version: "2026.06.04"
  architecture: x86_64
  runtimeInterface: katl-runtime-1
  role: install
`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.Node.Kubernetes.Kubeadm.ConfigRef != "control-plane" {
		t.Fatalf("configRef = %q", manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	}
	if len(manifest.Node.Networkd.Files) != 1 || !strings.Contains(manifest.Node.Networkd.Files[0].Content, "DHCP=yes") {
		t.Fatalf("networkd files = %#v", manifest.Node.Networkd.Files)
	}
	if manifest.Node.Sysctl.Settings["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("sysctl settings = %#v", manifest.Node.Sysctl.Settings)
	}
	if manifest.Node.Bootstrap == nil || manifest.Node.Bootstrap.KubernetesCatalogRef != "v1.36.1" {
		t.Fatalf("bootstrap = %#v", manifest.Node.Bootstrap)
	}
}

func TestDecodeRejectsMissingWipeTarget(t *testing.T) {
	_, err := Decode(strings.NewReader(strings.Replace(validManifest(), "\n\t\t\t\"wipeTarget\": true,", "", 1)))
	if err == nil || !strings.Contains(err.Error(), "install.wipeTarget") {
		t.Fatalf("Decode() error = %v, want wipeTarget rejection", err)
	}
}

func TestDecodeRejectsLegacyDestructiveAcknowledgementFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "allow destructive install",
			body: strings.Replace(validManifest(), `"wipeTarget": true,`, `"allowDestructiveInstall": true,`, 1),
		},
		{
			name: "destructive acknowledgement",
			body: strings.Replace(validManifest(), `"wipeTarget": true,`, `"destructiveInstallAcknowledgement": "I understand this install is destructive.",`, 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.body))
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Decode() error = %v, want legacy field rejection", err)
			}
		})
	}
}

func TestDecodeRejectsUnsafeSysctl(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unsupported key",
			body: manifestWithNode(`"sysctl": {"settings": {"kernel.hostname": "bad"}}`),
			want: "kernel.hostname",
		},
		{
			name: "unsafe value",
			body: manifestWithNode(`"sysctl": {"settings": {"net.ipv4.ip_forward": " 1"}}`),
			want: "value is unsafe",
		},
		{
			name: "invalid boolean",
			body: manifestWithNode(`"sysctl": {"settings": {"net.ipv4.ip_forward": "true"}}`),
			want: "expected 0 or 1",
		},
		{
			name: "invalid positive integer",
			body: manifestWithNode(`"sysctl": {"settings": {"vm.max_map_count": "0"}}`),
			want: "positive base-10 integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsInvalidIdentityScalars(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "invalid hostname",
			body: strings.Replace(validManifest(), `"hostname": "lab-node-01"`, `"hostname": "Bad_Host"`, 1),
			want: "node.identity.hostname",
		},
		{
			name: "invalid ssh key",
			body: strings.Replace(validManifest(), `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example`, `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 katl@example`, 1),
			want: "node.identity.ssh.authorizedKeys[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeAcceptsExtraDisks(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithInstall(`,
			"extraDisks": [
				{
					"name": "data",
					"selector": {"byID": "/dev/disk/by-id/ata-data"},
					"filesystem": "xfs",
					"mount": {"path": "/srv/data"},
					"wipe": true
				}
			]`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(manifest.Install.ExtraDisks) != 1 || manifest.Install.ExtraDisks[0].Mount.Path != "/srv/data" {
		t.Fatalf("extra disks = %#v", manifest.Install.ExtraDisks)
	}
}

func TestDecodeAcceptsKubeadmConfigRef(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithNode(`,
			"kubernetes": {
				"kubeadm": {
					"configRef": "control-plane"
				}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.Node.Kubernetes.Kubeadm.ConfigRef != "control-plane" {
		t.Fatalf("configRef = %q", manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	}
}

func TestDecodeAcceptsNetworkdDomain(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithNode(`,
			"networkd": {
				"files": [
					{
						"name": "10-lan.network",
						"content": "[Match]\nName=enp1s0\n"
					}
				]
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(manifest.Node.Networkd.Files) != 1 || manifest.Node.Networkd.Files[0].Name != "10-lan.network" {
		t.Fatalf("networkd = %#v", manifest.Node.Networkd)
	}
}

func TestDecodeAcceptsLocalKatlosImageRef(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithImageObject(`{
		"localRef": "payloads/katlos-install.squashfs",
		"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sizeBytes": 1073741824,
		"version": "2026.06.04",
		"architecture": "x86_64",
		"runtimeInterface": "katl-runtime-1",
		"role": "install"
	}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.KatlosImage.LocalRef != "payloads/katlos-install.squashfs" {
		t.Fatalf("localRef = %q", manifest.KatlosImage.LocalRef)
	}
}

func TestDecodeRejectsUnsafeKatlosImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name: "missing source",
			image: `{
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "url or localRef",
		},
		{
			name: "both sources",
			image: `{
				"url": "https://example.invalid/katlos.squashfs",
				"localRef": "payloads/katlos.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "must not set both",
		},
		{
			name: "relative url",
			image: `{
				"url": "payloads/katlos.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "url must be absolute",
		},
		{
			name: "bad local ref characters",
			image: `{
				"localRef": "payloads/katlos image.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "clean relative path",
		},
		{
			name: "bad local ref traversal",
			image: `{
				"localRef": "../payloads/katlos.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "dot segments",
		},
		{
			name: "bad sha",
			image: `{
				"url": "https://example.invalid/katlos.squashfs",
				"sha256": "not-a-digest",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "install"
			}`,
			want: "sha256 is invalid",
		},
		{
			name: "bad role",
			image: `{
				"url": "https://example.invalid/katlos.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sizeBytes": 1073741824,
				"version": "2026.06.04",
				"architecture": "x86_64",
				"role": "upgrade"
			}`,
			want: "role must be install",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(manifestWithImageObject(tt.image)))
			if err == nil {
				t.Fatal("Decode() error = nil, want katlosImage rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsLegacyLooseArtifacts(t *testing.T) {
	legacy := strings.Replace(validManifest(),
		`"katlosImage": {
			"url": "https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}`,
		`"artifacts": {
			"runtimeRoot": {
				"url": "https://example.invalid/root.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
		}`, 1)
	_, err := Decode(strings.NewReader(legacy))
	if err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "artifacts") {
		t.Fatalf("Decode() error = %v, want legacy artifacts rejection", err)
	}
}

func TestDecodeRejectsUnsafeNetworkdDomain(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{
			name: "path traversal",
			file: `{"name": "../10-lan.network", "content": "[Match]\nName=enp1s0\n"}`,
			want: "single path segment",
		},
		{
			name: "wrong extension",
			file: `{"name": "10-lan.conf", "content": "[Match]\nName=enp1s0\n"}`,
			want: "must end with",
		},
		{
			name: "empty content",
			file: `{"name": "10-lan.network", "content": ""}`,
			want: "content is required",
		},
		{
			name: "bad character",
			file: `{"name": "10 lan.network", "content": "[Match]\nName=enp1s0\n"}`,
			want: "unsupported character",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(manifestWithNode(fmt.Sprintf(`,
			"networkd": {
				"files": [%s]
			}`, tt.file))))
			if err == nil {
				t.Fatal("Decode() error = nil, want networkd rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsDuplicateNetworkdFiles(t *testing.T) {
	_, err := Decode(strings.NewReader(manifestWithNode(`,
			"networkd": {
				"files": [
					{"name": "10-lan.network", "content": "[Match]\nName=enp1s0\n"},
					{"name": "10-lan.network", "content": "[Match]\nName=enp2s0\n"}
				]
			}`)))
	if err == nil || !strings.Contains(err.Error(), "duplicates another networkd file") {
		t.Fatalf("Decode() error = %v, want duplicate networkd rejection", err)
	}
}

func TestDecodeRejectsUnsafeKubeadmConfigRef(t *testing.T) {
	tests := []string{
		"../control-plane",
		"ControlPlane",
		"control_plane",
		"-control-plane",
		"control-plane-",
		" control-plane",
		strings.Repeat("a", 64),
	}
	for _, configRef := range tests {
		t.Run(configRef, func(t *testing.T) {
			_, err := Decode(strings.NewReader(manifestWithNode(fmt.Sprintf(`,
			"kubernetes": {
				"kubeadm": {
					"configRef": %q
				}
			}`, configRef))))
			if err == nil {
				t.Fatal("Decode() error = nil, want configRef rejection")
			}
			if !strings.Contains(err.Error(), "node.kubernetes.kubeadm.configRef") {
				t.Fatalf("Decode() error = %q, want configRef field", err)
			}
		})
	}
}

func TestDecodeAcceptsBootstrapIntent(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithNode(`,
			"bootstrap": {
				"clusterName": "lab",
				"inventoryNodeName": "cp-1",
				"nodeAddress": "10.0.0.11",
				"controlPlaneEndpoint": "api.katl.test:6443",
				"bootstrapProfileRef": "control-plane",
				"profileResolvedID": "kubeadm:control-plane",
				"kubernetesCatalogRef": "v1.36",
				"access": {"method": "agent", "credentialRef": "vsock:1234:10240"},
				"labels": {"katl.dev/zone": "rack-a", "node-role.kubernetes.io/control-plane": ""},
				"taints": [{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"}]
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.Node.Bootstrap == nil || manifest.Node.Bootstrap.BootstrapProfileRef != "control-plane" {
		t.Fatalf("bootstrap intent = %#v", manifest.Node.Bootstrap)
	}
}

func TestDecodeRejectsUnsafeBootstrapIntent(t *testing.T) {
	tests := []struct {
		name      string
		bootstrap string
		want      string
	}{
		{
			name:      "unsupported access method",
			bootstrap: `"access": {"method": "token", "credentialRef": "secret-ref"}`,
			want:      "access.method",
		},
		{
			name:      "inline access token",
			bootstrap: `"access": {"method": "agent", "credentialRef": "abcdef.0123456789abcdef"}`,
			want:      "inline secret material",
		},
		{
			name:      "bad label key",
			bootstrap: `"labels": {"bad key": "value"}`,
			want:      "labels key",
		},
		{
			name:      "bad label value",
			bootstrap: `"labels": {"katl.dev/zone": "bad value"}`,
			want:      "labels",
		},
		{
			name:      "bad taint key",
			bootstrap: `"taints": [{"key": "bad key", "effect": "NoSchedule"}]`,
			want:      "taints",
		},
		{
			name:      "bad taint effect",
			bootstrap: `"taints": [{"key": "katl.dev/dedicated", "effect": "Sometimes"}]`,
			want:      "effect",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(manifestWithNode(`,
			"bootstrap": {` + tt.bootstrap + `}`)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsMissingOrUnsupportedSystemRole(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "missing", manifest: strings.Replace(validManifest(), `,`+"\n\t\t\t\"systemRole\": \"control-plane\"", "", 1), want: "node.systemRole is required"},
		{name: "padded", manifest: strings.Replace(validManifest(), `"systemRole": "control-plane"`, `"systemRole": " worker "`, 1), want: "must not contain leading or trailing whitespace"},
		{name: "unsupported", manifest: strings.Replace(validManifest(), `"systemRole": "control-plane"`, `"systemRole": "storage"`, 1), want: "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.manifest))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsRootDiskLayoutFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "root slot sizing", field: `"rootSlots":{"aMiB":4096,"bMiB":4096}`},
		{name: "state partition filesystem", field: `"state":{"filesystem":"xfs"}`},
		{name: "partition table", field: `"partitionTable":"gpt"`},
		{name: "root disk filesystem", field: `"filesystem":"ext4"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := manifestWithInstall(`,` + tt.field)
			_, err := Decode(strings.NewReader(manifest))
			if err == nil {
				t.Fatalf("Decode() error = nil, want unknown root layout field rejection")
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Decode() error = %q, want unknown field rejection", err)
			}
		})
	}
}

func TestDecodeRejectsDeferredFields(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "metadata name", manifest: manifestWithTop(`, "metadata": {"name": "lab-node-01"}`), want: "metadata"},
		{name: "metadata generation", manifest: manifestWithTop(`, "metadata": {"generation": "1"}`), want: "metadata"},
		{name: "metadata labels", manifest: manifestWithTop(`, "metadata": {"labels": {"env": "lab"}}`), want: "metadata"},
		{name: "node name", manifest: manifestWithNode(`, "name": "lab-node-01"`), want: "name"},
		{name: "node selectors", manifest: manifestWithNode(`, "selectors": {"rack": "a"}`), want: "selectors"},
		{name: "ssh enabled", manifest: manifestWithSSH(`, "enabled": true`), want: "enabled"},
		{name: "installer authorized keys", manifest: manifestWithSSH(`, "installerAuthorizedKeys": []`), want: "installerAuthorizedKeys"},
		{name: "machine id", manifest: manifestWithNode(`, "machineID": "0123456789abcdef0123456789abcdef"`), want: "machineID"},
		{name: "machine id policy", manifest: manifestWithNode(`, "machineIDPolicy": "preserve"`), want: "machineIDPolicy"},
		{name: "host accounts", manifest: manifestWithNode(`, "accounts": [{"name": "admin"}]`), want: "accounts"},
		{name: "sudo policy", manifest: manifestWithNode(`, "sudo": {"groups": ["wheel"]}`), want: "sudo"},
		{name: "pam policy", manifest: manifestWithNode(`, "pam": {"limits": []}`), want: "pam"},
		{name: "sysusers policy", manifest: manifestWithNode(`, "sysusers": ["u admin"]`), want: "sysusers"},
		{name: "ssh host keys", manifest: manifestWithSSH(`, "hostKeys": {"policy": "import"}`), want: "hostKeys"},
		{name: "sshd policy", manifest: manifestWithSSH(`, "sshd": {"passwordAuthentication": true}`), want: "sshd"},
		{name: "top-level etc files", manifest: manifestWithTop(`, "etc": {"files": {"/etc/hostname": "lab-node-01\n"}}`), want: "etc"},
		{name: "trust", manifest: manifestWithTop(`, "trust": {"roots": []}`), want: "trust"},
		{name: "boot", manifest: manifestWithTop(`, "boot": {"efi": true}`), want: "boot"},
		{name: "kernel args", manifest: manifestWithTop(`, "kernelArgs": ["quiet"]`), want: "kernelArgs"},
		{name: "extra disk mount options", manifest: manifestWithInstall(`,
			"extraDisks": [
				{
					"name": "data",
					"selector": {"byID": "/dev/disk/by-id/ata-data"},
					"filesystem": "xfs",
					"mount": {"path": "/srv/data", "options": ["noatime"]}
				}
			]`), want: "options"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.manifest))
			if err == nil {
				t.Fatal("Decode() error = nil, want rejection")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildDiskLayoutRequestUsesKatlOwnedRootProfile(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithInstall(`,
			"extraDisks": [
				{
					"name": "data",
					"selector": {"byID": "/dev/disk/by-id/ata-data"},
					"filesystem": "xfs",
					"mount": {"path": "/srv/data"},
					"wipe": true
				}
			]`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	request, err := BuildDiskLayoutRequest(manifest, RootDiskProfile{
		ESPSizeMiB:      512,
		XBOOTLDRSizeMiB: 1024,
		RootSlotSizeMiB: 16384,
		StateFilesystem: "ext4",
		StateMinSizeMiB: 8192,
		InitialRootSlot: disk.RootSlotB,
	}, 4096)
	if err != nil {
		t.Fatalf("BuildDiskLayoutRequest() error = %v", err)
	}

	if request.TargetDisk.ByID != "/dev/disk/by-id/ata-root" || request.TargetDisk.MinSizeMiB != 32768 {
		t.Fatalf("target disk selector = %#v", request.TargetDisk)
	}
	if request.RootA.SizeMiB != 16384 || request.RootB.SizeMiB != 16384 {
		t.Fatalf("root slot sizes = %d/%d, want Katl-owned profile size", request.RootA.SizeMiB, request.RootB.SizeMiB)
	}
	if request.State.Filesystem != "ext4" || request.State.MinSizeMiB != 8192 {
		t.Fatalf("state request = %#v, want Katl-owned profile", request.State)
	}
	if request.InitialRootSlot != disk.RootSlotB || request.RuntimeRootSizeMiB != 4096 {
		t.Fatalf("root profile fields = %#v", request)
	}
	if len(request.ExtraDisks) != 1 || request.ExtraDisks[0].Filesystem != "xfs" || request.ExtraDisks[0].MountPath != "/srv/data" || !request.ExtraDisks[0].Wipe {
		t.Fatalf("extra disks = %#v", request.ExtraDisks)
	}
}

func TestDefaultRootDiskProfileBuildsUsableLayoutRequest(t *testing.T) {
	manifest, err := Decode(strings.NewReader(manifestWithTarget(`{"serial": "root-serial"}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	request, err := BuildDiskLayoutRequest(manifest, DefaultRootDiskProfile(), 2048)
	if err != nil {
		t.Fatalf("BuildDiskLayoutRequest() error = %v", err)
	}
	if request.TargetDisk.Serial != "root-serial" {
		t.Fatalf("target selector = %#v", request.TargetDisk)
	}
	if request.ESPSizeMiB != disk.DefaultESPSizeMiB || request.RootA.SizeMiB == 0 || request.RootB.SizeMiB == 0 || request.State.Filesystem == "" {
		t.Fatalf("default layout request = %#v", request)
	}
}

func validManifest() string {
	return manifestWithTop("")
}

func manifestWithTop(extra string) string {
	return fmt.Sprintf(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
					]
				}
			},
			"systemRole": "control-plane"
		},
		"install": {
			"wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install-2026.06.04-x86_64.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}%s
	}`, extra)
}

func manifestWithNode(extra string) string {
	return strings.Replace(validManifest(), `"node": {`, `"node": {`+objectField(extra), 1)
}

func manifestWithSSH(extra string) string {
	return strings.Replace(validManifest(), `"ssh": {`, `"ssh": {`+objectField(extra), 1)
}

func manifestWithInstall(extra string) string {
	return strings.Replace(validManifest(), `"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}`, `"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}`+extra, 1)
}

func manifestWithTarget(targetDisk string) string {
	return strings.Replace(validManifest(), `{"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}`, targetDisk, 1)
}

func manifestWithImageObject(imageObject string) string {
	return fmt.Sprintf(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
					]
				}
			},
			"systemRole": "control-plane"
		},
		"install": {
			"wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"katlosImage": %s
	}`, imageObject)
}

func objectField(extra string) string {
	extra = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(extra), ","))
	return "\n\t\t\t" + extra + ","
}
