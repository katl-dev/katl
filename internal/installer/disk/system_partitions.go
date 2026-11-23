package disk

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

// BindSystemPartitions records the PARTUUID of each planned Katl partition on
// the bound target disk. Later install steps address those partitions only by
// the recorded PARTUUIDs, so a partition with the same GPT label on another
// disk, such as a previous Katl install, cannot be mounted or written.
func BindSystemPartitions(facts HardwareFacts, plan DiskLayoutPlan) (map[string]string, error) {
	target := findDevice(facts.BlockDevices, plan.TargetDisk.Path)
	if target == nil {
		return nil, fmt.Errorf("bind Katl partitions: target disk %s is no longer present", plan.TargetDisk.Path)
	}
	if current := discovery.IdentityOf(*target); current != plan.TargetDisk {
		return nil, fmt.Errorf("bind Katl partitions: %s is no longer the selected target disk", plan.TargetDisk.Path)
	}

	owners := partitionUUIDOwners(facts.BlockDevices)
	bound := make(map[string]string, len(plan.Partitions))
	for _, planned := range plan.Partitions {
		var matches []BlockDevice
		for _, partition := range target.Partitions {
			if partition.GPTLabel == planned.GPTLabel {
				matches = append(matches, partition)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("bind Katl partitions: target disk %s has %d partitions labelled %s, want 1", target.Path, len(matches), planned.GPTLabel)
		}
		uuid := strings.ToLower(strings.TrimSpace(matches[0].PartitionUUID))
		if uuid == "" {
			return nil, fmt.Errorf("bind Katl partitions: %s (%s) has no PARTUUID", matches[0].Path, planned.GPTLabel)
		}
		if others := owners[uuid]; len(others) > 1 {
			return nil, fmt.Errorf("bind Katl partitions: PARTUUID %s of %s is also used by %s; detach the other disk or give it new partition GUIDs, then rerun the install", uuid, matches[0].Path, strings.Join(withoutPath(others, matches[0].Path), ", "))
		}
		bound[planned.GPTLabel] = uuid
	}
	return bound, nil
}

func partitionUUIDOwners(devices []BlockDevice) map[string][]string {
	owners := make(map[string][]string)
	for _, device := range devices {
		for _, partition := range device.Partitions {
			if uuid := strings.ToLower(strings.TrimSpace(partition.PartitionUUID)); uuid != "" {
				owners[uuid] = append(owners[uuid], partition.Path)
			}
		}
	}
	return owners
}

func withoutPath(paths []string, path string) []string {
	out := make([]string, 0, len(paths))
	for _, candidate := range paths {
		if candidate != path {
			out = append(out, candidate)
		}
	}
	return out
}

// SystemPartitionDevice returns the device path of the bound Katl partition
// with the given GPT label.
func SystemPartitionDevice(plan DiskLayoutPlan, label string) (string, error) {
	uuid := plan.PartitionUUIDs[label]
	if uuid == "" {
		return "", fmt.Errorf("partition %s is not bound to target disk %s", label, plan.TargetDiskPath)
	}
	return "/dev/disk/by-partuuid/" + uuid, nil
}
