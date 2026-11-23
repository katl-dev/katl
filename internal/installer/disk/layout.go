package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

const (
	DefaultESPSizeMiB = 512
	MinimumRootMiB    = 1024
	VolumeMountRoot   = "/var/mnt"

	GPTLabelESP      = "KATL_ESP"
	GPTLabelXBOOTLDR = "KATL_XBOOTLDR"
	GPTLabelRootA    = "KATL_ROOT_A"
	GPTLabelRootB    = "KATL_ROOT_B"
	GPTLabelState    = "KATL_STATE"
)

type (
	TargetDiskSelector = discovery.TargetDiskSelector
	PartitionSelector  = discovery.PartitionSelector
	HardwareFacts      = discovery.HardwareFacts
	BlockDevice        = discovery.BlockDevice
	SignatureReport    = discovery.SignatureReport
	MountFact          = discovery.MountFact
)

const (
	DeviceDisk      = discovery.DeviceDisk
	DevicePartition = discovery.DevicePartition
)

type DiskLayoutRequest struct {
	TargetDisk TargetDiskSelector
	// RetainedTarget binds planning to a disk an interrupted install already
	// resolved, instead of resolving TargetDisk again.
	RetainedTarget     *discovery.DiskIdentity
	ESPSizeMiB         uint64
	XBOOTLDRSizeMiB    uint64
	RootA              RootSlotRequest
	RootB              RootSlotRequest
	State              StatePartitionRequest
	Volumes            []VolumeRequest
	InitialRootSlot    RootSlot
	RuntimeRootSizeMiB uint64
}

type RootSlotRequest struct {
	SizeMiB uint64
}

type StatePartitionRequest struct {
	Filesystem string
	MinSizeMiB uint64
}

type VolumeRequest struct {
	Name       string
	Disk       *TargetDiskSelector
	Partition  *PartitionSelector
	Filesystem string
	Wipe       bool
	// Retained binds planning to the disk an earlier attempt already resolved
	// and changed, instead of resolving the selector again.
	Retained *VolumeTarget
}

type RootSlot string

const (
	RootSlotA RootSlot = "root-a"
	RootSlotB RootSlot = "root-b"
)

type DiskLayoutPlan struct {
	TargetDiskPath string
	TargetDisk     discovery.DiskIdentity
	Partitions     []PartitionPlan
	VolumeMounts   []VolumePlan
	Boot           BootTargetMetadata
	Signatures     []SignatureReport
	// PartitionUUIDs maps each Katl GPT label to its PARTUUID on the target
	// disk once BindSystemPartitions has run after partitioning.
	PartitionUUIDs map[string]string
}

type PartitionPlan struct {
	Name       string
	GPTLabel   string
	Type       string
	Filesystem string
	MountPath  string
	SizeMiB    uint64
	Remaining  bool
}

type VolumePlan struct {
	Name       string
	TargetKind string
	// Disk is the disk holding the volume. It is revalidated before every
	// destructive command, and the volume's partition is looked up only on it.
	Disk discovery.DiskIdentity
	// Partition identifies the selected existing partition on Disk. It is
	// zero when Repartition creates the partition.
	Partition PartitionIdentity
	// MountSource is the PARTUUID= or UUID= source BindVolumePlans assigns
	// once the volume's partition is bound; it is empty until then.
	MountSource string
	Filesystem  string
	MountPath   string
	Wipe        bool
	Repartition bool
	TypeUUID    string
	Signatures  []SignatureReport
}

type VolumeBinding struct {
	Name           string
	PartitionUUID  string
	FilesystemUUID string
}

type BootTargetMetadata struct {
	RootSlot           RootSlot
	RootPartitionLabel string
	RootParameter      string
	PartitionUUIDToken string
}

