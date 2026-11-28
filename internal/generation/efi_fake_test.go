package generation

import (
	"cmp"
	"errors"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/efiboot"
)

// fakeEFI models the two systemd-boot variables. Firmware consumes the
// one-shot on every boot, including a boot chosen from the menu.
type fakeEFI struct {
	entries efiboot.Entries
	fault   string // "read", "read-back", "write" or "mismatch"
	written bool
}

var errEFIFault = errors.New("injected EFI fault")

func (f *fakeEFI) read(string) (efiboot.Entries, error) {
	if f.fault == "read" || (f.fault == "read-back" && f.written) {
		return efiboot.Entries{}, errEFIFault
	}
	return f.entries, nil
}

func (f *fakeEFI) set(entry *string) BootDefaultSetter {
	return func(_ string, path string) error {
		f.written = true
		switch f.fault {
		case "write":
			return errEFIFault
		case "mismatch":
			return nil
		}
		*entry = ""
		if path != "" {
			*entry = filepath.Base(path)
		}
		return nil
	}
}

func (f *fakeEFI) boot() string {
	entry := cmp.Or(f.entries.OneShot, f.entries.Default)
	f.entries.OneShot = ""
	return strings.TrimSuffix(strings.TrimPrefix(entry, "katl-"), ".conf")
}
