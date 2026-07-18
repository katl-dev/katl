package disk

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type InputCommandRunner interface {
	CommandRunner
	RunInput(ctx context.Context, input string, name string, args ...string) error
}

type RootSlotInstaller func(context.Context, RootSlotInstallRequest) (RootSlotInstallResult, error)

type DiskExecutor struct {
	Commands           CommandRunner
	InstallRootSlot    RootSlotInstaller
	InstallBoot        BootInstaller
	RootSlotState      SlotStore
	RecordStateMounted func(context.Context) error
}

type DiskExecutionRequest struct {
	Plan              DiskLayoutPlan
	RootSlotInstall   *RootSlotInstallRequest
	RetryRootSlot     bool
	AllowDestructive  bool
	DryRun            bool
	TargetMountPrefix string
}

type DiskExecutionResult struct {
	Operations []DiskOperation
	DryRun     bool
	Boot       *BootResult
}

type DiskOperation struct {
	Name        string
	Command     string
	Args        []string
	Stdin       string
	Destructive bool
}

type DiskOperationGroup string

const (
	PrepareOperations   DiskOperationGroup = "prepare"
	PartitionOperations DiskOperationGroup = "partition"
	FormatOperations    DiskOperationGroup = "format"
	MountOperations     DiskOperationGroup = "mount"
)

var ErrDestructiveInstallNotAllowed = errors.New("destructive install is not allowed")

func (e DiskExecutor) Execute(ctx context.Context, request DiskExecutionRequest) (DiskExecutionResult, error) {
	if err := checkBootPlan(request.Plan); err != nil {
		return DiskExecutionResult{}, err
	}
	operations := BuildDiskOperations(request.Plan, request.TargetMountPrefix)
	return e.executeOperations(ctx, request, operations)
}

func (e DiskExecutor) ExecuteGroup(ctx context.Context, request DiskExecutionRequest, group DiskOperationGroup) (DiskExecutionResult, error) {
	if err := checkBootPlan(request.Plan); err != nil {
		return DiskExecutionResult{}, err
	}
	operations := filterOperations(BuildDiskOperations(request.Plan, request.TargetMountPrefix), group)
	return e.executeOperations(ctx, request, operations)
}

func (e DiskExecutor) executeOperations(ctx context.Context, request DiskExecutionRequest, operations []DiskOperation) (DiskExecutionResult, error) {
	if hasDestructiveOperations(operations) && !request.AllowDestructive {
		return DiskExecutionResult{}, ErrDestructiveInstallNotAllowed
	}

	result := DiskExecutionResult{Operations: operations, DryRun: request.DryRun}
	if request.DryRun {
		return result, nil
	}
	if e.Commands == nil {
		return DiskExecutionResult{}, fmt.Errorf("command runner is required")
	}

	installRootSlot := e.InstallRootSlot
	if installRootSlot == nil {
		installRootSlot = func(ctx context.Context, request RootSlotInstallRequest) (RootSlotInstallResult, error) {
			return WriteRootSlot(request)
		}
	}
	for _, operation := range operations {
		if isRootWrite(operation) {
			if request.RootSlotInstall == nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: root slot install request is required", operation.Name)
			}
			if err := validateRootWrite(operation, request.RootSlotInstall.Plan); err != nil {
				return DiskExecutionResult{}, err
			}
			if _, err := runRootSlot(ctx, e.RootSlotState, *request.RootSlotInstall, request.RetryRootSlot, installRootSlot); err != nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
			}
			continue
		}
		if operation.Name == "install-systemd-boot" {
			installBoot := e.InstallBoot
			bootRequest := BootRequest{
				Plan:              request.Plan,
				TargetMountPrefix: request.TargetMountPrefix,
			}
			if installBoot == nil {
				commands, ok := e.Commands.(OutputCommandRunner)
				if !ok {
					return DiskExecutionResult{}, fmt.Errorf("%s: boot command runner must support output", operation.Name)
				}
				bootRequest.Commands = commands
				installBoot = InstallBoot
			}
			boot, err := installBoot(ctx, bootRequest)
			if err != nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
			}
			result.Boot = &boot
			continue
		}
		if operation.Stdin != "" {
			commands, ok := e.Commands.(InputCommandRunner)
			if !ok {
				return DiskExecutionResult{}, fmt.Errorf("%s: command runner must support stdin", operation.Name)
			}
			if err := commands.RunInput(ctx, operation.Stdin, operation.Command, operation.Args...); err != nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
			}
		} else if err := e.Commands.Run(ctx, operation.Command, operation.Args...); err != nil {
			return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
		}
		if operation.Name == "mount-state" && e.RecordStateMounted != nil {
			if err := e.RecordStateMounted(ctx); err != nil {
				return DiskExecutionResult{}, fmt.Errorf("record state checkpoint: %w", err)
			}
		}
	}

	return result, nil
}

