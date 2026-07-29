package discovery

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestStaticDiscoverySourceReturnsFacts(t *testing.T) {
	want := HardwareFacts{
		SystemUUID:     "8f82b423-1f07-4a34-a9ad-9a8809f47d4a",
		DMIProductUUID: "8f82b423-1f07-4a34-a9ad-9a8809f47d4a",
		NICs: []NICFact{
			{Name: "eno1", MACAddress: "52:54:00:12:34:56", Driver: "virtio_net", OperState: "up"},
		},
		Mounts: []MountFact{
			{Source: "/dev/nvme0n1p1", Target: "/boot", Filesystem: "vfat"},
		},
		BlockDevices: []BlockDevice{
			{
				Name:      "nvme0n1",
				Path:      "/dev/nvme0n1",
				Type:      DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/nvme-KATL_ROOT"},
				SizeBytes: 64 * 1024 * 1024 * 1024,
			},
		},
	}

	got, err := (StaticDiscoverySource{Facts: want}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover() = %#v, want %#v", got, want)
	}
}

func TestMatchTargetDiskByIDReportsSignatures(t *testing.T) {
	facts := HardwareFacts{
		BlockDevices: []BlockDevice{
			{
				Name:               "nvme0n1",
				Path:               "/dev/nvme0n1",
				Type:               DeviceDisk,
				ByID:               []string{"/dev/disk/by-id/nvme-KATL_ROOT"},
				Serial:             "root-serial",
				SizeBytes:          128 * 1024 * 1024 * 1024,
				PartitionSignature: "gpt",
				Partitions: []BlockDevice{
					{
						Name:                "nvme0n1p1",
						Path:                "/dev/nvme0n1p1",
						Type:                DevicePartition,
						FilesystemSignature: "vfat",
						PartitionSignature:  "esp",
					},
				},
			},
		},
	}

	match, err := MatchTargetDisk(facts, TargetDiskSelector{
		ByID:       "/dev/disk/by-id/nvme-KATL_ROOT",
		MinSizeMiB: 32768,
	})
	if err != nil {
		t.Fatalf("MatchTargetDisk() error = %v", err)
	}
	if match.Device.Path != "/dev/nvme0n1" {
		t.Fatalf("matched device path = %q, want /dev/nvme0n1", match.Device.Path)
	}

	wantSignatures := []SignatureReport{
		{DevicePath: "/dev/nvme0n1", Kind: "partition-table", Value: "gpt"},
		{DevicePath: "/dev/nvme0n1p1", Kind: "filesystem", Value: "vfat"},
		{DevicePath: "/dev/nvme0n1p1", Kind: "partition-type", Value: "esp"},
	}
	if !reflect.DeepEqual(match.Signatures, wantSignatures) {
		t.Fatalf("signatures = %#v, want %#v", match.Signatures, wantSignatures)
	}
}

func TestMatchTargetDiskRejectsUnsafeMatches(t *testing.T) {
	tests := []struct {
		name     string
		facts    HardwareFacts
		selector TargetDiskSelector
	}{
		{
			name: "read only disk",
			facts: factsWithDisk(BlockDevice{
				Path:      "/dev/sda",
				Type:      DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/ata-read-only"},
				ReadOnly:  true,
				SizeBytes: 64 * 1024 * 1024 * 1024,
			}),
			selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-read-only"},
		},
		{
			name: "partition matched instead of disk",
			facts: factsWithDisk(BlockDevice{
				Path:      "/dev/sda1",
				Type:      DevicePartition,
				ByID:      []string{"/dev/disk/by-id/ata-root-part1"},
				SizeBytes: 64 * 1024 * 1024 * 1024,
			}),
			selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-root-part1"},
		},
		{
			name: "disk is too small",
			facts: factsWithDisk(BlockDevice{
				Path:      "/dev/sda",
				Type:      DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/ata-small"},
				SizeBytes: 8 * 1024 * 1024 * 1024,
			}),
			selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-small", MinSizeMiB: 32768},
		},
		{
			name: "partition is mounted",
			facts: factsWithDisk(BlockDevice{
				Path:      "/dev/sda",
				Type:      DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/ata-mounted"},
				SizeBytes: 64 * 1024 * 1024 * 1024,
				Partitions: []BlockDevice{
					{
						Path:        "/dev/sda1",
						Type:        DevicePartition,
						Mountpoints: []string{"/boot"},
					},
				},
			}),
			selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-mounted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MatchTargetDisk(tt.facts, tt.selector)
			if !errors.Is(err, ErrUnsafeTargetDisk) {
				t.Fatalf("MatchTargetDisk() error = %v, want ErrUnsafeTargetDisk", err)
			}
		})
	}
}

