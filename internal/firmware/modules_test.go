package firmware

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntelGraphicsFirmware(t *testing.T) {
	for _, module := range []string{"i915/i915.ko", "xe/xe.ko"} {
		t.Run(module, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "usr/lib/modules/test/kernel/drivers/gpu/drm", module)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			modinfo := filepath.Join(root, "modinfo")
			if err := os.WriteFile(modinfo, []byte("#!/bin/sh\nprintf 'gpu/display.bin\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			_, _, err := VerifyInstallerModuleFirmware(root, modinfo)
			if err == nil || !strings.Contains(err.Error(), "requires gpu/display.bin") {
				t.Fatalf("missing graphics firmware: %v", err)
			}

			firmware := filepath.Join(root, "usr/lib/firmware/gpu/display.bin.zst")
			if err := os.MkdirAll(filepath.Dir(firmware), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := VerifyInstallerModuleFirmware(root, modinfo); err != nil {
				t.Fatalf("included graphics firmware: %v", err)
			}
		})
	}
}
