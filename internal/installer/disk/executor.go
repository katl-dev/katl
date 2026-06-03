package disk

import (
	"context"
	"errors"
	"fmt"
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
	Destructive bool
}

var ErrDestructiveInstallNotAllowed = errors.New("destructive install is not allowed")

func (e DiskExecutor) Execute(ctx context.Context, request DiskExecutionRequest) (DiskExecutionResult, error) {
	if err := checkBootPlan(request.Plan); err != nil {
		return DiskExecutionResult{}, err
	}
	operations := BuildDiskOperations(request.Plan, request.TargetMountPrefix)
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
		if err := e.Commands.Run(ctx, operation.Command, operation.Args...); err != nil {
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

func BuildDiskOperations(plan DiskLayoutPlan, targetMountPrefix string) []DiskOperation {
	if targetMountPrefix == "" {
		targetMountPrefix = "/mnt/target"
	}

	operations := []DiskOperation{
		{Name: "wipe-target-signatures", Command: "wipefs", Args: []string{"--all", plan.TargetDiskPath}, Destructive: true},
		{Name: "create-gpt", Command: "sfdisk", Args: []string{plan.TargetDiskPath}, Destructive: true},
	}

	for _, partition := range plan.Partitions {
		switch partition.Name {
		case "root-a", "root-b":
			if RootSlot(partition.Name) == plan.Boot.RootSlot {
				operations = append(operations, DiskOperation{Name: "write-" + partition.Name, Command: "katlos-write-root-slot", Args: []string{partition.GPTLabel}, Destructive: true})
			}
		default:
			operations = append(operations, DiskOperation{Name: "format-" + partition.Name, Command: "mkfs." + partition.Filesystem, Args: []string{"-L", partition.GPTLabel}, Destructive: true})
		}
		if partition.MountPath != "" && partition.Name != "root-a" {
			name := "mount-" + partition.Name
			if partition.Name == "state" {
				name = "mount-state"
			}
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
		operations = append(operations, DiskOperation{Name: "mount-extra-" + extra.Name, Command: "mount", Args: []string{extra.DevicePath, targetMountPrefix + extra.MountPath}})
	}

	return operations
}

func ValidateAppliedLayout(facts HardwareFacts, plan DiskLayoutPlan) error {
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
	if !mountExists(facts.Mounts, "/var") {
		return fmt.Errorf("state partition is not mounted at /var")
	}
	if !mountExists(facts.Mounts, "/efi") {
		return fmt.Errorf("ESP partition is not mounted at /efi")
	}
	if _, ok := findPartitionByName(plan.Partitions, "xbootldr"); ok && !mountExists(facts.Mounts, "/boot") {
		return fmt.Errorf("XBOOTLDR partition is not mounted at /boot")
	}

	for _, extra := range plan.ExtraMounts {
		if !mountExists(facts.Mounts, extra.MountPath) {
			return fmt.Errorf("extra disk %q is not mounted at %s", extra.Name, extra.MountPath)
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
