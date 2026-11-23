package disk

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

// Every disk Katl writes, the system disk and each data volume disk, is bound
// to the discovery.DiskIdentity resolved at planning. Destructive commands
// address it by diskseq after its identity is revalidated, and its partitions
// are then found only on that disk and mounted by a PARTUUID (or, for a data
// volume without one, a filesystem UUID) that no other device shares. Nothing
// after planning resolves a disk by kernel name or a partition by label alone.

// diskDevice names a bound disk by its kernel attachment sequence, so a
// command cannot reach a different disk that has since taken the same kernel
// name. A disk without a diskseq is refused rather than named by kernel name.
func diskDevice(disk discovery.DiskIdentity) (string, error) {
	if disk.DiskSeq == 0 {
		return "", fmt.Errorf("disk %s has no kernel attachment sequence (diskseq), so Katl cannot address it safely; KatlOS requires a kernel that reports /sys/class/block/%s/diskseq", disk.Path, path.Base(disk.Path))
	}
	return "/dev/disk/by-diskseq/" + strconv.FormatUint(disk.DiskSeq, 10), nil
}

// partitionOnDisk returns the one partition of a bound disk that matches.
// Partitions on other disks are never considered, so a matching partition
// left elsewhere, such as by an earlier install, cannot be chosen.
func partitionOnDisk(disk BlockDevice, description string, match func(BlockDevice) bool) (BlockDevice, error) {
	var matches []BlockDevice
	for _, partition := range disk.Partitions {
		if match(partition) {
			matches = append(matches, partition)
		}
	}
	if len(matches) != 1 {
		return BlockDevice{}, fmt.Errorf("disk %s has %d partitions %s, want 1", disk.Path, len(matches), description)
	}
	return matches[0], nil
}

// uniquePartitionUUID returns the partition's PARTUUID and refuses one that
// another partition shares, since PARTUUID= and /dev/disk/by-partuuid could
// then resolve to either.
func uniquePartitionUUID(facts HardwareFacts, partition BlockDevice) (string, error) {
	uuid := strings.ToLower(strings.TrimSpace(partition.PartitionUUID))
	if uuid == "" {
		return "", fmt.Errorf("%s has no PARTUUID", partition.Path)
	}
	var others []string
	for _, device := range facts.BlockDevices {
		for _, candidate := range device.Partitions {
			if candidate.Path != partition.Path && strings.EqualFold(strings.TrimSpace(candidate.PartitionUUID), uuid) {
				others = append(others, candidate.Path)
			}
		}
	}
	if len(others) > 0 {
		return "", fmt.Errorf("PARTUUID %s of %s is also used by %s; detach the other disk or give it new partition GUIDs", uuid, partition.Path, strings.Join(others, ", "))
	}
	return uuid, nil
}

// uniqueFilesystemUUID returns the partition's filesystem UUID and refuses
// one that another disk or partition shares, since UUID= could then resolve
// to either.
func uniqueFilesystemUUID(facts HardwareFacts, partition BlockDevice) (string, error) {
	uuid := strings.TrimSpace(partition.FilesystemUUID)
	if uuid == "" {
		return "", fmt.Errorf("%s has no PARTUUID or filesystem UUID", partition.Path)
	}
	var others []string
	var visit func([]BlockDevice)
	visit = func(devices []BlockDevice) {
		for _, device := range devices {
			if device.Path != partition.Path && strings.EqualFold(strings.TrimSpace(device.FilesystemUUID), uuid) {
				others = append(others, device.Path)
			}
			visit(device.Partitions)
		}
	}
	visit(facts.BlockDevices)
	if len(others) > 0 {
		return "", fmt.Errorf("filesystem UUID %s of %s is also used by %s; detach the other disk or give its filesystem a new UUID", uuid, partition.Path, strings.Join(others, ", "))
	}
	return uuid, nil
}
