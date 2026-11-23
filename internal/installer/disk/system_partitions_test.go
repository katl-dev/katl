package disk

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

// twoKatlDisks returns facts where the old Katl install on vda is listed
// before the new target vdb, so a label lookup would find vda first.
func twoKatlDisks() HardwareFacts {
	katlDisk := func(name, prefix string) BlockDevice {
		return BlockDevice{
			Path: "/dev/" + name, Type: DeviceDisk, DiskSeq: uint64(len(name)), SizeBytes: 32 << 30,
			Partitions: []BlockDevice{
				{Path: "/dev/" + name + "1", Type: DevicePartition, GPTLabel: GPTLabelESP, PartitionUUID: prefix + "-esp"},
				{Path: "/dev/" + name + "2", Type: DevicePartition, GPTLabel: GPTLabelRootA, PartitionUUID: prefix + "-root-a"},
				{Path: "/dev/" + name + "3", Type: DevicePartition, GPTLabel: GPTLabelRootB, PartitionUUID: prefix + "-root-b"},
				{Path: "/dev/" + name + "4", Type: DevicePartition, GPTLabel: GPTLabelState, PartitionUUID: prefix + "-state"},
			},
		}
	}
	return HardwareFacts{BlockDevices: []BlockDevice{katlDisk("vda", "old"), katlDisk("vdb", "new")}}
}

func boundTestPlan(facts HardwareFacts) DiskLayoutPlan {
	plan := executorPlan()
	plan.VolumeMounts = nil
	plan.PartitionUUIDs = nil
	plan.TargetDiskPath = "/dev/vdb"
	plan.TargetDisk = discovery.IdentityOf(facts.BlockDevices[1])
	return plan
}

func TestBindSystemPartitions(t *testing.T) {
	facts := twoKatlDisks()
	plan := boundTestPlan(facts)

	bound, err := BindSystemPartitions(facts, plan)
	if err != nil {
		t.Fatalf("BindSystemPartitions() error = %v", err)
	}

	want := map[string]string{GPTLabelESP: "new-esp", GPTLabelRootA: "new-root-a", GPTLabelRootB: "new-root-b", GPTLabelState: "new-state"}
	for label, uuid := range want {
		if bound[label] != uuid {
			t.Errorf("bound %s = %q, want %q from target disk vdb", label, bound[label], uuid)
		}
	}

	plan.PartitionUUIDs = bound
	operations := map[string]DiskOperation{}
	built, err := BuildDiskOperations(plan, "/mnt/target")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range built {
		operations[operation.Name] = operation
	}
	if got := operations["mount-state"].Args; len(got) != 2 || got[0] != "/dev/disk/by-partuuid/new-state" {
		t.Errorf("mount-state args = %q, want the target state PARTUUID", got)
	}
	if got := operations["mount-esp"].Args; len(got) != 2 || got[0] != "/dev/disk/by-partuuid/new-esp" {
		t.Errorf("mount-esp args = %q, want the target ESP PARTUUID", got)
	}
}

func TestBindSystemPartitionsRefuses(t *testing.T) {
	tests := []struct {
		name   string
		change func(*HardwareFacts, *DiskLayoutPlan)
		want   string
	}{
		{
			name: "target lacks a label the other disk has",
			change: func(facts *HardwareFacts, _ *DiskLayoutPlan) {
				facts.BlockDevices[1].Partitions = facts.BlockDevices[1].Partitions[:3]
			},
			want: "vdb has 0 partitions labelled KATL_STATE",
		},
		{
			name: "cloned partition identity",
			change: func(facts *HardwareFacts, _ *DiskLayoutPlan) {
				facts.BlockDevices[0].Partitions[3].PartitionUUID = "NEW-STATE"
			},
			want: "also used by /dev/vda4",
		},
		{
			name: "target disk replaced",
			change: func(facts *HardwareFacts, _ *DiskLayoutPlan) {
				facts.BlockDevices[1].DiskSeq++
			},
			want: "/dev/vdb is now (size=32.0GiB, diskseq=4), not the selected (size=32.0GiB, diskseq=3)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := twoKatlDisks()
			plan := boundTestPlan(facts)
			test.change(&facts, &plan)

			_, err := BindSystemPartitions(facts, plan)

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BindSystemPartitions() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMountRequiresBoundPartitions(t *testing.T) {
	facts := twoKatlDisks()
	plan := boundTestPlan(facts)
	commands := &NoopCommandRunner{}

	_, err := (DiskExecutor{Commands: commands}).ExecuteGroup(context.Background(), DiskExecutionRequest{Plan: plan, AllowDestructive: true}, MountOperations)

	if err == nil || !strings.Contains(err.Error(), "is not bound to target disk") {
		t.Fatalf("ExecuteGroup(mount) error = %v, want unbound partition failure", err)
	}
	if countCalls(commands.Calls, "mount") != 0 {
		t.Fatalf("mount ran before partitions were bound: %v", commands.Calls)
	}
}

func TestValidateAppliedLayoutRejectsOtherDiskMount(t *testing.T) {
	facts := twoKatlDisks()
	plan := boundTestPlan(facts)
	facts.Mounts = []MountFact{
		{Source: "/dev/vdb1", Target: "/mnt/target/efi"},
		{Source: "/dev/vda4", Target: "/mnt/target/var"},
	}

	err := ValidateAppliedLayoutAt(facts, plan, "/mnt/target")

	if err == nil || !strings.Contains(err.Error(), "mounted from /dev/vda4, not the target disk partition /dev/vdb4") {
		t.Fatalf("ValidateAppliedLayoutAt() error = %v, want other-disk mount failure", err)
	}
}
