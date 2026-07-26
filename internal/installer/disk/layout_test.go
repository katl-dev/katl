package disk

import (
	"strings"
	"testing"
)

func TestPlanDiskLayoutDefaultRootSlotsStateRemainingLabelsAndBootMetadata(t *testing.T) {
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768),
	)

	plan, err := PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk:         TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:              RootSlotRequest{SizeMiB: 4096},
		RootB:              RootSlotRequest{SizeMiB: 4096},
		State:              StatePartitionRequest{Filesystem: "ext4", MinSizeMiB: 8192},
		RuntimeRootSizeMiB: 2048,
	})
	if err != nil {
		t.Fatalf("PlanDiskLayout() error = %v", err)
	}

	assertPartition(t, plan, "esp", GPTLabelESP, 512, false)
	assertPartition(t, plan, "root-a", GPTLabelRootA, 4096, false)
	assertPartition(t, plan, "root-b", GPTLabelRootB, 4096, false)
	state := assertPartition(t, plan, "state", GPTLabelState, 24064, true)
	if state.MountPath != "/var" {
		t.Fatalf("state mount = %q, want /var", state.MountPath)
	}

	if plan.Boot.RootSlot != RootSlotA {
		t.Fatalf("boot root slot = %q, want root-a", plan.Boot.RootSlot)
	}
	if plan.Boot.RootPartitionLabel != GPTLabelRootA {
		t.Fatalf("boot root label = %q, want %s", plan.Boot.RootPartitionLabel, GPTLabelRootA)
	}
	if plan.Boot.RootParameter != "root=PARTUUID=${KATL_ROOT_A_PARTUUID}" {
		t.Fatalf("boot root parameter = %q", plan.Boot.RootParameter)
	}
}

func TestPlanDiskLayoutOptionalXBOOTLDREtcdAndRootBInitialBoot(t *testing.T) {
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 65536),
	)

	plan, err := PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk:      TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		XBOOTLDRSizeMiB: 1024,
		RootA:           RootSlotRequest{SizeMiB: 8192},
		RootB:           RootSlotRequest{SizeMiB: 8192},
		State:           StatePartitionRequest{Filesystem: "xfs", MinSizeMiB: 8192},
		Etcd:            &FixedPartitionRequest{Filesystem: "ext4", SizeMiB: 16384},
		InitialRootSlot: RootSlotB,
	})
	if err != nil {
		t.Fatalf("PlanDiskLayout() error = %v", err)
	}

	assertPartition(t, plan, "xbootldr", GPTLabelXBOOTLDR, 1024, false)
	assertPartition(t, plan, "etcd", GPTLabelEtcd, 16384, false)
	state := assertPartition(t, plan, "state", GPTLabelState, 31232, true)
	if state.Filesystem != "xfs" {
		t.Fatalf("state filesystem = %q, want xfs", state.Filesystem)
	}
	if plan.Boot.RootPartitionLabel != GPTLabelRootB {
		t.Fatalf("boot root label = %q, want %s", plan.Boot.RootPartitionLabel, GPTLabelRootB)
	}
	if plan.Boot.RootParameter != "root=PARTUUID=${KATL_ROOT_B_PARTUUID}" {
		t.Fatalf("boot root parameter = %q", plan.Boot.RootParameter)
	}
}

func TestPlanDiskLayoutRejectsInvalidRootSizingAndSmallDisk(t *testing.T) {
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 12288),
	)

	_, err := PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk:         TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:              RootSlotRequest{SizeMiB: 1024},
		RootB:              RootSlotRequest{SizeMiB: 1024},
		State:              StatePartitionRequest{MinSizeMiB: 1024},
		RuntimeRootSizeMiB: 2048,
	})
	if err == nil || !strings.Contains(err.Error(), "runtime root artifact") {
		t.Fatalf("PlanDiskLayout() error = %v, want runtime root sizing failure", err)
	}

	_, err = PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk: TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:      RootSlotRequest{SizeMiB: 4096},
		RootB:      RootSlotRequest{SizeMiB: 4096},
		State:      StatePartitionRequest{MinSizeMiB: 8192},
	})
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("PlanDiskLayout() error = %v, want disk-too-small failure", err)
	}
}

