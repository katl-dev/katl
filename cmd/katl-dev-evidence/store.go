package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const pinDir = ".pinned"

type entry struct {
	id       string
	path     string
	modified time.Time
	deadline time.Time
}

type store struct {
	location string
	root     *os.Root
	lock     *os.File
}

func openStore(location string) (*store, error) {
	if err := os.MkdirAll(location, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(location)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("evidence root %q is not a directory", location)
	}
	root, err := os.OpenRoot(location)
	if err != nil {
		return nil, err
	}
	lock, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	bound, err := lock.Stat()
	if err != nil || !os.SameFile(info, bound) {
		lock.Close()
		root.Close()
		return nil, fmt.Errorf("could not bind evidence root %q", location)
	}
	// Serialize budget checks and renames with other tool invocations.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		root.Close()
		return nil, fmt.Errorf("lock evidence store: %w", err)
	}
	return &store{location: location, root: root, lock: lock}, nil
}

func (s *store) close() {
	s.lock.Close()
	s.root.Close()
}

func validID(id string) bool {
	owner, name, ok := strings.Cut(id, "/")
	return ok && fs.ValidPath(id) && !strings.Contains(name, "/") &&
		owner != pinDir && !strings.ContainsFunc(id, unicode.IsControl)
}

func (s *store) find(entries []entry, operand string) (entry, error) {
	key := operand
	absolute := filepath.IsAbs(operand)
	if absolute {
		var inside bool
		key, inside = strings.CutPrefix(operand, s.location+string(filepath.Separator))
		if !inside || !fs.ValidPath(key) {
			return entry{}, fmt.Errorf("invalid evidence entry path %q", operand)
		}
	} else if !validID(key) {
		return entry{}, fmt.Errorf("invalid evidence entry %q; use owner/name or its current path", operand)
	}

	for _, e := range entries {
		if (absolute && e.path == key) || (!absolute && e.id == key) {
			return e, nil
		}
	}
	return entry{}, fmt.Errorf("unknown evidence entry %q; run katl-dev-evidence list", operand)
}

func (s *store) directories(path string) ([]fs.DirEntry, error) {
	dirs, err := fs.ReadDir(s.root.FS(), path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return dirs, err
}

func (s *store) entries(zone *time.Location) ([]entry, error) {
	var entries []entry
	readOwners := func(base string, deadline time.Time) error {
		owners, err := s.directories(base)
		if err != nil {
			return err
		}
		for _, owner := range owners {
			if !owner.IsDir() || owner.Name() == pinDir {
				continue
			}
			paths, err := s.directories(filepath.Join(base, owner.Name()))
			if err != nil {
				return err
			}
			for _, dir := range paths {
				id := filepath.Join(owner.Name(), dir.Name())
				if !dir.IsDir() || !validID(id) {
					continue
				}
				info, err := dir.Info()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				entries = append(entries, entry{id: id, path: filepath.Join(base, id), modified: info.ModTime(), deadline: deadline})
			}
		}
		return nil
	}
	if err := readOwners(".", time.Time{}); err != nil {
		return nil, err
	}

	pins, err := s.root.Lstat(pinDir)
	if errors.Is(err, fs.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if !pins.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", pinDir)
	}
	dates, err := s.directories(pinDir)
	if err != nil {
		return nil, err
	}
	for _, dir := range dates {
		date, err := time.ParseInLocation(time.DateOnly, dir.Name(), zone)
		if err != nil || !dir.IsDir() {
			continue
		}
		if err := readOwners(filepath.Join(pinDir, dir.Name()), date.AddDate(0, 0, 1)); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func (s *store) size(e entry) (int64, error) {
	var size int64
	err := fs.WalkDir(s.root.FS(), e.path, func(_ string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil // tmpfiles may expire files while we count them.
		}
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func (s *store) makeParents(path string) error {
	current := ""
	for _, part := range strings.Split(filepath.Dir(path), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		err := s.root.Mkdir(current, 0o700)
		if errors.Is(err, fs.ErrExist) {
			info, err := s.root.Lstat(current)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (s *store) create(entries []entry, id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid evidence entry %q; use owner/name", id)
	}
	var total int64
	for _, e := range entries {
		if e.id == id {
			return "", fmt.Errorf("entry %q already exists; run katl-dev-evidence list for its path", id)
		}
		size, err := s.size(e)
		if err != nil {
			return "", err
		}
		total += size
	}
	if total > defaultBudget {
		return "", budgetError(total, defaultBudget)
	}
	if err := s.makeParents(id); err != nil {
		return "", err
	}
	if err := s.root.Mkdir(id, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(s.location, id), nil
}

func (s *store) move(e entry, destination string) error {
	if destination == e.path {
		return nil
	}
	if err := s.makeParents(destination); err != nil {
		return err
	}
	if _, err := s.root.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("entry destination %q already exists", destination)
	}

	// tmpfiles skips a locked entry and its descendants while it is moved.
	dir, err := s.root.Open(e.path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("could not lock entry %q; retry after cleanup: %w", e.id, err)
	}
	bound, err := dir.Stat()
	if err != nil {
		return err
	}
	current, err := s.root.Lstat(e.path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(bound, current) {
		return fmt.Errorf("entry %q changed during cleanup; run katl-dev-evidence list", e.id)
	}
	if err := s.root.Rename(e.path, destination); err != nil {
		return err
	}
	s.tidyPinParents(e)
	return nil
}

func (s *store) remove(e entry) error {
	// Root-relative removal confines traversal even if a path is replaced
	// by a symlink after discovery.
	if err := s.root.RemoveAll(e.path); err != nil {
		return err
	}
	s.tidyPinParents(e)
	return nil
}

func (s *store) tidyPinParents(e entry) {
	if !e.deadline.IsZero() {
		owner := filepath.Dir(e.path)
		// Remove only empty bookkeeping directories.
		_ = s.root.Remove(owner)
		_ = s.root.Remove(filepath.Dir(owner))
	}
}

func (s *store) prune(out io.Writer, entries []entry, now time.Time, budget int64) error {
	type measuredEntry struct {
		entry
		size int64
	}
	var candidates []measuredEntry
	var total int64
	for _, e := range entries {
		if !e.deadline.IsZero() && !now.Before(e.deadline) {
			if err := s.move(e, e.id); err != nil {
				return fmt.Errorf("return expired pin %q to ordinary aging: %w", e.id, err)
			}
			e.path, e.deadline = e.id, time.Time{}
		}
		size, err := s.size(e)
		if err != nil {
			return err
		}
		total += size
		if e.deadline.IsZero() {
			candidates = append(candidates, measuredEntry{e, size})
		}
	}

	slices.SortFunc(candidates, func(a, b measuredEntry) int {
		return cmp.Or(a.modified.Compare(b.modified), cmp.Compare(a.id, b.id))
	})
	for _, e := range candidates {
		if total <= budget {
			break
		}
		if err := s.remove(e.entry); err != nil {
			return err
		}
		total -= e.size
		if _, err := fmt.Fprintln(out, e.id); err != nil {
			return err
		}
	}
	if total > budget {
		return budgetError(total, budget)
	}
	return nil
}

func budgetError(total, budget int64) error {
	return fmt.Errorf("evidence store is over budget (%d bytes, limit %d); run katl-dev-evidence list, then prune or release an entry to free space", total, budget)
}
