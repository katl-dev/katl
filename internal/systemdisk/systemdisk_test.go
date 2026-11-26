package systemdisk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

type fakePartition struct {
	number int
	label  string
	uuid   string
}

type fakeSysfs struct {
	root  string
	major uint32
}

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"dev/block", "class/block"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeSysfs{root: root, major: 259}
}

// addDisk adds a disk under a PCI device path, with the attribute files and
// subdirectories the kernel creates, and minor numbers minorBase and
// minorBase+N for partition N. Partition names follow the kernel: nvme0n1p2,
// but vda2.
func (s *fakeSysfs) addDisk(t *testing.T, name, serial string, seq uint64, minorBase uint32, partitions ...fakePartition) {
	t.Helper()
	deviceDir := filepath.Join(s.root, "devices/pci0000:00", name+"-ctrl")
	diskDir := filepath.Join(deviceDir, "block", name)
	mustWrite(t, filepath.Join(deviceDir, "serial"), serial+"\n")
	mustWrite(t, filepath.Join(diskDir, "diskseq"), fmt.Sprintf("%d\n", seq))
	mustWrite(t, filepath.Join(diskDir, "uevent"), "MAJOR=259\nDEVNAME="+name+"\nDEVTYPE=disk\nDISKSEQ="+fmt.Sprint(seq)+"\n")
	mustWrite(t, filepath.Join(diskDir, "size"), "67108864\n")
	mustWrite(t, filepath.Join(diskDir, "queue/logical_block_size"), "512\n")
	for _, dir := range []string{"holders", "slaves", "power", "trace", "mq/0"} {
		if err := os.MkdirAll(filepath.Join(diskDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(deviceDir, filepath.Join(diskDir, "device")); err != nil {
		t.Fatal(err)
	}
	s.link(t, name, diskDir, minorBase)
	separator := ""
	if last := name[len(name)-1]; last >= '0' && last <= '9' {
		separator = "p"
	}
	for _, partition := range partitions {
		partName := fmt.Sprintf("%s%s%d", name, separator, partition.number)
		partDir := filepath.Join(diskDir, partName)
		mustWrite(t, filepath.Join(partDir, "partition"), fmt.Sprintf("%d\n", partition.number))
		mustWrite(t, filepath.Join(partDir, "start"), "2048\n")
		mustWrite(t, filepath.Join(partDir, "uevent"), fmt.Sprintf("MAJOR=259\nMINOR=%d\nDEVNAME=%s\nDEVTYPE=partition\nDISKSEQ=%d\nPARTN=%d\nPARTNAME=%s\nPARTUUID=%s\n", minorBase+uint32(partition.number), partName, seq, partition.number, partition.label, partition.uuid))
		for _, dir := range []string{"holders", "power", "trace"} {
			if err := os.MkdirAll(filepath.Join(partDir, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		s.link(t, partName, partDir, minorBase+uint32(partition.number))
	}
}

func (s *fakeSysfs) link(t *testing.T, name, target string, minor uint32) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(s.root, "class/block", name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.root, "dev/block", fmt.Sprintf("%d:%d", s.major, minor))); err != nil {
		t.Fatal(err)
	}
}

func (s *fakeSysfs) setLoaderESP(t *testing.T, uuid string) {
	t.Helper()
	value := []byte{7, 0, 0, 0}
	for _, unit := range utf16.Encode([]rune(strings.ToUpper(uuid) + "\x00")) {
		value = append(value, byte(unit), byte(unit>>8))
	}
	mustWrite(t, filepath.Join(s.root, loaderDevicePartUUIDVar), string(value))
}

func (s *fakeSysfs) device(minor uint32) Device {
	return Device{Major: s.major, Minor: minor}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func katlDisk(prefix string) []fakePartition {
	return []fakePartition{
		{1, "KATL_ESP", prefix + "-esp"},
		{2, "KATL_ROOT_A", prefix + "-root-a"},
		{3, "KATL_ROOT_B", prefix + "-root-b"},
		{4, "KATL_STATE", prefix + "-state"},
	}
}

// twoKatlDisks attaches an earlier install as vda, enumerated first so a
// lookup by label alone would pick its partitions, and the running install
// as nvme0n1 with its root slot A at minor 16+2.
func twoKatlDisks(t *testing.T) *fakeSysfs {
	sysfs := newFakeSysfs(t)
	sysfs.addDisk(t, "vda", "katl-old", 7, 0, katlDisk("old")...)
	sysfs.addDisk(t, "nvme0n1", "katl-new", 9, 16, katlDisk("new")...)
	return sysfs
}

const runningRootMinor = 16 + 2

func TestResolveUsesRootDisk(t *testing.T) {
	sysfs := twoKatlDisks(t)
	sysfs.setLoaderESP(t, "new-esp")

	layout, err := Resolve(sysfs.root, sysfs.major, runningRootMinor, true)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if got := layout.ESP.DevicePath(); got != "/dev/disk/by-diskseq/9-part1" {
		t.Errorf("ESP device = %s, want /dev/disk/by-diskseq/9-part1", got)
	}
	if got := layout.State.DevicePath(); got != "/dev/disk/by-diskseq/9-part4" {
		t.Errorf("state device = %s, want /dev/disk/by-diskseq/9-part4", got)
	}
	if layout.ESP.Name != "nvme0n1p1" || layout.State.UUID != "new-state" {
		t.Errorf("layout = %+v, want partitions of nvme0n1", layout)
	}
}

func TestResolveRefuses(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*testing.T, *fakeSysfs)
		want     string
		guidance string
	}{
		{
			name: "state only on other disk",
			setup: func(t *testing.T, s *fakeSysfs) {
				s.addDisk(t, "vda", "katl-old", 7, 0, katlDisk("old")...)
				s.addDisk(t, "nvme0n1", "katl-new", 9, 16, katlDisk("new")[:3]...)
			},
			want:     "nvme0n1, which holds the running root, has 0 partitions labelled KATL_STATE",
			guidance: "reinstall the node",
		},
		{
			name: "duplicate label on root disk",
			setup: func(t *testing.T, s *fakeSysfs) {
				s.addDisk(t, "nvme0n1", "katl-new", 9, 16, append(katlDisk("new"), fakePartition{5, "KATL_ESP", "new-esp2"})...)
			},
			want:     "nvme0n1, which holds the running root, has 2 partitions labelled KATL_ESP",
			guidance: "reinstall the node",
		},
		{
			name: "shared partition GUIDs",
			setup: func(t *testing.T, s *fakeSysfs) {
				s.addDisk(t, "vda", "katl-copy", 7, 0, katlDisk("new")...)
				s.addDisk(t, "nvme0n1", "katl-new", 9, 16, katlDisk("new")...)
			},
			want:     "vda1 on disk vda (serial katl-copy) has the same PARTUUID new-esp as nvme0n1p1 on the system disk nvme0n1 (serial katl-new)",
			guidance: "Do not wipe either disk",
		},
		{
			name: "firmware booted another ESP",
			setup: func(t *testing.T, s *fakeSysfs) {
				s.addDisk(t, "vda", "katl-old", 7, 0, katlDisk("old")...)
				s.addDisk(t, "nvme0n1", "katl-new", 9, 16, katlDisk("new")...)
				s.setLoaderESP(t, "old-esp")
			},
			want:     "firmware booted the ESP with PARTUUID old-esp",
			guidance: "firmware boot order",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sysfs := newFakeSysfs(t)
			test.setup(t, sysfs)

			_, err := Resolve(sysfs.root, sysfs.major, runningRootMinor, true)

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, test.want)
			}
			var failure *Failure
			if !errors.As(err, &failure) || !strings.Contains(failure.Guidance, test.guidance) {
				t.Fatalf("Resolve() error = %#v, want guidance containing %q", err, test.guidance)
			}
		})
	}
}

