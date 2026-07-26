package disk

import (
	"fmt"
	"slices"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

const (
	DefaultESPSizeMiB  = 512
	MinimumRootMiB     = 1024
	ExtraDiskMountRoot = "/var/lib/katl/mnt"

	GPTLabelESP      = "KATL_ESP"
	GPTLabelXBOOTLDR = "KATL_XBOOTLDR"
	GPTLabelRootA    = "KATL_ROOT_A"
	GPTLabelRootB    = "KATL_ROOT_B"
	GPTLabelState    = "KATL_STATE"
	GPTLabelEtcd     = "KATL_ETCD"
)

type TargetDiskSelector = discovery.TargetDiskSelector
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
	ExtraDisks         []ExtraDiskRequest
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

type ExtraDiskRequest struct {
	Name       string
	Selector   TargetDiskSelector
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
	ExtraMounts    []ExtraDiskPlan
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

type ExtraDiskPlan struct {
	Name        string
	DevicePath  string
	MountSource string
	Filesystem  string
	MountPath   string
	Wipe        bool
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

	extraMounts, err := planExtraDisks(facts, target.Device, normalized.ExtraDisks)
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
		ExtraMounts:    extraMounts,
		Boot:           boot,
		Signatures:     target.Signatures,
	}, nil
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

func planExtraDisks(facts HardwareFacts, target BlockDevice, requests []ExtraDiskRequest) ([]ExtraDiskPlan, error) {
	seenNames := make(map[string]struct{}, len(requests))
	plans := make([]ExtraDiskPlan, 0, len(requests))
	for _, request := range requests {
		if err := validateExtraDiskName(request.Name); err != nil {
			return nil, err
		}
		if _, exists := seenNames[request.Name]; exists {
			return nil, fmt.Errorf("extra disk name %q is duplicated", request.Name)
		}
		seenNames[request.Name] = struct{}{}
		match, err := discovery.MatchTargetDisk(facts, request.Selector)
		if err != nil {
			return nil, fmt.Errorf("extra disk %q: %w", request.Name, err)
		}
		if match.Device.Path == target.Path {
			return nil, fmt.Errorf("extra disk %q resolves to target root disk %s", request.Name, target.Path)
		}
		mountSource, err := persistentDevicePath(match.Device, request.Selector)
		if err != nil {
			return nil, fmt.Errorf("extra disk %q: %w", request.Name, err)
		}

		filesystem := request.Filesystem
		if filesystem == "" {
			filesystem = "ext4"
		}
		if !IsSupportedExtraDiskFilesystem(filesystem) {
			return nil, fmt.Errorf("extra disk %q filesystem %q is unsupported", request.Name, filesystem)
		}
		if !request.Wipe {
			switch {
			case match.Device.FilesystemSignature == "":
				return nil, fmt.Errorf("extra disk %q has no reusable filesystem; set wipe to true to format it as %s", request.Name, filesystem)
			case match.Device.FilesystemSignature != filesystem:
				return nil, fmt.Errorf("extra disk %q has filesystem %s, not requested %s; set wipe to true to reformat it", request.Name, match.Device.FilesystemSignature, filesystem)
			}
		}
		plans = append(plans, ExtraDiskPlan{
			Name:        request.Name,
			DevicePath:  match.Device.Path,
			MountSource: mountSource,
			Filesystem:  filesystem,
			MountPath:   ExtraDiskMountRoot + "/" + request.Name,
			Wipe:        request.Wipe,
			Signatures:  match.Signatures,
		})
	}

	return plans, nil
}

func IsSupportedExtraDiskFilesystem(filesystem string) bool {
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

func validateExtraDiskName(name string) error {
	if name == "" {
		return fmt.Errorf("extra disk name is required")
	}
	for i, value := range name {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-' && i > 0 && i < len(name)-1 {
			continue
		}
		return fmt.Errorf("extra disk name %q must use lowercase letters, digits, and internal dashes", name)
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
