package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSlotStoreFile(t *testing.T) {
	store := NewFileSlotStore(t.TempDir())
	store.now = func() time.Time { return time.Date(2026, 6, 3, 17, 0, 0, 0, time.UTC) }
	state := RootSlotState{
		PlannedSlot:        RootSlotB,
		ArtifactDigest:     "abc123",
		ActiveSlotGuard:    RootSlotA,
		WriteStarted:       true,
		WriteComplete:      true,
		ValidationComplete: true,
	}

	if err := store.SaveSlotState(context.Background(), state); err != nil {
		t.Fatalf("SaveSlotState() error = %v", err)
	}
	loaded, err := store.LoadSlotState(context.Background())
	if err != nil {
		t.Fatalf("LoadSlotState() error = %v", err)
	}
	if loaded.PlannedSlot != state.PlannedSlot || loaded.ArtifactDigest != state.ArtifactDigest || loaded.ActiveSlotGuard != state.ActiveSlotGuard || !loaded.ValidationComplete {
		t.Fatalf("loaded state = %#v", loaded)
	}

	data, err := os.ReadFile(filepath.Join(store.dir, "root-slot.json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	for _, key := range []string{"plannedSlot", "artifactDigest", "activeSlotGuard", "writeStarted", "writeComplete", "validationComplete"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("state file missing %s: %s", key, data)
		}
	}
}

func TestRunRootSlotDone(t *testing.T) {
	artifact := []byte("runtime-root")
	store := &memSlotStore{state: RootSlotState{
		PlannedSlot:        RootSlotA,
		ArtifactDigest:     digest(artifact),
		WriteStarted:       true,
		WriteComplete:      true,
		ValidationComplete: true,
	}}
	writes := 0

	result, err := runRootSlot(context.Background(), store, *rootInstall(artifact, newMemSlot(len(artifact))), false, func(context.Context, RootSlotInstallRequest) (RootSlotInstallResult, error) {
		writes++
		return RootSlotInstallResult{}, nil
	})
	if err != nil {
		t.Fatalf("runRootSlot() error = %v", err)
	}
	if writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
	if result.BytesWritten != int64(len(artifact)) || result.SHA256 != digest(artifact) {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunRootSlotPartial(t *testing.T) {
	artifact := []byte("runtime-root")
	store := &memSlotStore{state: RootSlotState{
		PlannedSlot:    RootSlotA,
		ArtifactDigest: digest(artifact),
		WriteStarted:   true,
	}}

	_, err := runRootSlot(context.Background(), store, *rootInstall(artifact, newMemSlot(len(artifact))), false, nil)
	if !errors.Is(err, ErrSlotRetryRequired) {
		t.Fatalf("runRootSlot() error = %v, want ErrSlotRetryRequired", err)
	}
}

func TestRunRootSlotRetry(t *testing.T) {
	artifact := []byte("runtime-root")
	store := &memSlotStore{state: RootSlotState{
		PlannedSlot:    RootSlotA,
		ArtifactDigest: digest(artifact),
		WriteStarted:   true,
	}}
	target := newMemSlot(len(artifact))

	if _, err := runRootSlot(context.Background(), store, *rootInstall(artifact, target), true, nil); err != nil {
		t.Fatalf("runRootSlot() error = %v", err)
	}
	if len(store.saves) != 3 {
		t.Fatalf("saved states = %d, want 3", len(store.saves))
	}
	last := store.saves[len(store.saves)-1]
	if !last.WriteStarted || !last.WriteComplete || !last.ValidationComplete {
		t.Fatalf("final state = %#v", last)
	}
	if !bytes.Equal(target.data, artifact) {
		t.Fatalf("target = %q, want artifact", target.data)
	}
}

func TestRunRootSlotConflict(t *testing.T) {
	artifact := []byte("runtime-root")
	store := &memSlotStore{state: RootSlotState{
		PlannedSlot:    RootSlotB,
		ArtifactDigest: digest(artifact),
	}}

	_, err := runRootSlot(context.Background(), store, *rootInstall(artifact, newMemSlot(len(artifact))), true, nil)
	if !errors.Is(err, ErrSlotStateConflict) {
		t.Fatalf("runRootSlot() error = %v, want ErrSlotStateConflict", err)
	}
}

func TestDiskExecutorRejectsPartialSlot(t *testing.T) {
	artifact := []byte("runtime-root")
	store := &memSlotStore{state: RootSlotState{
		PlannedSlot:    RootSlotA,
		ArtifactDigest: digest(artifact),
		WriteStarted:   true,
	}}

	_, err := (DiskExecutor{
		Commands:      &NoopCommandRunner{},
		Discovery:     executorDiscovery(),
		RootSlotState: store,
	}).Execute(context.Background(), DiskExecutionRequest{
		Plan:             executorPlan(),
		RootSlotInstall:  rootInstall(artifact, newMemSlot(len(artifact))),
		AllowDestructive: true,
	})
	if !errors.Is(err, ErrSlotRetryRequired) {
		t.Fatalf("Execute() error = %v, want ErrSlotRetryRequired", err)
	}
}

type memSlotStore struct {
	state RootSlotState
	saves []RootSlotState
	empty bool
}

func (s *memSlotStore) SaveSlotState(_ context.Context, state RootSlotState) error {
	s.state = state
	s.saves = append(s.saves, state)
	s.empty = false
	return nil
}

func (s *memSlotStore) LoadSlotState(context.Context) (RootSlotState, error) {
	if s.empty {
		return RootSlotState{}, os.ErrNotExist
	}
	return s.state, nil
}
