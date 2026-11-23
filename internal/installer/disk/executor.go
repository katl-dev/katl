package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
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
	BeforeOperation    func(DiskOperation) error
	// Discovery rediscovers hardware to revalidate a volume disk before each
	// destructive command on it.
	Discovery discovery.DiscoverySource
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
	// Partition names the Katl GPT label of the target-disk partition this
	// operation addresses; it runs only once that partition is bound.
	Partition string
	// Volume names the data volume this operation addresses. A destructive
	// volume operation runs only after the volume disk is revalidated, and any
	// other runs only once the volume is bound.
	Volume string
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
	operations, err := BuildDiskOperations(request.Plan, request.TargetMountPrefix)
	if err != nil {
		return DiskExecutionResult{}, err
	}
	return e.executeOperations(ctx, request, operations)
}

func (e DiskExecutor) ExecuteGroup(ctx context.Context, request DiskExecutionRequest, group DiskOperationGroup) (DiskExecutionResult, error) {
	if err := checkBootPlan(request.Plan); err != nil {
		return DiskExecutionResult{}, err
	}
	operations, err := BuildDiskOperations(request.Plan, request.TargetMountPrefix)
	if err != nil {
		return DiskExecutionResult{}, err
	}
	return e.executeOperations(ctx, request, filterOperations(operations, group))
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
	volumes := volumeChecker{executor: e, plan: request.Plan}
	if hasDestructiveOperations(operations) {
		if err := volumes.checkAll(ctx); err != nil {
			return DiskExecutionResult{}, err
		}
	}
	for _, operation := range operations {
		if operation.Partition != "" {
			if _, err := SystemPartitionDevice(request.Plan, operation.Partition); err != nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
			}
		}
		if operation.Volume != "" {
			if err := volumes.check(ctx, operation); err != nil {
				return DiskExecutionResult{}, fmt.Errorf("%s: %w", operation.Name, err)
			}
		}
		// Any command may change the disks, so the next check rediscovers.
		volumes.facts = nil
		if e.BeforeOperation != nil {
			if err := e.BeforeOperation(operation); err != nil {
				return DiskExecutionResult{}, err
			}
		}
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
			if installBoot == nil {
				installBoot = InstallBoot
			}
			boot, err := installBoot(ctx, BootRequest{
				Commands:          e.Commands,
				Plan:              request.Plan,
				TargetMountPrefix: request.TargetMountPrefix,
			})
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

func BuildDiskOperations(plan DiskLayoutPlan, targetMountPrefix string) ([]DiskOperation, error) {
	if targetMountPrefix == "" {
		targetMountPrefix = "/mnt/target"
	}
	target, err := diskDevice(plan.TargetDisk)
	if err != nil {
		return nil, fmt.Errorf("target disk: %w", err)
	}

	operations := []DiskOperation{
		{Name: "wipe-target-signatures", Command: "wipefs", Args: []string{"--all", target}, Destructive: true},
		{Name: "create-gpt", Command: "systemd-repart", Args: repartArgs(target), Definitions: systemDefinitions(plan), Env: []string{"SYSTEMD_REPART_MKFS_OPTIONS_EXT4=-O verity"}, Destructive: true},
		{Name: "reread-partitions", Command: "partprobe", Args: []string{target}},
		{Name: "settle-partitions", Command: "udevadm", Args: []string{"settle"}},
	}

	for _, partition := range plan.Partitions {
		switch partition.Name {
		case "root-a", "root-b":
			if RootSlot(partition.Name) == plan.Boot.RootSlot {
				operations = append(operations, DiskOperation{Name: "write-" + partition.Name, Command: "katlos-write-root-slot", Args: []string{partition.GPTLabel}, Destructive: true, Partition: partition.GPTLabel})
			}
		}
		if partition.MountPath != "" && partition.Name != "root-a" {
			name := "mount-" + partition.Name
			if partition.Name == "state" {
				name = "mount-state"
			}
			operations = append(operations, DiskOperation{Name: "create-mountpoint-" + partition.Name, Command: "mkdir", Args: []string{"-p", targetMountPrefix + partition.MountPath}})
			source, _ := SystemPartitionDevice(plan, partition.GPTLabel)
			operations = append(operations, DiskOperation{Name: name, Command: "mount", Args: []string{source, targetMountPrefix + partition.MountPath}, Partition: partition.GPTLabel})
		}
	}

	if _, ok := findPartitionByName(plan.Partitions, "esp"); ok {
		operations = append(operations, DiskOperation{Name: "install-systemd-boot", Command: "bootctl", Args: []string{"install"}, Destructive: true})
	}

	for _, volume := range plan.VolumeMounts {
		prepare, err := volumeOperations(volume)
		if err != nil {
			return nil, err
		}
		operations = append(operations, prepare...)
		operations = append(operations, DiskOperation{Name: "create-mountpoint-volume-" + volume.Name, Command: "mkdir", Args: []string{"-p", targetMountPrefix + volume.MountPath}})
		operations = append(operations, DiskOperation{Name: "mount-volume-" + volume.Name, Command: "mount", Args: []string{volume.MountSource, targetMountPrefix + volume.MountPath}, Volume: volume.Name})
	}

	return operations, nil
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

// repartArgs seeds partition and disk GUIDs randomly so every install gets
// fresh identities; the default machine-id seed would repeat them across
// installs that share an installer machine-id.
func repartArgs(device string) []string {
	return []string{"--dry-run=no", "--empty=force", "--discard=no", "--seed=random", "--definitions={definitions}", device}
}

// PrepareVolumes executes the volume provisioning plans of a live apply, as
// install does: every volume is rechecked against hardware rediscovered
// through source before the first destructive command, and its own volume
// again before each one. It returns the volumes a destructive command was
// started on, even on failure, since their earlier storage may be gone.
func PrepareVolumes(ctx context.Context, commands CommandRunner, source discovery.DiscoverySource, plans []VolumePlan) ([]string, error) {
	var operations []DiskOperation
	for _, plan := range plans {
		prepare, err := volumeOperations(plan)
		if err != nil {
			return nil, err
		}
		operations = append(operations, prepare...)
	}
	var changed []string
	executor := DiskExecutor{Commands: commands, Discovery: source, BeforeOperation: func(operation DiskOperation) error {
		if operation.Destructive && !slices.Contains(changed, operation.Volume) {
			changed = append(changed, operation.Volume)
		}
		return nil
	}}
	request := DiskExecutionRequest{Plan: DiskLayoutPlan{VolumeMounts: plans}, AllowDestructive: true}
	_, err := executor.executeOperations(ctx, request, operations)
	return changed, err
}

// volumeChecker rechecks volume targets against discovered hardware. Facts
// stay current until the next command runs, so the check before the first
// destructive command and the check of that command share one discovery.
type volumeChecker struct {
	executor DiskExecutor
	plan     DiskLayoutPlan
	facts    *HardwareFacts
}

func (c *volumeChecker) current(ctx context.Context) (HardwareFacts, error) {
	if c.facts != nil {
		return *c.facts, nil
	}
	if c.executor.Discovery == nil {
		return HardwareFacts{}, fmt.Errorf("%w: hardware discovery is required to recheck volume disks", ErrVolumeRefused)
	}
	// udev settles first so aliases dropped while the kernel rereads a
	// partition table are back before the disks are compared.
	if err := c.executor.Commands.Run(ctx, "udevadm", "settle"); err != nil {
		return HardwareFacts{}, fmt.Errorf("settle udev before rechecking volume disks: %w", err)
	}
	facts, err := c.executor.Discovery.Discover(ctx)
	if err != nil {
		return HardwareFacts{}, fmt.Errorf("rediscover volume disks: %w", err)
	}
	c.facts = &facts
	return facts, nil
}

// checkAll rechecks every planned volume, so a changed volume disk is refused
// before any destructive command, including those on the system disk.
func (c *volumeChecker) checkAll(ctx context.Context) error {
	if len(c.plan.VolumeMounts) == 0 {
		return nil
	}
	facts, err := c.current(ctx)
	if err != nil {
		return err
	}
	for _, volume := range c.plan.VolumeMounts {
		if err := checkVolume(facts, volume); err != nil {
			return fmt.Errorf("%w: volume %q: %w", ErrVolumeRefused, volume.Name, err)
		}
	}
	return nil
}

// check enforces the bound-disk rule for one volume operation: a destructive
// command runs only on a rechecked target, and any other only once the volume
// is bound.
func (c *volumeChecker) check(ctx context.Context, operation DiskOperation) error {
	volume, ok := findVolume(c.plan.VolumeMounts, operation.Volume)
	if !ok {
		return fmt.Errorf("volume %q is not in the disk plan", operation.Volume)
	}
	if !operation.Destructive {
		if volume.MountSource == "" {
			return fmt.Errorf("volume %q is not bound to a partition on disk %s", volume.Name, volume.Disk.Path)
		}
		return nil
	}
	facts, err := c.current(ctx)
	if err != nil {
		return err
	}
	if err := checkVolume(facts, volume); err != nil {
		return fmt.Errorf("%w: volume %q: %w", ErrVolumeRefused, volume.Name, err)
	}
	return nil
}

func findVolume(volumes []VolumePlan, name string) (VolumePlan, bool) {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return VolumePlan{}, false
}

func volumeOperations(volume VolumePlan) ([]DiskOperation, error) {
	if !volume.Repartition && !volume.Wipe {
		return nil, nil
	}
	device, err := volumeDevice(volume)
	if err != nil {
		return nil, fmt.Errorf("volume %q: %w", volume.Name, err)
	}
	if volume.Repartition {
		return []DiskOperation{
			{Name: "repart-volume-" + volume.Name, Command: "systemd-repart", Args: repartArgs(device), Definitions: []string{volumeDefinition(volume)}, Destructive: true, Volume: volume.Name},
			{Name: "settle-volume-" + volume.Name, Command: "udevadm", Args: []string{"settle"}},
		}, nil
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
	args = append(args, device)
	return []DiskOperation{
		{Name: "format-wipe-volume-" + volume.Name, Command: "wipefs", Args: []string{"--all", device}, Destructive: true, Volume: volume.Name},
		{Name: "format-volume-" + volume.Name, Command: "mkfs." + volume.Filesystem, Args: args, Destructive: true, Volume: volume.Name},
	}, nil
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

func ValidateAppliedLayout(facts HardwareFacts, plan DiskLayoutPlan) error {
	return ValidateAppliedLayoutAt(facts, plan, "")
}

// ValidateAppliedLayoutAt checks the installed layout through discovered
// hardware: the Katl partitions are on the bound target disk and mounted from
// it, and each volume is mounted from its bound partition.
func ValidateAppliedLayoutAt(facts HardwareFacts, plan DiskLayoutPlan, targetMountPrefix string) error {
	if targetMountPrefix == "" {
		targetMountPrefix = "/"
	}
	target, err := discovery.MatchBoundDisk(facts, plan.TargetDisk)
	if err != nil {
		return fmt.Errorf("target disk: %w", err)
	}

	partitionsByLabel := make(map[string]BlockDevice)
	for _, partition := range target.Partitions {
		partitionsByLabel[partition.GPTLabel] = partition
	}
	for _, expected := range plan.Partitions {
		partition, ok := partitionsByLabel[expected.GPTLabel]
		if !ok {
			return fmt.Errorf("partition label %s not found", expected.GPTLabel)
		}
		if bound := plan.PartitionUUIDs[expected.GPTLabel]; bound != "" && !strings.EqualFold(strings.TrimSpace(partition.PartitionUUID), bound) {
			return fmt.Errorf("%s on %s has PARTUUID %s, not the bound %s", expected.GPTLabel, target.Path, partition.PartitionUUID, bound)
		}
	}
	if _, ok := partitionsByLabel[plan.Boot.RootPartitionLabel]; !ok {
		return fmt.Errorf("boot root label %s not found", plan.Boot.RootPartitionLabel)
	}
	for _, expected := range plan.Partitions {
		if expected.MountPath == "" || expected.Name == "root-a" {
			continue
		}
		target := targetPath(targetMountPrefix, expected.MountPath)
		source, ok := mountSource(facts.Mounts, target)
		if !ok {
			return fmt.Errorf("%s partition is not mounted at %s", expected.GPTLabel, target)
		}
		if want := partitionsByLabel[expected.GPTLabel].Path; source != want {
			return fmt.Errorf("%s is mounted from %s, not the target disk partition %s", target, source, want)
		}
	}

	for _, volume := range plan.VolumeMounts {
		target := targetPath(targetMountPrefix, volume.MountPath)
		source, ok := mountSource(facts.Mounts, target)
		if !ok {
			return fmt.Errorf("volume %q is not mounted at %s", volume.Name, target)
		}
		partition, err := boundVolumePartition(facts, volume)
		if err != nil {
			return fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		binding, err := bindVolume(facts, volume)
		if err != nil {
			return fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		if bound, _ := VolumeBindingMountSource(binding); volume.MountSource == "" || bound != volume.MountSource {
			return fmt.Errorf("volume %q partition %s is %s, not the bound %q", volume.Name, partition.Path, bound, volume.MountSource)
		}
		if source != partition.Path {
			return fmt.Errorf("%s is mounted from %s, not volume %q partition %s", target, source, volume.Name, partition.Path)
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

func mountSource(mounts []MountFact, target string) (string, bool) {
	for _, mount := range mounts {
		if mount.Target == target {
			return mount.Source, true
		}
	}
	return "", false
}
