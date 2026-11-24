// Package systemdisk identifies the KatlOS system disk of a running node and
// renders the mounts for its ESP and state partitions.
//
// The system disk is the disk holding the partition mounted at /. Katl
// partitions are looked up only on that disk, so another disk carrying Katl
// GPT labels, such as an earlier install or a moved disk, is never mounted.
package systemdisk

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/katl-dev/katl/internal/installer/disk"
)

// loaderDevicePartUUIDVar is the systemd-boot variable naming the ESP the
// firmware booted from.
const loaderDevicePartUUIDVar = "firmware/efi/efivars/LoaderDevicePartUUID-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"

const reinstallGuidance = "Check that the node booted from its installed disk; if that disk's partitions were changed, reinstall the node."

// Failure explains why the system disk could not be resolved and what the
// operator should do about it.
type Failure struct {
	Cause    string
	Guidance string
}

func (f *Failure) Error() string {
	return f.Cause
}

type Partition struct {
	Name    string
	Number  int
	Label   string
	UUID    string
	DiskSeq uint64

	// sysfs is the sysfs root the partition was read from.
	sysfs string
}

type Disk struct {
	Name       string
	Seq        uint64
	Partitions []Partition
}

type Layout struct {
	Disk  Disk
	Root  Partition
	ESP   Partition
	State Partition
}

// DevicePath addresses a partition by its disk's attachment sequence, so the
// mount cannot reach a different disk that later takes the same kernel name.
// The path is a udev link, so it suits units, whose device dependency waits
// for udev; a running process opens the partition through Node instead.
func (p Partition) DevicePath() string {
	if p.DiskSeq != 0 {
		return fmt.Sprintf("/dev/disk/by-diskseq/%d-part%d", p.DiskSeq, p.Number)
	}
	return "/dev/" + p.Name
}

// Node returns the kernel device node of the partition, found afresh in sysfs
// by its disk's diskseq and its partition number. Rewriting a partition table
// can make the kernel remove and add the partition again, and udev then
// recreates its by-diskseq link some time later, while the kernel node exists
// as soon as the kernel has added the partition. Only diskseq and the
// partition number identify it: a partition the kernel adds again reports no
// PARTUUID or label until the next scan.
func (p Partition) Node() (string, error) {
	if p.DiskSeq == 0 {
		return "", fmt.Errorf("partition %s has no disk sequence number, so Katl cannot address it safely", p.Name)
	}
	entries, err := os.ReadDir(filepath.Join(p.sysfs, "class/block"))
	if err != nil {
		return "", fmt.Errorf("list block devices: %w", err)
	}
	for _, entry := range entries {
		dir, err := filepath.EvalSymlinks(filepath.Join(p.sysfs, "class/block", entry.Name()))
		if err != nil || !isPartition(dir) {
			continue
		}
		candidate, err := readPartition(p.sysfs, dir)
		if err != nil {
			continue
		}
		if candidate.DiskSeq == p.DiskSeq && candidate.Number == p.Number {
			return "/dev/" + candidate.Name, nil
		}
	}
	return "", fmt.Errorf("the kernel reports no partition %d on the disk with diskseq %d; the disk was detached or its partition table changed, so reboot the node and check its disks", p.Number, p.DiskSeq)
}

// partitionOf describes the partition with the given device number.
func partitionOf(sysfs string, device Device) (Partition, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysfs, "dev/block", fmt.Sprintf("%d:%d", device.Major, device.Minor)))
	if err != nil || !isPartition(dir) {
		return Partition{}, fmt.Errorf("%d:%d is not a disk partition", device.Major, device.Minor)
	}
	return readPartition(sysfs, dir)
}

