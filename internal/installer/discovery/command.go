package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type OutputCommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type CommandDiscoverySource struct {
	Commands    OutputCommandRunner
	ByIDDir     string
	ByPathDir   string
	SysBlockDir string
}

func NewCommandDiscoverySource(commands OutputCommandRunner) CommandDiscoverySource {
	return CommandDiscoverySource{Commands: commands}
}

func (s CommandDiscoverySource) Discover(ctx context.Context) (HardwareFacts, error) {
	if s.Commands == nil {
		return HardwareFacts{}, fmt.Errorf("discovery command runner is required")
	}

	lsblk, err := s.Commands.Output(ctx, "lsblk", "--json", "--bytes", "--output", "NAME,PATH,TYPE,SIZE,RO,ROTA,TRAN,MODEL,SERIAL,WWN,FSTYPE,UUID,PTTYPE,PTUUID,PARTTYPE,PARTUUID,PARTLABEL,MOUNTPOINTS")
	if err != nil {
		return HardwareFacts{}, fmt.Errorf("lsblk discovery: %w", err)
	}
	facts, err := parseLSBLK(lsblk)
	if err != nil {
		return HardwareFacts{}, err
	}
	byID, err := readAliases(firstNonEmpty(s.ByIDDir, "/dev/disk/by-id"))
	if err != nil {
		return HardwareFacts{}, err
	}
	byPath, err := readAliases(firstNonEmpty(s.ByPathDir, "/dev/disk/by-path"))
	if err != nil {
		return HardwareFacts{}, err
	}
	attachAliases(facts.BlockDevices, byID, byPath)
	if err := attachSysfsIdentity(facts.BlockDevices, firstNonEmpty(s.SysBlockDir, "/sys/class/block")); err != nil {
		return HardwareFacts{}, err
	}

	findmnt, err := s.Commands.Output(ctx, "findmnt", "--json", "--real", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS")
	if err != nil {
		return HardwareFacts{}, fmt.Errorf("findmnt discovery: %w", err)
	}
	mounts, err := parseFindmnt(findmnt)
	if err != nil {
		return HardwareFacts{}, err
	}
	facts.Mounts = mounts

	ipLinks, err := s.Commands.Output(ctx, "ip", "-json", "link", "show")
	if err != nil {
		return HardwareFacts{}, fmt.Errorf("ip link discovery: %w", err)
	}
	nics, err := parseIPLinks(ipLinks)
	if err != nil {
		return HardwareFacts{}, err
	}
	facts.NICs = nics

	return facts, nil
}

func readAliases(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read disk aliases in %s: %w", dir, err)
	}
	aliases := make(map[string][]string)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		alias := filepath.Join(dir, entry.Name())
		target, err := os.Readlink(alias)
		if err != nil {
			return nil, fmt.Errorf("read disk alias %s: %w", alias, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		target = filepath.Clean(target)
		aliases[target] = append(aliases[target], alias)
	}
	for target := range aliases {
		sort.Strings(aliases[target])
	}
	return aliases, nil
}

func attachAliases(devices []BlockDevice, byID, byPath map[string][]string) {
	for i := range devices {
		if aliases := byID[devices[i].Path]; len(aliases) > 0 {
			devices[i].ByID = append(devices[i].ByID, aliases...)
			sort.Strings(devices[i].ByID)
		}
		if aliases := byPath[devices[i].Path]; len(aliases) > 0 {
			devices[i].ByPath = append(devices[i].ByPath, aliases...)
			sort.Strings(devices[i].ByPath)
		}
		attachAliases(devices[i].Partitions, byID, byPath)
	}
}

// attachSysfsIdentity reads each disk's kernel attachment sequence number and
// each partition's number on its disk. Kernels without diskseq leave it zero.
func attachSysfsIdentity(devices []BlockDevice, sysBlockDir string) error {
	for i := range devices {
		device := &devices[i]
		var attribute string
		var value *uint64
		switch device.Type {
		case DeviceDisk:
			attribute, value = "diskseq", &device.DiskSeq
		case DevicePartition:
			attribute, value = "partition", &device.PartitionNumber
		}
		if attribute != "" && device.Name != "" {
			data, err := os.ReadFile(filepath.Join(sysBlockDir, device.Name, attribute))
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("read %s for %s: %w", attribute, device.Path, err)
			}
			if err == nil {
				parsed, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
				if err != nil {
					return fmt.Errorf("parse %s for %s: %w", attribute, device.Path, err)
				}
				*value = parsed
			}
		}
		if err := attachSysfsIdentity(device.Partitions, sysBlockDir); err != nil {
			return err
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseLSBLK(data []byte) (HardwareFacts, error) {
	var raw struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return HardwareFacts{}, fmt.Errorf("decode lsblk json: %w", err)
	}

	facts := HardwareFacts{
		BlockDevices: make([]BlockDevice, 0, len(raw.BlockDevices)),
	}
	for _, device := range raw.BlockDevices {
		facts.BlockDevices = append(facts.BlockDevices, convertLSBLKDevice(device))
	}
	return facts, nil
}

type lsblkDevice struct {
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	Type        string        `json:"type"`
	Size        uint64        `json:"size"`
	ReadOnly    bool          `json:"ro"`
	Rotational  bool          `json:"rota"`
	Transport   *string       `json:"tran"`
	Model       *string       `json:"model"`
	Serial      *string       `json:"serial"`
	WWN         *string       `json:"wwn"`
	FSType      *string       `json:"fstype"`
	UUID        *string       `json:"uuid"`
	PTType      *string       `json:"pttype"`
	PTUUID      *string       `json:"ptuuid"`
	PartType    *string       `json:"parttype"`
	PartUUID    *string       `json:"partuuid"`
	PartLabel   *string       `json:"partlabel"`
	Mountpoints []string      `json:"mountpoints"`
	Children    []lsblkDevice `json:"children"`
}

func convertLSBLKDevice(raw lsblkDevice) BlockDevice {
	devicePath := raw.Path
	if devicePath == "" && raw.Name != "" {
		devicePath = "/dev/" + raw.Name
	}

	device := BlockDevice{
		Name:                raw.Name,
		Path:                devicePath,
		Type:                DeviceType(raw.Type),
		WWN:                 stringValue(raw.WWN),
		Serial:              stringValue(raw.Serial),
		Model:               stringValue(raw.Model),
		GPTLabel:            stringValue(raw.PartLabel),
		PartitionUUID:       stringValue(raw.PartUUID),
		PartitionTableUUID:  stringValue(raw.PTUUID),
		FilesystemUUID:      stringValue(raw.UUID),
		SizeBytes:           raw.Size,
		ReadOnly:            raw.ReadOnly,
		Rotational:          raw.Rotational,
		Transport:           stringValue(raw.Transport),
		FilesystemSignature: stringValue(raw.FSType),
		PartitionSignature:  stringValue(raw.PTType),
		Mountpoints:         compactStrings(raw.Mountpoints),
		Partitions:          make([]BlockDevice, 0, len(raw.Children)),
	}
	if device.Type == DevicePartition && stringValue(raw.PartType) != "" {
		device.PartitionSignature = stringValue(raw.PartType)
	}

	for _, child := range raw.Children {
		device.Partitions = append(device.Partitions, convertLSBLKDevice(child))
	}

	return device
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func parseFindmnt(data []byte) ([]MountFact, error) {
	var raw struct {
		Filesystems []findmntFilesystem `json:"filesystems"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode findmnt json: %w", err)
	}

	var mounts []MountFact
	for _, filesystem := range raw.Filesystems {
		mounts = appendFindmntFilesystem(mounts, filesystem)
	}
	return mounts, nil
}

type findmntFilesystem struct {
	Source   string              `json:"source"`
	Target   string              `json:"target"`
	FSType   string              `json:"fstype"`
	Options  string              `json:"options"`
	Children []findmntFilesystem `json:"children"`
}

func appendFindmntFilesystem(mounts []MountFact, filesystem findmntFilesystem) []MountFact {
	mounts = append(mounts, MountFact{
		Source:     filesystem.Source,
		Target:     filesystem.Target,
		Filesystem: filesystem.FSType,
		Options:    splitOptions(filesystem.Options),
	})
	for _, child := range filesystem.Children {
		mounts = appendFindmntFilesystem(mounts, child)
	}
	return mounts
}

func parseIPLinks(data []byte) ([]NICFact, error) {
	var raw []struct {
		IfName    string `json:"ifname"`
		Address   string `json:"address"`
		LinkType  string `json:"link_type"`
		OperState string `json:"operstate"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode ip link json: %w", err)
	}

	nics := make([]NICFact, 0, len(raw))
	for _, link := range raw {
		if link.LinkType != "ether" {
			continue
		}
		nics = append(nics, NICFact{
			Name:       link.IfName,
			MACAddress: link.Address,
			OperState:  strings.ToLower(link.OperState),
		})
	}
	return nics, nil
}

func splitOptions(options string) []string {
	if options == "" {
		return nil
	}
	return compactStrings(strings.Split(options, ","))
}

func compactStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
