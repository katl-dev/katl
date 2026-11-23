package discovery

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TargetDiskSelector selects one whole disk. Every set criterion must match;
// MinSizeMiB is a safety check on the selected disk, not a criterion.
type TargetDiskSelector struct {
	ByID       string
	WWN        string
	Serial     string
	ByPath     string
	GPTGUID    string
	Name       string
	Model      string
	Size       string
	Type       string
	MinSizeMiB uint64
}

// Disk types derived from the kernel's transport and rotational flags.
const (
	DiskTypeNVMe = "nvme"
	DiskTypeSSD  = "ssd"
	DiskTypeHDD  = "hdd"
)

var gptGUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate checks that the selector names at least one criterion and that
// every criterion is well formed.
func (s TargetDiskSelector) Validate() error {
	if len(s.criteria()) == 0 {
		return ErrMissingTargetDiskSelector
	}
	if s.ByID != "" && !canonicalUnder(s.ByID, "/dev/disk/by-id/") {
		return fmt.Errorf("byID must be an absolute /dev/disk/by-id path")
	}
	if s.ByPath != "" && !canonicalUnder(s.ByPath, "/dev/disk/by-path/") {
		return fmt.Errorf("byPath must be an absolute /dev/disk/by-path path")
	}
	if s.Name != "" && !validKernelDevicePath(s.Name) {
		return fmt.Errorf("name must be a kernel device path such as /dev/sda; use byID or byPath for /dev/disk aliases")
	}
	if s.GPTGUID != "" && !gptGUIDPattern.MatchString(s.GPTGUID) {
		return fmt.Errorf("gptGUID must be a GUID such as 6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b")
	}
	if s.Size != "" {
		if _, err := ParseSizeMatch(s.Size); err != nil {
			return err
		}
	}
	if s.Type != "" && !slices.Contains([]string{DiskTypeNVMe, DiskTypeSSD, DiskTypeHDD}, s.Type) {
		return fmt.Errorf("type must be one of nvme, ssd, or hdd")
	}
	for _, criterion := range s.criteria() {
		if strings.TrimSpace(criterion.value) != criterion.value {
			return fmt.Errorf("%s must not contain leading or trailing whitespace", criterion.field)
		}
	}
	return nil
}

// Stable reports whether the selector includes an identifier of the disk
// hardware itself rather than its attachment, contents, enumeration, or class.
func (s TargetDiskSelector) Stable() bool {
	return s.ByID != "" || s.WWN != "" || s.Serial != ""
}

func (s TargetDiskSelector) String() string {
	criteria := s.criteria()
	parts := make([]string, 0, len(criteria))
	for _, criterion := range criteria {
		parts = append(parts, criterion.field+"="+strconv.Quote(criterion.value))
	}
	return strings.Join(parts, ", ")
}

type selectorCriterion struct {
	field string
	value string
	match func(BlockDevice) bool
}

func (s TargetDiskSelector) criteria() []selectorCriterion {
	var out []selectorCriterion
	add := func(field, value string, match func(BlockDevice) bool) {
		if value != "" {
			out = append(out, selectorCriterion{field: field, value: value, match: match})
		}
	}
	add("byID", s.ByID, func(d BlockDevice) bool { return slices.Contains(d.ByID, s.ByID) })
	add("wwn", s.WWN, func(d BlockDevice) bool { return d.WWN == s.WWN })
	add("serial", s.Serial, func(d BlockDevice) bool { return d.Serial == s.Serial })
	add("byPath", s.ByPath, func(d BlockDevice) bool { return slices.Contains(d.ByPath, s.ByPath) })
	add("gptGUID", s.GPTGUID, func(d BlockDevice) bool {
		return d.PartitionSignature == "gpt" && strings.EqualFold(d.PartitionTableUUID, s.GPTGUID)
	})
	add("name", s.Name, func(d BlockDevice) bool { return d.Path == s.Name })
	add("model", s.Model, func(d BlockDevice) bool { return globMatch(s.Model, d.Model) })
	add("size", s.Size, func(d BlockDevice) bool {
		size, err := ParseSizeMatch(s.Size)
		return err == nil && size.Matches(d.SizeBytes)
	})
	add("type", s.Type, func(d BlockDevice) bool { return DiskType(d) == s.Type })
	return out
}

