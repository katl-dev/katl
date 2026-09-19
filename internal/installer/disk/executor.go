package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
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
	Definitions []string
	Env         []string
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
		if len(operation.Definitions) > 0 {
			if err := runRepartOperation(ctx, e.Commands, operation); err != nil {
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

func runRepartOperation(ctx context.Context, commands CommandRunner, operation DiskOperation) error {
	dir, err := os.MkdirTemp("", "katl-repart-")
	if err != nil {
		return fmt.Errorf("create repart definition directory: %w", err)
	}
	defer os.RemoveAll(dir)
	for i, definition := range operation.Definitions {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d-katl.conf", i)), []byte(definition), 0o600); err != nil {
			return fmt.Errorf("write repart definition: %w", err)
		}
	}
	args := make([]string, len(operation.Args))
	for i, arg := range operation.Args {
		args[i] = strings.ReplaceAll(arg, "{definitions}", dir)
	}
	if len(operation.Env) > 0 {
		args = append(append(append([]string{}, operation.Env...), operation.Command), args...)
		return commands.Run(ctx, "env", args...)
	}
	return commands.Run(ctx, operation.Command, args...)
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
		return operation.Name == "wipe-target-signatures"
	case PartitionOperations:
		return operation.Name == "create-gpt" || operation.Name == "reread-partitions" || operation.Name == "settle-partitions" || strings.HasPrefix(operation.Name, "repart-volume-") || strings.HasPrefix(operation.Name, "settle-volume-")
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
		{Name: "create-gpt", Command: "systemd-repart", Args: repartArgs(plan.TargetDiskPath), Definitions: systemDefinitions(plan), Env: []string{"SYSTEMD_REPART_MKFS_OPTIONS_EXT4=-O verity"}, Destructive: true},
		{Name: "reread-partitions", Command: "partprobe", Args: []string{plan.TargetDiskPath}},
		{Name: "settle-partitions", Command: "udevadm", Args: []string{"settle"}},
	}

	for _, partition := range plan.Partitions {
		switch partition.Name {
		case "root-a", "root-b":
			if RootSlot(partition.Name) == plan.Boot.RootSlot {
				operations = append(operations, DiskOperation{Name: "write-" + partition.Name, Command: "katlos-write-root-slot", Args: []string{partition.GPTLabel}, Destructive: true})
			}
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

	for _, volume := range plan.VolumeMounts {
		operations = append(operations, volumeOperations(volume)...)
		operations = append(operations, DiskOperation{Name: "create-mountpoint-volume-" + volume.Name, Command: "mkdir", Args: []string{"-p", targetMountPrefix + volume.MountPath}})
		operations = append(operations, DiskOperation{Name: "mount-volume-" + volume.Name, Command: "mount", Args: []string{volume.MountSource, targetMountPrefix + volume.MountPath}})
	}

	return operations
}

func volumeDefinition(volume VolumePlan) string {
	return strings.Join([]string{
		"[Partition]",
		"Type=" + volume.TypeUUID,
		"Label=" + volumePartitionLabel(volume.Name),
		"Format=" + volume.Filesystem,
		"",
	}, "\n")
}

// Repart creates and formats Katl-owned partitions in definition order. Root
// slots remain unformatted because their immutable images are written separately.
func systemDefinitions(plan DiskLayoutPlan) []string {
	var definitions []string
	for _, partition := range plan.Partitions {
		lines := []string{"[Partition]", "Type=" + partitionTypeGUID(partition.Type), "Label=" + partition.GPTLabel}
		if !partition.Remaining {
			size := fmt.Sprintf("%dM", partition.SizeMiB)
			lines = append(lines, "SizeMinBytes="+size, "SizeMaxBytes="+size)
		}
		if partition.Name != "root-a" && partition.Name != "root-b" {
			lines = append(lines, "Format="+partition.Filesystem)
		}
		definitions = append(definitions, strings.Join(lines, "\n")+"\n")
	}
	return definitions
}

func repartArgs(device string) []string {
	return []string{"--dry-run=no", "--empty=force", "--discard=no", "--definitions={definitions}", device}
}

// PrepareVolume executes the provisioning plan shared by install and live apply.
// The caller must validate current device ownership before invoking it.
func PrepareVolume(ctx context.Context, commands CommandRunner, plan VolumePlan) error {
	_, err := (DiskExecutor{Commands: commands}).executeOperations(ctx, DiskExecutionRequest{AllowDestructive: true}, volumeOperations(plan))
	return err
}

func volumeOperations(volume VolumePlan) []DiskOperation {
	if volume.Repartition {
		return []DiskOperation{
			{Name: "repart-volume-" + volume.Name, Command: "systemd-repart", Args: repartArgs(volume.DevicePath), Definitions: []string{volumeDefinition(volume)}, Destructive: true},
			{Name: "settle-volume-" + volume.Name, Command: "udevadm", Args: []string{"settle"}},
		}
	}
	if !volume.Wipe {
		return nil
	}
	// A selected existing partition grants no authority over its parent disk.
	// Format it in place; repart here would create a nested partition table.
	args := []string{}
	switch volume.Filesystem {
	case "xfs":
		args = append(args, "-K")
	case "ext4":
		args = append(args, "-E", "nodiscard")
	case "btrfs":
		args = append(args, "--nodiscard")
	}
	args = append(args, volume.DevicePath)
	return []DiskOperation{
		{Name: "format-wipe-volume-" + volume.Name, Command: "wipefs", Args: []string{"--all", volume.DevicePath}, Destructive: true},
		{Name: "format-volume-" + volume.Name, Command: "mkfs." + volume.Filesystem, Args: args, Destructive: true},
	}
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

	for _, volume := range plan.VolumeMounts {
		target := targetPath(targetMountPrefix, volume.MountPath)
		if !mountExists(facts.Mounts, target) {
			return fmt.Errorf("volume %q is not mounted at %s", volume.Name, target)
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
