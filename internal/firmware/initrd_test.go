package firmware

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestSplitEarlyInitrd(t *testing.T) {
	early := testCPIO(t, map[string][]byte{
		"early_cpio":                            nil,
		"kernel/x86/microcode/AuthenticAMD.bin": []byte("amd"),
		"kernel/x86/microcode/GenuineIntel.bin": []byte("intel"),
	})
	early = append(early, make([]byte, (512-len(early)%512)%512)...)
	normal := append(slices.Clone(zstdMagic), []byte("normal")...)
	combined := append(slices.Clone(early), normal...)
	report, err := SplitEarlyInitrd(combined)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(report.Early, early) || !bytes.Equal(report.Initramfs, normal) {
		t.Fatalf("split initrd lengths = early %d normal %d, want %d and %d", len(report.Early), len(report.Initramfs), len(early), len(normal))
	}
	if report.EarlySHA256 == "" {
		t.Fatal("early microcode digest is empty")
	}
}

func TestSplitEarlyInitrdRejectsMissingVendor(t *testing.T) {
	early := testCPIO(t, map[string][]byte{
		"kernel/x86/microcode/GenuineIntel.bin": []byte("intel"),
	})
	combined := append(early, zstdMagic...)
	_, err := SplitEarlyInitrd(combined)
	if err == nil || !strings.Contains(err.Error(), "AMD early CPU microcode capability is missing") {
		t.Fatalf("SplitEarlyInitrd() error = %v", err)
	}
}

func TestSplitEarlyInitrdRejectsMicrocodeInNormalInitramfs(t *testing.T) {
	normal := append(slices.Clone(zstdMagic), []byte("microcode")...)
	_, err := SplitEarlyInitrd(normal)
	if err == nil || !strings.Contains(err.Error(), "does not start with an uncompressed early cpio archive") {
		t.Fatalf("SplitEarlyInitrd() error = %v", err)
	}
}

func TestVerifyPackageInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packages.tsv")
	content := strings.Join([]string{
		"linux-firmware\t20260622-1.fc44.noarch",
		"microcode_ctl\t2:2.1-74.fc44.x86_64",
		"amd-ucode-firmware\t20260622-1.fc44.noarch",
		"intel-gpu-firmware\t20260622-1.fc44.noarch",
		"amd-gpu-firmware\t20260622-1.fc44.noarch",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	packages, err := VerifyPackageInventory("runtime", path)
	if err != nil {
		t.Fatal(err)
	}
	if packages["microcode_ctl"] != "2:2.1-74.fc44.x86_64" {
		t.Fatalf("microcode_ctl = %q", packages["microcode_ctl"])
	}
}

func TestVerifyPackageInventoryReportsCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packages.tsv")
	if err := os.WriteFile(path, []byte("linux-firmware\t1.noarch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyPackageInventory("installer", path)
	if err == nil || !strings.Contains(err.Error(), "Intel CPU microcode capability is missing") || !strings.Contains(err.Error(), "microcode_ctl") {
		t.Fatalf("VerifyPackageInventory() error = %v", err)
	}
}

func testCPIO(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	names = append(names, "TRAILER!!!")
	for index, name := range names {
		data := files[name]
		header := "070701" +
			hexField(uint64(index+1)) +
			hexField(0o100644) +
			hexField(0) +
			hexField(0) +
			hexField(1) +
			hexField(0) +
			hexField(uint64(len(data))) +
			hexField(0) +
			hexField(0) +
			hexField(0) +
			hexField(0) +
			hexField(uint64(len(name)+1)) +
			hexField(0)
		if len(header) != newcHeaderSize {
			t.Fatalf("header length = %d", len(header))
		}
		out.WriteString(header)
		out.WriteString(name)
		out.WriteByte(0)
		for out.Len()%4 != 0 {
			out.WriteByte(0)
		}
		out.Write(data)
		for out.Len()%4 != 0 {
			out.WriteByte(0)
		}
	}
	return out.Bytes()
}

func hexField(value uint64) string {
	return fmt.Sprintf("%08x", value)
}
