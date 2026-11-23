package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/systemdisk"
)

// bootedSystemDisk resolves the system disk from the partition backing /, with
// the checks the boot-time generator makes: it refuses when another disk shares
// a PARTUUID with the system disk or the firmware booted a different ESP.
func bootedSystemDisk() (systemdisk.Layout, error) {
	var root unix.Stat_t
	if err := unix.Stat("/", &root); err != nil {
		return systemdisk.Layout{}, fmt.Errorf("identify the system disk: stat /: %w", err)
	}
	layout, err := systemdisk.Resolve("/sys", unix.Major(root.Dev), unix.Minor(root.Dev), true)
	if failure := (*systemdisk.Failure)(nil); errors.As(err, &failure) {
		return systemdisk.Layout{}, fmt.Errorf("identify the system disk: %s. %s", failure.Cause, failure.Guidance)
	}
	if err != nil {
		return systemdisk.Layout{}, fmt.Errorf("identify the system disk: %w", err)
	}
	return layout, nil
}

// rootSlots are the two root partitions on the system disk. Disk addresses
// the system disk by diskseq, so the relabel cannot reach a disk that later
// takes its kernel name.
type rootSlots struct {
	Disk             string
	ActivePart       int
	InactivePart     int
	InactiveDevice   string
	InactivePartUUID string
}

// findRootSlots finds the root slots on the system disk. The active slot is
// the partition backing /, which must be the root the current generation
// records; the inactive slot is the one other partition on that disk with the
// same GPT type. Root slot labels are not used: an install names them
// KATL_ROOT_A and KATL_ROOT_B, and sysupdate renames them on upgrade.
func (e *Executor) findRootSlots(ctx context.Context, activePartUUID string) (rootSlots, error) {
	layout, err := e.volumeDevices.systemDisk()
	if err != nil {
		return rootSlots{}, err
	}
	root := layout.Root
	if !strings.EqualFold(root.UUID, activePartUUID) {
		return rootSlots{}, fmt.Errorf("find active root slot: the running root %s has PARTUUID %s, but the current generation records %s; reboot into the current generation before upgrading", root.Name, root.UUID, activePartUUID)
	}
	if layout.Disk.Seq == 0 {
		return rootSlots{}, fmt.Errorf("find root slots: system disk %s reports no diskseq", layout.Disk.Name)
	}
	diskPath := fmt.Sprintf("/dev/disk/by-diskseq/%d", layout.Disk.Seq)
	types, err := e.partitionTypes(ctx, diskPath, layout.Disk)
	if err != nil {
		return rootSlots{}, err
	}

	rootType := types[root.Number]
	if rootType == "" {
		return rootSlots{}, fmt.Errorf("find root slots: running root %s on %s has no GPT type", root.Name, diskPath)
	}
	var peers []systemdisk.Partition
	for _, partition := range layout.Disk.Partitions {
		if strings.EqualFold(types[partition.Number], rootType) {
			peers = append(peers, partition)
		}
	}
	if len(peers) != 2 {
		return rootSlots{}, fmt.Errorf("find inactive root slot: found %d root partitions with type %s on system disk %s, want 2", len(peers), rootType, layout.Disk.Name)
	}
	inactive := peers[0]
	if inactive.Number == root.Number {
		inactive = peers[1]
	}
	if inactive.UUID == "" || strings.EqualFold(inactive.UUID, root.UUID) {
		return rootSlots{}, fmt.Errorf("find inactive root slot: %s has PARTUUID %q, want one distinct from the running root", inactive.Name, inactive.UUID)
	}
	return rootSlots{
		Disk:             diskPath,
		ActivePart:       root.Number,
		InactivePart:     inactive.Number,
		InactiveDevice:   rootedRuntimePath(e.Root, inactive.DevicePath()),
		InactivePartUUID: inactive.UUID,
	}, nil
}

// partitionTypes reads the GPT type of each partition on the disk at
// diskPath, keyed by partition number. sysfs does not report partition types,
// so lsblk reads them; each partition must have the PARTUUID sysfs reported,
// or the partition table changed in between.
func (e *Executor) partitionTypes(ctx context.Context, diskPath string, system systemdisk.Disk) (map[int]string, error) {
	output, err := e.toolOutput(ctx, "lsblk", "--json", "--output", "PARTN,PARTUUID,PARTTYPE", diskPath)
	if err != nil {
		return nil, fmt.Errorf("read root partition types on %s: %w", diskPath, err)
	}
	// Without a tree column lsblk lists the disk and its partitions as one
	// flat array; the disk's row has no partition number.
	var table struct {
		BlockDevices []struct {
			Number   *int   `json:"partn"`
			UUID     string `json:"partuuid"`
			TypeGUID string `json:"parttype"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(output), &table); err != nil {
		return nil, fmt.Errorf("read root partition types on %s: invalid lsblk output %q", diskPath, output)
	}
	expected := make(map[int]string, len(system.Partitions))
	for _, partition := range system.Partitions {
		expected[partition.Number] = partition.UUID
	}
	types := map[int]string{}
	for _, row := range table.BlockDevices {
		if row.Number == nil {
			continue
		}
		number := *row.Number
		if uuid, ok := expected[number]; !ok || !strings.EqualFold(uuid, row.UUID) {
			return nil, fmt.Errorf("read root partition types on %s: partition %d has PARTUUID %s, but sysfs reported %q for system disk %s; retry the upgrade", diskPath, number, row.UUID, uuid, system.Name)
		}
		types[number] = strings.ToLower(row.TypeGUID)
	}
	return types, nil
}

// prepareSysupdateSlots labels the active slot as sysupdate version 0 and the
// inactive slot as free, so sysupdate writes the candidate to the inactive
// slot.
func (e *Executor) prepareSysupdateSlots(ctx context.Context, slots rootSlots) error {
	for _, request := range []struct {
		part  int
		label string
	}{
		{slots.ActivePart, "katl_0"},
		{slots.InactivePart, "_empty"},
	} {
		part := strconv.Itoa(request.part)
		if _, err := e.toolOutput(ctx, "sfdisk", "--part-label", slots.Disk, part, request.label); err != nil {
			return fmt.Errorf("set root slot label %s: %w", request.label, err)
		}
		if _, err := e.toolOutput(ctx, "partx", "--update", "--nr", part, slots.Disk); err != nil {
			return fmt.Errorf("refresh root slot label %s: %w", request.label, err)
		}
	}
	return nil
}
