package efiboot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/sys/unix"
)

const (
	efiVarsPath   = "/sys/firmware/efi/efivars"
	loaderVarGUID = "-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"
)

// Reader observes the systemd-boot variables on mounted efivarfs. Its zero
// value uses the running kernel to verify the mount.
type Reader struct {
	Statfs func(int, *unix.Statfs_t) error
}

func (r Reader) Read(root string) (Entries, error) {
	var entries Entries
	dir, err := os.Open(filepath.Join(root, efiVarsPath))
	if err != nil {
		return entries, fmt.Errorf("open EFI variables: %w. From the node console, mount efivarfs at %s and verify Katl can read it", err, efiVarsPath)
	}
	defer dir.Close()

	statfs := r.Statfs
	if statfs == nil {
		statfs = unix.Fstatfs
	}
	var fs unix.Statfs_t
	if err := statfs(int(dir.Fd()), &fs); err != nil {
		return entries, fmt.Errorf("inspect efivarfs: %w. From the node console, verify Katl can read %s", err, efiVarsPath)
	}
	if fs.Type != unix.EFIVARFS_MAGIC {
		return entries, fmt.Errorf("from the node console, mount efivarfs at %s and verify it is visible to Katl", efiVarsPath)
	}

	for _, variable := range []struct {
		name  string
		entry *string
	}{
		{"LoaderEntryOneShot", &entries.OneShot},
		{"LoaderEntryDefault", &entries.Default},
	} {
		name := variable.name
		entry, err := readEFIBootEntry(dir, name)
		if err != nil {
			return Entries{}, fmt.Errorf("read EFI variable %s: %w. From the node console, restore access to this variable before retrying", name, err)
		}
		*variable.entry = entry
	}
	return entries, nil
}

func readEFIBootEntry(dir *os.File, name string) (string, error) {
	// Resolve the variable on the same filesystem whose type was checked.
	filename := name + loaderVarGUID
	fd, err := unix.Openat(int(dir.Fd()), filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), filename)
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}

	// efivarfs prefixes the UTF-16LE entry ID with four attribute bytes.
	const attributesSize = 4
	if len(data) <= attributesSize || len(data)%2 != 0 {
		return "", errors.New("invalid EFI entry data")
	}
	payload := data[attributesSize:]
	units := make([]uint16, len(payload)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(payload[2*i:])
	}
	if end := slices.Index(units, 0); end >= 0 {
		units = units[:end]
	}
	entry := string(utf16.Decode(units))
	if entry == "" || strings.ContainsRune(entry, unicode.ReplacementChar) {
		return "", errors.New("invalid EFI entry ID")
	}
	return entry, nil
}