func TestResolveWholeDiskRoot(t *testing.T) {
	sysfs := newFakeSysfs(t)
	sysfs.addDisk(t, "vda", "katl-boot", 7, 0)

	_, err := Resolve(sysfs.root, sysfs.major, 0, true)

	if err == nil || !strings.Contains(err.Error(), "vda is not a disk partition") {
		t.Fatalf("Resolve() error = %v, want root-not-partition failure", err)
	}
}

func TestBootPartition(t *testing.T) {
	sysfs := twoKatlDisks(t)
	layout, err := Resolve(sysfs.root, sysfs.major, runningRootMinor, true)
	if err != nil {
		t.Fatal(err)
	}

	partition, found, err := layout.BootPartition("new-root-b")
	if err != nil || !found || partition.DevicePath() != "/dev/disk/by-diskseq/9-part3" {
		t.Fatalf("bound partition = %+v, %t, %v, want the inactive slot on the system disk", partition, found, err)
	}
	if _, found, err := layout.BootPartition("old-root-b"); err != nil || found {
		t.Fatalf("bound decoy partition = %t, %v, want absent", found, err)
	}

	// The running root is still unambiguous, but the candidate would not be.
	sysfs.addDisk(t, "vdb", "katl-clone", 11, 32, fakePartition{3, "KATL_ROOT_B", "new-root-b"})
	if _, found, err := layout.BootPartition("new-root-b"); err == nil || found {
		t.Fatalf("bound cloned partition = %t, %v, want an ambiguity error", found, err)
	}
}