// Resolve finds the system disk from the device number major:minor backing /
// and reads its Katl partitions from sysfs. With checkBoot it also refuses a
// boot that may have come from another disk: a clone that shares PARTUUIDs
// with the system disk, or firmware that booted a different ESP. The clone
// check sees only the disks the kernel has probed when it runs, so it is best
// effort; the mounts themselves never leave the system disk.
func Resolve(sysfs string, major, minor uint32, checkBoot bool) (Layout, error) {
	rootDir, err := filepath.EvalSymlinks(filepath.Join(sysfs, "dev/block", fmt.Sprintf("%d:%d", major, minor)))
	if err != nil {
		return Layout{}, &Failure{Cause: fmt.Sprintf("the running root %d:%d is not a known block device", major, minor), Guidance: reinstallGuidance}
	}
	if !isPartition(rootDir) {
		return Layout{}, &Failure{Cause: fmt.Sprintf("the running root %s is not a disk partition", filepath.Base(rootDir)), Guidance: reinstallGuidance}
	}

	diskDir := filepath.Dir(rootDir)
	system := Disk{Name: filepath.Base(diskDir)}
	if value, err := os.ReadFile(filepath.Join(diskDir, "diskseq")); err == nil {
		system.Seq, _ = strconv.ParseUint(strings.TrimSpace(string(value)), 10, 64)
	}
	entries, err := os.ReadDir(diskDir)
	if err != nil {
		return Layout{}, fmt.Errorf("read system disk %s: %w", system.Name, err)
	}
	var root Partition
	for _, entry := range entries {
		dir := filepath.Join(diskDir, entry.Name())
		if !isPartition(dir) {
			continue
		}
		partition, err := readPartition(sysfs, dir)
		if err != nil {
			return Layout{}, err
		}
		system.Partitions = append(system.Partitions, partition)
		if dir == rootDir {
			root = partition
		}
	}

	layout := Layout{Disk: system, Root: root}
	if layout.ESP, err = uniqueLabel(system, disk.GPTLabelESP); err != nil {
		return Layout{}, err
	}
	if layout.State, err = uniqueLabel(system, disk.GPTLabelState); err != nil {
		return Layout{}, err
	}
	if !checkBoot {
		return layout, nil
	}
	if err := checkNoClones(sysfs, diskDir, layout); err != nil {
		return Layout{}, err
	}
	if err := checkBootedESP(sysfs, diskDir, layout); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func uniqueLabel(system Disk, label string) (Partition, error) {
	var matches []Partition
	for _, partition := range system.Partitions {
		if partition.Label == label {
			matches = append(matches, partition)
		}
	}
	if len(matches) != 1 {
		return Partition{}, &Failure{Cause: fmt.Sprintf("system disk %s, which holds the running root, has %d partitions labelled %s, want 1", system.Name, len(matches), label), Guidance: reinstallGuidance}
	}
	if matches[0].UUID == "" {
		return Partition{}, &Failure{Cause: fmt.Sprintf("system disk partition %s (%s) has no PARTUUID", matches[0].Name, label), Guidance: reinstallGuidance}
	}
	return matches[0], nil
}

// checkNoClones fails when a partition on another disk shares a PARTUUID with
// the system disk. The firmware and initrd select partitions by PARTUUID, so
// a cloned disk makes it impossible to know which disk was meant.
func checkNoClones(sysfs, diskDir string, layout Layout) error {
	owned := map[string]string{}
	for _, partition := range []Partition{layout.Root, layout.ESP, layout.State} {
		if partition.UUID != "" {
			owned[partition.UUID] = partition.Name
		}
	}
	entries, err := os.ReadDir(filepath.Join(sysfs, "class/block"))
	if err != nil {
		return fmt.Errorf("list block devices: %w", err)
	}
	for _, entry := range entries {
		dir, err := filepath.EvalSymlinks(filepath.Join(sysfs, "class/block", entry.Name()))
		if err != nil || filepath.Dir(dir) == diskDir || !isPartition(dir) {
			continue
		}
		other, err := readPartition(sysfs, dir)
		if err != nil {
			continue
		}
		if name, ok := owned[other.UUID]; ok {
			return &Failure{
				Cause:    fmt.Sprintf("%s on disk %s has the same PARTUUID %s as %s on the system disk %s", other.Name, describeDisk(filepath.Dir(dir)), other.UUID, name, describeDisk(diskDir)),
				Guidance: "The two disks share partition GUIDs: one may be a copy of the other or an earlier install that reused the same GUIDs, or they may be one disk seen over two paths. It is uncertain which disk this node booted. Do not wipe either disk. Power off, detach the other disk, and boot again; to attach it again later, first give it new partition GUIDs, for example with sgdisk --randomize-guids.",
			}
		}
	}
	return nil
}

// checkBootedESP fails when systemd-boot reports that the firmware booted an
// ESP other than the system disk's, because boot entries written to /efi would
// then not be the ones the firmware reads.
func checkBootedESP(sysfs, diskDir string, layout Layout) error {
	value, err := os.ReadFile(filepath.Join(sysfs, loaderDevicePartUUIDVar))
	if err != nil {
		return nil
	}
	booted := decodeEFIString(value)
	if booted == "" || booted == layout.ESP.UUID {
		return nil
	}
	return &Failure{
		Cause:    fmt.Sprintf("the firmware booted the ESP with PARTUUID %s, but the system disk %s, which holds the running root, has ESP %s", booted, describeDisk(diskDir), layout.ESP.UUID),
		Guidance: "Boot entries written to this node's ESP would not be the ones the firmware reads. Set the firmware boot order to start from the node's installed disk, or detach the disk the firmware booted, then reboot.",
	}
}

// describeDisk names a disk by kernel name and, when the device reports one,
// its serial number.
func describeDisk(dir string) string {
	name := filepath.Base(dir)
	for _, path := range []string{"serial", "device/serial"} {
		if value, err := os.ReadFile(filepath.Join(dir, path)); err == nil && strings.TrimSpace(string(value)) != "" {
			return fmt.Sprintf("%s (serial %s)", name, strings.TrimSpace(string(value)))
		}
	}
	return name
}

func decodeEFIString(value []byte) string {
	if len(value) < 4 {
		return ""
	}
	value = value[4:]
	units := make([]uint16, 0, len(value)/2)
	for i := 0; i+1 < len(value); i += 2 {
		unit := uint16(value[i]) | uint16(value[i+1])<<8
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return strings.ToLower(strings.TrimSpace(string(utf16.Decode(units))))
}

func isPartition(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "partition"))
	return err == nil
}

func readPartition(sysfs, dir string) (Partition, error) {
	content, err := os.ReadFile(filepath.Join(dir, "uevent"))
	if err != nil {
		return Partition{}, fmt.Errorf("read partition %s: %w", filepath.Base(dir), err)
	}
	partition := Partition{Name: filepath.Base(dir), sysfs: sysfs}
	if value, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "diskseq")); err == nil {
		partition.DiskSeq, _ = strconv.ParseUint(strings.TrimSpace(string(value)), 10, 64)
	}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "DEVNAME":
			partition.Name = value
		case "PARTN":
			partition.Number, _ = strconv.Atoi(value)
		case "PARTNAME":
			partition.Label = value
		case "PARTUUID":
			partition.UUID = strings.ToLower(value)
		}
	}
	if partition.Number == 0 {
		return Partition{}, fmt.Errorf("partition %s has no partition number", partition.Name)
	}
	return partition, nil
}
