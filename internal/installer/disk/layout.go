package disk

import (
	"fmt"
	"path"
	"strings"

	"github.com/zariel/katl/internal/installer/discovery"
)

const (
	DefaultESPSizeMiB = 512
	MinimumRootMiB    = 1024

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
	MountPath  string
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
	Name       string
	DevicePath string
	Filesystem string
	MountPath  string
	Wipe       bool
	Signatures []SignatureReport
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
	seenMounts := make(map[string]string, len(requests))
	plans := make([]ExtraDiskPlan, 0, len(requests))
	for _, request := range requests {
		mountPath, err := normalizeExtraMountPath(request.MountPath)
		if err != nil {
			return nil, fmt.Errorf("extra disk %q: %w", request.Name, err)
		}
		if conflictWith, ok := mountConflict(seenMounts, mountPath); ok {
			return nil, fmt.Errorf("extra disk %q mount %s conflicts with %s", request.Name, mountPath, conflictWith)
		}
		seenMounts[mountPath] = request.Name

		match, err := discovery.MatchTargetDisk(facts, request.Selector)
		if err != nil {
			return nil, fmt.Errorf("extra disk %q: %w", request.Name, err)
		}
		if match.Device.Path == target.Path {
			return nil, fmt.Errorf("extra disk %q resolves to target root disk %s", request.Name, target.Path)
		}

		filesystem := request.Filesystem
		if filesystem == "" {
			filesystem = "ext4"
		}
		plans = append(plans, ExtraDiskPlan{
			Name:       request.Name,
			DevicePath: match.Device.Path,
			Filesystem: filesystem,
			MountPath:  mountPath,
			Wipe:       request.Wipe,
			Signatures: match.Signatures,
		})
	}

	return plans, nil
}

func normalizeExtraMountPath(mountPath string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(mountPath), "/"))
	if clean == "." || clean == "/" {
		return "", fmt.Errorf("mount path is required")
	}
	if isReservedMountPath(clean) {
		return "", fmt.Errorf("mount path %s is reserved", clean)
	}
	if !strings.HasPrefix(clean, "/srv/") && !strings.HasPrefix(clean, "/var/lib/katl/extra/") {
		return "", fmt.Errorf("mount path %s must be under /srv or /var/lib/katl/extra", clean)
	}
	return clean, nil
}

func isReservedMountPath(mountPath string) bool {
	switch mountPath {
	case "/", "/boot", "/efi", "/usr", "/etc", "/run", "/tmp", "/var", "/var/lib/kubelet", "/var/lib/containerd", "/var/lib/etcd":
		return true
	default:
		return false
	}
}

func mountConflict(seen map[string]string, candidate string) (string, bool) {
	for existing, name := range seen {
		if existing == candidate || strings.HasPrefix(existing, candidate+"/") || strings.HasPrefix(candidate, existing+"/") {
			return name, true
		}
	}
	return "", false
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
