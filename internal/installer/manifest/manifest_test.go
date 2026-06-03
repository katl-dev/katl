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
	if manifest.Artifacts.RuntimeRoot.URL == "" || manifest.Artifacts.UKI == nil {
		t.Fatalf("artifacts = %#v", manifest.Artifacts)
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
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"
					]
				}
			}
		},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"artifacts": {
			"runtimeRoot": {
				"url": "https://example.invalid/root.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			"uki": {
				"url": "https://example.invalid/katl.efi",
				"sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
			"sysexts": [
				{
					"name": "kubelet",
					"url": "https://example.invalid/kubelet.sysext.raw",
					"sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
				}
			]
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

func objectField(extra string) string {
	extra = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(extra), ","))
	return "\n\t\t\t" + extra + ","
}