func TestPartitionNode(t *testing.T) {
	// The decoy vda sorts first and also has a partition 3.
	sysfs := newFakeSysfs(t)
	sysfs.addDisk(t, "vda", "katl-old", 7, 0, katlDisk("old")...)
	sysfs.addDisk(t, "vdb", "katl-new", 9, 16, katlDisk("new")...)
	layout, err := Resolve(sysfs.root, sysfs.major, runningRootMinor, false)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	var inactive Partition
	for _, partition := range layout.Disk.Partitions {
		if partition.Number == 3 {
			inactive = partition
		}
	}

	// partx --update removes the partition and adds it again through BLKPG,
	// after which the kernel reports neither its PARTUUID nor its label, and
	// udev has not yet linked it under /dev/disk/by-diskseq.
	mustWrite(t, filepath.Join(sysfs.root, "devices/pci0000:00/vdb-ctrl/block/vdb/vdb3/uevent"), "MAJOR=259\nMINOR=19\nDEVNAME=vdb3\nDEVTYPE=partition\nDISKSEQ=9\nPARTN=3\n")

	t.Run("re-added partition", func(t *testing.T) {
		node, err := inactive.Node()
		if err != nil || node != "/dev/vdb3" {
			t.Fatalf("Node() = %q, %v; want /dev/vdb3", node, err)
		}
	})

	t.Run("removed partition", func(t *testing.T) {
		if err := os.Remove(filepath.Join(sysfs.root, "class/block/vdb3")); err != nil {
			t.Fatal(err)
		}

		_, err := inactive.Node()

		if err == nil || !strings.Contains(err.Error(), "no partition 3 on the disk with diskseq 9") {
			t.Fatalf("Node() error = %v, want the missing partition named", err)
		}
	})
}

