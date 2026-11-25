package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// takes its kernel name. Inactive is the bound inactive slot, opened through
// Inactive.Node when it is written.
type rootSlots struct {
	Disk             string
	ActivePart       int
	InactivePart     int
	Inactive         systemdisk.Partition
	InactivePartUUID string
	// RootType is the GPT type of both slots when no sysupdate transfer is
	// interrupted.
	RootType string
}

// findRootSlots finds the root slots on the system disk. The active slot is
// the partition backing /, which must be the root the current generation
// records; the inactive slot is the one other partition on that disk with the
// same GPT type, or with the type sysupdate gives it while a transfer to it is
// partial or pending, which an interrupted transfer leaves on disk. Root slot labels are not used: an install names them
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
	inactiveTypes, err := inactiveSlotTypes(rootType)
	if err != nil {
		return rootSlots{}, fmt.Errorf("find inactive root slot: %w", err)
	}
	var peers []systemdisk.Partition
	for _, partition := range layout.Disk.Partitions {
		if partition.Number == root.Number || slices.Contains(inactiveTypes, types[partition.Number]) {
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
		Inactive:         inactive,
		InactivePartUUID: inactive.UUID,
		RootType:         rootType,
	}, nil
}

// partitionTypes reads the GPT type of each partition on the disk at
// diskPath, keyed by partition number. sysfs does not report partition types,
// so lsblk reads them; each partition must have the PARTUUID sysfs reported,
// or the partition table changed in between.
func (e *Executor) partitionTypes(ctx context.Context, diskPath string, system systemdisk.Disk) (map[int]string, error) {
	output, err := e.toolOutput(ctx, "lsblk", "--json", "--output", "PARTN,PKNAME,PARTUUID,PARTTYPE", diskPath)
	if err != nil {
		return nil, fmt.Errorf("read root partition types on %s: %w", diskPath, err)
	}
	// Without a tree column lsblk lists the disk, its partitions and any devices
	// stacked on them as one flat array. Only rows whose parent is the disk are
	// its partitions.
	var table struct {
		BlockDevices []struct {
			Number   *int   `json:"partn"`
			Parent   string `json:"pkname"`
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
		if row.Number == nil || row.Parent != system.Name {
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

// prepareSysupdateSlots sets the slots to the state sysupdate expects before
// staging: the active slot is sysupdate version 0, and the inactive slot is
// free, with the root type that an interrupted transfer may have replaced. It
// is idempotent, so a retry after a failed staging starts from the same state.
//
// It rewrites the GPT only, with --no-tell-kernel: sysupdate matches slots by
// the labels on the disk, and Katl reads no root slot label from the kernel.
// Asking the kernel to re-read would remove and add the unmounted inactive
// slot, and a partition added that way reports no PARTUUID in sysfs until the
// next boot, so findRootSlots would refuse every later plan or retry.
func (e *Executor) prepareSysupdateSlots(ctx context.Context, slots rootSlots) error {
	active, inactive := strconv.Itoa(slots.ActivePart), strconv.Itoa(slots.InactivePart)
	for _, request := range []struct {
		flag, part, value, what string
	}{
		{"--part-label", active, "katl_0", "label"},
		{"--part-type", inactive, slots.RootType, "type"},
		{"--part-label", inactive, "_empty", "label"},
	} {
		if _, err := e.toolOutput(ctx, "sfdisk", "--no-tell-kernel", request.flag, slots.Disk, request.part, request.value); err != nil {
			return fmt.Errorf("set root slot %s %s: %w", request.what, request.value, err)
		}
	}
	return nil
}

// Sysupdate marks a partition it is writing with a type derived from the
// partition's own type, keyed by these IDs (GPT_SYSUPDATE_PARTIAL_APP_ID and
// GPT_SYSUPDATE_PENDING_APP_ID in systemd's sysupdate-partition.c). An
// interrupted transfer leaves the derived type until the next sysupdate run
// vacuums it.
var (
	sysupdatePartialAppID = [16]byte{0xac, 0xcf, 0xa0, 0xc2, 0xda, 0x24, 0x46, 0x0a, 0x9f, 0xc9, 0x0b, 0xb8, 0xfc, 0x78, 0x52, 0x19}
	sysupdatePendingAppID = [16]byte{0x80, 0xf3, 0xd6, 0x1e, 0x23, 0x83, 0x43, 0xb9, 0x81, 0xf5, 0xce, 0x37, 0x93, 0xf4, 0x7d, 0x4c}
)

// inactiveSlotTypes lists the GPT types the inactive root slot may carry: the
// root type, or the partial or pending type sysupdate derives from it.
func inactiveSlotTypes(rootType string) ([]string, error) {
	base, err := hex.DecodeString(strings.ReplaceAll(rootType, "-", ""))
	if err != nil || len(base) != 16 {
		return nil, fmt.Errorf("root GPT type %q is not a UUID", rootType)
	}
	types := []string{strings.ToLower(rootType)}
	for _, app := range [][16]byte{sysupdatePartialAppID, sysupdatePendingAppID} {
		// sd_id128_get_app_specific: HMAC-SHA256 keyed by the type, over the
		// app ID, first 16 bytes, as an RFC 4122 version 4 UUID.
		mac := hmac.New(sha256.New, base)
		mac.Write(app[:])
		id := mac.Sum(nil)[:16]
		id[6] = id[6]&0x0f | 0x40
		id[8] = id[8]&0x3f | 0x80
		h := hex.EncodeToString(id)
		types = append(types, h[:8]+"-"+h[8:12]+"-"+h[12:16]+"-"+h[16:20]+"-"+h[20:])
	}
	return types, nil
}
