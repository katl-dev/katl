package generation

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBootability(t *testing.T) {
	for _, test := range []struct {
		name, evidence string
		change         func(t *testing.T, root string, spec *GenerationSpec)
		want           bootabilityState
	}{
		{name: "matching image on a larger partition", want: bootable, evidence: "runtime SHA-256"},
		{name: "different bytes under the same PARTUUID", want: bootUnavailable, evidence: "expected",
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				writeBootFixture(t, bootDevicePath(root, spec.Root.Slot), bootRootImage("different image"))
			}},
		{name: "bound partition absent", want: bootUnavailable, evidence: "absent from the system disk",
			change: func(_ *testing.T, _ string, spec *GenerationSpec) {
				spec.Root.PartitionUUID = "99999999-2222-3333-4444-555555555555"
			}},
		{name: "loader entry missing", want: bootUnavailable, evidence: "/efi/loader/entries/",
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				removeBootFixture(t, filepath.Join(root, "efi", spec.Boot.LoaderEntryPath))
			}},
		{name: "UKI missing", want: bootUnavailable, evidence: "/efi/EFI/katl/",
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				removeBootFixture(t, rootedPathUnchecked(root, spec.Boot.UKIPath))
			}},
		{name: "sysext missing", want: bootUnavailable, evidence: "/sysext/",
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				removeBootFixture(t, rootedPathUnchecked(root, spec.Sysexts[len(spec.Sysexts)-1].Path))
			}},
		{name: "bundled confext missing", want: bootUnavailable, evidence: "/bundled-confext/",
			change: func(_ *testing.T, _ string, spec *GenerationSpec) {
				spec.BundledConfexts = []ExtensionRef{{Path: "/var/lib/katl/generations/current/bundled-confext/missing.raw"}}
			}},
		{name: "generated confext missing", want: bootUnavailable, evidence: "/confext",
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				removeBootFixture(t, rootedPathUnchecked(root, spec.Confexts[0].Path))
			}},
		{name: "partition node cannot be opened", want: bootIndeterminate,
			change: func(t *testing.T, root string, spec *GenerationSpec) {
				removeBootFixture(t, bootDevicePath(root, spec.Root.Slot))
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _ := managementFixture(t)
			spec, status, err := ReadGeneration(root, "current")
			if err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(t, root, &spec)
			}
			probe := bootabilityCheck{root: root}
			got := probe.check(spec, status)
			if got.state != test.want || !strings.Contains(got.evidence, test.evidence) {
				t.Fatalf("bootability = %+v, want %d with %q", got, test.want, test.evidence)
			}
			if test.want == bootIndeterminate && !errors.Is(got.err, os.ErrNotExist) {
				t.Fatalf("indeterminate cause = %v, want missing device", got.err)
			}
		})
	}
}

type rootReadError struct {
	*bytes.Reader
}

func (r rootReadError) ReadAt(p []byte, off int64) (int, error) {
	// A runtime data sector can fail while the superblock remains readable.
	readable := 512 - off
	if readable <= 0 {
		return 0, unix.EIO
	}
	if int64(len(p)) > readable {
		n, _ := r.Reader.ReadAt(p[:readable], off)
		return n, unix.EIO
	}
	return r.Reader.ReadAt(p, off)
}

func TestRootArtifactReadError(t *testing.T) {
	got := readRootArtifact(rootReadError{bytes.NewReader(bootRootImage("runtime"))}, "root-test", 4096)
	if got.state != bootIndeterminate || !errors.Is(got.err, unix.EIO) {
		t.Fatalf("bootability = %+v, want indeterminate with EIO", got)
	}
}

func TestRootArtifactRejectsImplausibleExtent(t *testing.T) {
	for _, used := range []uint64{95, 4097, 1 << 63} {
		image := append(bootRootImage("runtime"), make([]byte, 4096)...)
		binary.LittleEndian.PutUint64(image[40:48], used)
		got := readRootArtifact(bytes.NewReader(image), "root-test", 4096)
		if got.state != bootIndeterminate || !strings.Contains(got.err.Error(), "implausible SquashFS bytes_used") {
			t.Errorf("bytes_used %d: bootability = %+v, want an indeterminate extent", used, got)
		}
	}
}

