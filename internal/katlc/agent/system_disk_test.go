package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/systemdisk"
)

const (
	espType   = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	rootType  = "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	stateType = "0fc63daf-8483-4772-8e79-3d69d8477de4"
)

type hostPartition struct {
	number         int
	label, uuid, t string
}

type hostDisk struct {
	name       string
	seq        uint64
	partitions []hostPartition
}

// hostDisks models the block devices of a node: a sysfs tree that
// systemdisk.Resolve reads, and lsblk and blkid answers for the same disks
// whether addressed by kernel name or by diskseq.
type hostDisks struct {
	sysfs string
	disks []hostDisk
	calls []string
}

// newHostDisks attaches disks in order, giving disk i minors 16*i to 16*i+15.
func newHostDisks(t *testing.T, disks ...hostDisk) *hostDisks {
	t.Helper()
	sysfs := t.TempDir()
	for i, disk := range disks {
		diskDir := filepath.Join(sysfs, "devices/virtual/block", disk.name)
		writeTestFile(t, filepath.Join(diskDir, "diskseq"), fmt.Sprintf("%d\n", disk.seq))
		for _, partition := range disk.partitions {
			name := fmt.Sprintf("%s%d", disk.name, partition.number)
			dir := filepath.Join(diskDir, name)
			minor := 16*i + partition.number
			writeTestFile(t, filepath.Join(dir, "partition"), fmt.Sprintf("%d\n", partition.number))
			writeTestFile(t, filepath.Join(dir, "uevent"), fmt.Sprintf("MAJOR=253\nMINOR=%d\nDEVNAME=%s\nDEVTYPE=partition\nPARTN=%d\nPARTNAME=%s\nPARTUUID=%s\n", minor, name, partition.number, partition.label, partition.uuid))
			for _, link := range []string{filepath.Join("dev/block", fmt.Sprintf("253:%d", minor)), filepath.Join("class/block", name)} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(sysfs, link)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, filepath.Join(sysfs, link)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return &hostDisks{sysfs: sysfs, disks: disks}
}

// booted resolves the system disk as the agent does, with / backed by the
// given partition of disk index.
func (h *hostDisks) booted(index, partition int) func() (systemdisk.Layout, error) {
	return func() (systemdisk.Layout, error) {
		return systemdisk.Resolve(h.sysfs, 253, uint32(16*index+partition), true)
	}
}

func (h *hostDisks) run(_ context.Context, argv []string, _ func(int)) ToolResult {
	h.calls = append(h.calls, strings.Join(argv, " "))
	device := argv[len(argv)-1]
	switch argv[0] {
	case "lsblk":
		for _, disk := range h.disks {
			if device != "/dev/"+disk.name && device != fmt.Sprintf("/dev/disk/by-diskseq/%d", disk.seq) {
				continue
			}
			// lsblk lists the disk and its partitions as one flat array when
			// no tree column is selected, as util-linux 2.42 prints for
			// lsblk --json --output PARTN,PARTUUID,PARTTYPE DISK.
			rows := []string{`{"partn": null, "partuuid": null, "parttype": null}`}
			for _, partition := range disk.partitions {
				rows = append(rows, fmt.Sprintf(`{"partn": %d, "partuuid": %q, "parttype": %q}`, partition.number, partition.uuid, partition.t))
			}
			return ToolResult{Stdout: []byte("{\n   \"blockdevices\": [\n      " + strings.Join(rows, ",") + "\n   ]\n}\n")}
		}
		return ToolResult{ExitStatus: 32, Stderr: []byte(device + ": not a block device")}
	case "blkid":
		var matches []string
		for _, disk := range h.disks {
			for _, partition := range disk.partitions {
				if strings.Contains(strings.Join(argv, " "), "PARTUUID="+partition.uuid) {
					matches = append(matches, fmt.Sprintf("/dev/%s%d", disk.name, partition.number))
				}
			}
		}
		if len(matches) == 0 {
			return ToolResult{ExitStatus: 2}
		}
		return ToolResult{Stdout: []byte(strings.Join(matches, "\n") + "\n")}
	default:
		return ToolResult{}
	}
}

func katlHostDisk(name string, seq uint64, prefix string) hostDisk {
	return hostDisk{name: name, seq: seq, partitions: []hostPartition{
		{1, "KATL_ESP", prefix + "-esp", espType},
		{2, "KATL_ROOT_A", prefix + "-root-a", rootType},
		{3, "KATL_ROOT_B", prefix + "-root-b", rootType},
		{4, "KATL_STATE", prefix + "-state", stateType},
	}}
}

func TestFindRootSlots(t *testing.T) {
	// The decoy is an earlier install, attached first so that a lookup by
	// label or by first match reaches it before the running install.
	decoy := katlHostDisk("vda", 3, "old")
	system := katlHostDisk("vdb", 7, "new")
	// An upgraded node's slots carry sysupdate labels rather than the
	// install's.
	upgraded := katlHostDisk("vdb", 7, "new")
	upgraded.partitions[1].label, upgraded.partitions[2].label = "_empty", "katl_1"

	t.Run("beside a decoy Katl disk", func(t *testing.T) {
		for _, disk := range []hostDisk{system, upgraded} {
			host := newHostDisks(t, decoy, disk)
			executor := &Executor{RunTool: host.run, volumeDevices: volumeDevices{bootedDisk: host.booted(1, 3)}}

			slots, err := executor.findRootSlots(context.Background(), "new-root-b")
			if err != nil {
				t.Fatalf("findRootSlots() error = %v", err)
			}
			want := rootSlots{Disk: "/dev/disk/by-diskseq/7", ActivePart: 3, InactivePart: 2, InactiveDevice: "/dev/disk/by-diskseq/7-part2", InactivePartUUID: "new-root-a"}
			if slots != want {
				t.Fatalf("findRootSlots() = %+v, want %+v", slots, want)
			}
		}
	})

	for _, test := range []struct {
		name   string
		disks  []hostDisk
		system int // index of the disk holding the running root, partition 2
		active string
		want   string
	}{
		{
			// The generation's root exists only on the decoy disk, so the
			// node is not running the generation it would upgrade from.
			name:   "generation root on decoy disk",
			disks:  []hostDisk{decoy, system},
			system: 1,
			active: "old-root-a",
			want:   "the running root vdb2 has PARTUUID new-root-a, but the current generation records old-root-a",
		},
		{
			name: "clone of running root",
			disks: []hostDisk{system, {name: "vdc", seq: 9, partitions: []hostPartition{
				{1, "KATL_ROOT_A", "new-root-a", rootType},
			}}},
			active: "new-root-a",
			want:   "same PARTUUID new-root-a",
		},
		{
			name: "missing peer",
			disks: []hostDisk{{name: "vdb", seq: 7, partitions: []hostPartition{
				{1, "KATL_ESP", "new-esp", espType}, {2, "KATL_ROOT_A", "new-root-a", rootType}, {4, "KATL_STATE", "new-state", stateType},
			}}},
			active: "new-root-a",
			want:   "found 1 root partitions",
		},
		{
			name: "extra peer",
			disks: []hostDisk{{name: "vdb", seq: 7, partitions: append(append([]hostPartition(nil), system.partitions...),
				hostPartition{5, "KATL_ROOT_C", "new-root-c", rootType})}},
			active: "new-root-a",
			want:   "found 3 root partitions",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newHostDisks(t, test.disks...)
			executor := &Executor{RunTool: host.run, volumeDevices: volumeDevices{bootedDisk: host.booted(test.system, 2)}}

			_, err := executor.findRootSlots(context.Background(), test.active)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("findRootSlots() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPrepareSysupdateSlotsAddressesDiskSeq(t *testing.T) {
	host := newHostDisks(t, katlHostDisk("vda", 3, "old"), katlHostDisk("vdb", 7, "new"))
	executor := &Executor{RunTool: host.run, volumeDevices: volumeDevices{bootedDisk: host.booted(1, 2)}}
	slots, err := executor.findRootSlots(context.Background(), "new-root-a")
	if err != nil {
		t.Fatal(err)
	}
	host.calls = nil

	if err := executor.prepareSysupdateSlots(context.Background(), slots); err != nil {
		t.Fatalf("prepareSysupdateSlots() error = %v", err)
	}
	want := []string{
		"sfdisk --part-label /dev/disk/by-diskseq/7 2 katl_0",
		"partx --update --nr 2 /dev/disk/by-diskseq/7",
		"sfdisk --part-label /dev/disk/by-diskseq/7 3 _empty",
		"partx --update --nr 3 /dev/disk/by-diskseq/7",
	}
	if !reflect.DeepEqual(host.calls, want) {
		t.Fatalf("relabel commands = %q, want %q", host.calls, want)
	}
}
