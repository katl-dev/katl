package disk

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

var (
	volumeDisk            = discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 3, SizeBytes: 8 << 30}
	dataPartitionIdentity = PartitionIdentity{Number: 1, UUID: "data-part", SizeBytes: 1 << 30}
)

func repartitionVolume() VolumePlan {
	return VolumePlan{Name: "data", TargetKind: "disk", Disk: volumeDisk, Filesystem: "xfs", MountPath: "/var/mnt/data", Wipe: true, Repartition: true}
}

func formatVolume() VolumePlan {
	return VolumePlan{Name: "data", TargetKind: "partition", Disk: volumeDisk, Partition: dataPartitionIdentity, Filesystem: "xfs", MountPath: "/var/mnt/data", Wipe: true}
}

func volumeDiskWith(partitions ...BlockDevice) BlockDevice {
	return BlockDevice{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 3, ByDiskSeq: "/dev/disk/by-diskseq/3", SizeBytes: 8 << 30, Partitions: partitions}
}

func dataPartition() BlockDevice {
	return BlockDevice{Path: "/dev/vdb1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", SizeBytes: 1 << 30}
}

// destructiveCalls lists the commands that write a disk.
func destructiveCalls(calls []CommandCall) []string {
	var names []string
	for _, call := range calls {
		if call.Name == "wipefs" || call.Name == "systemd-repart" || call.Name == "env" || strings.HasPrefix(call.Name, "mkfs.") {
			names = append(names, call.Name)
		}
	}
	return names
}

// mentions reports whether err names every identifying part, such as a device
// path or identity, without depending on how the message is worded.
func mentions(err error, parts ...string) bool {
	if err == nil {
		return false
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			return false
		}
	}
	return true
}

func TestPrepareVolumeRefusesChangedTarget(t *testing.T) {
	for _, test := range []struct {
		name  string
		plan  VolumePlan
		disks []BlockDevice
		want  []string
	}{
		{
			// The planned disk is now vdc; the disk that took its kernel name
			// has a different diskseq.
			name: "disk renamed before repartitioning",
			plan: repartitionVolume(),
			disks: []BlockDevice{
				{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 4, ByDiskSeq: "/dev/disk/by-diskseq/4", SizeBytes: 8 << 30},
				{Path: "/dev/vdc", Type: DeviceDisk, DiskSeq: 3, ByDiskSeq: "/dev/disk/by-diskseq/3", SizeBytes: 8 << 30},
			},
			want: []string{"/dev/vdb", "diskseq=4", "diskseq=3"},
		},
		{
			name: "disk detached before repartitioning",
			plan: repartitionVolume(),
			want: []string{"/dev/vdb", "no longer present"},
		},
		{
			name:  "disk mounted before repartitioning",
			plan:  repartitionVolume(),
			disks: []BlockDevice{volumeDiskWith(BlockDevice{Path: "/dev/vdb1", Type: DevicePartition, Mountpoints: []string{"/srv"}})},
			want:  []string{"/dev/vdb", "mounted"},
		},
		{
			name:  "disk renamed before formatting a partition",
			plan:  formatVolume(),
			disks: []BlockDevice{{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 4, ByDiskSeq: "/dev/disk/by-diskseq/4", SizeBytes: 8 << 30, Partitions: []BlockDevice{dataPartition()}}},
			want:  []string{"/dev/vdb", "diskseq=4"},
		},
		{
			// A clone shares the PARTUUID, so the partition's identity is
			// ambiguous.
			name: "partition PARTUUID shared with another disk",
			plan: formatVolume(),
			disks: []BlockDevice{
				volumeDiskWith(dataPartition()),
				{Path: "/dev/vdc", Type: DeviceDisk, DiskSeq: 5, ByDiskSeq: "/dev/disk/by-diskseq/5", SizeBytes: 8 << 30, Partitions: []BlockDevice{
					{Path: "/dev/vdc1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part"},
				}},
			},
			want: []string{"data-part", "/dev/vdb1", "/dev/vdc1"},
		},
		{
			name: "partition mounted before formatting",
			plan: formatVolume(),
			disks: []BlockDevice{volumeDiskWith(BlockDevice{
				Path: "/dev/vdb1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", SizeBytes: 1 << 30, Mountpoints: []string{"/srv"},
			})},
			want: []string{"/dev/vdb1", "mounted"},
		},
		{
			// The partition table was rewritten and partition 1 is now a
			// Katl partition.
			name: "partition number now holds a Katl partition",
			plan: formatVolume(),
			disks: []BlockDevice{volumeDiskWith(BlockDevice{
				Path: "/dev/vdb1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", SizeBytes: 1 << 30, GPTLabel: GPTLabelState,
			})},
			want: []string{"/dev/vdb1", "KATL_STATE"},
		},
		{
			name: "partition number now holds a different partition",
			plan: formatVolume(),
			disks: []BlockDevice{volumeDiskWith(BlockDevice{
				Path: "/dev/vdb1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", SizeBytes: 2 << 30,
			})},
			want: []string{"partition 1", "/dev/vdb", "size=2147483648"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &NoopCommandRunner{}
			source := discovery.StaticDiscoverySource{Facts: HardwareFacts{BlockDevices: test.disks}}

			_, err := PrepareVolumes(context.Background(), commands, source, []VolumePlan{test.plan})

			if !errors.Is(err, ErrVolumeRefused) || !mentions(err, test.want...) {
				t.Fatalf("PrepareVolumes() error = %v, want ErrVolumeRefused naming %q", err, test.want)
			}
			if got := destructiveCalls(commands.Calls); len(got) != 0 {
				t.Fatalf("destructive commands = %v, want none", got)
			}
		})
	}
}