func TestPlanDiskLayoutExtraDiskMountRequests(t *testing.T) {
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768),
		BlockDevice{
			Path:               "/dev/sdb",
			Type:               DeviceDisk,
			ByID:               []string{"/dev/disk/by-id/ata-data"},
			SizeBytes:          128 * 1024 * 1024 * 1024,
			PartitionSignature: "gpt",
		},
	)

	plan, err := PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk: TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:      RootSlotRequest{SizeMiB: 4096},
		RootB:      RootSlotRequest{SizeMiB: 4096},
		State:      StatePartitionRequest{MinSizeMiB: 8192},
		ExtraDisks: []ExtraDiskRequest{
			{
				Name:       "data",
				Selector:   TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data"},
				Filesystem: "xfs",
				Wipe:       true,
			},
		},
	})
	if err != nil {
		t.Fatalf("PlanDiskLayout() error = %v", err)
	}

	if len(plan.ExtraMounts) != 1 {
		t.Fatalf("extra mount count = %d, want 1", len(plan.ExtraMounts))
	}
	extra := plan.ExtraMounts[0]
	if extra.DevicePath != "/dev/sdb" || extra.MountSource != "/dev/disk/by-id/ata-data" || extra.MountPath != "/var/lib/katl/mnt/data" || extra.Filesystem != "xfs" || !extra.Wipe {
		t.Fatalf("extra mount = %#v", extra)
	}
	if len(extra.Signatures) != 1 || extra.Signatures[0].Value != "gpt" {
		t.Fatalf("extra signatures = %#v, want gpt signature", extra.Signatures)
	}
}

func TestPlanDiskLayoutRejectsDuplicateExtraDiskNames(t *testing.T) {
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768),
		diskForLayout("/dev/sdb", "/dev/disk/by-id/ata-data-a", 16384),
		diskForLayout("/dev/sdc", "/dev/disk/by-id/ata-data-b", 16384),
	)

	_, err := PlanDiskLayout(facts, DiskLayoutRequest{
		TargetDisk: TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:      RootSlotRequest{SizeMiB: 4096},
		RootB:      RootSlotRequest{SizeMiB: 4096},
		State:      StatePartitionRequest{MinSizeMiB: 8192},
		ExtraDisks: []ExtraDiskRequest{
			{Name: "data", Selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data-a"}, Wipe: true},
			{Name: "data", Selector: TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data-b"}, Wipe: true},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("PlanDiskLayout() error = %v, want duplicate name failure", err)
	}
}

func TestPlanDiskLayoutReusesOnlyMatchingExtraDiskFilesystem(t *testing.T) {
	extra := diskForLayout("/dev/sdb", "/dev/disk/by-id/ata-data", 16384)
	extra.FilesystemSignature = "ext4"
	facts := layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768),
		extra,
	)
	request := DiskLayoutRequest{
		TargetDisk: TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:      RootSlotRequest{SizeMiB: 4096},
		RootB:      RootSlotRequest{SizeMiB: 4096},
		State:      StatePartitionRequest{MinSizeMiB: 8192},
		ExtraDisks: []ExtraDiskRequest{{
			Name:       "data",
			Selector:   TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data"},
			Filesystem: "ext4",
		}},
	}

	if _, err := PlanDiskLayout(facts, request); err != nil {
		t.Fatalf("PlanDiskLayout() error = %v", err)
	}

	request.ExtraDisks[0].Filesystem = "xfs"
	if _, err := PlanDiskLayout(facts, request); err == nil || !strings.Contains(err.Error(), "set wipe to true") {
		t.Fatalf("PlanDiskLayout() error = %v, want safe reuse refusal", err)
	}

	facts.BlockDevices[1].FilesystemSignature = ""
	request.ExtraDisks[0].Filesystem = "ext4"
	if _, err := PlanDiskLayout(facts, request); err == nil || !strings.Contains(err.Error(), "set wipe to true") {
		t.Fatalf("PlanDiskLayout() error = %v, want blank disk refusal", err)
	}
}

func assertPartition(t *testing.T, plan DiskLayoutPlan, name, label string, sizeMiB uint64, remaining bool) PartitionPlan {
	t.Helper()

	for _, partition := range plan.Partitions {
		if partition.Name != name {
			continue
		}
		if partition.GPTLabel != label {
			t.Fatalf("%s GPT label = %q, want %q", name, partition.GPTLabel, label)
		}
		if partition.SizeMiB != sizeMiB {
			t.Fatalf("%s size = %d MiB, want %d", name, partition.SizeMiB, sizeMiB)
		}
		if partition.Remaining != remaining {
			t.Fatalf("%s remaining = %t, want %t", name, partition.Remaining, remaining)
		}
		return partition
	}

	t.Fatalf("partition %q not found in %#v", name, plan.Partitions)
	return PartitionPlan{}
}

func diskForLayout(devicePath, byID string, sizeMiB uint64) BlockDevice {
	return BlockDevice{
		Path:      devicePath,
		Type:      DeviceDisk,
		ByID:      []string{byID},
		SizeBytes: sizeMiB * 1024 * 1024,
	}
}

func layoutFacts(devices ...BlockDevice) HardwareFacts {
	return HardwareFacts{BlockDevices: devices}
}
