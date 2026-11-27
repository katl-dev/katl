package workstationtest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// WorktreeTempDir returns a directory for command fixtures that a nested
// hermetic job can see, because the job binds the worktree. The test removes
// it when it ends. Each directory's owner lock lives as long as its test
// process, so a later call also removes directories that killed tests left
// behind.
func WorktreeTempDir(t testing.TB, worktree string) string {
	t.Helper()
	root := filepath.Join(worktree, "_build", "test-fixtures")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The root lock orders creation against removal, so a sweep never sees a
	// directory before its owner holds the owner lock.
	rootLock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer rootLock.Close()
	if err := unix.Flock(int(rootLock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// Removing other tests' leftovers is best effort: it must not fail this
	// test, and a later call retries.
	if err := removeAbandoned(root); err != nil {
		t.Logf("remove abandoned test fixtures: %v", err)
	}

	dir, err := os.MkdirTemp(root, "test-")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenFile(filepath.Join(dir, ".owner"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
		_ = owner.Close()
	})
	return dir
}

// removeAbandoned removes the fixture directories whose owner lock no process
// holds, going on past any it cannot remove. The caller holds the root lock.
func removeAbandoned(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		owner, err := os.Open(filepath.Join(dir, ".owner"))
		if errors.Is(err, fs.ErrNotExist) {
			// The owner died between creating the directory and its lock.
			errs = append(errs, os.RemoveAll(dir))
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			err = os.RemoveAll(dir)
		} else if errors.Is(err, unix.EWOULDBLOCK) {
			err = nil
		}
		_ = owner.Close()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