func PlanDiskLayout(facts HardwareFacts, request DiskLayoutRequest) (DiskLayoutPlan, error) {
	normalized, err := normalizeLayoutRequest(request)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	var target discovery.TargetDiskMatch
	if normalized.RetainedTarget != nil {
		target, err = discovery.MatchRetainedDisk(facts, *normalized.RetainedTarget, normalized.TargetDisk)
	} else {
		target, err = discovery.MatchTargetDisk(facts, normalized.TargetDisk)
	}
	if err != nil {
		return DiskLayoutPlan{}, fmt.Errorf("match target disk: %w", err)
	}

	if err := checkAddressable(target.Device); err != nil {
		return DiskLayoutPlan{}, fmt.Errorf("target disk: %w", err)
	}
	partitions, err := planTargetPartitions(target.Device, normalized)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	volumeMounts, err := PlanVolumes(facts, target.Device, normalized.Volumes, nil)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	boot, err := planBootTarget(normalized.InitialRootSlot)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	return DiskLayoutPlan{
		TargetDiskPath: target.Device.Path,
		TargetDisk:     discovery.IdentityOf(target.Device),
		Partitions:     partitions,
		VolumeMounts:   volumeMounts,
		Boot:           boot,
		Signatures:     target.Signatures,
	}, nil
}

func VolumeBindingMountSource(binding VolumeBinding) (string, error) {
	switch {
	case strings.TrimSpace(binding.PartitionUUID) != "":
		return "PARTUUID=" + strings.TrimSpace(binding.PartitionUUID), nil
	case strings.TrimSpace(binding.FilesystemUUID) != "":
		return "UUID=" + strings.TrimSpace(binding.FilesystemUUID), nil
	default:
		return "", fmt.Errorf("volume %q has no discovered partition or filesystem UUID", binding.Name)
	}
}

// BindingFault names the part of the binding rule a recorded volume binding
// fails.
type BindingFault string

const (
	// BindingUnrecorded is a binding without a PARTUUID.
	BindingUnrecorded BindingFault = "unrecorded"
	// BindingAbsent is a PARTUUID that no attached partition carries.
	BindingAbsent BindingFault = "absent"
	// BindingDuplicated is a PARTUUID that more than one partition carries,
	// as a cloned disk does.
	BindingDuplicated BindingFault = "duplicated"
	// BindingOnSystemDisk is a PARTUUID carried by a partition of the system
	// disk.
	BindingOnSystemDisk BindingFault = "system-disk"
	// BindingStale is a partition whose filesystem UUID is not the recorded
	// one, because the filesystem was recreated after the binding was made.
	BindingStale BindingFault = "stale"
)

// VolumeBindingError reports a recorded volume binding that does not resolve
// by the binding rule.
type VolumeBindingError struct {
	Volume string
	Fault  BindingFault
	Cause  string
}

func (e *VolumeBindingError) Error() string {
	return fmt.Sprintf("volume %q %s", e.Volume, e.Cause)
}

// MatchVolumeBinding returns the partition a recorded binding resolves to by
// the binding rule. system is the disk holding the node's root.
func MatchVolumeBinding(facts HardwareFacts, system BlockDevice, binding VolumeBinding) (BlockDevice, error) {
	_, partition, err := matchVolumeBinding(facts, system, binding)
	return partition, err
}

// matchVolumeBinding applies the binding rule and returns the partition a
// binding resolves to and the disk holding it. Exactly one attached partition
// must carry the recorded PARTUUID, it must not be on the system disk, and,
// where a filesystem UUID is recorded, the partition's filesystem UUID must
// match it. Every destructive volume step changes one of the two identifiers,
// so a stale binding is visible on the disk itself.
func matchVolumeBinding(facts HardwareFacts, system BlockDevice, binding VolumeBinding) (BlockDevice, BlockDevice, error) {
	partUUID := strings.TrimSpace(binding.PartitionUUID)
	if partUUID == "" {
		return BlockDevice{}, BlockDevice{}, &VolumeBindingError{Volume: binding.Name, Fault: BindingUnrecorded, Cause: "has no recorded PARTUUID"}
	}
	var disks, matches []BlockDevice
	for _, candidate := range facts.BlockDevices {
		for _, partition := range candidate.Partitions {
			if strings.EqualFold(strings.TrimSpace(partition.PartitionUUID), partUUID) {
				disks = append(disks, candidate)
				matches = append(matches, partition)
			}
		}
	}
	switch len(matches) {
	case 0:
		return BlockDevice{}, BlockDevice{}, &VolumeBindingError{Volume: binding.Name, Fault: BindingAbsent, Cause: fmt.Sprintf("is bound to PARTUUID %s, which no attached partition carries", partUUID)}
	case 1:
	default:
		paths := make([]string, 0, len(matches))
		for _, match := range matches {
			paths = append(paths, match.Path)
		}
		return BlockDevice{}, BlockDevice{}, &VolumeBindingError{Volume: binding.Name, Fault: BindingDuplicated, Cause: fmt.Sprintf("is bound to PARTUUID %s, which %d partitions carry: %s", partUUID, len(matches), strings.Join(paths, ", "))}
	}
	disk, partition := disks[0], matches[0]
	if disk.Path == system.Path {
		return BlockDevice{}, BlockDevice{}, &VolumeBindingError{Volume: binding.Name, Fault: BindingOnSystemDisk, Cause: fmt.Sprintf("is bound to PARTUUID %s, which is %s on the system disk %s", partUUID, partition.Path, disk.Path)}
	}
	recorded := strings.TrimSpace(binding.FilesystemUUID)
	if current := strings.TrimSpace(partition.FilesystemUUID); recorded != "" && !strings.EqualFold(current, recorded) {
		if current == "" {
			current = "none"
		}
		return BlockDevice{}, BlockDevice{}, &VolumeBindingError{Volume: binding.Name, Fault: BindingStale, Cause: fmt.Sprintf("is bound to filesystem UUID %s on %s (PARTUUID %s), but that partition now has filesystem UUID %s", recorded, partition.Path, partUUID, current)}
	}
	return disk, partition, nil
}

