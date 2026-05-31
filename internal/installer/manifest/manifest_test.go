package manifest

import (
	"strings"
	"testing"

	"git.cbannister.xyz/chris/katl/internal/installer/disk"
)

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
			manifest := `{
				"apiVersion": "install.katl.dev/v1alpha1",
				"kind": "InstallManifest",
				"install": {
					"allowDestructiveInstall": true,
					"targetDisk": {"byID": "/dev/disk/by-id/ata-root"},
					` + tt.field + `
				}
			}`
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

func TestBuildDiskLayoutRequestUsesKatlOwnedRootProfile(t *testing.T) {
	manifest, err := Decode(strings.NewReader(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"metadata": {"name": "lab-node-01"},
		"node": {"name": "lab-node-01"},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768},
			"extraDisks": [
				{
					"name": "data",
					"selector": {"byID": "/dev/disk/by-id/ata-data"},
					"filesystem": "xfs",
					"mount": {"path": "/srv/data"},
					"wipe": true
				}
			]
		},
		"artifacts": {"runtimeRoot": {"url": "https://example.invalid/root.squashfs", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		"etc": {"files": {"/etc/hostname": "lab-node-01\n"}},
		"trust": {"roots": []},
		"boot": {"efi": true}
	}`))
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
	manifest, err := Decode(strings.NewReader(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"serial": "root-serial"}
		}
	}`))
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
