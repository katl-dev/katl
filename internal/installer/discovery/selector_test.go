package discovery

import (
	"errors"
	"strings"
	"testing"
)

const gib = 1024 * 1024 * 1024

// selectorFacts has a virtio disk with no hardware identity, a SATA SSD, and an
// NVMe disk, so every criterion has a distinguishing and a shared value.
func selectorFacts() HardwareFacts {
	return HardwareFacts{BlockDevices: []BlockDevice{
		{
			Name: "vda", Path: "/dev/vda", Type: DeviceDisk, SizeBytes: 64 * gib, Rotational: true, DiskSeq: 1,
			ByPath:             []string{"/dev/disk/by-path/pci-0000:00:04.0"},
			PartitionSignature: "gpt", PartitionTableUUID: "6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b",
			Partitions: []BlockDevice{{Name: "vda1", Path: "/dev/vda1", Type: DevicePartition, ByPath: []string{"/dev/disk/by-path/pci-0000:00:04.0-part1"}}},
		},
		{
			Name: "sda", Path: "/dev/sda", Type: DeviceDisk, SizeBytes: 500 * 1000 * 1000 * 1000, Transport: "sata", DiskSeq: 2,
			ByID: []string{"/dev/disk/by-id/ata-Samsung_SSD_870_S1"}, Serial: "S1", WWN: "0x5002538e0000001", Model: "Samsung SSD 870",
		},
		{
			Name: "nvme0n1", Path: "/dev/nvme0n1", Type: DeviceDisk, SizeBytes: 64 * gib, Transport: "nvme", DiskSeq: 3,
			ByID: []string{"/dev/disk/by-id/nvme-KATL_N1"}, Serial: "N1", Model: "KATL NVMe",
		},
		{Name: "loop0", Path: "/dev/loop0", Type: "loop", SizeBytes: 64 * gib},
	}}
}

func TestMatchDiskIdentitySelectsExactlyOneDisk(t *testing.T) {
	tests := []struct {
		name     string
		selector TargetDiskSelector
		want     string
	}{
		{"by-path without hardware identity", TargetDiskSelector{ByPath: "/dev/disk/by-path/pci-0000:00:04.0"}, "/dev/vda"},
		{"gpt guid ignores case", TargetDiskSelector{GPTGUID: "6F1E3C2A-8D4B-4F5E-9A7C-1B2D3E4F5A6B"}, "/dev/vda"},
		{"kernel name", TargetDiskSelector{Name: "/dev/sda"}, "/dev/sda"},
		{"minimum size", TargetDiskSelector{Size: ">= 400GB"}, "/dev/sda"},
		{"model glob", TargetDiskSelector{Model: "Samsung*"}, "/dev/sda"},
		{"type", TargetDiskSelector{Type: DiskTypeNVMe}, "/dev/nvme0n1"},
		{"criteria narrow each other", TargetDiskSelector{Size: "64GiB", Type: DiskTypeHDD}, "/dev/vda"},
		{"stable identity", TargetDiskSelector{Serial: "N1"}, "/dev/nvme0n1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchDiskIdentity(selectorFacts(), tt.selector)
			if err != nil {
				t.Fatalf("MatchDiskIdentity() error = %v", err)
			}
			if got.Path != tt.want {
				t.Fatalf("MatchDiskIdentity() = %s, want %s", got.Path, tt.want)
			}
		})
	}
}

func TestMatchDiskIdentityReportsCandidates(t *testing.T) {
	tests := []struct {
		name     string
		selector TargetDiskSelector
		want     []string
		absent   []string
	}{
		{
			name:     "ambiguous size lists only matching disks",
			selector: TargetDiskSelector{Size: "64GiB"},
			want:     []string{"matched 2 whole disks", "/dev/vda (", "byPath=/dev/disk/by-path/pci-0000:00:04.0", "/dev/nvme0n1 (", "serial=N1"},
			absent:   []string{"/dev/sda (", "/dev/loop0"},
		},
		{
			name:     "criteria conflict lists every disk",
			selector: TargetDiskSelector{Name: "/dev/sda", Type: DiskTypeNVMe},
			want:     []string{"matched no whole disk", "/dev/vda (", "/dev/sda (", "/dev/nvme0n1 ("},
		},
		{
			name:     "stale gpt guid explains repartitioning",
			selector: TargetDiskSelector{GPTGUID: "00000000-0000-4000-8000-000000000000"},
			want:     []string{"matched no whole disk", "repartitioned", "gptGUID=6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MatchDiskIdentity(selectorFacts(), tt.selector)
			if err == nil {
				t.Fatal("MatchDiskIdentity() error = nil, want failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("error %q contains %q", err, absent)
				}
			}
		})
	}
}

func TestMatchDiskIdentityCountsUnsafeDisks(t *testing.T) {
	facts := selectorFacts()
	facts.Mounts = []MountFact{{Source: "/dev/nvme0n1", Target: "/run/media"}}
	facts.BlockDevices[2].Mountpoints = []string{"/run/media"}

	_, err := MatchTargetDisk(facts, TargetDiskSelector{Size: "64GiB"})
	if err == nil || !strings.Contains(err.Error(), "matched 2 whole disks") {
		t.Fatalf("MatchTargetDisk() error = %v, want ambiguity that includes the mounted disk", err)
	}
}