func volumePartitionLabel(name string) string {
	return "u-" + name
}

func volumePartitionTypeUUID(name string) string {
	sum := sha256.Sum256([]byte("katl-volume-type:" + name))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(sum[:16])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
}

func collectVolumeSignatures(partition BlockDevice) []SignatureReport {
	var signatures []SignatureReport
	if partition.FilesystemSignature != "" {
		signatures = append(signatures, SignatureReport{DevicePath: partition.Path, Kind: "filesystem", Value: partition.FilesystemSignature})
	}
	if partition.PartitionSignature != "" {
		signatures = append(signatures, SignatureReport{DevicePath: partition.Path, Kind: "partition-type", Value: partition.PartitionSignature})
	}
	return signatures
}

func validateReusableVolume(plan VolumePlan, existingFilesystem string) error {
	if plan.Wipe {
		return nil
	}
	switch {
	case existingFilesystem == "":
		return fmt.Errorf("volume %q has no reusable filesystem; set wipe to true to format it as %s", plan.Name, plan.Filesystem)
	case existingFilesystem != plan.Filesystem:
		return fmt.Errorf("volume %q has filesystem %s, not requested %s; set wipe to true to reformat it", plan.Name, existingFilesystem, plan.Filesystem)
	default:
		return nil
	}
}

func isKatlPartitionLabel(label string) bool {
	return slices.Contains([]string{
		GPTLabelESP,
		GPTLabelXBOOTLDR,
		GPTLabelRootA,
		GPTLabelRootB,
		GPTLabelState,
	}, label)
}

func normalizeLayoutRequest(request DiskLayoutRequest) (DiskLayoutRequest, error) {
	if request.ESPSizeMiB == 0 {
		request.ESPSizeMiB = DefaultESPSizeMiB
	}
	if request.State.Filesystem == "" {
		request.State.Filesystem = "ext4"
	}
	if request.InitialRootSlot == "" {
		request.InitialRootSlot = RootSlotA
	}

	if err := validateRootSlot("root-a", request.RootA, request.RuntimeRootSizeMiB); err != nil {
		return DiskLayoutRequest{}, err
	}
	if err := validateRootSlot("root-b", request.RootB, request.RuntimeRootSizeMiB); err != nil {
		return DiskLayoutRequest{}, err
	}
	if request.State.MinSizeMiB == 0 {
		return DiskLayoutRequest{}, fmt.Errorf("state partition minimum size is required")
	}

	return request, nil
}

func validateRootSlot(name string, slot RootSlotRequest, runtimeRootSizeMiB uint64) error {
	if slot.SizeMiB < MinimumRootMiB {
		return fmt.Errorf("%s size must be at least %d MiB", name, MinimumRootMiB)
	}
	if runtimeRootSizeMiB > 0 && slot.SizeMiB < runtimeRootSizeMiB {
		return fmt.Errorf("%s size %d MiB is smaller than runtime root artifact %d MiB", name, slot.SizeMiB, runtimeRootSizeMiB)
	}
	return nil
}