// changingDiscovery reports the volume disk unchanged until the command runner
// has run wipefs, then reports another disk under its kernel name.
type changingDiscovery struct {
	commands *NoopCommandRunner
}

func (d changingDiscovery) Discover(context.Context) (HardwareFacts, error) {
	if countCalls(d.commands.Calls, "wipefs") > 0 {
		return HardwareFacts{BlockDevices: []BlockDevice{{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 9, ByDiskSeq: "/dev/disk/by-diskseq/9", SizeBytes: 8 << 30}}}, nil
	}
	return HardwareFacts{BlockDevices: []BlockDevice{volumeDiskWith(dataPartition())}}, nil
}

func TestPrepareVolumeRechecksBeforeEachCommand(t *testing.T) {
	commands := &NoopCommandRunner{}

	_, err := PrepareVolumes(context.Background(), commands, changingDiscovery{commands: commands}, []VolumePlan{formatVolume()})

	if !errors.Is(err, discovery.ErrTargetDiskChanged) {
		t.Fatalf("PrepareVolumes() error = %v, want ErrTargetDiskChanged", err)
	}
	if got := destructiveCalls(commands.Calls); !reflect.DeepEqual(got, []string{"wipefs"}) {
		t.Fatalf("destructive commands = %v, want wipefs only: mkfs must recheck the disk after wipefs", got)
	}
}

func TestPrepareVolumeAddressesByDiskSequence(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	for _, test := range []struct {
		name    string
		plan    VolumePlan
		command string
		want    string
	}{
		{"repartitioned disk", repartitionVolume(), "systemd-repart", "/dev/disk/by-diskseq/3"},
		{"selected partition", formatVolume(), "wipefs", "/dev/disk/by-diskseq/3-part1"},
		{"selected partition format", formatVolume(), "mkfs.xfs", "/dev/disk/by-diskseq/3-part1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := discovery.StaticDiscoverySource{Facts: HardwareFacts{BlockDevices: []BlockDevice{volumeDiskWith(dataPartition())}}}
			if test.plan.Repartition {
				source.Facts.BlockDevices[0].Partitions = nil
			}
			commands := &NoopCommandRunner{}

			if _, err := PrepareVolumes(context.Background(), commands, source, []VolumePlan{test.plan}); err != nil {
				t.Fatalf("PrepareVolumes() error = %v", err)
			}

			call := findCall(commands.Calls, test.command)
			if len(call.Args) == 0 || call.Args[len(call.Args)-1] != test.want {
				t.Fatalf("%s args = %q, want target %s", test.command, call.Args, test.want)
			}
		})
	}
}

