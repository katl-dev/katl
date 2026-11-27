package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type fileID struct {
	device uint64
	inode  uint64
}

func identity(stat unix.Stat_t) fileID {
	return fileID{uint64(stat.Dev), stat.Ino}
}

type directory struct {
	file   *os.File
	id     fileID
	parent *directory
	name   string
	path   string
}

func (d *directory) fd() int {
	return int(d.file.Fd())
}

func (d *directory) stat(name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(d.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return stat, err
}

func (d *directory) check() error {
	stat, err := d.parent.stat(d.name)
	if err != nil {
		return err
	}
	if identity(stat) != d.id {
		return fmt.Errorf("%q changed during cleanup; run katl-dev-evidence list", d.path)
	}
	return nil
}

func (d *directory) remove() error {
	// Linux removes directories by name. Check that the slot still names
	// the open directory before removing it, after handling its contents.
	if err := d.check(); err != nil {
		return err
	}
	return unix.Unlinkat(d.parent.fd(), d.name, unix.AT_REMOVEDIR)
}

func (d *directory) makeWritable() error {
	var stat unix.Stat_t
	if err := unix.Fstat(d.fd(), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IWUSR != 0 || int(stat.Uid) != os.Geteuid() {
		return nil
	}

	// Unlinking requires write access to the containing directory. Changing
	// only the bound directory avoids modifying symlink targets or hard links.
	if err := d.check(); err != nil {
		return err
	}
	mode := uint32(stat.Mode) & 0o7777
	if err := unix.Fchmod(d.fd(), mode|unix.S_IWUSR); err != nil {
		return fmt.Errorf("make directory %q writable: %w", d.path, err)
	}
	return nil
}

func bindDirectory(fd int, path string, parent *directory, name string, deviceOf func(unix.Stat_t) uint64) (*directory, error) {
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if parent != nil && deviceOf(stat) != parent.id.device {
		file.Close()
		return nil, mountError(path)
	}
	return &directory{file: file, id: identity(stat), parent: parent, name: name, path: path}, nil
}

func mountError(path string) error {
	return fmt.Errorf("refusing to cross mount at %q", path)
}

const directoryFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW

func openEvidenceRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("invalid evidence root %q", path)
	}

	// Walk from the filesystem root so even the passwd home path cannot
	// redirect the store through a symlink.
	fd, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(fd) }()
	current := ""
	for _, name := range strings.Split(path[1:], "/") {
		current += "/" + name
		next, err := unix.Openat(fd, name, directoryFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, fmt.Errorf("create evidence root component %q: %w", current, err)
			}
			next, err = unix.Openat(fd, name, directoryFlags, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("open evidence root component %q without following symlinks: %w", current, err)
		}
		unix.Close(fd)
		fd = next
	}

	// Convert the bound descriptor to an os.Root without reopening its path.
	return os.OpenRoot(filepath.Join(descriptorDirectory, fmt.Sprint(fd)))
}

func (s *store) openDir(parent *directory, name string, create bool) (*directory, error) {
	path := filepath.Join(parent.path, name)
	fd, err := openChildDirectory(parent.fd(), name)
	if create && errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parent.fd(), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create directory %q: %w", path, err)
		}
		fd, err = openChildDirectory(parent.fd(), name)
	}
	switch {
	case errors.Is(err, unix.EXDEV):
		return nil, mountError(path)
	case err != nil:
		return nil, fmt.Errorf("open directory %q: %w", path, err)
	}
	return bindDirectory(fd, path, parent, name, s.deviceOf)
}

func (s *store) openPath(path string, create bool) (*directory, error) {
	dir := s.dir
	if path == "." {
		return dir, nil
	}
	for _, name := range strings.Split(path, "/") {
		next, err := s.openDir(dir, name, create)
		if err != nil {
			s.closePath(dir)
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

func (s *store) closePath(dir *directory) {
	for ; dir != s.dir; dir = dir.parent {
		dir.file.Close()
	}
}
