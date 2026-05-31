package installer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

type DeviceType string

const (
	DeviceDisk      DeviceType = "disk"
	DevicePartition DeviceType = "part"
)

type HardwareFacts struct {
	BlockDevices   []BlockDevice
	NICs           []NICFact
	SystemUUID     string
	DMIProductUUID string
	Mounts         []MountFact
}

type BlockDevice struct {
	Name                string
	Path                string
	Type                DeviceType
	ByID                []string
	WWN                 string
	Serial              string
	Model               string
	GPTLabel            string
	SizeBytes           uint64
	ReadOnly            bool
	FilesystemSignature string
	PartitionSignature  string
	Mountpoints         []string
	Partitions          []BlockDevice
}

type NICFact struct {
	Name       string
	MACAddress string
	Driver     string
	OperState  string
}

type MountFact struct {
	Source     string
	Target     string
	Filesystem string
	Options    []string
}

type DiscoverySource interface {
	Discover(context.Context) (HardwareFacts, error)
}

type StaticDiscoverySource struct {
	Facts HardwareFacts
	Err   error
}

func (s StaticDiscoverySource) Discover(ctx context.Context) (HardwareFacts, error) {
	select {
	case <-ctx.Done():
		return HardwareFacts{}, ctx.Err()
	default:
	}
	if s.Err != nil {
		return HardwareFacts{}, s.Err
	}
	return s.Facts, nil
}

type TargetDiskSelector struct {
	ByID       string
	WWN        string
	Serial     string
	MinSizeMiB uint64
}

type TargetDiskMatch struct {
	Device     BlockDevice
	Signatures []SignatureReport
}

type SignatureReport struct {
	DevicePath string
	Kind       string
	Value      string
}

var (
	ErrMissingTargetDiskSelector = errors.New("target disk selector is required")
	ErrUnsafeTargetDisk          = errors.New("unsafe target disk match")
)

func MatchTargetDisk(facts HardwareFacts, selector TargetDiskSelector) (TargetDiskMatch, error) {
	if err := selector.validate(); err != nil {
		return TargetDiskMatch{}, err
	}

	var matches []BlockDevice
	for _, device := range facts.BlockDevices {
		if selector.matches(device) {
			matches = append(matches, device)
		}
	}

	switch len(matches) {
	case 0:
		return TargetDiskMatch{}, fmt.Errorf("target disk selector matched no disks")
	case 1:
	default:
		return TargetDiskMatch{}, fmt.Errorf("target disk selector matched %d disks", len(matches))
	}

	device := matches[0]
	if err := validateTargetDiskSafety(facts, device, selector); err != nil {
		return TargetDiskMatch{}, err
	}

	return TargetDiskMatch{
		Device:     device,
		Signatures: collectSignatures(device),
	}, nil
}

func (s TargetDiskSelector) validate() error {
	hasSelector := 0
	if strings.TrimSpace(s.ByID) != "" {
		hasSelector++
		if !strings.HasPrefix(s.ByID, "/dev/disk/by-id/") || path.Clean(s.ByID) != s.ByID {
			return fmt.Errorf("%w: by-id selector must be an absolute /dev/disk/by-id path", ErrMissingTargetDiskSelector)
		}
	}
	if strings.TrimSpace(s.WWN) != "" {
		hasSelector++
	}
	if strings.TrimSpace(s.Serial) != "" {
		hasSelector++
	}

	if hasSelector == 0 {
		return ErrMissingTargetDiskSelector
	}
	if hasSelector > 1 {
		return fmt.Errorf("target disk selector must use exactly one stable identity")
	}

	return nil
}

func (s TargetDiskSelector) matches(device BlockDevice) bool {
	switch {
	case s.ByID != "":
		for _, alias := range device.ByID {
			if alias == s.ByID {
				return true
			}
		}
		return device.Path == s.ByID
	case s.WWN != "":
		return device.WWN == s.WWN
	case s.Serial != "":
		return device.Serial == s.Serial
	default:
		return false
	}
}

func validateTargetDiskSafety(facts HardwareFacts, device BlockDevice, selector TargetDiskSelector) error {
	if device.Type != DeviceDisk {
		return fmt.Errorf("%w: %s is %s, not a whole disk", ErrUnsafeTargetDisk, device.Path, device.Type)
	}
	if device.ReadOnly {
		return fmt.Errorf("%w: %s is read-only", ErrUnsafeTargetDisk, device.Path)
	}
	if selector.MinSizeMiB > 0 && device.SizeBytes < selector.MinSizeMiB*1024*1024 {
		return fmt.Errorf("%w: %s is smaller than %d MiB", ErrUnsafeTargetDisk, device.Path, selector.MinSizeMiB)
	}
	if diskHasMounts(facts, device) {
		return fmt.Errorf("%w: %s or one of its partitions is mounted", ErrUnsafeTargetDisk, device.Path)
	}

	return nil
}

func diskHasMounts(facts HardwareFacts, device BlockDevice) bool {
	paths := map[string]struct{}{device.Path: {}}
	for _, partition := range device.Partitions {
		paths[partition.Path] = struct{}{}
		if len(partition.Mountpoints) > 0 {
			return true
		}
	}
	if len(device.Mountpoints) > 0 {
		return true
	}

	for _, mount := range facts.Mounts {
		if _, ok := paths[mount.Source]; ok {
			return true
		}
	}

	return false
}

func collectSignatures(device BlockDevice) []SignatureReport {
	var signatures []SignatureReport
	add := func(devicePath, kind, value string) {
		if value == "" {
			return
		}
		signatures = append(signatures, SignatureReport{
			DevicePath: devicePath,
			Kind:       kind,
			Value:      value,
		})
	}

	add(device.Path, "filesystem", device.FilesystemSignature)
	add(device.Path, "partition-table", device.PartitionSignature)
	for _, partition := range device.Partitions {
		add(partition.Path, "filesystem", partition.FilesystemSignature)
		add(partition.Path, "partition-type", partition.PartitionSignature)
	}

	return signatures
}