func TestDiskWithoutDiskSequenceIsRefused(t *testing.T) {
	unsequenced := discovery.DiskIdentity{Path: "/dev/vdb", SizeBytes: 8 << 30}
	want := []string{"/dev/vdb", "diskseq"}

	t.Run("volume", func(t *testing.T) {
		plan := formatVolume()
		plan.Disk = unsequenced
		commands := &NoopCommandRunner{}

		_, err := PrepareVolumes(context.Background(), commands, discovery.StaticDiscoverySource{}, []VolumePlan{plan})

		if !mentions(err, want...) {
			t.Fatalf("PrepareVolumes() error = %v, want %q", err, want)
		}
		if len(commands.Calls) != 0 {
			t.Fatalf("commands = %+v, want none", commands.Calls)
		}
	})
	t.Run("system disk", func(t *testing.T) {
		plan := executorPlan()
		plan.TargetDisk = unsequenced

		_, err := BuildDiskOperations(plan, "")

		if !mentions(err, want...) {
			t.Fatalf("BuildDiskOperations() error = %v, want %q", err, want)
		}
	})
}

func TestBindVolumePlans(t *testing.T) {
	dataDisk := discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 2}
	repartitioned := VolumePlan{Name: "data", Disk: dataDisk, Repartition: true}
	selected := VolumePlan{Name: "data", Disk: dataDisk, Partition: PartitionIdentity{Number: 3, SizeBytes: 1 << 30}}
	// An earlier install's volume disk is enumerated before the bound disk
	// and carries the same u-data label.
	oldDisk := BlockDevice{Path: "/dev/vda", Type: DeviceDisk, DiskSeq: 1, ByDiskSeq: "/dev/disk/by-diskseq/1", Partitions: []BlockDevice{
		{Path: "/dev/vda1", Type: DevicePartition, PartitionNumber: 1, GPTLabel: "u-data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"},
	}}
	boundDisk := func(partitions ...BlockDevice) BlockDevice {
		return BlockDevice{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 2, ByDiskSeq: "/dev/disk/by-diskseq/2", Partitions: partitions}
	}

	for _, test := range []struct {
		name    string
		plan    VolumePlan
		disks   []BlockDevice
		source  string
		binding VolumeBinding
		err     []string
	}{
		{
			name:    "repartitioned disk ignores label on another disk",
			plan:    repartitioned,
			disks:   []BlockDevice{oldDisk, boundDisk(BlockDevice{Path: "/dev/vdb1", Type: DevicePartition, PartitionNumber: 1, GPTLabel: "u-data", PartitionUUID: "New-Part", FilesystemUUID: "new-fs"})},
			source:  "PARTUUID=new-part",
			binding: VolumeBinding{Name: "data", PartitionUUID: "new-part", FilesystemUUID: "new-fs"},
		},
		{
			name:  "repartitioned disk without its partition does not fall back",
			plan:  repartitioned,
			disks: []BlockDevice{oldDisk, boundDisk()},
			err:   []string{"/dev/vdb", "u-data"},
		},
		{
			name: "disk renamed before binding",
			plan: repartitioned,
			disks: []BlockDevice{{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 7, ByDiskSeq: "/dev/disk/by-diskseq/7", Partitions: []BlockDevice{
				{Path: "/dev/vdb1", Type: DevicePartition, GPTLabel: "u-data", PartitionUUID: "other-part"},
			}}},
			err: []string{"/dev/vdb", "diskseq=7"},
		},
		{
			name:  "PARTUUID shared with another disk",
			plan:  repartitioned,
			disks: []BlockDevice{oldDisk, boundDisk(BlockDevice{Path: "/dev/vdb1", Type: DevicePartition, GPTLabel: "u-data", PartitionUUID: "old-part"})},
			err:   []string{"old-part", "/dev/vdb1", "/dev/vda1"},
		},
		{
			name:    "selected partition without PARTUUID binds by filesystem UUID",
			plan:    selected,
			disks:   []BlockDevice{oldDisk, boundDisk(BlockDevice{Path: "/dev/vdb3", Type: DevicePartition, PartitionNumber: 3, SizeBytes: 1 << 30, FilesystemUUID: "data-fs"})},
			source:  "UUID=data-fs",
			binding: VolumeBinding{Name: "data", FilesystemUUID: "data-fs"},
		},
		{
			name:  "filesystem UUID shared with another disk",
			plan:  selected,
			disks: []BlockDevice{oldDisk, boundDisk(BlockDevice{Path: "/dev/vdb3", Type: DevicePartition, PartitionNumber: 3, SizeBytes: 1 << 30, FilesystemUUID: "old-fs"})},
			err:   []string{"old-fs", "/dev/vdb3", "/dev/vda1"},
		},
		{
			// Without a PARTUUID the partition is found by number, so a
			// rewritten table must not bind whatever now has that number.
			name:  "selected partition number reused by another partition",
			plan:  selected,
			disks: []BlockDevice{boundDisk(BlockDevice{Path: "/dev/vdb3", Type: DevicePartition, PartitionNumber: 3, SizeBytes: 4 << 30, FilesystemUUID: "data-fs"})},
			err:   []string{"partition 3", "/dev/vdb", "size=4294967296"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := HardwareFacts{BlockDevices: test.disks}

			plans, bindings, err := BindVolumePlans(facts, []VolumePlan{test.plan})

			if test.err != nil {
				if !mentions(err, test.err...) {
					t.Fatalf("BindVolumePlans() error = %v, want %q", err, test.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BindVolumePlans() error = %v", err)
			}
			if got := plans[0].MountSource; got != test.source {
				t.Errorf("mount source = %q, want %q", got, test.source)
			}
			if bindings[0] != test.binding {
				t.Errorf("binding = %+v, want %+v", bindings[0], test.binding)
			}
		})
	}
}

func TestDiskExecutorRefusesUnboundVolumeMount(t *testing.T) {
	plan := executorPlan()
	plan.VolumeMounts[0].MountSource = ""
	commands := &NoopCommandRunner{}

	_, err := (DiskExecutor{Commands: commands, Discovery: executorDiscovery()}).ExecuteGroup(context.Background(), DiskExecutionRequest{Plan: plan, AllowDestructive: true}, MountOperations)

	if err == nil || !strings.Contains(err.Error(), `volume "data" is not bound`) {
		t.Fatalf("ExecuteGroup(mount) error = %v, want unbound volume failure", err)
	}
	for _, call := range commands.Calls {
		if call.Name == "mount" && strings.Contains(strings.Join(call.Args, " "), "/var/mnt/data") {
			t.Fatalf("unbound volume was mounted: %+v", call)
		}
	}
}

func TestDiskExecutorRechecksVolumeDisksBeforeWipingSystemDisk(t *testing.T) {
	plan := executorPlan()
	source := discovery.StaticDiscoverySource{Facts: HardwareFacts{BlockDevices: []BlockDevice{
		{Path: "/dev/sdb", Type: DeviceDisk, DiskSeq: 9, ByDiskSeq: "/dev/disk/by-diskseq/9", SizeBytes: 16 << 30},
	}}}
	commands := &NoopCommandRunner{}

	_, err := (DiskExecutor{Commands: commands, Discovery: source}).ExecuteGroup(context.Background(), DiskExecutionRequest{Plan: plan, AllowDestructive: true}, PrepareOperations)

	if !errors.Is(err, ErrVolumeRefused) || !errors.Is(err, discovery.ErrTargetDiskChanged) {
		t.Fatalf("ExecuteGroup(prepare) error = %v, want ErrVolumeRefused and ErrTargetDiskChanged", err)
	}
	if got := destructiveCalls(commands.Calls); len(got) != 0 {
		t.Fatalf("destructive commands = %v, want none on the system disk while a volume disk changed", got)
	}
}

func TestPlanVolumesRefusesSharedTargets(t *testing.T) {
	data := diskForLayout("/dev/sdb", "/dev/disk/by-id/ata-data", 16384)
	data.Partitions = []BlockDevice{
		{Path: "/dev/sdb1", Type: DevicePartition, PartitionNumber: 1, ByID: []string{"/dev/disk/by-id/ata-data-part1"}, PartitionUUID: "part-1", FilesystemSignature: "xfs"},
		{Path: "/dev/sdb2", Type: DevicePartition, PartitionNumber: 2, ByID: []string{"/dev/disk/by-id/ata-data-part2"}, PartitionUUID: "part-2", FilesystemSignature: "xfs"},
	}
	root := diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768)
	facts := layoutFacts(root, data)
	wholeDisk := func(name string) VolumeRequest {
		return VolumeRequest{Name: name, Disk: &TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data"}, Filesystem: "xfs", Wipe: true}
	}
	partition := func(name, byID string) VolumeRequest {
		return VolumeRequest{Name: name, Partition: &PartitionSelector{ByID: byID}, Filesystem: "xfs"}
	}

	// A kept volume is found by its binding; its partition is not mounted
	// here, so only its claim can protect it.
	kept := []VolumeBinding{{Name: "a", PartitionUUID: "part-1"}}

	for _, test := range []struct {
		name     string
		requests []VolumeRequest
		kept     []VolumeBinding
		want     []string
	}{
		{"same disk twice", []VolumeRequest{wholeDisk("a"), wholeDisk("b")}, nil, []string{`"a"`, `"b"`, "/dev/sdb", "select a different disk"}},
		{"partition on a repartitioned disk", []VolumeRequest{partition("a", "/dev/disk/by-id/ata-data-part1"), wholeDisk("b")}, nil, []string{`"a"`, `"b"`, "/dev/sdb", "select a different disk"}},
		{"same partition twice", []VolumeRequest{partition("a", "/dev/disk/by-id/ata-data-part1"), partition("b", "/dev/disk/by-id/ata-data-part1")}, nil, []string{`"a"`, `"b"`, "partition 1", "/dev/sdb", "select a different partition"}},
		{"repartitioned disk holding a kept volume", []VolumeRequest{wholeDisk("b")}, kept, []string{`"a"`, `"b"`, "/dev/sdb", "select a different disk"}},
		{"partition of a kept volume", []VolumeRequest{partition("b", "/dev/disk/by-id/ata-data-part1")}, kept, []string{`"a"`, `"b"`, "partition 1", "/dev/sdb", "select a different partition"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanVolumes(facts, root, test.requests, test.kept)

			if !mentions(err, test.want...) {
				t.Fatalf("PlanVolumes() error = %v, want refusal naming %q", err, test.want)
			}
		})
	}

	if _, err := PlanVolumes(facts, root, []VolumeRequest{partition("a", "/dev/disk/by-id/ata-data-part1"), partition("b", "/dev/disk/by-id/ata-data-part2")}, nil); err != nil {
		t.Fatalf("PlanVolumes() for two partitions of one disk error = %v", err)
	}
	if _, err := PlanVolumes(facts, root, []VolumeRequest{partition("b", "/dev/disk/by-id/ata-data-part2")}, kept); err != nil {
		t.Fatalf("PlanVolumes() beside a kept volume on the same disk error = %v", err)
	}
}

func TestPlanningRefusesDiskWithoutDiskSeqLink(t *testing.T) {
	root := diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768)
	// A null_blk disk reports a diskseq, but udev links no nullb device by it.
	data := diskForLayout("/dev/nullb0", "/dev/disk/by-id/nullb-data", 16384)
	data.ByDiskSeq = ""
	data.Partitions = []BlockDevice{{Path: "/dev/nullb0p1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", FilesystemSignature: "xfs"}}
	facts := layoutFacts(root, data)

	for _, test := range []struct {
		name    string
		request VolumeRequest
	}{
		{"repartitioned disk", VolumeRequest{Name: "data", Disk: &TargetDiskSelector{ByID: "/dev/disk/by-id/nullb-data"}, Filesystem: "xfs", Wipe: true}},
		{"formatted partition", VolumeRequest{Name: "data", Partition: &PartitionSelector{PartUUID: "data-part"}, Filesystem: "xfs", Wipe: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanVolumes(facts, root, []VolumeRequest{test.request}, nil)

			if !mentions(err, "/dev/nullb0", "by-diskseq") {
				t.Fatalf("PlanVolumes() error = %v, want refusal naming /dev/nullb0 and its missing by-diskseq link", err)
			}
		})
	}

	// A preserved partition is never written, so it needs no link.
	preserved := VolumeRequest{Name: "data", Partition: &PartitionSelector{PartUUID: "data-part"}, Filesystem: "xfs"}
	if _, err := PlanVolumes(facts, root, []VolumeRequest{preserved}, nil); err != nil {
		t.Fatalf("PlanVolumes() for a preserved partition error = %v", err)
	}

	t.Run("system disk", func(t *testing.T) {
		unlinked := root
		unlinked.ByDiskSeq = ""

		_, err := PlanDiskLayout(layoutFacts(unlinked), DiskLayoutRequest{
			TargetDisk: TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
			RootA:      RootSlotRequest{SizeMiB: 4096},
			RootB:      RootSlotRequest{SizeMiB: 4096},
			State:      StatePartitionRequest{MinSizeMiB: 8192},
		})

		if !mentions(err, "/dev/nvme0n1", "by-diskseq") {
			t.Fatalf("PlanDiskLayout() error = %v, want refusal naming /dev/nvme0n1 and its missing by-diskseq link", err)
		}
	})
}

