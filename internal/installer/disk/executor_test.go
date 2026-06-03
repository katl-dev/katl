package disk

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	if countOps(result.Operations, "write-root-a") != 1 || countOps(result.Operations, "write-root-b") != 0 {
		t.Fatalf("root write operations = %#v", result.Operations)
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
	recorded := 0
	_, err := (DiskExecutor{
		Commands: commands,
		RecordStateMounted: func(context.Context) error {
			recorded++
			return nil
		},
	}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		AllowDestructive: true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(commands.Calls) == 0 {
		t.Fatalf("expected command calls")
	}
	if recorded != 1 {
		t.Fatalf("state mount checkpoint count = %d, want 1", recorded)
	}
}

func TestDiskExecutorWritesRootSlot(t *testing.T) {
	artifact := []byte("runtime-root")
	target := newMemSlot(len(artifact) + 4096)
	commands := &NoopCommandRunner{}
	installed := 0
	recorded := 0

	_, err := (DiskExecutor{
		Commands: commands,
		InstallRootSlot: func(ctx context.Context, request RootSlotInstallRequest) (RootSlotInstallResult, error) {
			installed++
			if request.Plan.Slot != RootSlotA || request.Plan.TargetPartition.GPTLabel != GPTLabelRootA {
				t.Fatalf("root slot plan = %#v", request.Plan)
			}
			return WriteRootSlot(request)
		},
		RecordStateMounted: func(context.Context) error {
			recorded++
			return nil
		},
	}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		RootSlotInstall:  rootInstall(artifact, target),
		AllowDestructive: true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if installed != 1 {
		t.Fatalf("root slot installs = %d, want 1", installed)
	}
	if recorded != 1 {
		t.Fatalf("state mount checkpoint count = %d, want 1", recorded)
	}
	if countCalls(commands.Calls, "katlos-write-root-slot") != 0 {
		t.Fatalf("root slot external command calls = %#v", commands.Calls)
	}
	if got := string(target.data[:len(artifact)]); got != string(artifact) {
		t.Fatalf("written bytes = %q, want artifact", got)
	}
	if !target.synced {
		t.Fatal("target was not synced")
	}
}

func TestDiskExecutorStopsOnRootSlotFailure(t *testing.T) {
	artifact := []byte("runtime-root")
	target := newMemSlot(len(artifact))
	recorded := 0

	_, err := (DiskExecutor{
		Commands: &NoopCommandRunner{},
		RecordStateMounted: func(context.Context) error {
			recorded++
			return nil
		},
	}).Execute(context.Background(), DiskExecutionRequest{
		Plan: executorPlan(),
		RootSlotInstall: &RootSlotInstallRequest{
			Plan:     writePlan([]byte("expected")),
			Artifact: bytes.NewReader([]byte("corrupt!")),
			Target:   target,
		},
		AllowDestructive: true,
	})
	if err == nil || !strings.Contains(err.Error(), "write-root-a") || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Execute() error = %v, want write-root-a digest failure", err)
	}
	if recorded != 0 {
		t.Fatalf("state mount checkpoint count = %d, want 0", recorded)
	}
	if target.writes != 0 {
		t.Fatalf("target writes = %d, want 0", target.writes)
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
			{Name: "root-a", GPTLabel: GPTLabelRootA, Filesystem: "squashfs", MountPath: "/", SizeMiB: 1024},
			{Name: "root-b", GPTLabel: GPTLabelRootB, Filesystem: "squashfs", SizeMiB: 1024},
			{Name: "state", GPTLabel: GPTLabelState, Filesystem: "ext4", MountPath: "/var"},
		},
		ExtraMounts: []ExtraDiskPlan{
			{Name: "data", DevicePath: "/dev/sdb", Filesystem: "xfs", MountPath: "/srv/data", Wipe: true},
		},
		Boot: BootTargetMetadata{RootSlot: RootSlotA, RootPartitionLabel: GPTLabelRootA},
	}
}

func rootInstall(data []byte, target RootSlotDevice) *RootSlotInstallRequest {
	return &RootSlotInstallRequest{
		Plan:     writePlan(data),
		Artifact: bytes.NewReader(data),
		Target:   target,
	}
}

func countOps(operations []DiskOperation, name string) int {
	count := 0
	for _, operation := range operations {
		if operation.Name == name {
			count++
		}
	}
	return count
}

func countCalls(calls []CommandCall, name string) int {
	count := 0
	for _, call := range calls {
		if call.Name == name {
			count++
		}
	}
	return count
}

type NoopCommandRunner struct {
	Calls []CommandCall
}

type CommandCall struct {
	Name string
	Args []string
}

func (r *NoopCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	return nil
}
