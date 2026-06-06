package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootSlotDevicePath(t *testing.T) {
	path, err := RootSlotDevicePath(RootSlotTarget{GPTLabel: GPTLabelRootA}, "/dev/test-labels")
	if err != nil {
		t.Fatalf("RootSlotDevicePath() error = %v", err)
	}
	if path != "/dev/test-labels/KATL_ROOT_A" {
		t.Fatalf("path = %q", path)
	}

	_, err = RootSlotDevicePath(RootSlotTarget{GPTLabel: "../KATL_ROOT_A"}, "/dev/test-labels")
	if err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("RootSlotDevicePath() error = %v, want unsafe label rejection", err)
	}
}

func TestFileRootSlotDeviceOpener(t *testing.T) {
	root := t.TempDir()
	devicePath := filepath.Join(root, GPTLabelRootA)
	if err := os.WriteFile(devicePath, []byte("root-slot"), 0o600); err != nil {
		t.Fatalf("write device fixture: %v", err)
	}

	device, err := (FileRootSlotDeviceOpener{PartLabelRoot: root}).OpenRootSlotDevice(context.Background(), RootSlotTarget{GPTLabel: GPTLabelRootA})
	if err != nil {
		t.Fatalf("OpenRootSlotDevice() error = %v", err)
	}
	file, ok := device.(*os.File)
	if !ok {
		t.Fatalf("device = %T, want *os.File", device)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close device: %v", err)
	}
}
