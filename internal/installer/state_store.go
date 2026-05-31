package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type StateStore interface {
	SaveCheckpoint(context.Context, Checkpoint) error
	LoadCheckpoint(context.Context) (Checkpoint, error)
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

func (s *FileStateStore) checkpointPath() string {
	return filepath.Join(s.dir, "state.json")
}

type MemoryStateStore struct {
	Checkpoints []Checkpoint
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
