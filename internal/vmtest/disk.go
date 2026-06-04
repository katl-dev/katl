package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type DiskKind string

const (
	DiskTarget   DiskKind = "target"
	DiskExtra    DiskKind = "extra"
	DiskSnapshot DiskKind = "snapshot"
)

type DiskFormat string

const (
	DiskQCOW2 DiskFormat = "qcow2"
	DiskRaw   DiskFormat = "raw"
)

type DiskFixture struct {
	Name         string     `json:"name"`
	Kind         DiskKind   `json:"kind"`
	Format       DiskFormat `json:"format,omitempty"`
	Size         string     `json:"size,omitempty"`
	Source       string     `json:"source,omitempty"`
	SourceFormat DiskFormat `json:"sourceFormat,omitempty"`
}

type DiskPlan struct {
	Name            string     `json:"name"`
	Kind            DiskKind   `json:"kind"`
	Format          DiskFormat `json:"format"`
	Size            string     `json:"size,omitempty"`
	Source          string     `json:"source,omitempty"`
	SourceFormat    DiskFormat `json:"sourceFormat,omitempty"`
	HostPath        string     `json:"hostPath"`
	AttachmentOrder int        `json:"attachmentOrder"`
	GuestSelector   string     `json:"guestSelector"`
	CreateCommand   []string   `json:"createCommand"`
}

type DiskRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type ExecDiskRunner struct{}

func (ExecDiskRunner) Run(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func TargetDisk(name, format, size string) DiskFixture {
	return DiskFixture{Name: name, Kind: DiskTarget, Format: DiskFormat(format), Size: size}
}

func ExtraDisk(name, format, size string) DiskFixture {
	return DiskFixture{Name: name, Kind: DiskExtra, Format: DiskFormat(format), Size: size}
}

func SnapshotDisk(name, source string, sourceFormat DiskFormat) DiskFixture {
	return DiskFixture{Name: name, Kind: DiskSnapshot, Format: DiskQCOW2, Source: source, SourceFormat: sourceFormat}
}

func CreateDisks(ctx context.Context, runner DiskRunner, plans []DiskPlan) error {
	for _, plan := range plans {
		if err := os.MkdirAll(filepath.Dir(plan.HostPath), 0o755); err != nil {
			return err
		}
		if len(plan.CreateCommand) == 0 {
			continue
		}
		if err := runner.Run(ctx, plan.CreateCommand[0], plan.CreateCommand[1:]...); err != nil {
			return fmt.Errorf("create disk %s: %w", plan.Name, err)
		}
	}
	return nil
}

func CleanupDisks(result Result) error {
	if keepDisks(result.Keep, result.Status) {
		return nil
	}
	var errs []error
	for _, disk := range result.Disks {
		if err := os.Remove(disk.HostPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func keepDisks(keep KeepPolicy, status Status) bool {
	switch keep {
	case KeepAlways:
		return true
	case KeepFailed:
		return status == StatusFailed
	default:
		return false
	}
}

func planDisks(dir string, fixtures []DiskFixture) ([]DiskPlan, error) {
	plans := make([]DiskPlan, 0, len(fixtures))
	seen := map[string]bool{}
	for index, fixture := range fixtures {
		plan, err := planDisk(dir, index, fixture)
		if err != nil {
			return nil, err
		}
		key := clean(plan.Name)
		if seen[key] {
			return nil, fmt.Errorf("duplicate disk fixture %q", plan.Name)
		}
		seen[key] = true
		plans = append(plans, plan)
	}
	return plans, nil
}

func planDisk(dir string, index int, fixture DiskFixture) (DiskPlan, error) {
	if fixture.Name == "" {
		return DiskPlan{}, errors.New("disk fixture name is required")
	}
	name := clean(fixture.Name)
	if name == "" {
		return DiskPlan{}, fmt.Errorf("disk fixture %q has no usable name", fixture.Name)
	}
	kind := fixture.Kind
	if kind == "" {
		kind = DiskTarget
	}
	format := fixture.Format
	if format == "" {
		format = DiskQCOW2
	}
	if err := checkDiskKind(kind); err != nil {
		return DiskPlan{}, err
	}
	if err := checkDiskFormat(format); err != nil {
		return DiskPlan{}, err
	}
	plan := DiskPlan{
		Name:            fixture.Name,
		Kind:            kind,
		Format:          format,
		Size:            fixture.Size,
		Source:          fixture.Source,
		SourceFormat:    fixture.SourceFormat,
		AttachmentOrder: index,
		GuestSelector:   "/dev/disk/by-id/virtio-katl-" + name,
	}
	plan.HostPath = filepath.Join(dir, fmt.Sprintf("%02d-%s.%s", index, name, format))
	if kind == DiskSnapshot {
		return snapshotPlan(plan)
	}
	if plan.Size == "" {
		return DiskPlan{}, fmt.Errorf("disk fixture %q size is required", fixture.Name)
	}
	plan.CreateCommand = []string{"qemu-img", "create", "-f", string(format), plan.HostPath, plan.Size}
	return plan, nil
}

func snapshotPlan(plan DiskPlan) (DiskPlan, error) {
	if plan.Source == "" {
		return DiskPlan{}, fmt.Errorf("snapshot disk %q source is required", plan.Name)
	}
	if plan.SourceFormat == "" {
		plan.SourceFormat = DiskQCOW2
	}
	if err := checkDiskFormat(plan.SourceFormat); err != nil {
		return DiskPlan{}, err
	}
	plan.Format = DiskQCOW2
	plan.HostPath = strings.TrimSuffix(plan.HostPath, filepath.Ext(plan.HostPath)) + ".snapshot.qcow2"
	plan.CreateCommand = []string{
		"qemu-img", "create",
		"-f", string(DiskQCOW2),
		"-F", string(plan.SourceFormat),
		"-b", plan.Source,
		plan.HostPath,
	}
	return plan, nil
}

func checkDiskKind(kind DiskKind) error {
	switch kind {
	case DiskTarget, DiskExtra, DiskSnapshot:
		return nil
	default:
		return fmt.Errorf("unsupported disk kind %q", kind)
	}
}

func checkDiskFormat(format DiskFormat) error {
	switch format {
	case DiskQCOW2, DiskRaw:
		return nil
	default:
		return fmt.Errorf("unsupported disk format %q", format)
	}
}