func (s TargetDiskSelector) matches(device BlockDevice) bool {
	criteria := s.criteria()
	if len(criteria) == 0 {
		return false
	}
	for _, criterion := range criteria {
		if !criterion.match(device) {
			return false
		}
	}
	return true
}

// globMatch reports whether value matches pattern, where * matches any run of
// characters, including /, and ? matches one character. Matching is
// case-sensitive and has no escapes, because device models contain / and \.
func globMatch(pattern, value string) bool {
	if pattern == "" {
		return value == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(value); i++ {
			if globMatch(pattern[1:], value[i:]) {
				return true
			}
		}
		return false
	case '?':
		_, size := utf8.DecodeRuneInString(value)
		return value != "" && globMatch(pattern[1:], value[size:])
	default:
		return value != "" && pattern[0] == value[0] && globMatch(pattern[1:], value[1:])
	}
}

// DiskType classifies a disk as nvme, ssd, or hdd.
func DiskType(device BlockDevice) string {
	switch {
	case device.Transport == "nvme":
		return DiskTypeNVMe
	case device.Rotational:
		return DiskTypeHDD
	default:
		return DiskTypeSSD
	}
}

func canonicalUnder(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && len(value) > len(prefix) && path.Clean(value) == value
}

func validKernelDevicePath(value string) bool {
	name, ok := strings.CutPrefix(value, "/dev/")
	return ok && name != "" && !strings.Contains(name, "/") && name != "." && name != ".."
}

// MatchTargetDisk resolves the selector to exactly one whole disk and checks
// that the disk is safe to overwrite.
func MatchTargetDisk(facts HardwareFacts, selector TargetDiskSelector) (TargetDiskMatch, error) {
	device, err := MatchDiskIdentity(facts, selector)
	if err != nil {
		return TargetDiskMatch{}, err
	}
	if err := validateTargetDiskSafety(facts, device, selector.MinSizeMiB); err != nil {
		return TargetDiskMatch{}, err
	}
	return TargetDiskMatch{Device: device, Signatures: collectSignatures(device)}, nil
}

