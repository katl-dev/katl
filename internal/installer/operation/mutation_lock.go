package operation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// MutationLock excludes concurrent node mutations across agent and SSH command
// processes. Keep it held until the externally visible mutation is complete.
type MutationLock struct {
	file *os.File
}

func (s Store) AcquireMutationLock() (*MutationLock, error) {
	file, err := os.OpenFile(filepath.Join(s.Root, ".mutation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open node mutation lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another node mutation is executing; wait for it before retrying: %w", err)
	}
	return &MutationLock{file: file}, nil
}

func (lock *MutationLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	return errors.Join(err, lock.file.Close())
}
