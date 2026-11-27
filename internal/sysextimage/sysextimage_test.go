package sysextimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckExtractor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		help    string
		wantErr string
	}{
		{name: "supports extract", help: " --extract[=X]          check if all files are well encoded, optionally extract to X"},
		{name: "predates extract", help: " -d#                    set output message level to # (maximum 9)", wantErr: "supports --extract"},
		{name: "absent", wantErr: "install erofs-utils"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.help != "" {
				// PATH holds only this script, so it uses shell builtins.
				script := "#!/bin/sh\nprintf '%s\\n' 'usage: [options] IMAGE' '" + tc.help + "'\n"
				if err := os.WriteFile(filepath.Join(bin, "fsck.erofs"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)

			err := CheckExtractor(t.Context())

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckExtractor() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckExtractor() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRootPartitionWithoutGPT(t *testing.T) {
	image := strings.NewReader(strings.Repeat("\x00", 16<<10))

	if _, _, err := rootPartition(image); err == nil || !strings.Contains(err.Error(), "no GPT partition table") {
		t.Fatalf("rootPartition() error = %v, want no GPT partition table", err)
	}
}