func TestPrepareVolumesChecksEveryVolumeFirst(t *testing.T) {
	other := discovery.DiskIdentity{Path: "/dev/vdc", DiskSeq: 4, SizeBytes: 8 << 30}
	first := VolumePlan{Name: "first", TargetKind: "disk", Disk: other, Filesystem: "xfs", MountPath: "/var/mnt/first", Wipe: true, Repartition: true}
	otherDisk := BlockDevice{Path: "/dev/vdc", Type: DeviceDisk, DiskSeq: 4, ByDiskSeq: "/dev/disk/by-diskseq/4", SizeBytes: 8 << 30}

	t.Run("refusal of a later volume", func(t *testing.T) {
		// The second volume's disk was replaced, so its recheck refuses.
		replaced := BlockDevice{Path: "/dev/vdb", Type: DeviceDisk, DiskSeq: 9, SizeBytes: 8 << 30, Partitions: []BlockDevice{dataPartition()}}
		source := discovery.StaticDiscoverySource{Facts: HardwareFacts{BlockDevices: []BlockDevice{otherDisk, replaced}}}
		commands := &NoopCommandRunner{}

		changed, err := PrepareVolumes(context.Background(), commands, source, []VolumePlan{first, formatVolume()})

		if !errors.Is(err, discovery.ErrTargetDiskChanged) {
			t.Fatalf("PrepareVolumes() error = %v, want ErrTargetDiskChanged", err)
		}
		if got := destructiveCalls(commands.Calls); len(got) != 0 || len(changed) != 0 {
			t.Fatalf("destructive commands = %v, changed = %v; want none before every volume is checked", got, changed)
		}
	})

	t.Run("command failure", func(t *testing.T) {
		source := discovery.StaticDiscoverySource{Facts: HardwareFacts{BlockDevices: []BlockDevice{otherDisk, volumeDiskWith(dataPartition())}}}
		commands := &failingCommandRunner{fail: "wipefs"}

		changed, err := PrepareVolumes(context.Background(), commands, source, []VolumePlan{first, formatVolume()})

		if err == nil {
			t.Fatal("PrepareVolumes() error = nil, want the wipefs failure")
		}
		if !reflect.DeepEqual(changed, []string{"first", "data"}) {
			t.Fatalf("changed = %v, want first, repartitioned, and data, whose wipefs had started", changed)
		}
	})
}