func filterOperations(operations []DiskOperation, group DiskOperationGroup) []DiskOperation {
	filtered := make([]DiskOperation, 0, len(operations))
	for _, operation := range operations {
		if operationInGroup(operation, group) {
			filtered = append(filtered, operation)
		}
	}
	return filtered
}

func operationInGroup(operation DiskOperation, group DiskOperationGroup) bool {
	switch group {
	case PrepareOperations:
		return operation.Name == "wipe-target-signatures" || strings.HasPrefix(operation.Name, "wipe-extra-")
	case PartitionOperations:
		return operation.Name == "create-gpt" || operation.Name == "reread-partitions" || operation.Name == "settle-partitions"
	case FormatOperations:
		return strings.HasPrefix(operation.Name, "format-")
	case MountOperations:
		return strings.HasPrefix(operation.Name, "create-mountpoint-") || strings.HasPrefix(operation.Name, "mount-") || operation.Name == "install-systemd-boot"
	default:
		return false
	}
}

func BuildDiskOperations(plan DiskLayoutPlan, targetMountPrefix string) []DiskOperation {
	if targetMountPrefix == "" {
		targetMountPrefix = "/mnt/target"
	}

	operations := []DiskOperation{
		{Name: "wipe-target-signatures", Command: "wipefs", Args: []string{"--all", plan.TargetDiskPath}, Destructive: true},
		{Name: "create-gpt", Command: "sfdisk", Args: []string{plan.TargetDiskPath}, Stdin: sfdiskScript(plan), Destructive: true},
		{Name: "reread-partitions", Command: "partprobe", Args: []string{plan.TargetDiskPath}},
		{Name: "settle-partitions", Command: "udevadm", Args: []string{"settle"}},
	}

	for _, partition := range plan.Partitions {
		switch partition.Name {
		case "root-a", "root-b":
			if RootSlot(partition.Name) == plan.Boot.RootSlot {
				operations = append(operations, DiskOperation{Name: "write-" + partition.Name, Command: "katlos-write-root-slot", Args: []string{partition.GPTLabel}, Destructive: true})
			}
		default:
			operations = append(operations, DiskOperation{Name: "format-" + partition.Name, Command: "mkfs." + partition.Filesystem, Args: formatArgs(partition), Destructive: true})
		}
		if partition.MountPath != "" && partition.Name != "root-a" {
			name := "mount-" + partition.Name
			if partition.Name == "state" {
				name = "mount-state"
			}
			operations = append(operations, DiskOperation{Name: "create-mountpoint-" + partition.Name, Command: "mkdir", Args: []string{"-p", targetMountPrefix + partition.MountPath}})
			operations = append(operations, DiskOperation{Name: name, Command: "mount", Args: []string{"LABEL=" + partition.GPTLabel, targetMountPrefix + partition.MountPath}})
		}
	}

	if _, ok := findPartitionByName(plan.Partitions, "esp"); ok {
		operations = append(operations, DiskOperation{Name: "install-systemd-boot", Command: "bootctl", Args: []string{"install"}, Destructive: true})
	}

	for _, extra := range plan.ExtraMounts {
		if extra.Wipe {
			operations = append(operations, DiskOperation{Name: "wipe-extra-" + extra.Name, Command: "wipefs", Args: []string{"--all", extra.DevicePath}, Destructive: true})
		}
		operations = append(operations, DiskOperation{Name: "format-extra-" + extra.Name, Command: "mkfs." + extra.Filesystem, Args: []string{extra.DevicePath}, Destructive: true})
		operations = append(operations, DiskOperation{Name: "create-mountpoint-extra-" + extra.Name, Command: "mkdir", Args: []string{"-p", targetMountPrefix + extra.MountPath}})
		operations = append(operations, DiskOperation{Name: "mount-extra-" + extra.Name, Command: "mount", Args: []string{extra.DevicePath, targetMountPrefix + extra.MountPath}})
	}

	return operations
}

