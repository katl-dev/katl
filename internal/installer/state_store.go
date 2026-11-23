package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

type StateStore interface {
	SaveCheckpoint(context.Context, Checkpoint) error
	LoadCheckpoint(context.Context) (Checkpoint, error)
	SaveStatus(context.Context, installstatus.Record) error
	SaveDiskBinding(context.Context, DiskBinding) error
	// LoadDiskBinding returns an error wrapping os.ErrNotExist while no
	// attempt has changed a disk.
	LoadDiskBinding(context.Context) (DiskBinding, error)
}

// DiskBinding is the disk and request an installer boot committed to when it
// first changed a disk. It is stored apart from the status record, which each
// attempt and the handoff service rewrite, so every later attempt in the boot
// stays bound to it. The state directory is on /run, so the next installer
// boot starts unbound; a successful install ends the boot with a reboot.
type DiskBinding struct {
	RequestDigest string                 `json:"requestDigest"`
	TargetDisk    discovery.DiskIdentity `json:"targetDisk"`
	// Volumes holds each volume an attempt changed, by name. It only gains
	// entries.
	Volumes map[string]disk.VolumeTarget `json:"volumes,omitempty"`
}

type Checkpoint struct {
	CurrentStep    StepID    `json:"currentStep"`
	CompletedSteps []StepID  `json:"completedSteps"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type InstallState struct {
	ManifestPath string     `json:"manifestPath"`
	Checkpoint   Checkpoint `json:"checkpoint"`
}

type FileStateStore struct {
	dir string
	now func() time.Time
}

func NewFileStateStore(dir string) *FileStateStore {
	return &FileStateStore{
		dir: dir,
		now: time.Now,
	}
}

func (s *FileStateStore) SaveCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	checkpoint.UpdatedAt = s.now().UTC()
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(s.checkpointPath(), data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}

	return nil
}

func (s *FileStateStore) LoadCheckpoint(ctx context.Context) (Checkpoint, error) {
	select {
	case <-ctx.Done():
		return Checkpoint{}, ctx.Err()
	default:
	}

	data, err := os.ReadFile(s.checkpointPath())
	if err != nil {
		return Checkpoint{}, fmt.Errorf("read checkpoint: %w", err)
	}

	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("decode checkpoint: %w", err)
	}

	return checkpoint, nil
}

func (s *FileStateStore) SaveStatus(ctx context.Context, record installstatus.Record) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = s.now().UTC()
	}
	if err := installstatus.WriteFile(s.statusPath(), record); err != nil {
		return err
	}
	return nil
}

func (s *FileStateStore) LoadStatus(ctx context.Context) (installstatus.Record, error) {
	select {
	case <-ctx.Done():
		return installstatus.Record{}, ctx.Err()
	default:
	}

	record, err := installstatus.ReadFile(s.statusPath())
	if err != nil {
		return installstatus.Record{}, err
	}
	return record, nil
}

func (s *FileStateStore) SaveDiskBinding(ctx context.Context, binding DiskBinding) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal disk binding: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := persistedrecord.WriteFileAtomic(s.bindingPath(), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write disk binding: %w", err)
	}
	return nil
}

func (s *FileStateStore) LoadDiskBinding(ctx context.Context) (DiskBinding, error) {
	select {
	case <-ctx.Done():
		return DiskBinding{}, ctx.Err()
	default:
	}

	data, err := os.ReadFile(s.bindingPath())
	if err != nil {
		return DiskBinding{}, fmt.Errorf("read disk binding: %w", err)
	}
	var binding DiskBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return DiskBinding{}, fmt.Errorf("decode disk binding: %w", err)
	}
	return binding, nil
}

func (s *FileStateStore) bindingPath() string {
	return filepath.Join(s.dir, "disk-binding.json")
}

func (s *FileStateStore) checkpointPath() string {
	return filepath.Join(s.dir, "state.json")
}

func (s *FileStateStore) statusPath() string {
	return filepath.Join(s.dir, "status.json")
}

type MemoryStateStore struct {
	Checkpoints []Checkpoint
	Statuses    []installstatus.Record
	Binding     *DiskBinding
}

func (s *MemoryStateStore) SaveCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.Checkpoints = append(s.Checkpoints, checkpoint)
	return nil
}

func (s *MemoryStateStore) LoadCheckpoint(context.Context) (Checkpoint, error) {
	if len(s.Checkpoints) == 0 {
		return Checkpoint{}, os.ErrNotExist
	}

	return s.Checkpoints[len(s.Checkpoints)-1], nil
}

func (s *MemoryStateStore) SaveStatus(ctx context.Context, record installstatus.Record) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.Statuses = append(s.Statuses, record)
	return nil
}

func (s *MemoryStateStore) LoadStatus(context.Context) (installstatus.Record, error) {
	if len(s.Statuses) == 0 {
		return installstatus.Record{}, os.ErrNotExist
	}

	return s.Statuses[len(s.Statuses)-1], nil
}

func (s *MemoryStateStore) SaveDiskBinding(_ context.Context, binding DiskBinding) error {
	s.Binding = &binding
	return nil
}

func (s *MemoryStateStore) LoadDiskBinding(context.Context) (DiskBinding, error) {
	if s.Binding == nil {
		return DiskBinding{}, os.ErrNotExist
	}
	return *s.Binding, nil
}