// failingCommandRunner fails every run of one command.
type failingCommandRunner struct {
	NoopCommandRunner
	fail string
}

func (r *failingCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	_ = r.NoopCommandRunner.Run(ctx, name, args...)
	if name == r.fail {
		return errors.New(name + " failed")
	}
	return nil
}

func TestPlanVolumesRefusesPreservedPartitionWithSharedIdentity(t *testing.T) {
	data := diskForLayout("/dev/sdb", "/dev/disk/by-id/ata-data", 16384)
	data.Partitions = []BlockDevice{{
		Path: "/dev/sdb1", Type: DevicePartition, PartitionNumber: 1, ByID: []string{"/dev/disk/by-id/ata-data-part1"}, PartitionUUID: "data-part", FilesystemSignature: "xfs",
	}}
	clone := diskForLayout("/dev/sdc", "/dev/disk/by-id/ata-clone", 16384)
	clone.Partitions = []BlockDevice{{Path: "/dev/sdc1", Type: DevicePartition, PartitionNumber: 1, PartitionUUID: "data-part", FilesystemSignature: "xfs"}}
	root := diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768)

	_, err := PlanVolumes(layoutFacts(root, data, clone), root, []VolumeRequest{{
		Name: "data", Partition: &PartitionSelector{ByID: "/dev/disk/by-id/ata-data-part1"}, Filesystem: "xfs",
	}}, nil)

	if !mentions(err, "data-part", "/dev/sdb1", "/dev/sdc1") {
		t.Fatalf("PlanVolumes() error = %v, want shared PARTUUID refusal", err)
	}
}

