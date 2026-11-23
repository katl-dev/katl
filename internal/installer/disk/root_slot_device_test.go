package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileRootSlotDeviceOpener(t *testing.T) {
	devicePath := filepath.Join(t.TempDir(), "root-slot")
	if err := os.WriteFile(devicePath, []byte("root-slot"), 0o600); err != nil {
		t.Fatalf("write device fixture: %v", err)
	}

	device, err := (FileRootSlotDeviceOpener{}).OpenRootSlotDevice(context.Background(), devicePath)
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
