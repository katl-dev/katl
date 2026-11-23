package discovery

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
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
	ByPath              []string
	WWN                 string
	Serial              string
	Model               string
	GPTLabel            string
	PartitionUUID       string
	PartitionTableUUID  string
	FilesystemUUID      string
	SizeBytes           uint64
	ReadOnly            bool
	Rotational          bool
	Transport           string
	DiskSeq             uint64
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

type TargetDiskMatch struct {
	Device     BlockDevice
	Signatures []SignatureReport
}

type PartitionSelector struct {
	ByID           string
	PartUUID       string
	FilesystemUUID string
	PartLabel      string
}

type PartitionMatch struct {
	Device     BlockDevice
	ParentDisk BlockDevice
	Signatures []SignatureReport
}

type SignatureReport struct {
	DevicePath string
	Kind       string
	Value      string
}

var (
	ErrMissingTargetDiskSelector = errors.New("target disk selector must set at least one of byID, wwn, serial, byPath, gptGUID, name, model, size, or type")
	ErrUnsafeTargetDisk          = errors.New("unsafe target disk match")
	ErrTargetDiskChanged         = errors.New("selected target disk changed")
)

func MatchPartition(facts HardwareFacts, selector PartitionSelector) (PartitionMatch, error) {
	if err := selector.validate(); err != nil {
		return PartitionMatch{}, err
	}

	var matches []PartitionMatch
	for _, disk := range facts.BlockDevices {
		for _, partition := range disk.Partitions {
			if selector.matches(partition) {
				matches = append(matches, PartitionMatch{
					Device:     partition,
					ParentDisk: disk,
					Signatures: collectSignatures(partition),
				})
			}
		}
	}

	switch len(matches) {
	case 0:
		return PartitionMatch{}, fmt.Errorf("partition selector matched no partitions")
	case 1:
	default:
		return PartitionMatch{}, fmt.Errorf("partition selector matched %d partitions", len(matches))
	}

	match := matches[0]
	if err := validatePartitionSafety(facts, match.Device); err != nil {
		return PartitionMatch{}, err
	}
	return match, nil
}

func (s PartitionSelector) validate() error {
	hasSelector := 0
	if strings.TrimSpace(s.ByID) != "" {
		hasSelector++
		if !strings.HasPrefix(s.ByID, "/dev/disk/by-id/") || path.Clean(s.ByID) != s.ByID {
			return fmt.Errorf("partition by-id selector must be an absolute /dev/disk/by-id path")
		}
	}
	if strings.TrimSpace(s.PartUUID) != "" {
		hasSelector++
	}
	if strings.TrimSpace(s.FilesystemUUID) != "" {
		hasSelector++
	}
	if strings.TrimSpace(s.PartLabel) != "" {
		hasSelector++
	}
	if hasSelector == 0 {
		return fmt.Errorf("partition selector is required")
	}
	if hasSelector > 1 {
		return fmt.Errorf("partition selector must use exactly one stable identity")
	}
	return nil
}

func (s PartitionSelector) matches(device BlockDevice) bool {
	if device.Type != DevicePartition {
		return false
	}
	switch {
	case s.ByID != "":
		return slices.Contains(device.ByID, s.ByID) || device.Path == s.ByID
	case s.PartUUID != "":
		return device.PartitionUUID == s.PartUUID
	case s.FilesystemUUID != "":
		return device.FilesystemUUID == s.FilesystemUUID
	case s.PartLabel != "":
		return device.GPTLabel == s.PartLabel
	default:
		return false
	}
}

func validatePartitionSafety(facts HardwareFacts, device BlockDevice) error {
	if device.Type != DevicePartition {
		return fmt.Errorf("%w: %s is %s, not a partition", ErrUnsafeTargetDisk, device.Path, device.Type)
	}
	if device.ReadOnly {
		return fmt.Errorf("%w: %s is read-only", ErrUnsafeTargetDisk, device.Path)
	}
	if partitionHasMounts(facts, device) {
		return fmt.Errorf("%w: partition %s is mounted", ErrUnsafeTargetDisk, device.Path)
	}
	return nil
}

func partitionHasMounts(facts HardwareFacts, device BlockDevice) bool {
	if len(device.Mountpoints) > 0 {
		return true
	}
	sources := map[string]struct{}{device.Path: {}}
	if device.PartitionUUID != "" {
		sources["PARTUUID="+device.PartitionUUID] = struct{}{}
	}
	if device.FilesystemUUID != "" {
		sources["UUID="+device.FilesystemUUID] = struct{}{}
	}
	for _, alias := range device.ByID {
		sources[alias] = struct{}{}
	}
	for _, mount := range facts.Mounts {
		if _, ok := sources[mount.Source]; ok {
			return true
		}
	}
	return false
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