func planTargetPartitions(target BlockDevice, request DiskLayoutRequest) ([]PartitionPlan, error) {
	targetMiB := target.SizeBytes / 1024 / 1024
	fixedMiB := request.ESPSizeMiB + request.XBOOTLDRSizeMiB + request.RootA.SizeMiB + request.RootB.SizeMiB
	if targetMiB < fixedMiB+request.State.MinSizeMiB {
		return nil, fmt.Errorf("target disk %s is too small: %d MiB available, %d MiB required", target.Path, targetMiB, fixedMiB+request.State.MinSizeMiB)
	}

	stateSizeMiB := targetMiB - fixedMiB
	partitions := []PartitionPlan{
		{Name: "esp", GPTLabel: GPTLabelESP, Type: "esp", Filesystem: "vfat", MountPath: "/efi", SizeMiB: request.ESPSizeMiB},
	}
	if request.XBOOTLDRSizeMiB > 0 {
		partitions = append(partitions, PartitionPlan{Name: "xbootldr", GPTLabel: GPTLabelXBOOTLDR, Type: "xbootldr", Filesystem: "vfat", MountPath: "/boot", SizeMiB: request.XBOOTLDRSizeMiB})
	}

	partitions = append(
		partitions,
		PartitionPlan{Name: "root-a", GPTLabel: GPTLabelRootA, Type: "root-x86-64", Filesystem: "squashfs", MountPath: "/", SizeMiB: request.RootA.SizeMiB},
		PartitionPlan{Name: "root-b", GPTLabel: GPTLabelRootB, Type: "root-x86-64", Filesystem: "squashfs", SizeMiB: request.RootB.SizeMiB},
	)

	partitions = append(partitions, PartitionPlan{Name: "state", GPTLabel: GPTLabelState, Type: "var", Filesystem: request.State.Filesystem, MountPath: "/var", SizeMiB: stateSizeMiB, Remaining: true})

	return partitions, nil
}

func IsSupportedVolumeFilesystem(filesystem string) bool {
	for _, capability := range extraDiskFilesystemCapabilities {
		if capability.Filesystem == filesystem {
			return true
		}
	}
	return false
}

type extraDiskFilesystemCapability struct {
	Filesystem       string
	FormatterPackage string
}

var extraDiskFilesystemCapabilities = []extraDiskFilesystemCapability{
	{Filesystem: "ext4", FormatterPackage: "e2fsprogs"},
	{Filesystem: "xfs", FormatterPackage: "xfsprogs"},
	{Filesystem: "btrfs", FormatterPackage: "btrfs-progs"},
}

func VerifyInstallerFilesystemPackages(packages map[string]string) error {
	for _, capability := range extraDiskFilesystemCapabilities {
		if packages[capability.FormatterPackage] == "" {
			return fmt.Errorf("installer %s extra-disk formatting capability is missing: expected package %s", capability.Filesystem, capability.FormatterPackage)
		}
	}
	return nil
}

func persistentDevicePath(device BlockDevice, selector discovery.TargetDiskSelector) (string, error) {
	if selector.ByID != "" {
		return selector.ByID, nil
	}
	aliases := slices.Clone(device.ByID)
	slices.Sort(aliases)
	if len(aliases) == 0 {
		return "", fmt.Errorf("selected disk %s has no durable /dev/disk/by-id path for boot-time mounting", device.Path)
	}
	return aliases[0], nil
}

func validateMountName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	for i, value := range name {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-' && i > 0 && i < len(name)-1 {
			continue
		}
		return fmt.Errorf("%s name %q must use lowercase letters, digits, and internal dashes", kind, name)
	}
	return nil
}

func planBootTarget(slot RootSlot) (BootTargetMetadata, error) {
	switch slot {
	case RootSlotA:
		return bootTarget(RootSlotA, GPTLabelRootA), nil
	case RootSlotB:
		return bootTarget(RootSlotB, GPTLabelRootB), nil
	default:
		return BootTargetMetadata{}, fmt.Errorf("unsupported initial root slot %q", slot)
	}
}

func bootTarget(slot RootSlot, label string) BootTargetMetadata {
	token := "${" + label + "_PARTUUID}"
	return BootTargetMetadata{
		RootSlot:           slot,
		RootPartitionLabel: label,
		RootParameter:      "root=PARTUUID=" + token,
		PartitionUUIDToken: token,
	}
}