// MatchDiskIdentity resolves the selector to exactly one whole disk. Every
// whole disk counts toward uniqueness, including mounted or read-only disks, so
// a selector never resolves differently because another disk is in use.
func MatchDiskIdentity(facts HardwareFacts, selector TargetDiskSelector) (BlockDevice, error) {
	if err := selector.Validate(); err != nil {
		return BlockDevice{}, err
	}

	var disks, matches []BlockDevice
	for _, device := range facts.BlockDevices {
		if device.Type != DeviceDisk {
			if selector.identifies(device) {
				return BlockDevice{}, fmt.Errorf("%w: %s is %s, not a whole disk", ErrUnsafeTargetDisk, device.Path, device.Type)
			}
			continue
		}
		disks = append(disks, device)
		if selector.matches(device) {
			matches = append(matches, device)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if partition, ok := selector.identifiedPartition(facts); ok {
			return BlockDevice{}, fmt.Errorf("%w: %s is a partition, not a whole disk; select the disk that contains it", ErrUnsafeTargetDisk, partition.Path)
		}
		message := fmt.Sprintf("target disk selector (%s) matched no whole disk", selector)
		if selector.GPTGUID != "" {
			message += "; a GPT disk GUID changes whenever the disk is repartitioned, including by a KatlOS install, so update the selector from the installer inventory"
		}
		return BlockDevice{}, fmt.Errorf("%s; %s", message, describeCandidates("disks present", disks))
	default:
		return BlockDevice{}, fmt.Errorf("target disk selector (%s) matched %d whole disks; add criteria or select by byID, wwn, or serial so exactly one disk matches; %s", selector, len(matches), describeCandidates("matching disks", matches))
	}
}

// identifies reports whether a path-like criterion names this device, so a
// non-disk device can be rejected by name rather than reported as no match.
func (s TargetDiskSelector) identifies(device BlockDevice) bool {
	return (s.Name != "" && device.Path == s.Name) ||
		(s.ByID != "" && slices.Contains(device.ByID, s.ByID)) ||
		(s.ByPath != "" && slices.Contains(device.ByPath, s.ByPath))
}

func (s TargetDiskSelector) identifiedPartition(facts HardwareFacts) (BlockDevice, bool) {
	for _, disk := range facts.BlockDevices {
		for _, partition := range disk.Partitions {
			if s.identifies(partition) {
				return partition, true
			}
		}
	}
	return BlockDevice{}, false
}

// DiskIdentity is the resolved disk an install is bound to. DiskSeq is the
// kernel's per-boot attachment sequence number, so a disk detached and
// replaced under the same name has a different identity.
type DiskIdentity struct {
	Path      string `json:"path"`
	DiskSeq   uint64 `json:"diskSeq,omitempty"`
	SizeBytes uint64 `json:"sizeBytes"`
	WWN       string `json:"wwn,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Model     string `json:"model,omitempty"`
}

func IdentityOf(device BlockDevice) DiskIdentity {
	return DiskIdentity{
		Path:      device.Path,
		DiskSeq:   device.DiskSeq,
		SizeBytes: device.SizeBytes,
		WWN:       device.WWN,
		Serial:    device.Serial,
		Model:     device.Model,
	}
}

// MatchRetainedDisk finds the disk an install already resolved and checks it is
// still the same, still satisfies the selector, and is still safe to
// overwrite. The GPT disk GUID is not rechecked because installation replaces
// the partition table.
func MatchRetainedDisk(facts HardwareFacts, retained DiskIdentity, selector TargetDiskSelector) (TargetDiskMatch, error) {
	var device *BlockDevice
	for i := range facts.BlockDevices {
		if facts.BlockDevices[i].Path == retained.Path {
			device = &facts.BlockDevices[i]
		}
	}
	if device == nil {
		return TargetDiskMatch{}, fmt.Errorf("%w: selected disk %s is no longer present", ErrTargetDiskChanged, retained.Path)
	}
	if current := IdentityOf(*device); current != retained {
		return TargetDiskMatch{}, fmt.Errorf("%w: %s is now %s, not the selected %s", ErrTargetDiskChanged, retained.Path, describeIdentity(current), describeIdentity(retained))
	}
	if device.Type != DeviceDisk {
		return TargetDiskMatch{}, fmt.Errorf("%w: %s is %s, not a whole disk", ErrUnsafeTargetDisk, device.Path, device.Type)
	}
	unchanging := selector
	unchanging.GPTGUID = ""
	if len(unchanging.criteria()) > 0 && !unchanging.matches(*device) {
		return TargetDiskMatch{}, fmt.Errorf("%w: %s no longer matches the target disk selector (%s)", ErrTargetDiskChanged, device.Path, unchanging)
	}
	if err := validateTargetDiskSafety(facts, *device, selector.MinSizeMiB); err != nil {
		return TargetDiskMatch{}, err
	}
	return TargetDiskMatch{Device: *device, Signatures: collectSignatures(*device)}, nil
}

func validateTargetDiskSafety(facts HardwareFacts, device BlockDevice, minSizeMiB uint64) error {
	if device.Type != DeviceDisk {
		return fmt.Errorf("%w: %s is %s, not a whole disk", ErrUnsafeTargetDisk, device.Path, device.Type)
	}
	if device.ReadOnly {
		return fmt.Errorf("%w: %s is read-only", ErrUnsafeTargetDisk, device.Path)
	}
	if minSizeMiB > 0 && device.SizeBytes < minSizeMiB*1024*1024 {
		return fmt.Errorf("%w: %s is smaller than %d MiB", ErrUnsafeTargetDisk, device.Path, minSizeMiB)
	}
	if diskHasMounts(facts, device) {
		return fmt.Errorf("%w: %s or one of its partitions is mounted", ErrUnsafeTargetDisk, device.Path)
	}
	return nil
}

func describeCandidates(label string, disks []BlockDevice) string {
	if len(disks) == 0 {
		return label + ": none"
	}
	descriptions := make([]string, 0, len(disks))
	for _, disk := range disks {
		descriptions = append(descriptions, DescribeDisk(disk))
	}
	return label + ": " + strings.Join(descriptions, "; ")
}

// DescribeDisk lists the identities an operator can use to select a disk.
func DescribeDisk(device BlockDevice) string {
	fields := []string{"size=" + FormatSize(device.SizeBytes) + " (" + strconv.FormatUint(device.SizeBytes, 10) + " bytes)", "type=" + DiskType(device)}
	add := func(name, value string) {
		if value != "" {
			fields = append(fields, name+"="+value)
		}
	}
	for _, alias := range device.ByID {
		add("byID", alias)
	}
	add("wwn", device.WWN)
	add("serial", device.Serial)
	for _, alias := range device.ByPath {
		add("byPath", alias)
	}
	if device.PartitionSignature == "gpt" {
		add("gptGUID", device.PartitionTableUUID)
	}
	add("model", device.Model)
	if device.ReadOnly {
		fields = append(fields, "read-only")
	}
	if len(device.Mountpoints) > 0 || slices.ContainsFunc(device.Partitions, func(p BlockDevice) bool { return len(p.Mountpoints) > 0 }) {
		fields = append(fields, "mounted")
	}
	return device.Path + " (" + strings.Join(fields, ", ") + ")"
}

func describeIdentity(identity DiskIdentity) string {
	fields := []string{"size=" + FormatSize(identity.SizeBytes)}
	if identity.DiskSeq != 0 {
		fields = append(fields, "diskseq="+strconv.FormatUint(identity.DiskSeq, 10))
	}
	if identity.WWN != "" {
		fields = append(fields, "wwn="+identity.WWN)
	}
	if identity.Serial != "" {
		fields = append(fields, "serial="+identity.Serial)
	}
	if identity.Model != "" {
		fields = append(fields, "model="+identity.Model)
	}
	return "(" + strings.Join(fields, ", ") + ")"
}

// SizeMatch is a parsed size criterion such as ">= 500GB".
type SizeMatch struct {
	Op    string
	Bytes uint64
}

var sizeUnits = map[string]float64{
	"b":   1,
	"kb":  1e3,
	"mb":  1e6,
	"gb":  1e9,
	"tb":  1e12,
	"pb":  1e15,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
	"pib": 1 << 50,
}

var sizePattern = regexp.MustCompile(`^(>=|<=|==|>|<)?\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$`)

// ParseSizeMatch parses an optional comparison (>=, <=, >, <, ==) followed by
// a size with a byte (B), decimal (KB, MB, GB, TB, PB), or binary (KiB, MiB,
// GiB, TiB, PiB) unit. Without a comparison the size must match exactly. The
// unit is required: read as bytes, a bare number such as ">= 500" would match
// every disk.
func ParseSizeMatch(value string) (SizeMatch, error) {
	trimmed := strings.TrimSpace(value)
	parts := sizePattern.FindStringSubmatch(trimmed)
	if parts == nil {
		return SizeMatch{}, fmt.Errorf("size %q must be a size such as \">= 500GB\" or \"<= 2TiB\"", value)
	}
	if parts[3] == "" {
		return SizeMatch{}, fmt.Errorf("size %q has no unit; write it with one, such as %q or %q, or %q for bytes", value, trimmed+"GB", trimmed+"GiB", trimmed+"B")
	}
	unit, ok := sizeUnits[strings.ToLower(parts[3])]
	if !ok {
		return SizeMatch{}, fmt.Errorf("size %q has unknown unit %q; use B, KB, MB, GB, TB, PB, KiB, MiB, GiB, TiB, or PiB", value, parts[3])
	}
	number, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return SizeMatch{}, fmt.Errorf("size %q: %w", value, err)
	}
	bytes := number * unit
	if bytes >= math.MaxUint64 {
		return SizeMatch{}, fmt.Errorf("size %q is too large", value)
	}
	op := parts[1]
	if op == "" {
		op = "=="
	}
	return SizeMatch{Op: op, Bytes: uint64(math.Round(bytes))}, nil
}

func (m SizeMatch) Matches(size uint64) bool {
	switch m.Op {
	case ">=":
		return size >= m.Bytes
	case "<=":
		return size <= m.Bytes
	case ">":
		return size > m.Bytes
	case "<":
		return size < m.Bytes
	default:
		return size == m.Bytes
	}
}

// FormatSize renders a byte count in binary units for operator messages.
func FormatSize(bytes uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return strconv.FormatUint(bytes, 10) + "B"
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + units[unit]
}