func TestMatchDiskIdentityRejectsNonDisks(t *testing.T) {
	tests := []struct {
		name     string
		selector TargetDiskSelector
	}{
		{"partition by name", TargetDiskSelector{Name: "/dev/vda1"}},
		{"partition by path", TargetDiskSelector{ByPath: "/dev/disk/by-path/pci-0000:00:04.0-part1"}},
		{"loop device", TargetDiskSelector{Name: "/dev/loop0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MatchDiskIdentity(selectorFacts(), tt.selector)
			if !errors.Is(err, ErrUnsafeTargetDisk) || !strings.Contains(err.Error(), "not a whole disk") {
				t.Fatalf("MatchDiskIdentity() error = %v, want not-a-whole-disk refusal", err)
			}
		})
	}
}

func TestTargetDiskSelectorValidate(t *testing.T) {
	tests := []struct {
		name     string
		selector TargetDiskSelector
		want     string
	}{
		{"no criteria", TargetDiskSelector{MinSizeMiB: 1024}, "at least one"},
		{"by-id outside by-id", TargetDiskSelector{ByID: "/dev/sda"}, "byID"},
		{"by-path outside by-path", TargetDiskSelector{ByPath: "/dev/disk/by-id/x"}, "byPath"},
		{"name is an alias", TargetDiskSelector{Name: "/dev/disk/by-id/x"}, "kernel device path"},
		{"name without dev", TargetDiskSelector{Name: "sda"}, "kernel device path"},
		{"gpt guid malformed", TargetDiskSelector{GPTGUID: "not-a-guid"}, "gptGUID"},
		{"type unknown", TargetDiskSelector{Type: "floppy"}, "type"},
		{"size unparseable", TargetDiskSelector{Size: "big"}, "size"},
		{"serial padded", TargetDiskSelector{Serial: " S1"}, "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.selector.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestParseSizeMatch(t *testing.T) {
	tests := []struct {
		value string
		want  SizeMatch
	}{
		{">= 500GB", SizeMatch{Op: ">=", Bytes: 500_000_000_000}},
		{"<=2TiB", SizeMatch{Op: "<=", Bytes: 2 << 40}},
		{"> 1.5tb", SizeMatch{Op: ">", Bytes: 1_500_000_000_000}},
		{"64GiB", SizeMatch{Op: "==", Bytes: 64 << 30}},
		{"== 4096B", SizeMatch{Op: "==", Bytes: 4096}},
	}
	for _, tt := range tests {
		got, err := ParseSizeMatch(tt.value)
		if err != nil {
			t.Errorf("ParseSizeMatch(%q) error = %v", tt.value, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSizeMatch(%q) = %+v, want %+v", tt.value, got, tt.want)
		}
	}
	for _, value := range []string{"", ">=", "500 parsecs", "=> 1GB", "-1GB"} {
		if _, err := ParseSizeMatch(value); err == nil {
			t.Errorf("ParseSizeMatch(%q) error = nil, want failure", value)
		}
	}
}

// A bare number would otherwise be read as bytes, so ">= 500" would match
// every disk.
func TestParseSizeMatchRequiresUnit(t *testing.T) {
	for _, value := range []string{">= 500", "64", " < 2.5 "} {
		_, err := ParseSizeMatch(value)
		if err == nil || !strings.Contains(err.Error(), "has no unit") || !strings.Contains(err.Error(), strings.TrimSpace(value)+`GB"`) {
			t.Errorf("ParseSizeMatch(%q) error = %v, want a missing-unit error suggesting a unit", value, err)
		}
	}
}

func TestMatchRetainedDisk(t *testing.T) {
	selector := TargetDiskSelector{GPTGUID: "6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b", Type: DiskTypeHDD}
	retained := IdentityOf(selectorFacts().BlockDevices[0])

	t.Run("repartitioned disk keeps its binding", func(t *testing.T) {
		facts := selectorFacts()
		facts.BlockDevices[0].PartitionTableUUID = "11111111-2222-4333-8444-555555555555"
		match, err := MatchRetainedDisk(facts, retained, selector)
		if err != nil || match.Device.Path != "/dev/vda" {
			t.Fatalf("MatchRetainedDisk() = %s, %v; want /dev/vda", match.Device.Path, err)
		}
	})

	changed := []struct {
		name   string
		mutate func(*HardwareFacts)
		target error
	}{
		{"disk removed", func(f *HardwareFacts) { f.BlockDevices = f.BlockDevices[1:] }, ErrTargetDiskChanged},
		{"disk replaced under the same name", func(f *HardwareFacts) { f.BlockDevices[0].DiskSeq = 9 }, ErrTargetDiskChanged},
		{"disk resized", func(f *HardwareFacts) { f.BlockDevices[0].SizeBytes = 128 * gib }, ErrTargetDiskChanged},
		{"disk no longer matches unchanging criteria", func(f *HardwareFacts) { f.BlockDevices[0].Rotational = false }, ErrTargetDiskChanged},
		{"disk mounted since selection", func(f *HardwareFacts) {
			f.BlockDevices[0].Partitions[0].Mountpoints = []string{"/mnt"}
		}, ErrUnsafeTargetDisk},
	}
	for _, tt := range changed {
		t.Run(tt.name, func(t *testing.T) {
			facts := selectorFacts()
			tt.mutate(&facts)
			if _, err := MatchRetainedDisk(facts, retained, selector); !errors.Is(err, tt.target) {
				t.Fatalf("MatchRetainedDisk() error = %v, want %v", err, tt.target)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tt := range []struct {
		pattern, value string
		want           bool
	}{
		{"WD*", "WD/Blue SN570", true},
		{"*SSD*", "Samsung SSD 870", true},
		{"Samsung SSD 87?", "Samsung SSD 870", true},
		{"samsung*", "Samsung SSD 870", false},
		{"[x]", "[x]", true},
		{`a\*`, `a\b`, true},
		{"KATL", "KATL NVMe", false},
	} {
		if got := globMatch(tt.pattern, tt.value); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}
