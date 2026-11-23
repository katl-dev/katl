package disk

import (
	"errors"
	"testing"
)

func TestMatchVolumeBinding(t *testing.T) {
	root := diskForLayout("/dev/vda", "/dev/disk/by-id/virtio-root", 32768)
	root.Partitions = []BlockDevice{
		{Path: "/dev/vda5", Type: DevicePartition, PartitionNumber: 5, PartitionUUID: "state-part", FilesystemUUID: "state-fs", GPTLabel: GPTLabelState},
	}
	volumeDisk := func(path, partUUID, fsUUID string) BlockDevice {
		disk := diskForLayout(path, "/dev/disk/by-id/virtio-"+path[len("/dev/"):], 4096)
		disk.Partitions = []BlockDevice{{Path: path + "1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: partUUID, FilesystemUUID: fsUUID, GPTLabel: "u-data"}}
		return disk
	}
	bound := VolumeBinding{Name: "data", PartitionUUID: "data-part", FilesystemUUID: "data-fs"}

	for _, test := range []struct {
		name    string
		disks   []BlockDevice
		binding VolumeBinding
		want    string
		fault   BindingFault
		mention []string
	}{
		{name: "bound partition", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "data-fs")}, binding: bound, want: "/dev/vdb1"},
		// A dd clone carries both identifiers, so with the original detached it
		// is the volume.
		{name: "clone replacing a detached original", disks: []BlockDevice{volumeDisk("/dev/vdc", "data-part", "data-fs")}, binding: bound, want: "/dev/vdc1"},
		{name: "binding without a filesystem UUID", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "data-fs")}, binding: VolumeBinding{Name: "data", PartitionUUID: "data-part"}, want: "/dev/vdb1"},
		{name: "PARTUUID on two disks", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "data-fs"), volumeDisk("/dev/vdc", "data-part", "data-fs")}, binding: bound, fault: BindingDuplicated, mention: []string{"data-part", "/dev/vdb1", "/dev/vdc1"}},
		// A crash after mkfs leaves the PARTUUID in place under a new filesystem.
		{name: "filesystem recreated", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "new-fs")}, binding: bound, fault: BindingStale, mention: []string{"data-fs", "new-fs", "/dev/vdb1"}},
		{name: "filesystem removed", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "")}, binding: bound, fault: BindingStale, mention: []string{"data-fs", "none"}},
		{name: "partition on the system disk", binding: VolumeBinding{Name: "data", PartitionUUID: "state-part", FilesystemUUID: "state-fs"}, fault: BindingOnSystemDisk, mention: []string{"/dev/vda5", "system disk /dev/vda"}},
		{name: "partition detached", binding: bound, fault: BindingAbsent, mention: []string{"data-part"}},
		{name: "binding by filesystem UUID only", disks: []BlockDevice{volumeDisk("/dev/vdb", "data-part", "data-fs")}, binding: VolumeBinding{Name: "data", FilesystemUUID: "data-fs"}, fault: BindingUnrecorded},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := layoutFacts(append([]BlockDevice{root}, test.disks...)...)

			partition, err := MatchVolumeBinding(facts, root, test.binding)

			if test.fault == "" {
				if err != nil || partition.Path != test.want {
					t.Fatalf("MatchVolumeBinding() = %s, %v; want %s", partition.Path, err, test.want)
				}
				return
			}
			var bindingErr *VolumeBindingError
			if !errors.As(err, &bindingErr) || bindingErr.Fault != test.fault || bindingErr.Volume != "data" {
				t.Fatalf("MatchVolumeBinding() error = %#v, want %s fault for data", err, test.fault)
			}
			if !mentions(err, test.mention...) {
				t.Fatalf("MatchVolumeBinding() error = %v, want it to name %q", err, test.mention)
			}
		})
	}
}
