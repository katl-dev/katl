package installer

import (
	"context"
	"errors"
	"testing"
)

func TestDiskExecutorDryRunOutput(t *testing.T) {
	plan := executorPlan()
	result, err := (DiskExecutor{}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             plan,
		AllowDestructive: true,
		DryRun:           true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.DryRun {
		t.Fatalf("DryRun = false, want true")
	}
	if len(result.Operations) == 0 {
		t.Fatalf("dry-run operations are empty")
	}
	if result.Operations[0].Name != "wipe-target-signatures" || result.Operations[0].Command != "wipefs" {
		t.Fatalf("first operation = %#v", result.Operations[0])
	}
}

func TestDiskExecutorRefusesDestructiveActionsWithoutPermission(t *testing.T) {
	_, err := (DiskExecutor{}).Execute(context.Background(), DiskExecutionRequest{
		Plan:   executorPlan(),
		DryRun: true,
	})
	if !errors.Is(err, ErrDestructiveInstallNotAllowed) {
		t.Fatalf("Execute() error = %v, want ErrDestructiveInstallNotAllowed", err)
	}
}

func TestDiskExecutorRecordsCheckpointAfterStateMount(t *testing.T) {
	commands := &NoopCommandRunner{}
	store := &MemoryStateStore{}
	_, err := (DiskExecutor{Commands: commands, Store: store}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		AllowDestructive: true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(commands.Calls) == 0 {
		t.Fatalf("expected command calls")
	}
	if len(store.Checkpoints) != 1 {
		t.Fatalf("checkpoint count = %d, want 1", len(store.Checkpoints))
	}
	if store.Checkpoints[0].CurrentStep != FormatFilesystems {
		t.Fatalf("checkpoint step = %s, want %s", store.Checkpoints[0].CurrentStep, FormatFilesystems)
	}
}

func TestValidateAppliedLayout(t *testing.T) {
	plan := executorPlan()
	facts := HardwareFacts{
		BlockDevices: []BlockDevice{
			{
				Path: "/dev/nvme0n1",
				Type: DeviceDisk,
				Partitions: []BlockDevice{
					{Path: "/dev/nvme0n1p1", GPTLabel: GPTLabelESP},
					{Path: "/dev/nvme0n1p2", GPTLabel: GPTLabelRootA},
					{Path: "/dev/nvme0n1p3", GPTLabel: GPTLabelRootB},
					{Path: "/dev/nvme0n1p4", GPTLabel: GPTLabelState},
				},
			},
		},
		Mounts: []MountFact{
			{Source: "/dev/nvme0n1p4", Target: "/var", Filesystem: "ext4"},
			{Source: "/dev/sdb", Target: "/srv/data", Filesystem: "xfs"},
		},
	}
	if err := ValidateAppliedLayout(facts, plan); err != nil {
		t.Fatalf("ValidateAppliedLayout() error = %v", err)
	}

	facts.Mounts = facts.Mounts[:1]
	if err := ValidateAppliedLayout(facts, plan); err == nil {
		t.Fatalf("ValidateAppliedLayout() error = nil, want missing extra mount failure")
	}
}

func executorPlan() DiskLayoutPlan {
	return DiskLayoutPlan{
		TargetDiskPath: "/dev/nvme0n1",
		Partitions: []PartitionPlan{
			{Name: "esp", GPTLabel: GPTLabelESP, Filesystem: "vfat", MountPath: "/efi"},
			{Name: "root-a", GPTLabel: GPTLabelRootA, Filesystem: "squashfs", MountPath: "/"},
			{Name: "root-b", GPTLabel: GPTLabelRootB, Filesystem: "squashfs"},
			{Name: "state", GPTLabel: GPTLabelState, Filesystem: "ext4", MountPath: "/var"},
		},
		ExtraMounts: []ExtraDiskPlan{
			{Name: "data", DevicePath: "/dev/sdb", Filesystem: "xfs", MountPath: "/srv/data", Wipe: true},
		},
		Boot: BootTargetMetadata{RootSlot: RootSlotA, RootPartitionLabel: GPTLabelRootA},
	}
}
