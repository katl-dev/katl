package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultPartLabelRoot = "/dev/disk/by-partlabel"

type RootSlotDeviceOpener interface {
	OpenRootSlotDevice(context.Context, RootSlotTarget) (RootSlotDevice, error)
}

type FileRootSlotDeviceOpener struct {
	PartLabelRoot string
}

func (o FileRootSlotDeviceOpener) OpenRootSlotDevice(ctx context.Context, target RootSlotTarget) (RootSlotDevice, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	path, err := RootSlotDevicePath(target, o.PartLabelRoot)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open root slot device %s: %w", path, err)
	}
	return file, nil
}

func RootSlotDevicePath(target RootSlotTarget, partLabelRoot string) (string, error) {
	label := strings.TrimSpace(target.GPTLabel)
	if label == "" {
		return "", fmt.Errorf("root slot GPT label is required")
	}
	if filepath.IsAbs(label) || filepath.Clean(label) != label || strings.Contains(label, string(filepath.Separator)) {
		return "", fmt.Errorf("root slot GPT label %q must be a single path segment", target.GPTLabel)
	}
	if partLabelRoot == "" {
		partLabelRoot = defaultPartLabelRoot
	}
	return filepath.Join(partLabelRoot, label), nil
}