func TestPlanVolumesSuggestsWipeOnlyWhenPartitionIsMissing(t *testing.T) {
	data := diskForLayout("/dev/sdb", "/dev/disk/by-id/ata-data", 16384)
	root := diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768)
	request := []VolumeRequest{{Name: "data", Disk: &TargetDiskSelector{ByID: "/dev/disk/by-id/ata-data"}, Filesystem: "xfs"}}

	_, err := PlanVolumes(layoutFacts(root, data), root, request, nil)
	if err == nil || !strings.Contains(err.Error(), "set wipe to true") {
		t.Fatalf("PlanVolumes() with no u-data partition error = %v, want wipe advice", err)
	}

	data.Partitions = []BlockDevice{
		{Path: "/dev/sdb1", Type: DevicePartition, PartitionNumber: 1, GPTLabel: "u-data", PartitionUUID: "a", FilesystemSignature: "xfs"},
		{Path: "/dev/sdb2", Type: DevicePartition, PartitionNumber: 2, GPTLabel: "u-data", PartitionUUID: "b", FilesystemSignature: "xfs"},
	}
	_, err = PlanVolumes(layoutFacts(root, data), root, request, nil)
	if !mentions(err, "/dev/sdb", "u-data") || strings.Contains(err.Error(), "wipe") {
		t.Fatalf("PlanVolumes() with two u-data partitions error = %v, want ambiguity without wipe advice", err)
	}
}
