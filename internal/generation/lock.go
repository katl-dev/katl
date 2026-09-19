package generation

import (
	"os"
	"path/filepath"
	"syscall"
)

// The lock covers immutable-spec validation and publication as well as status
// validation and replacement. It is never removed: replacing the inode would
// allow concurrent writers to hold different locks for the same state directory.
func withStateLock(dir string, action func() error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return action()
}
