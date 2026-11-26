package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
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

func (s *Server) readEFIBootEntries() ([2]string, error) {
	var entries [2]string
	dir, err := os.Open(rootedPath(s.Root, efiVarsPath))
	if err != nil {
		return entries, fmt.Errorf("open EFI variables: %w. From the node console, mount efivarfs at %s and verify the agent can read it", err, efiVarsPath)
	}
	defer dir.Close()

	var fs unix.Statfs_t
	if err := s.efiStatfs(int(dir.Fd()), &fs); err != nil {
		return entries, fmt.Errorf("inspect efivarfs: %w. From the node console, verify the agent can read %s", err, efiVarsPath)
	}
	if fs.Type != unix.EFIVARFS_MAGIC {
		return entries, fmt.Errorf("from the node console, mount efivarfs at %s and verify it is visible to the agent", efiVarsPath)
	}

	for i, name := range [...]string{"LoaderEntryOneShot", "LoaderEntryDefault"} {
		entry, err := readEFIBootEntry(dir, name)
		if err != nil {
			return entries, fmt.Errorf("read EFI variable %s: %w. From the node console, restore access to this variable before retrying", name, err)
		}
		entries[i] = entry
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
