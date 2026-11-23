package disk

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

// ErrVolumeRefused marks a volume whose disk or partition is no longer the one
// planned, or is no longer safe to use, when it is rechecked before a
// destructive command.
var ErrVolumeRefused = errors.New("volume target check failed")

// PartitionIdentity records a selected partition at planning. The partition is
// found again on its bound disk by Number and must still match every other
// field, so a partition table rewritten since planning is refused.
type PartitionIdentity struct {
	Number    uint64
	UUID      string
	Label     string
	SizeBytes uint64
}

func partitionIdentityOf(partition BlockDevice) PartitionIdentity {
	return PartitionIdentity{
		Number:    partition.PartitionNumber,
		UUID:      strings.ToLower(strings.TrimSpace(partition.PartitionUUID)),
		Label:     partition.GPTLabel,
		SizeBytes: partition.SizeBytes,
	}
}

func (p PartitionIdentity) String() string {
	fields := []string{fmt.Sprintf("size=%d", p.SizeBytes)}
	if p.UUID != "" {
		fields = append(fields, "partuuid="+p.UUID)
	}
	if p.Label != "" {
		fields = append(fields, "label="+p.Label)
	}
	return "(" + strings.Join(fields, ", ") + ")"
}

// PlanVolumes plans requests beside the volumes a running node keeps
// unchanged, which are found by their bindings; install keeps none. Every
// volume claims its disk or partition, so no request can share one with
// another request or with a kept volume.
func PlanVolumes(facts HardwareFacts, target BlockDevice, requests []VolumeRequest, kept []VolumeBinding) ([]VolumePlan, error) {
	seenNames := make(map[string]struct{}, len(requests)+len(kept))
	claims := volumeClaims{}
	for _, binding := range kept {
		seenNames[binding.Name] = struct{}{}
		plan, err := keptVolume(facts, binding)
		if err != nil {
			return nil, err
		}
		if err := claims.add(plan); err != nil {
			return nil, err
		}
	}
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

		plan, disk, partition, err := planVolume(facts, target, request)
		if err != nil {
			return nil, err
		}
		if err := claims.add(plan); err != nil {
			return nil, err
		}
		if err := validateAddressableVolume(plan, disk, partition); err != nil {
			return nil, err
		}
		if !plan.Repartition && !plan.Wipe {
			// A preserved volume is mounted by its current identity, so a clone
			// sharing it must block planning rather than surface after install.
			if _, err := bindVolume(facts, plan); err != nil {
				return nil, fmt.Errorf("volume %q: %w", plan.Name, err)
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// keptVolume describes the partition a kept volume is bound to, so requests
// are claimed around it.
func keptVolume(facts HardwareFacts, binding VolumeBinding) (VolumePlan, error) {
	disk, partition, err := matchVolumeBinding(facts, binding)
	if err != nil {
		return VolumePlan{}, err
	}
	return VolumePlan{Name: binding.Name, Disk: discovery.IdentityOf(disk), Partition: partitionIdentityOf(partition)}, nil
}

// planVolume resolves one request to its disk and, for an existing partition,
// returns that partition too.
func planVolume(facts HardwareFacts, target BlockDevice, request VolumeRequest) (VolumePlan, BlockDevice, BlockDevice, error) {
	if request.Disk != nil {
		match, err := discovery.MatchTargetDisk(facts, *request.Disk)
		if err != nil {
			return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q disk: %w", request.Name, err)
		}
		if match.Device.Path == target.Path {
			return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q resolves to target root disk %s", request.Name, target.Path)
		}
		if request.Wipe {
			return VolumePlan{
				Name: request.Name, TargetKind: "disk", Disk: discovery.IdentityOf(match.Device),
				Filesystem: request.Filesystem, MountPath: VolumeMountRoot + "/" + request.Name, Wipe: true, Repartition: true,
				TypeUUID: volumePartitionTypeUUID(request.Name), Signatures: match.Signatures,
			}, match.Device, BlockDevice{}, nil
		}
		label := volumePartitionLabel(request.Name)
		var matches []BlockDevice
		for _, partition := range match.Device.Partitions {
			if partition.GPTLabel == label {
				matches = append(matches, partition)
			}
		}
		switch len(matches) {
		case 0:
			return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q disk: partition label %s was not found on %s; set wipe to true to initialize this disk", request.Name, label, match.Device.Path)
		case 1:
		default:
			return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q disk: %s has %d partitions labelled %s; relabel or remove the extra partitions, or select one with a partition selector", request.Name, match.Device.Path, len(matches), label)
		}
		plan := existingPartitionVolume(request, "disk", match.Device, matches[0])
		plan.Signatures = collectVolumeSignatures(matches[0])
		if err := validateReusableVolume(plan, matches[0].FilesystemSignature); err != nil {
			return VolumePlan{}, BlockDevice{}, BlockDevice{}, err
		}
		return plan, match.Device, matches[0], nil
	}

	match, err := discovery.MatchPartition(facts, *request.Partition)
	if err != nil {
		return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q partition: %w", request.Name, err)
	}
	if match.ParentDisk.Path == target.Path {
		return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q partition is on target root disk %s", request.Name, target.Path)
	}
	if isKatlPartitionLabel(match.Device.GPTLabel) {
		return VolumePlan{}, BlockDevice{}, BlockDevice{}, fmt.Errorf("volume %q resolves to Katl-managed partition %s", request.Name, match.Device.GPTLabel)
	}
	plan := existingPartitionVolume(request, "partition", match.ParentDisk, match.Device)
	plan.Wipe = request.Wipe
	plan.Signatures = match.Signatures
	if err := validateReusableVolume(plan, match.Device.FilesystemSignature); err != nil {
		return VolumePlan{}, BlockDevice{}, BlockDevice{}, err
	}
	return plan, match.ParentDisk, match.Device, nil
}

func existingPartitionVolume(request VolumeRequest, kind string, disk, partition BlockDevice) VolumePlan {
	return VolumePlan{
		Name: request.Name, TargetKind: kind, Disk: discovery.IdentityOf(disk), Partition: partitionIdentityOf(partition),
		Filesystem: request.Filesystem, MountPath: VolumeMountRoot + "/" + request.Name,
	}
}

// volumeClaims refuses two volumes on the same partition, or a volume on a
// disk that another volume repartitions.
type volumeClaims struct {
	wholeDisks map[string]string
	partitions map[string]map[uint64]string
}

func (c *volumeClaims) add(plan VolumePlan) error {
	if c.wholeDisks == nil {
		c.wholeDisks = map[string]string{}
		c.partitions = map[string]map[uint64]string{}
	}
	disk := plan.Disk.Path
	if other, ok := c.wholeDisks[disk]; ok {
		return wholeDiskConflict(other, plan.Name, disk, other)
	}
	if plan.Repartition {
		for _, other := range c.partitions[disk] {
			return wholeDiskConflict(other, plan.Name, disk, plan.Name)
		}
		c.wholeDisks[disk] = plan.Name
		return nil
	}
	if other, ok := c.partitions[disk][plan.Partition.Number]; ok {
		return fmt.Errorf("volumes %q and %q both select partition %d of %s; select a different partition for one of them", other, plan.Name, plan.Partition.Number, disk)
	}
	if c.partitions[disk] == nil {
		c.partitions[disk] = map[uint64]string{}
	}
	c.partitions[disk][plan.Partition.Number] = plan.Name
	return nil
}

func wholeDiskConflict(first, second, disk, repartitioned string) error {
	return fmt.Errorf("volumes %q and %q both select disk %s, which volume %q repartitions as a whole; select a different disk for one of them", first, second, disk, repartitioned)
}

// validateAddressableVolume refuses a volume Katl could not address without a
// kernel name or find again on its disk after preparation.
func validateAddressableVolume(plan VolumePlan, disk, partition BlockDevice) error {
	if !plan.Repartition && plan.Partition.Number == 0 {
		return fmt.Errorf("volume %q: the kernel did not report the partition number of %s; check that /sys/class/block/%s/partition exists, then retry", plan.Name, partition.Path, path.Base(partition.Path))
	}
	if !plan.Repartition && !plan.Wipe {
		return nil
	}
	if err := checkAddressable(disk); err != nil {
		return fmt.Errorf("volume %q: %w", plan.Name, err)
	}
	return nil
}

// volumeDevice addresses what a volume's destructive commands write: the bound
// disk, or the selected partition on it, by the disk's kernel attachment
// sequence.
func volumeDevice(plan VolumePlan) (string, error) {
	device, err := diskDevice(plan.Disk)
	if err != nil || plan.Repartition {
		return device, err
	}
	return fmt.Sprintf("%s-part%d", device, plan.Partition.Number), nil
}

// volumePartition finds a volume's partition on its bound disk: the one Katl
// created when it repartitioned the disk, otherwise the selected partition by
// number, which must still match its planned identity.
func volumePartition(disk BlockDevice, plan VolumePlan) (BlockDevice, error) {
	if plan.Repartition {
		label := volumePartitionLabel(plan.Name)
		return partitionOnDisk(disk, "labelled "+label, func(candidate BlockDevice) bool {
			return candidate.GPTLabel == label
		})
	}
	partition, err := partitionOnDisk(disk, fmt.Sprintf("numbered %d", plan.Partition.Number), func(candidate BlockDevice) bool {
		return candidate.PartitionNumber == plan.Partition.Number
	})
	if err != nil {
		return BlockDevice{}, err
	}
	if isKatlPartitionLabel(partition.GPTLabel) {
		return BlockDevice{}, fmt.Errorf("%s is now Katl-managed partition %s", partition.Path, partition.GPTLabel)
	}
	if current := partitionIdentityOf(partition); current != plan.Partition {
		return BlockDevice{}, fmt.Errorf("%w: partition %d of %s is now %s, not the selected %s", discovery.ErrTargetDiskChanged, plan.Partition.Number, disk.Path, current, plan.Partition)
	}
	return partition, nil
}

// checkVolume rechecks a planned volume against current facts: its disk is
// still the planned disk, what Katl writes is still safe to overwrite, and a
// preserved partition still has an identity no other device shares.
func checkVolume(facts HardwareFacts, plan VolumePlan) error {
	if plan.Repartition {
		_, err := discovery.MatchRetainedDisk(facts, plan.Disk, discovery.TargetDiskSelector{})
		return err
	}
	disk, err := discovery.MatchBoundDisk(facts, plan.Disk)
	if err != nil {
		return err
	}
	partition, err := volumePartition(disk, plan)
	if err != nil {
		return err
	}
	if err := discovery.ValidatePartitionSafety(facts, partition); err != nil {
		return err
	}
	if plan.Partition.UUID != "" {
		if _, err := uniquePartitionUUID(facts, partition); err != nil {
			return err
		}
	}
	if !plan.Wipe {
		if _, err := bindVolume(facts, plan); err != nil {
			return err
		}
	}
	return nil
}

// BindVolumePlans finds each prepared volume's partition on its bound disk and
// assigns the PARTUUID= (or, without a PARTUUID, UUID=) source it is mounted
// and persisted by. It refuses a disk whose identity changed and an identity
// that another device shares.
func BindVolumePlans(facts HardwareFacts, plans []VolumePlan) ([]VolumePlan, []VolumeBinding, error) {
	bound := append([]VolumePlan(nil), plans...)
	bindings := make([]VolumeBinding, 0, len(plans))
	for i := range bound {
		binding, err := bindVolume(facts, bound[i])
		if err != nil {
			return nil, nil, fmt.Errorf("volume %q: %w", bound[i].Name, err)
		}
		source, err := VolumeBindingMountSource(binding)
		if err != nil {
			return nil, nil, err
		}
		bound[i].MountSource = source
		bindings = append(bindings, binding)
	}
	return bound, bindings, nil
}

func bindVolume(facts HardwareFacts, plan VolumePlan) (VolumeBinding, error) {
	partition, err := boundVolumePartition(facts, plan)
	if err != nil {
		return VolumeBinding{}, err
	}
	binding := VolumeBinding{Name: plan.Name, FilesystemUUID: strings.TrimSpace(partition.FilesystemUUID)}
	if strings.TrimSpace(partition.PartitionUUID) != "" {
		binding.PartitionUUID, err = uniquePartitionUUID(facts, partition)
	} else {
		binding.FilesystemUUID, err = uniqueFilesystemUUID(facts, partition)
	}
	if err != nil {
		return VolumeBinding{}, err
	}
	return binding, nil
}

func boundVolumePartition(facts HardwareFacts, plan VolumePlan) (BlockDevice, error) {
	disk, err := discovery.MatchBoundDisk(facts, plan.Disk)
	if err != nil {
		return BlockDevice{}, err
	}
	return volumePartition(disk, plan)
}
