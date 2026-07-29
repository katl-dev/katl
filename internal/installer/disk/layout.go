package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

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
	GPTLabelEtcd     = "KATL_ETCD"
)

type TargetDiskSelector = discovery.TargetDiskSelector
type PartitionSelector = discovery.PartitionSelector
type HardwareFacts = discovery.HardwareFacts
type BlockDevice = discovery.BlockDevice
type SignatureReport = discovery.SignatureReport
type MountFact = discovery.MountFact

const (
	DeviceDisk      = discovery.DeviceDisk
	DevicePartition = discovery.DevicePartition
)

type DiskLayoutRequest struct {
	TargetDisk         TargetDiskSelector
	ESPSizeMiB         uint64
	XBOOTLDRSizeMiB    uint64
	RootA              RootSlotRequest
	RootB              RootSlotRequest
	State              StatePartitionRequest
	Etcd               *FixedPartitionRequest
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

type FixedPartitionRequest struct {
	Filesystem string
	SizeMiB    uint64
}

type VolumeRequest struct {
	Name       string
	Disk       *TargetDiskSelector
	Partition  *PartitionSelector
	Filesystem string
	Wipe       bool
}

type RootSlot string

const (
	RootSlotA RootSlot = "root-a"
	RootSlotB RootSlot = "root-b"
)

type DiskLayoutPlan struct {
	TargetDiskPath string
	Partitions     []PartitionPlan
	VolumeMounts   []VolumePlan
	Boot           BootTargetMetadata
	Signatures     []SignatureReport
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
	Name        string
	TargetKind  string
	DevicePath  string
	MountSource string
	Filesystem  string
	MountPath   string
	Wipe        bool
	Repartition bool
	TypeUUID    string
	Signatures  []SignatureReport
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

	target, err := discovery.MatchTargetDisk(facts, normalized.TargetDisk)
	if err != nil {
		return DiskLayoutPlan{}, fmt.Errorf("match target disk: %w", err)
	}

	partitions, err := planTargetPartitions(target.Device, normalized)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	volumeMounts, err := PlanVolumes(facts, target.Device, normalized.Volumes)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	boot, err := planBootTarget(normalized.InitialRootSlot)
	if err != nil {
		return DiskLayoutPlan{}, err
	}

	return DiskLayoutPlan{
		TargetDiskPath: target.Device.Path,
		Partitions:     partitions,
		VolumeMounts:   volumeMounts,
		Boot:           boot,
		Signatures:     target.Signatures,
	}, nil
}

func PlanVolumes(facts HardwareFacts, target BlockDevice, requests []VolumeRequest) ([]VolumePlan, error) {
	seenNames := make(map[string]struct{}, len(requests))
	plans := make([]VolumePlan, 0, len(requests))
	for _, request := range requests {
		if err := validateMountName("volume", request.Name); err != nil {
			return nil, err
		}
		if _, exists := seenNames[request.Name]; exists {
			return nil, fmt.Errorf("volume name %q is duplicated", request.Name)
		}
		seenNames[request.Name] = struct{}{}

		if (request.Disk == nil) == (request.Partition == nil) {
			return nil, fmt.Errorf("volume %q must select exactly one of disk or partition", request.Name)
		}
		if !IsSupportedVolumeFilesystem(request.Filesystem) {
			return nil, fmt.Errorf("volume %q filesystem %q is unsupported", request.Name, request.Filesystem)
		}

		var plan VolumePlan
		if request.Disk != nil {
			match, err := discovery.MatchTargetDisk(facts, *request.Disk)
			if err != nil {
				return nil, fmt.Errorf("volume %q disk: %w", request.Name, err)
			}
			if match.Device.Path == target.Path {
				return nil, fmt.Errorf("volume %q resolves to target root disk %s", request.Name, target.Path)
			}
			label := volumePartitionLabel(request.Name)
			if request.Wipe {
				plan = VolumePlan{
					Name: request.Name, TargetKind: "disk", DevicePath: match.Device.Path,
					MountSource: "/dev/disk/by-partlabel/" + label, Filesystem: request.Filesystem,
					MountPath: VolumeMountRoot + "/" + request.Name, Wipe: true, Repartition: true,
					TypeUUID: volumePartitionTypeUUID(request.Name), Signatures: match.Signatures,
				}
			} else {
				partition, err := volumePartitionOnDisk(match.Device, label)
				if err != nil {
					return nil, fmt.Errorf("volume %q disk: %w", request.Name, err)
				}
				plan = VolumePlan{
					Name: request.Name, TargetKind: "disk", DevicePath: partition.Path,
					MountSource: "/dev/disk/by-partlabel/" + label, Filesystem: request.Filesystem,
					MountPath: VolumeMountRoot + "/" + request.Name, Signatures: collectVolumeSignatures(partition),
				}
				if err := validateReusableVolume(plan, partition.FilesystemSignature); err != nil {
					return nil, err
				}
			}
		} else {
			match, err := discovery.MatchPartition(facts, *request.Partition)
			if err != nil {
				return nil, fmt.Errorf("volume %q partition: %w", request.Name, err)
			}
			if match.ParentDisk.Path == target.Path {
				return nil, fmt.Errorf("volume %q partition is on target root disk %s", request.Name, target.Path)
			}
			if isKatlPartitionLabel(match.Device.GPTLabel) {
				return nil, fmt.Errorf("volume %q resolves to Katl-managed partition %s", request.Name, match.Device.GPTLabel)
			}
			mountSource, err := persistentPartitionPath(match.Device, *request.Partition)
			if err != nil {
				return nil, fmt.Errorf("volume %q partition: %w", request.Name, err)
			}
			plan = VolumePlan{
				Name: request.Name, TargetKind: "partition", DevicePath: match.Device.Path, MountSource: mountSource,
				Filesystem: request.Filesystem, MountPath: VolumeMountRoot + "/" + request.Name, Wipe: request.Wipe, Signatures: match.Signatures,
			}
			if err := validateReusableVolume(plan, match.Device.FilesystemSignature); err != nil {
				return nil, err
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func volumePartitionLabel(name string) string {
	return "u-" + name
}

func volumePartitionOnDisk(disk BlockDevice, label string) (BlockDevice, error) {
	var matches []BlockDevice
	for _, partition := range disk.Partitions {
		if partition.GPTLabel == label {
			matches = append(matches, partition)
		}
	}
	switch len(matches) {
	case 0:
		return BlockDevice{}, fmt.Errorf("partition label %s was not found; set wipe to true to initialize this disk", label)
	case 1:
		return matches[0], nil
	default:
		return BlockDevice{}, fmt.Errorf("partition label %s matched %d partitions", label, len(matches))
	}
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

func persistentPartitionPath(device BlockDevice, selector PartitionSelector) (string, error) {
	switch {
	case selector.ByID != "":
		return selector.ByID, nil
	case selector.PartUUID != "":
		return "PARTUUID=" + selector.PartUUID, nil
	case selector.FilesystemUUID != "":
		return "UUID=" + selector.FilesystemUUID, nil
	case selector.PartLabel != "":
		return "/dev/disk/by-partlabel/" + selector.PartLabel, nil
	default:
		return "", fmt.Errorf("stable partition identity is required")
	}
}

func isKatlPartitionLabel(label string) bool {
	return slices.Contains([]string{
		GPTLabelESP,
		GPTLabelXBOOTLDR,
		GPTLabelRootA,
		GPTLabelRootB,
		GPTLabelState,
		GPTLabelEtcd,
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
	if request.Etcd != nil && request.Etcd.SizeMiB == 0 {
		return DiskLayoutRequest{}, fmt.Errorf("etcd partition size is required when enabled")
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
	if request.Etcd != nil {
		fixedMiB += request.Etcd.SizeMiB
	}
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

	partitions = append(partitions,
		PartitionPlan{Name: "root-a", GPTLabel: GPTLabelRootA, Type: "root-x86-64", Filesystem: "squashfs", MountPath: "/", SizeMiB: request.RootA.SizeMiB},
		PartitionPlan{Name: "root-b", GPTLabel: GPTLabelRootB, Type: "root-x86-64", Filesystem: "squashfs", SizeMiB: request.RootB.SizeMiB},
	)

	if request.Etcd != nil {
		partitions = append(partitions, PartitionPlan{Name: "etcd", GPTLabel: GPTLabelEtcd, Type: "linux-generic", Filesystem: request.Etcd.Filesystem, MountPath: "/var/lib/etcd", SizeMiB: request.Etcd.SizeMiB})
	}

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