func TestDiscoveryErrorsProtectRollback(t *testing.T) {
	for _, fault := range []struct {
		name   string
		inject func(*testing.T, string)
		want   error
	}{
		{"partition attribute", func(t *testing.T, root string) {
			path := filepath.Join(root, "sys/devices/vda/vda2/partition")
			removeBootFixture(t, path)
			linkBootFixture(t, path, path)
		}, unix.ELOOP},
		{"clone scan", func(t *testing.T, root string) {
			dir := filepath.Join(root, "sys/devices/vdb/vdb1")
			writeBootFixture(t, filepath.Join(dir, "partition"), []byte("1"))
			linkBootFixture(t, filepath.Join(dir, "uevent"), "/proc/self/mem")
			linkBootFixture(t, filepath.Join(root, "sys/class/block/vdb1"), dir)
		}, unix.EIO},
	} {
		t.Run(fault.name, func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "old", "root-b", "1", now.Add(-90*24*time.Hour))
			managedFixture(t, root, "recent", "root-b", "1", now.Add(-time.Hour))
			fault.inject(t, root)

			spec, state, err := ReadGeneration(root, "old")
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckBootability(root, spec, state); !errors.Is(err, fault.want) || !strings.Contains(err.Error(), "bootability indeterminate") {
				t.Fatalf("check = %v, want indeterminate with %v", err, fault.want)
			}
			if err := Remove(root, "old"); err == nil || !strings.Contains(err.Error(), "bootability indeterminate") {
				t.Fatalf("remove = %v, want indeterminate protection", err)
			}
			keepLast := 1
			if removed, err := Prune(root, Retention{KeepLast: &keepLast, MaxAge: "30d"}, now); err != nil || len(removed) != 0 {
				t.Fatalf("prune = %v, %v, want old generation retained", removed, err)
			}
		})
	}
}

func TestBootabilityGuidesSelectionAndCleanup(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		makeRoot     func(*testing.T, string)
	}{
		{name: "changed bytes", reason: "runtime SHA-256",
			makeRoot: func(t *testing.T, path string) { writeBootFixture(t, path, bootRootImage("other image")) }},
		{name: "indeterminate", reason: "bootability indeterminate",
			makeRoot: func(t *testing.T, path string) { linkBootFixture(t, path, path) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "old", "root-b", "1", now.Add(-90*24*time.Hour))
			managedFixture(t, root, "recent", "root-b", "1", now.Add(-time.Hour))
			path := bootDevicePath(root, "root-b")
			removeBootFixture(t, path)
			test.makeRoot(t, path)

			items, before, err := Inspect(root)
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(items, func(item ManagedGeneration) bool { return item.Spec.GenerationID == "old" })
			if index < 0 || items[index].UnavailableReason != "" {
				t.Fatalf("listed generations = %+v, want old with its boot artifacts present", items)
			}
			setter := func(_, entry string) error {
				t.Errorf("unexpected EFI write to %q", entry)
				return nil
			}
			err = Select(t.Context(), SelectRequest{Root: root, GenerationID: "old", SetDefault: setter, SetOneshot: setter})
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("selection = %v, want root evidence %q", err, test.reason)
			}
			if after, err := ReadBootSelection(root); err != nil || after != before {
				t.Fatalf("selection changed to %+v, %v", after, err)
			}
			if test.name == "indeterminate" {
				if err := Remove(root, "old"); err == nil || !strings.Contains(err.Error(), test.reason) {
					t.Fatalf("remove = %v, want indeterminate protection", err)
				}
				keepLast := 1
				if removed, err := Prune(root, Retention{KeepLast: &keepLast, MaxAge: "30d"}, now); err != nil || len(removed) != 0 {
					t.Fatalf("prune = %v, %v, want old generation retained", removed, err)
				}
			}
		})
	}
}

func bootRootImage(content string) []byte {
	// A minimal superblock and data model the relevant on-disk layout. Include
	// the standard mksquashfs padding in the fixture artifact, before slot tail.
	image := make([]byte, 4096)
	copy(image, "hsqs")
	binary.LittleEndian.PutUint64(image[40:48], uint64(96+len(content)))
	copy(image[96:], content)
	return image
}

func bootDevicePath(root, slot string) string {
	part := "1"
	if slot == "root-b" {
		part = "2"
	}
	return filepath.Join(root, "dev/disk/by-diskseq", "17-part"+part)
}

func writeBootDisk(t *testing.T, root string) {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(root, "sys")
	disk := filepath.Join(sys, "devices", "vda")
	writeBootFixture(t, filepath.Join(disk, "diskseq"), []byte("17\n"))
	for i, part := range []struct{ label, uuid string }{
		{"KATL_ROOT_A", "11111111-2222-3333-4444-555555555555"},
		{"KATL_ROOT_B", "22222222-2222-3333-4444-555555555555"},
		{"KATL_ESP", "33333333-2222-3333-4444-555555555555"},
		{"KATL_STATE", "44444444-2222-3333-4444-555555555555"},
	} {
		name := fmt.Sprintf("vda%d", i+1)
		dir := filepath.Join(disk, name)
		writeBootFixture(t, filepath.Join(dir, "partition"), []byte{byte('1' + i)})
		writeBootFixture(t, filepath.Join(dir, "uevent"), fmt.Appendf(nil, "DEVNAME=%s\nPARTN=%d\nPARTNAME=%s\nPARTUUID=%s\n", name, i+1, part.label, part.uuid))
		linkBootFixture(t, filepath.Join(sys, "class", "block", name), dir)
	}
	device := fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
	linkBootFixture(t, filepath.Join(sys, "dev", "block", device), filepath.Join(disk, "vda1"))
}

func writeBootFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func linkBootFixture(t *testing.T, path, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func removeBootFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
