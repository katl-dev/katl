package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireESPSpace(t *testing.T) {
	const mib = 1 << 20
	const uki = "/efi/EFI/katl/katl-root-b-1.efi"
	for _, tc := range []struct {
		name      string
		free      uint64
		leftover  map[string]int
		wantError []string
	}{
		{name: "room for the UKI and margin", free: 100 * mib},
		{name: "exactly the UKI and margin", free: 90*mib + espStagingMargin},
		{name: "one byte short", free: 90*mib + espStagingMargin - 1, wantError: []string{"need 98 MiB", "Free at least 1 MiB on /efi"}},
		{name: "full ESP", free: 0, wantError: []string{"0 MiB available", "need 98 MiB", "Free at least 98 MiB on /efi"}},
		{name: "partial copy is reclaimed", free: 10 * mib, leftover: map[string]int{".sysupdate.partial.katl-root-b-1.efi": 89 * mib}},
		{name: "pending copy is reclaimed", free: 10 * mib, leftover: map[string]int{".sysupdate.pending.katl-root-b-1.efi": 89 * mib}},
		{name: "previous UKI is reclaimed", free: 10 * mib, leftover: map[string]int{"katl-root-b-1.efi": 89 * mib}},
		{name: "other slot's files are not reclaimed", free: 10 * mib, leftover: map[string]int{"katl-root-a-1.efi": 89 * mib, ".sysupdate.partial.katl-root-a-1.efi": 89 * mib}, wantError: []string{"10 MiB available"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "efi/EFI/katl")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, size := range tc.leftover {
				if err := os.Truncate(filepath.Join(dir, name), 0); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(int64(size)); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}

			err := requireESPSpace(func(string) (uint64, error) { return tc.free, nil }, root, uki, 90*mib)

			if len(tc.wantError) == 0 {
				if err != nil {
					t.Fatalf("requireESPSpace() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("requireESPSpace() error = nil, want a refusal")
			}
			for _, want := range tc.wantError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("requireESPSpace() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
