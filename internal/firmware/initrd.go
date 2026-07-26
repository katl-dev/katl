package firmware

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const newcHeaderSize = 110

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var ErrSectionNotFound = errors.New("PE section not found")

type EarlyInitrd struct {
	Early       []byte
	Initramfs   []byte
	Entries     []string
	EarlySHA256 string
}

func SplitEarlyInitrd(data []byte) (EarlyInitrd, error) {
	entries, archiveEnd, err := parseEarlyCPIO(data)
	if err != nil {
		return EarlyInitrd{}, err
	}
	payloadStart := archiveEnd
	for payloadStart < len(data) && data[payloadStart] == 0 {
		payloadStart++
	}
	if payloadStart+len(zstdMagic) > len(data) || !bytes.Equal(data[payloadStart:payloadStart+len(zstdMagic)], zstdMagic) {
		return EarlyInitrd{}, fmt.Errorf("early CPU microcode archive is not followed by a zstd initramfs")
	}
	if err := requireMicrocodeVendors(entries); err != nil {
		return EarlyInitrd{}, err
	}
	early := slices.Clone(data[:payloadStart])
	initramfs := slices.Clone(data[payloadStart:])
	sum := sha256.Sum256(early)
	return EarlyInitrd{
		Early:       early,
		Initramfs:   initramfs,
		Entries:     entries,
		EarlySHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func VerifyEarlyArchive(data []byte) ([]string, error) {
	entries, archiveEnd, err := parseEarlyCPIO(data)
	if err != nil {
		return nil, err
	}
	for _, value := range data[archiveEnd:] {
		if value != 0 {
			return nil, fmt.Errorf("early CPU microcode input contains data after its cpio archive")
		}
	}
	if err := requireMicrocodeVendors(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func JoinEarlyInitrd(early, initramfs []byte) ([]byte, error) {
	if _, err := VerifyEarlyArchive(early); err != nil {
		return nil, err
	}
	if len(initramfs) < len(zstdMagic) || !bytes.Equal(initramfs[:len(zstdMagic)], zstdMagic) {
		return nil, fmt.Errorf("normal initramfs is not zstd-compressed")
	}
	combined := make([]byte, 0, len(early)+len(initramfs))
	combined = append(combined, early...)
	combined = append(combined, initramfs...)
	if _, err := SplitEarlyInitrd(combined); err != nil {
		return nil, fmt.Errorf("verify combined initrd: %w", err)
	}
	return combined, nil
}

func VerifyUKIInitrd(ukiPath, initrdPath string) (EarlyInitrd, error) {
	embedded, err := readPESection(ukiPath, ".initrd")
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("read UKI initrd: %w", err)
	}
	if _, err := readPESection(ukiPath, ".ucode"); err == nil {
		return EarlyInitrd{}, fmt.Errorf("UKI keeps CPU microcode in a separate .ucode section instead of the leading initrd archive")
	} else if !errors.Is(err, ErrSectionNotFound) {
		return EarlyInitrd{}, fmt.Errorf("inspect UKI microcode section: %w", err)
	}
	report, err := SplitEarlyInitrd(embedded)
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("verify UKI initrd: %w", err)
	}
	if strings.TrimSpace(initrdPath) == "" {
		return report, nil
	}
	loose, err := os.ReadFile(initrdPath)
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("read loose initrd %s: %w", initrdPath, err)
	}
	if !bytes.Equal(embedded, loose) {
		return EarlyInitrd{}, fmt.Errorf("UKI initrd does not match loose initrd %s", initrdPath)
	}
	return report, nil
}

func NormalizeInstaller(ukiPath, initrdPath, ukify string) error {
	if _, err := os.Stat(ukiPath); err != nil {
		return fmt.Errorf("stat installer UKI %s: %w", ukiPath, err)
	}
	if _, err := os.Stat(initrdPath); err != nil {
		return fmt.Errorf("stat installer initrd %s: %w", initrdPath, err)
	}
	microcode, err := readPESection(ukiPath, ".ucode")
	if errors.Is(err, ErrSectionNotFound) {
		_, verifyErr := VerifyUKIInitrd(ukiPath, initrdPath)
		return verifyErr
	}
	if err != nil {
		return fmt.Errorf("read installer UKI microcode: %w", err)
	}
	normal, err := readPESection(ukiPath, ".initrd")
	if err != nil {
		return fmt.Errorf("read installer UKI initrd: %w", err)
	}
	combined, err := JoinEarlyInitrd(microcode, normal)
	if err != nil {
		return fmt.Errorf("combine installer microcode and initramfs: %w", err)
	}
	linux, err := readPESection(ukiPath, ".linux")
	if err != nil {
		return fmt.Errorf("read installer UKI kernel: %w", err)
	}
	osRelease, err := readPESection(ukiPath, ".osrel")
	if err != nil {
		return fmt.Errorf("read installer UKI os-release: %w", err)
	}
	cmdline, err := readPESection(ukiPath, ".cmdline")
	if err != nil {
		return fmt.Errorf("read installer UKI command line: %w", err)
	}
	uname, err := readPESection(ukiPath, ".uname")
	if err != nil {
		return fmt.Errorf("read installer UKI kernel version: %w", err)
	}

	dir := filepath.Dir(ukiPath)
	work, err := os.MkdirTemp(dir, ".katl-installer-uki-")
	if err != nil {
		return fmt.Errorf("create installer UKI work directory: %w", err)
	}
	defer os.RemoveAll(work)
	write := func(name string, data []byte) (string, error) {
		path := filepath.Join(work, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
		return path, nil
	}
	linuxPath, err := write("linux", linux)
	if err != nil {
		return fmt.Errorf("write installer kernel input: %w", err)
	}
	initrdPathTmp, err := write("initrd", combined)
	if err != nil {
		return fmt.Errorf("write installer initrd input: %w", err)
	}
	osReleasePath, err := write("os-release", osRelease)
	if err != nil {
		return fmt.Errorf("write installer os-release input: %w", err)
	}
	cmdlinePath, err := write("cmdline", cmdline)
	if err != nil {
		return fmt.Errorf("write installer command-line input: %w", err)
	}
	outputPath := filepath.Join(work, "installer.efi")
	if strings.TrimSpace(ukify) == "" {
		ukify = "ukify"
	}
	version := strings.Trim(string(uname), "\x00\r\n ")
	if version == "" {
		return fmt.Errorf("installer UKI kernel version is empty")
	}
	output, err := exec.Command(
		ukify,
		"build",
		"--linux", linuxPath,
		"--initrd", initrdPathTmp,
		"--os-release", "@"+osReleasePath,
		"--cmdline", "@"+cmdlinePath,
		"--uname", version,
		"--output", outputPath,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rebuild installer UKI: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if _, err := VerifyUKIInitrd(outputPath, initrdPathTmp); err != nil {
		return fmt.Errorf("verify rebuilt installer UKI: %w", err)
	}
	ukiInfo, err := os.Stat(ukiPath)
	if err != nil {
		return fmt.Errorf("stat installer UKI mode: %w", err)
	}
	initrdInfo, err := os.Stat(initrdPath)
	if err != nil {
		return fmt.Errorf("stat installer initrd mode: %w", err)
	}
	if err := os.Chmod(outputPath, ukiInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("set rebuilt installer UKI mode: %w", err)
	}
	if err := writeAtomic(initrdPath, combined, initrdInfo.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(outputPath, ukiPath); err != nil {
		return fmt.Errorf("replace installer UKI: %w", err)
	}
	_, err = VerifyUKIInitrd(ukiPath, initrdPath)
	return err
}

func WriteSplit(input, earlyPath, initramfsPath string) (EarlyInitrd, error) {
	data, err := os.ReadFile(input)
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("read initrd %s: %w", input, err)
	}
	report, err := SplitEarlyInitrd(data)
	if err != nil {
		return EarlyInitrd{}, err
	}
	if err := writeAtomic(earlyPath, report.Early, 0o644); err != nil {
		return EarlyInitrd{}, err
	}
	if err := writeAtomic(initramfsPath, report.Initramfs, 0o644); err != nil {
		return EarlyInitrd{}, err
	}
	return report, nil
}

func WriteJoined(earlyPath, initramfsPath, outputPath string) (EarlyInitrd, error) {
	early, err := os.ReadFile(earlyPath)
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("read early microcode archive %s: %w", earlyPath, err)
	}
	initramfs, err := os.ReadFile(initramfsPath)
	if err != nil {
		return EarlyInitrd{}, fmt.Errorf("read normal initramfs %s: %w", initramfsPath, err)
	}
	combined, err := JoinEarlyInitrd(early, initramfs)
	if err != nil {
		return EarlyInitrd{}, err
	}
	if err := writeAtomic(outputPath, combined, 0o644); err != nil {
		return EarlyInitrd{}, err
	}
	return SplitEarlyInitrd(combined)
}

func parseEarlyCPIO(data []byte) ([]string, int, error) {
	if len(data) < newcHeaderSize || (string(data[:6]) != "070701" && string(data[:6]) != "070702") {
		return nil, 0, fmt.Errorf("initrd does not start with an uncompressed early cpio archive")
	}
	var entries []string
	offset := 0
	for {
		if offset+newcHeaderSize > len(data) {
			return nil, 0, fmt.Errorf("early cpio header is truncated at byte %d", offset)
		}
		header := data[offset : offset+newcHeaderSize]
		if string(header[:6]) != "070701" && string(header[:6]) != "070702" {
			return nil, 0, fmt.Errorf("invalid early cpio header magic at byte %d", offset)
		}
		fileSize, err := parseHexField(header[54:62])
		if err != nil {
			return nil, 0, fmt.Errorf("parse early cpio file size at byte %d: %w", offset, err)
		}
		nameSize, err := parseHexField(header[94:102])
		if err != nil {
			return nil, 0, fmt.Errorf("parse early cpio name size at byte %d: %w", offset, err)
		}
		if nameSize == 0 {
			return nil, 0, fmt.Errorf("early cpio entry at byte %d has an empty name", offset)
		}
		nameStart := offset + newcHeaderSize
		nameEnd, ok := checkedEnd(nameStart, nameSize, len(data))
		if !ok {
			return nil, 0, fmt.Errorf("early cpio entry name is truncated at byte %d", offset)
		}
		nameBytes := data[nameStart:nameEnd]
		if nameBytes[len(nameBytes)-1] != 0 {
			return nil, 0, fmt.Errorf("early cpio entry name is not NUL-terminated at byte %d", offset)
		}
		name := strings.TrimPrefix(string(nameBytes[:len(nameBytes)-1]), "./")
		dataStart := align4(nameEnd)
		dataEnd, ok := checkedEnd(dataStart, fileSize, len(data))
		if !ok {
			return nil, 0, fmt.Errorf("early cpio entry %q is truncated", name)
		}
		offset = align4(dataEnd)
		if offset > len(data) {
			return nil, 0, fmt.Errorf("early cpio entry %q padding is truncated", name)
		}
		if name == "TRAILER!!!" {
			sort.Strings(entries)
			return entries, offset, nil
		}
		entries = append(entries, name)
	}
}

func parseHexField(value []byte) (uint64, error) {
	number, err := strconv.ParseUint(string(value), 16, 64)
	if err != nil {
		return 0, err
	}
	return number, nil
}

func checkedEnd(start int, size uint64, limit int) (int, bool) {
	if size > uint64(limit) || uint64(start)+size > uint64(limit) {
		return 0, false
	}
	return start + int(size), true
}

func align4(value int) int {
	return (value + 3) &^ 3
}

func requireMicrocodeVendors(entries []string) error {
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		present[entry] = true
	}
	for _, capability := range []struct {
		path   string
		vendor string
	}{
		{"kernel/x86/microcode/GenuineIntel.bin", "Intel"},
		{"kernel/x86/microcode/AuthenticAMD.bin", "AMD"},
	} {
		if !present[capability.path] {
			return fmt.Errorf("%s early CPU microcode capability is missing: expected /%s in the leading cpio archive", capability.vendor, capability.path)
		}
	}
	return nil
}

func readPESection(path, name string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var dos [64]byte
	if _, err := file.ReadAt(dos[:], 0); err != nil {
		return nil, fmt.Errorf("read DOS header: %w", err)
	}
	peOffset := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	var header [24]byte
	if _, err := file.ReadAt(header[:], peOffset); err != nil {
		return nil, fmt.Errorf("read PE header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte{'P', 'E', 0, 0}) {
		return nil, fmt.Errorf("invalid PE signature")
	}
	sectionCount := int(binary.LittleEndian.Uint16(header[6:8]))
	optionalSize := int64(binary.LittleEndian.Uint16(header[20:22]))
	sectionOffset := peOffset + int64(len(header)) + optionalSize
	for index := range sectionCount {
		var section [40]byte
		offset := sectionOffset + int64(index*len(section))
		if _, err := file.ReadAt(section[:], offset); err != nil {
			return nil, fmt.Errorf("read PE section header %d: %w", index, err)
		}
		sectionName := string(bytes.TrimRight(section[:8], "\x00"))
		if sectionName != name {
			continue
		}
		virtualSize := int64(binary.LittleEndian.Uint32(section[8:12]))
		rawSize := int64(binary.LittleEndian.Uint32(section[16:20]))
		rawOffset := int64(binary.LittleEndian.Uint32(section[20:24]))
		size := rawSize
		if virtualSize > 0 && virtualSize <= rawSize {
			size = virtualSize
		}
		if size < 0 || rawOffset < 0 || rawOffset+size > info.Size() {
			return nil, fmt.Errorf("PE section %s points outside the file", name)
		}
		data := make([]byte, size)
		if _, err := file.ReadAt(data, rawOffset); err != nil {
			return nil, fmt.Errorf("read PE section %s: %w", name, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrSectionNotFound, name)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s directory: %w", path, err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", path, err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary %s: %w", path, err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set temporary %s mode: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
