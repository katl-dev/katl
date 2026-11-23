package disk

import (
	"context"
	"fmt"
	"os"
)

type RootSlotDeviceOpener interface {
	OpenRootSlotDevice(ctx context.Context, devicePath string) (RootSlotDevice, error)
}

type FileRootSlotDeviceOpener struct{}

func (FileRootSlotDeviceOpener) OpenRootSlotDevice(ctx context.Context, devicePath string) (RootSlotDevice, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	file, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open root slot device %s: %w", devicePath, err)
	}
	return file, nil
}
