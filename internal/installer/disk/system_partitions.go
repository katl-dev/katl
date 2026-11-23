package disk

import (
	"fmt"

	"github.com/katl-dev/katl/internal/installer/discovery"
)

// BindSystemPartitions records the PARTUUID of each planned Katl partition on
// the bound target disk. Later install steps address those partitions only by
// the recorded PARTUUIDs, so a partition with the same GPT label on another
// disk, such as a previous Katl install, cannot be mounted or written.
func BindSystemPartitions(facts HardwareFacts, plan DiskLayoutPlan) (map[string]string, error) {
	target, err := discovery.MatchBoundDisk(facts, plan.TargetDisk)
	if err != nil {
		return nil, fmt.Errorf("bind Katl partitions: %w", err)
	}

	bound := make(map[string]string, len(plan.Partitions))
	for _, planned := range plan.Partitions {
		partition, err := partitionOnDisk(target, "labelled "+planned.GPTLabel, func(candidate BlockDevice) bool {
			return candidate.GPTLabel == planned.GPTLabel
		})
		if err != nil {
			return nil, fmt.Errorf("bind Katl partitions: %w", err)
		}
		uuid, err := uniquePartitionUUID(facts, partition)
		if err != nil {
			return nil, fmt.Errorf("bind Katl partitions: %s: %w, then rerun the install", planned.GPTLabel, err)
		}
		bound[planned.GPTLabel] = uuid
	}
	return bound, nil
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