func sfdiskScript(plan DiskLayoutPlan) string {
	var builder strings.Builder
	builder.WriteString("label: gpt\n")
	for _, partition := range plan.Partitions {
		fields := []string{"type=" + partitionTypeGUID(partition.Type), "name=\"" + partition.GPTLabel + "\""}
		if !partition.Remaining {
			fields = append([]string{"size=" + fmt.Sprintf("%dMiB", partition.SizeMiB)}, fields...)
		}
		builder.WriteString(strings.Join(fields, ", "))
		builder.WriteByte('\n')
	}
	return builder.String()
}

func partitionTypeGUID(kind string) string {
	switch kind {
	case "esp":
		return "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	case "xbootldr":
		return "bc13c2ff-59e6-4262-a352-b275fd6f7172"
	case "root-x86-64":
		return "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	case "var":
		return "4d21b016-b534-45c2-a9fb-5c16e091fd2d"
	default:
		return "0fc63daf-8483-4772-8e79-3d69d8477de4"
	}
}

func formatArgs(partition PartitionPlan) []string {
	device := partLabelDevice(partition.GPTLabel)
	switch partition.Filesystem {
	case "vfat":
		return []string{"-n", partition.GPTLabel, device}
	case "ext4":
		args := []string{"-L", partition.GPTLabel}
		if partition.Name == "state" {
			args = append(args, "-O", "verity")
		}
		return append(args, device)
	default:
		return []string{"-L", partition.GPTLabel, device}
	}
}

func partLabelDevice(label string) string {
	return "/dev/disk/by-partlabel/" + label
}

func ValidateAppliedLayout(facts HardwareFacts, plan DiskLayoutPlan) error {
	return ValidateAppliedLayoutAt(facts, plan, "")
}

func ValidateAppliedLayoutAt(facts HardwareFacts, plan DiskLayoutPlan, targetMountPrefix string) error {
	if targetMountPrefix == "" {
		targetMountPrefix = "/"
	}
	target := findDevice(facts.BlockDevices, plan.TargetDiskPath)
	if target == nil {
		return fmt.Errorf("target disk %s not found", plan.TargetDiskPath)
	}

	partitionsByLabel := make(map[string]BlockDevice)
	for _, partition := range target.Partitions {
		partitionsByLabel[partition.GPTLabel] = partition
	}
	for _, expected := range plan.Partitions {
		if _, ok := partitionsByLabel[expected.GPTLabel]; !ok {
			return fmt.Errorf("partition label %s not found", expected.GPTLabel)
		}
	}
	if _, ok := partitionsByLabel[plan.Boot.RootPartitionLabel]; !ok {
		return fmt.Errorf("boot root label %s not found", plan.Boot.RootPartitionLabel)
	}
	if !mountExists(facts.Mounts, targetPath(targetMountPrefix, "/var")) {
		return fmt.Errorf("state partition is not mounted at %s", targetPath(targetMountPrefix, "/var"))
	}
	if !mountExists(facts.Mounts, targetPath(targetMountPrefix, "/efi")) {
		return fmt.Errorf("ESP partition is not mounted at %s", targetPath(targetMountPrefix, "/efi"))
	}
	if _, ok := findPartitionByName(plan.Partitions, "xbootldr"); ok && !mountExists(facts.Mounts, targetPath(targetMountPrefix, "/boot")) {
		return fmt.Errorf("XBOOTLDR partition is not mounted at %s", targetPath(targetMountPrefix, "/boot"))
	}

	for _, extra := range plan.ExtraMounts {
		target := targetPath(targetMountPrefix, extra.MountPath)
		if !mountExists(facts.Mounts, target) {
			return fmt.Errorf("extra disk %q is not mounted at %s", extra.Name, target)
		}
	}

	return nil
}

func isRootWrite(operation DiskOperation) bool {
	return operation.Name == "write-root-a" || operation.Name == "write-root-b"
}

func validateRootWrite(operation DiskOperation, plan RootSlotWritePlan) error {
	if len(operation.Args) != 1 || operation.Args[0] != plan.TargetPartition.GPTLabel {
		return fmt.Errorf("%s target label does not match root slot plan", operation.Name)
	}
	return nil
}

func hasDestructiveOperations(operations []DiskOperation) bool {
	for _, operation := range operations {
		if operation.Destructive {
			return true
		}
	}
	return false
}

func findDevice(devices []BlockDevice, devicePath string) *BlockDevice {
	for i := range devices {
		if devices[i].Path == devicePath {
			return &devices[i]
		}
	}
	return nil
}

func mountExists(mounts []MountFact, target string) bool {
	for _, mount := range mounts {
		if mount.Target == target {
			return true
		}
	}
	return false
}
