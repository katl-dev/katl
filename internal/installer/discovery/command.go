package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type OutputCommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type CommandDiscoverySource struct {
	Commands OutputCommandRunner
	ByIDDir  string
}

func NewCommandDiscoverySource(commands OutputCommandRunner) CommandDiscoverySource {
	return CommandDiscoverySource{Commands: commands}
}

func (s CommandDiscoverySource) Discover(ctx context.Context) (HardwareFacts, error) {
	if s.Commands == nil {
		return HardwareFacts{}, fmt.Errorf("discovery command runner is required")
	}

	lsblk, err := s.Commands.Output(ctx, "lsblk", "--json", "--bytes", "--output", "NAME,PATH,TYPE,SIZE,RO,MODEL,SERIAL,WWN,FSTYPE,PTTYPE,PARTTYPE,PARTLABEL,MOUNTPOINTS")
	if err != nil {
		return HardwareFacts{}, fmt.Errorf("lsblk discovery: %w", err)
	}
	facts, err := parseLSBLK(lsblk)
	if err != nil {
		return HardwareFacts{}, err
	}
	aliases, err := readByIDAliases(firstNonEmpty(s.ByIDDir, "/dev/disk/by-id"))
	if err != nil {
		return HardwareFacts{}, err
	}
	attachByIDAliases(facts.BlockDevices, aliases)

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

func readByIDAliases(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read disk by-id aliases: %w", err)
	}
	aliases := make(map[string][]string)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		alias := filepath.Join(dir, entry.Name())
		target, err := os.Readlink(alias)
		if err != nil {
			return nil, fmt.Errorf("read disk by-id alias %s: %w", alias, err)
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

func attachByIDAliases(devices []BlockDevice, aliases map[string][]string) {
	for i := range devices {
		if byID := aliases[devices[i].Path]; len(byID) > 0 {
			devices[i].ByID = append(devices[i].ByID, byID...)
			sort.Strings(devices[i].ByID)
		}
		attachByIDAliases(devices[i].Partitions, aliases)
	}
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
	Model       *string       `json:"model"`
	Serial      *string       `json:"serial"`
	WWN         *string       `json:"wwn"`
	FSType      *string       `json:"fstype"`
	PTType      *string       `json:"pttype"`
	PartType    *string       `json:"parttype"`
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
		SizeBytes:           raw.Size,
		ReadOnly:            raw.ReadOnly,
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
		Filesystems []struct {
			Source  string `json:"source"`
			Target  string `json:"target"`
			FSType  string `json:"fstype"`
			Options string `json:"options"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode findmnt json: %w", err)
	}

	mounts := make([]MountFact, 0, len(raw.Filesystems))
	for _, filesystem := range raw.Filesystems {
		mounts = append(mounts, MountFact{
			Source:     filesystem.Source,
			Target:     filesystem.Target,
			Filesystem: filesystem.FSType,
			Options:    splitOptions(filesystem.Options),
		})
	}
	return mounts, nil
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
