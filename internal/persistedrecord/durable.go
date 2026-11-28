package persistedrecord

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// This file owns every durable write to Katl's persisted state and to the
// ESP. A write returns only after its content and its directory entry are on
// stable storage, so a caller may publish a reference to the path (an EFI
// variable, a selection record, a loader entry naming a UKI) as soon as the
// call returns.
//
// A replacement writes a temporary file in the destination directory, fsyncs
// it, renames it over the destination and fsyncs the directory. On ext4 the
// rename is atomic, so a crash leaves the old or the new file. vfat, which
// holds the ESP, does not promise an atomic rename over an existing file on
// every crash path, but the content is already durable before the rename, so
// a crash leaves the old file, the new file, or the complete new content
// under its temporary name. Temporary names start with "." and end in
// ".tmp-<random>", so they never match systemd-boot's "*.conf" entry glob,
// the "*.efi" UKI names or Katl's own record names, and readers ignore them.

const tempInfix = ".tmp-"

// WriteFileAtomic durably replaces path with data.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	return WriteReaderAtomic(path, bytes.NewReader(data), mode)
}

// WriteReaderAtomic durably replaces path with the content read from source.
func WriteReaderAtomic(path string, source io.Reader, mode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted record path is required")
	}
	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create persisted record directory: %w", err)
	}
	tmp, err := fsys.CreateTemp(dir, "."+filepath.Base(path)+tempInfix)
	if err != nil {
		return fmt.Errorf("create persisted record temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = fsys.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, source); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write persisted record temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod persisted record temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync persisted record temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close persisted record temp file: %w", err)
	}
	if err := fsys.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace persisted record: %w", err)
	}
	cleanup = false
	return SyncDir(dir)
}

// WriteFileNoReplace durably creates path and fails if it already exists.
func WriteFileNoReplace(path string, data []byte, mode os.FileMode, dirMode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted record path is required")
	}
	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create persisted record directory: %w", err)
	}
	file, err := fsys.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("persisted record already exists: %s", path)
		}
		return fmt.Errorf("create persisted record: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write persisted record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync persisted record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close persisted record: %w", err)
	}
	return SyncDir(dir)
}

// RemoveFile durably removes path. A missing path is not an error, but the
// directory is still synced so an earlier unsynced removal becomes durable.
func RemoveFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted record path is required")
	}
	if err := fsys.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove persisted record: %w", err)
	}
	err := SyncDir(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SyncDir makes the entries of directory path durable. Callers that publish a
// whole directory with a rename use it on the parent after the rename.
func SyncDir(path string) error {
	if err := fsys.SyncDir(path); err != nil {
		return fmt.Errorf("sync persisted record directory: %w", err)
	}
	return nil
}

// SyncFile makes a file another program wrote, such as a UKI that
// systemd-sysupdate installed, durable together with its directory entry.
func SyncFile(path string) error {
	if err := syncContent(path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

func syncContent(path string) error {
	handle, err := fsys.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open persisted file: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("sync persisted file: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close persisted file: %w", err)
	}
	return nil
}

// SyncTree makes every regular file and directory under root durable. A
// caller that builds a tree with ordinary writes uses it before publishing a
// record that refers to the tree. Symlinks are durable through their
// directory.
func SyncTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return SyncDir(path)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return syncContent(path)
	})
}

// fileSystem is the narrow seam crash tests replace to observe which writes
// have reached stable storage.
type fileSystem interface {
	MkdirAll(path string, perm os.FileMode) error
	CreateTemp(dir, pattern string) (file, error)
	OpenFile(name string, flag int, perm os.FileMode) (file, error)
	Rename(oldpath, newpath string) error
	Remove(name string) error
	SyncDir(path string) error
}

type file interface {
	io.Writer
	Name() string
	Chmod(mode os.FileMode) error
	Sync() error
	Close() error
}

var fsys fileSystem = osFileSystem{}

type osFileSystem struct{}

func (osFileSystem) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

func (osFileSystem) CreateTemp(dir, pattern string) (file, error) { return os.CreateTemp(dir, pattern) }

func (osFileSystem) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	return os.OpenFile(name, flag, perm)
}

func (osFileSystem) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFileSystem) Remove(name string) error { return os.Remove(name) }

func (osFileSystem) SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