func TestGenerate(t *testing.T) {
	installed := "BOOT_IMAGE=/x root=PARTUUID=new-root-a rootfstype=squashfs ro"
	tests := []struct {
		name    string
		boot    func(*fakeSysfs) Boot
		copy    bool
		wantErr bool
		want    map[string]string
	}{
		{
			name: "boot mounts the root disk",
			boot: func(s *fakeSysfs) Boot {
				return Boot{Sysfs: s.root, Cmdline: installed, Root: s.device(runningRootMinor)}
			},
			want: map[string]string{
				"var.mount":                               "What=/dev/disk/by-diskseq/9-part4",
				"efi.mount":                               "What=/dev/disk/by-diskseq/9-part1",
				"local-fs.target.requires/var.mount":      "../var.mount",
				"local-fs.target.wants/efi.mount":         "../efi.mount",
				"local-fs.target.requires/" + failureUnit: "",
				"local-fs.target.requires/efi.mount":      "",
				"local-fs.target.wants/var.mount":         "",
				failureUnit:                               "",
			},
		},
		{
			name: "ambiguous boot fails before mounting",
			copy: true,
			boot: func(s *fakeSysfs) Boot {
				return Boot{Sysfs: s.root, Cmdline: installed, Root: s.device(runningRootMinor)}
			},
			wantErr: true,
			want: map[string]string{
				failureUnit: "ExecStart=/usr/bin/false",
				"local-fs.target.requires/" + failureUnit: "../" + failureUnit,
				"var.mount": "",
				"efi.mount": "",
			},
		},
		{
			// A copy attached after boot must not replace mounted units.
			name: "reload keeps the mounted partitions",
			copy: true,
			boot: func(s *fakeSysfs) Boot {
				state, esp := s.device(16+4), s.device(16+1)
				return Boot{Sysfs: s.root, Cmdline: installed, Root: s.device(runningRootMinor), Var: &state, EFI: &esp}
			},
			want: map[string]string{
				"var.mount":                          "What=/dev/disk/by-diskseq/9-part4",
				"efi.mount":                          "What=/dev/disk/by-diskseq/9-part1",
				"local-fs.target.requires/var.mount": "../var.mount",
				failureUnit:                          "",
			},
		},
		{
			name: "initrd generates nothing",
			boot: func(s *fakeSysfs) Boot {
				return Boot{Sysfs: s.root, Cmdline: installed, Initrd: true, Root: s.device(runningRootMinor)}
			},
			want: map[string]string{"var.mount": "", "efi.mount": "", failureUnit: ""},
		},
		{
			// The direct-boot development runtime has its root on a whole disk.
			name: "root not selected by PARTUUID generates nothing",
			boot: func(s *fakeSysfs) Boot {
				return Boot{Sysfs: s.root, Cmdline: "root=/dev/vda rootfstype=squashfs ro", Root: s.device(0)}
			},
			want: map[string]string{"var.mount": "", "efi.mount": "", failureUnit: ""},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sysfs := newFakeSysfs(t)
			prefix := "old"
			if test.copy {
				prefix = "new"
			}
			sysfs.addDisk(t, "vda", "katl-other", 7, 0, katlDisk(prefix)...)
			sysfs.addDisk(t, "nvme0n1", "katl-new", 9, 16, katlDisk("new")...)
			dir := t.TempDir()

			err := Generate(dir, test.boot(sysfs))

			if (err != nil) != test.wantErr {
				t.Fatalf("Generate() error = %v, want error %v", err, test.wantErr)
			}
			for path, want := range test.want {
				full := filepath.Join(dir, path)
				if want == "" {
					if _, err := os.Lstat(full); !os.IsNotExist(err) {
						t.Errorf("%s exists, want none", path)
					}
					continue
				}
				if target, err := os.Readlink(full); err == nil {
					if target != want {
						t.Errorf("%s -> %s, want %s", path, target, want)
					}
					continue
				}
				content, err := os.ReadFile(full)
				if err != nil || !strings.Contains(string(content), want+"\n") {
					t.Errorf("%s = %q (%v), want line %q", path, content, err, want)
				}
			}
		})
	}
}

func TestMountUnit(t *testing.T) {
	want := `# Generated by katl-system-disk-generator
[Unit]
Description=Katl writable state partition
Documentation=man:systemd.mount(5)
DefaultDependencies=no
Before=local-fs.target
Conflicts=umount.target
Before=umount.target

[Mount]
What=/dev/disk/by-diskseq/9-part4
Where=/var
Type=auto
Options=rw
`
	if got := mountUnit("Katl writable state partition", "/dev/disk/by-diskseq/9-part4", "/var", "auto", "rw"); got != want {
		t.Errorf("mountUnit() =\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteFailure(t *testing.T) {
	dir := t.TempDir()

	if err := WriteFailure(dir, &Failure{Cause: "system disk vdb has 0 partitions labelled KATL_STATE, want 1", Guidance: "Reinstall the node."}); err != nil {
		t.Fatalf("WriteFailure() error = %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(dir, failureUnit))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/bin/cat "+filepath.Join(dir, failureMessageFile)+"\n") || !strings.Contains(string(unit), "ExecStart=/usr/bin/false\n") {
		t.Errorf("%s does not report the message and fail:\n%s", failureUnit, unit)
	}
	message, err := os.ReadFile(filepath.Join(dir, failureMessageFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message), "0 partitions labelled KATL_STATE") || !strings.Contains(string(message), "Reinstall the node.") {
		t.Errorf("failure message lacks cause or recovery guidance:\n%s", message)
	}
}
