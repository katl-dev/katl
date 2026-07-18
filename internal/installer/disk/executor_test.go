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
	createGPT := findOp(result.Operations, "create-gpt")
	if createGPT.Command != "sfdisk" || !strings.Contains(createGPT.Stdin, `type=c12a7328-f81f-11d2-ba4b-00a0c93ec93b, name="KATL_ESP"`) {
		t.Fatalf("create-gpt operation = %#v", createGPT)
	}
	if strings.Contains(createGPT.Stdin, "unit:") {
		t.Fatalf("sfdisk input contains unsupported unit header: %q", createGPT.Stdin)
	}
	formatESP := findOp(result.Operations, "format-esp")
	if !strings.HasSuffix(strings.Join(formatESP.Args, " "), "/dev/disk/by-partlabel/KATL_ESP") {
		t.Fatalf("format ESP args = %#v", formatESP.Args)
	}
	formatState := findOp(result.Operations, "format-state")
	if got := strings.Join(formatState.Args, " "); got != "-L KATL_STATE -O verity /dev/disk/by-partlabel/KATL_STATE" {
		t.Fatalf("format state args = %q", got)
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

func TestDiskExecutorExecutesOperationGroups(t *testing.T) {
	plan := executorPlan()
	commands := &NoopCommandRunner{}
	if _, err := (DiskExecutor{Commands: commands}).ExecuteGroup(context.Background(), DiskExecutionRequest{
		Plan:             plan,
		AllowDestructive: true,
	}, PartitionOperations); err != nil {
		t.Fatalf("ExecuteGroup(partition) error = %v", err)
	}
	if got := callNames(commands.Calls); !strings.Contains(got, "sfdisk") || !strings.Contains(got, "partprobe") || !strings.Contains(got, "udevadm") {
		t.Fatalf("partition calls = %s", got)
	}
	if strings.Contains(callNames(commands.Calls), "mkfs.") || strings.Contains(callNames(commands.Calls), "mount") {
		t.Fatalf("partition group ran later operations: %#v", commands.Calls)
	}

	commands = &NoopCommandRunner{}
	result, err := (DiskExecutor{Commands: commands}).ExecuteGroup(context.Background(), DiskExecutionRequest{
		Plan:             plan,
		AllowDestructive: true,
	}, MountOperations)
	if err != nil {
		t.Fatalf("ExecuteGroup(mount) error = %v", err)
	}
	if result.Boot == nil || result.Boot.RootPartitionUUID == "" {
		t.Fatalf("boot result = %#v", result.Boot)
	}
	if countCalls(commands.Calls, "mkdir") == 0 || countCalls(commands.Calls, "mount") == 0 || countCalls(commands.Calls, "bootctl") != 1 {
		t.Fatalf("mount calls = %#v", commands.Calls)
	}
}

func TestDiskExecutorRecordsCheckpointAfterStateMount(t *testing.T) {
	artifact := []byte("runtime-root")
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
		RootSlotInstall:  rootInstall(artifact, newMemSlot(len(artifact))),
		AllowDestructive: true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(commands.Calls) == 0 {
		t.Fatalf("expected command calls")
	}
	if commands.Inputs["create-gpt"] == "" || !strings.Contains(commands.Inputs["create-gpt"], `name="KATL_ROOT_A"`) {
		t.Fatalf("create-gpt input = %q", commands.Inputs["create-gpt"])
	}
	if recorded != 1 {
		t.Fatalf("state mount checkpoint count = %d, want 1", recorded)
	}
	if countCalls(commands.Calls, "bootctl") != 1 {
		t.Fatalf("bootctl calls = %#v", commands.Calls)
	}
}

func TestDiskExecutorRequiresRootSlotInstall(t *testing.T) {
	commands := &NoopCommandRunner{}

	_, err := (DiskExecutor{Commands: commands}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		AllowDestructive: true,
	})
	if err == nil || !strings.Contains(err.Error(), "root slot install request is required") {
		t.Fatalf("Execute() error = %v, want missing root slot install request", err)
	}
	if countCalls(commands.Calls, "katlos-write-root-slot") != 0 {
		t.Fatalf("root slot external command calls = %#v", commands.Calls)
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

func TestDiskExecutorInstallsBoot(t *testing.T) {
	artifact := []byte("runtime-root")
	commands := &NoopCommandRunner{}

	result, err := (DiskExecutor{Commands: commands}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		RootSlotInstall:  rootInstall(artifact, newMemSlot(len(artifact))),
		AllowDestructive: true,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Boot == nil {
		t.Fatalf("boot result is nil")
	}
	if result.Boot.ESP.PartitionUUID == "" || result.Boot.RootPartitionUUID == "" {
		t.Fatalf("boot result = %#v", result.Boot)
	}
	if countCalls(commands.Calls, "bootctl") != 1 {
		t.Fatalf("bootctl calls = %#v", commands.Calls)
	}
}

func TestDiskExecutorRejectsXBOOTLDR(t *testing.T) {
	plan := executorPlan()
	plan.Partitions = append(plan.Partitions[:1], append([]PartitionPlan{
		{Name: "xbootldr", GPTLabel: GPTLabelXBOOTLDR, Filesystem: "ext4", MountPath: "/boot"},
	}, plan.Partitions[1:]...)...)
	commands := &NoopCommandRunner{}

	_, err := (DiskExecutor{Commands: commands}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             plan,
		RootSlotInstall:  rootInstall([]byte("runtime-root"), newMemSlot(len("runtime-root"))),
		AllowDestructive: true,
	})
	if err == nil || !strings.Contains(err.Error(), "XBOOTLDR filesystem") {
		t.Fatalf("Execute() error = %v, want XBOOTLDR filesystem failure", err)
	}
	if len(commands.Calls) != 0 {
		t.Fatalf("command calls = %#v, want none", commands.Calls)
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
			{Source: "/dev/nvme0n1p1", Target: "/efi", Filesystem: "vfat"},
			{Source: "/dev/nvme0n1p4", Target: "/var", Filesystem: "ext4"},
			{Source: "/dev/sdb", Target: "/srv/data", Filesystem: "xfs"},
		},
	}
	if err := ValidateAppliedLayout(facts, plan); err != nil {
		t.Fatalf("ValidateAppliedLayout() error = %v", err)
	}
	prefixed := facts
	prefixed.Mounts = []MountFact{
		{Source: "/dev/nvme0n1p1", Target: "/target/efi", Filesystem: "vfat"},
		{Source: "/dev/nvme0n1p4", Target: "/target/var", Filesystem: "ext4"},
		{Source: "/dev/sdb", Target: "/target/srv/data", Filesystem: "xfs"},
	}
	if err := ValidateAppliedLayoutAt(prefixed, plan, "/target"); err != nil {
		t.Fatalf("ValidateAppliedLayoutAt() error = %v", err)
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
			{Name: "esp", GPTLabel: GPTLabelESP, Type: "esp", Filesystem: "vfat", MountPath: "/efi", SizeMiB: 512},
			{Name: "root-a", GPTLabel: GPTLabelRootA, Type: "root-x86-64", Filesystem: "squashfs", MountPath: "/", SizeMiB: 1024},
			{Name: "root-b", GPTLabel: GPTLabelRootB, Type: "root-x86-64", Filesystem: "squashfs", SizeMiB: 1024},
			{Name: "state", GPTLabel: GPTLabelState, Type: "var", Filesystem: "ext4", MountPath: "/var", Remaining: true},
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

func findOp(operations []DiskOperation, name string) DiskOperation {
	for _, operation := range operations {
		if operation.Name == name {
			return operation
		}
	}
	return DiskOperation{}
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

func callNames(calls []CommandCall) string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return strings.Join(names, " ")
}

type NoopCommandRunner struct {
	Calls  []CommandCall
	Inputs map[string]string
}

type CommandCall struct {
	Name string
	Args []string
}

func (r *NoopCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	return nil
}

func (r *NoopCommandRunner) RunInput(_ context.Context, input string, name string, args ...string) error {
	if r.Inputs == nil {
		r.Inputs = make(map[string]string)
	}
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	r.Inputs["create-gpt"] = input
	return nil
}

func (r *NoopCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	if name == "blkid" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "PARTLABEL=") {
				label := strings.TrimPrefix(arg, "PARTLABEL=")
				return []byte("uuid-" + strings.ToLower(label) + "\n"), nil
			}
		}
	}
	return []byte(""), nil
}