func TestMatchTargetDiskRejectsUnstableOrAmbiguousSelectors(t *testing.T) {
	tests := []struct {
		name     string
		facts    HardwareFacts
		selector TargetDiskSelector
	}{
		{
			name:     "missing selector",
			facts:    HardwareFacts{},
			selector: TargetDiskSelector{},
		},
		{
			name:     "short kernel path selector",
			facts:    HardwareFacts{},
			selector: TargetDiskSelector{ByID: "/dev/sda"},
		},
		{
			name: "ambiguous match",
			facts: HardwareFacts{BlockDevices: []BlockDevice{
				{Path: "/dev/sda", Type: DeviceDisk, Serial: "duplicate", SizeBytes: 64 * 1024 * 1024 * 1024},
				{Path: "/dev/sdb", Type: DeviceDisk, Serial: "duplicate", SizeBytes: 64 * 1024 * 1024 * 1024},
			}},
			selector: TargetDiskSelector{Serial: "duplicate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := MatchTargetDisk(tt.facts, tt.selector); err == nil {
				t.Fatalf("MatchTargetDisk() error = nil, want failure")
			}
		})
	}
}

func TestMatchPartitionUsesStableIdentitiesAndRejectsMountedOrAmbiguousMatches(t *testing.T) {
	data := BlockDevice{
		Path:                "/dev/nvme1n1p1",
		Type:                DevicePartition,
		ByID:                []string{"/dev/disk/by-id/nvme-data-part1"},
		GPTLabel:            "u-local-hostpath",
		PartitionUUID:       "part-uuid",
		FilesystemUUID:      "fs-uuid",
		FilesystemSignature: "xfs",
	}
	facts := HardwareFacts{BlockDevices: []BlockDevice{{
		Path:       "/dev/nvme1n1",
		Type:       DeviceDisk,
		Partitions: []BlockDevice{data},
	}}}

	for name, selector := range map[string]PartitionSelector{
		"by-id":           {ByID: "/dev/disk/by-id/nvme-data-part1"},
		"partuuid":        {PartUUID: "part-uuid"},
		"filesystem uuid": {FilesystemUUID: "fs-uuid"},
		"partlabel":       {PartLabel: "u-local-hostpath"},
	} {
		t.Run(name, func(t *testing.T) {
			match, err := MatchPartition(facts, selector)
			if err != nil {
				t.Fatalf("MatchPartition() error = %v", err)
			}
			if match.Device.Path != data.Path || match.ParentDisk.Path != "/dev/nvme1n1" {
				t.Fatalf("match = %#v", match)
			}
		})
	}

	mounted := facts
	mounted.Mounts = []MountFact{{Source: "UUID=fs-uuid", Target: "/var/mnt/data"}}
	if _, err := MatchPartition(mounted, PartitionSelector{FilesystemUUID: "fs-uuid"}); !errors.Is(err, ErrUnsafeTargetDisk) {
		t.Fatalf("mounted MatchPartition() error = %v, want ErrUnsafeTargetDisk", err)
	}

	ambiguous := facts
	ambiguous.BlockDevices = append(ambiguous.BlockDevices, BlockDevice{
		Path: "/dev/nvme2n1", Type: DeviceDisk, Partitions: []BlockDevice{{
			Path: "/dev/nvme2n1p1", Type: DevicePartition, GPTLabel: "u-local-hostpath",
		}},
	})
	if _, err := MatchPartition(ambiguous, PartitionSelector{PartLabel: "u-local-hostpath"}); err == nil {
		t.Fatal("ambiguous MatchPartition() error = nil, want failure")
	}
}

func factsWithDisk(device BlockDevice) HardwareFacts {
	return HardwareFacts{BlockDevices: []BlockDevice{device}}
}
