package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const pinDir = ".pinned"

type entry struct {
	id       string
	path     string
	modified time.Time
	deadline time.Time
	parent   fileID
	identity fileID
}

type store struct {
	location string
	root     *os.Root
	dir      *directory

	beforeRemove func()
	deviceOf     func(unix.Stat_t) uint64
	ownerOf      func(unix.Stat_t) uint32
}

func openStore(location string) (*store, error) {
	root, err := openEvidenceRoot(location)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		root.Close()
		return nil, err
	}
	// Serialize budget checks and renames with other tool invocations.
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		file.Close()
		root.Close()
		return nil, fmt.Errorf("lock evidence store: %w", err)
	}
	return &store{
		location: location,
		root:     root,
		dir:      &directory{file: file, id: identity(stat), path: location},
		deviceOf: func(stat unix.Stat_t) uint64 { return uint64(stat.Dev) },
		ownerOf:  func(stat unix.Stat_t) uint32 { return stat.Uid },
	}, nil
}

func (s *store) close() {
	s.dir.file.Close()
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

func (d *directory) readSorted() ([]fs.DirEntry, error) {
	children, err := d.file.ReadDir(-1)
	slices.SortFunc(children, func(a, b fs.DirEntry) int {
		return cmp.Compare(a.Name(), b.Name())
	})
	return children, err
}

// withDirectories opens each child relative to the directory we enumerated.
func (s *store) withDirectories(parent *directory, skip string, visit func(*directory) error) error {
	children, err := parent.readSorted()
	if err != nil {
		return err
	}
	var errs []error
	for _, child := range children {
		if !child.IsDir() || child.Name() == skip {
			continue
		}
		dir, err := s.openDir(parent, child.Name(), false)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = visit(dir)
			dir.file.Close()
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *store) entries(zone *time.Location) ([]entry, error) {
	var entries []entry
	readOwners := func(base *directory, prefix string, deadline time.Time) error {
		return s.withDirectories(base, pinDir, func(owner *directory) error {
			children, err := owner.readSorted()
			if err != nil {
				return err
			}
			for _, child := range children {
				id := filepath.Join(owner.name, child.Name())
				if !child.IsDir() || !validID(id) {
					continue
				}
				stat, err := owner.stat(child.Name())
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
					continue
				}
				entries = append(entries, entry{
					id: id, path: filepath.Join(prefix, id),
					modified: time.Unix(stat.Mtim.Unix()), deadline: deadline,
					parent: owner.id, identity: identity(stat),
				})
			}
			return nil
		})
	}

	ordinaryErr := readOwners(s.dir, "", time.Time{})
	pins, err := s.openDir(s.dir, pinDir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return entries, ordinaryErr
	}
	if err != nil {
		return entries, errors.Join(ordinaryErr, err)
	}
	defer pins.file.Close()
	pinErr := s.withDirectories(pins, "", func(dir *directory) error {
		date, err := time.ParseInLocation(time.DateOnly, dir.name, zone)
		if err != nil {
			return nil
		}
		return readOwners(dir, filepath.Join(pinDir, dir.name), date.AddDate(0, 0, 1))
	})
	return entries, errors.Join(ordinaryErr, pinErr)
}

func (s *store) bind(e entry) (*directory, error) {
	parent, err := s.openPath(filepath.Dir(e.path), false)
	if err != nil {
		return nil, err
	}
	changed := fmt.Errorf("entry %q changed during cleanup; run katl-dev-evidence list", e.path)
	if parent.id != e.parent {
		s.closePath(parent)
		return nil, changed
	}
	dir, err := s.openDir(parent, filepath.Base(e.path), false)
	if err != nil {
		s.closePath(parent)
		return nil, err
	}
	if dir.id != e.identity {
		s.closePath(dir)
		return nil, changed
	}
	return dir, nil
}

// walk optionally prepares each bound directory before visiting its contents.
// It visits children after descendants without following symlinks or mounts.
func (s *store) walk(dir *directory, prepare func(*directory) error, visit func(*directory, string, *directory) error) error {
	if prepare != nil {
		if err := prepare(dir); err != nil {
			return err
		}
	}
	children, err := dir.file.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, child := range children {
		name := child.Name()
		var subdir *directory
		if child.IsDir() {
			subdir, err = s.openDir(dir, name, false)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			err = s.walk(subdir, prepare, visit)
		}
		if err == nil {
			err = visit(dir, name, subdir)
		}
		if subdir != nil {
			subdir.file.Close()
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		err = nil // tmpfiles may have removed a child.
	}
	return nil
}

func (s *store) size(e entry) (int64, error) {
	dir, err := s.bind(e)
	if err != nil {
		return 0, err
	}
	defer s.closePath(dir)
	var size int64
	err = s.walk(dir, nil, func(parent *directory, name string, child *directory) error {
		if child != nil {
			return nil
		}
		stat, err := parent.stat(name)
		if stat.Mode&unix.S_IFMT == unix.S_IFREG {
			size += stat.Size
		}
		return err
	})
	return size, err
}

func (s *store) measure(e entry, diagnostics io.Writer) (int64, bool) {
	size, err := s.size(e)
	if err != nil {
		fmt.Fprintf(diagnostics, "could not measure evidence entry %q: %v\n", filepath.Join(s.location, e.path), err)
		return 0, false
	}
	return size, true
}

func (s *store) create(entries []entry, id string, diagnostics io.Writer) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid evidence entry %q; use owner/name", id)
	}
	var total int64
	for _, e := range entries {
		if e.id == id {
			return "", fmt.Errorf("entry %q already exists; run katl-dev-evidence list for its path", id)
		}
		size, _ := s.measure(e, diagnostics)
		total += size
	}
	if total > defaultBudget {
		return "", budgetError(total, defaultBudget)
	}
	parent, err := s.openPath(filepath.Dir(id), true)
	if err != nil {
		return "", err
	}
	defer s.closePath(parent)
	if err := unix.Mkdirat(parent.fd(), filepath.Base(id), 0o700); err != nil {
		return "", err
	}
	return filepath.Join(s.location, id), nil
}

func (s *store) move(e entry, destination string) (entry, error) {
	source, err := s.bind(e)
	if err != nil {
		return entry{}, err
	}
	defer s.closePath(source)
	if destination == e.path {
		return e, nil
	}

	parent, err := s.openPath(filepath.Dir(destination), true)
	if err != nil {
		return entry{}, err
	}
	defer s.closePath(parent)
	// tmpfiles skips a locked entry and its descendants while it is moved.
	if err := unix.Flock(source.fd(), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return entry{}, fmt.Errorf("could not lock entry %q; retry after cleanup: %w", e.id, err)
	}
	if err := source.check(); err != nil {
		return entry{}, err
	}
	if err := renameEntry(source.parent, source.name, parent, filepath.Base(destination)); err != nil {
		return entry{}, fmt.Errorf("move %q to %q: %w", filepath.Join(s.location, e.path), filepath.Join(s.location, destination), err)
	}
	s.tidyPinParents(e, source)
	e.path, e.parent = destination, parent.id
	return e, nil
}

func (s *store) remove(e entry) error {
	if s.beforeRemove != nil {
		s.beforeRemove()
	}
	dir, err := s.bind(e)
	if err != nil {
		return err
	}
	defer s.closePath(dir)
	if err := s.walk(dir, s.prepareRemoval, func(parent *directory, name string, child *directory) error {
		if child != nil {
			return child.remove()
		}
		stat, err := parent.stat(name)
		if err != nil {
			return err
		}
		path := filepath.Join(parent.path, name)
		if err := s.checkOwner(path, stat); err != nil {
			return err
		}
		if err := unix.Unlinkat(parent.fd(), name, 0); err != nil {
			return fmt.Errorf("remove %q: %w", path, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := dir.remove(); err != nil {
		return err
	}
	s.tidyPinParents(e, dir)
	return nil
}

// prepareRemoval lets removal unlink the children of a bound directory the
// caller owns. Changing only the bound directory avoids modifying symlink
// targets or hard links.
func (s *store) prepareRemoval(dir *directory) error {
	var stat unix.Stat_t
	if err := unix.Fstat(dir.fd(), &stat); err != nil {
		return fmt.Errorf("stat %q: %w", dir.path, err)
	}
	if err := s.checkOwner(dir.path, stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IWUSR != 0 {
		return nil
	}
	if err := dir.check(); err != nil {
		return err
	}
	mode := uint32(stat.Mode) & 0o7777
	if err := unix.Fchmod(dir.fd(), mode|unix.S_IWUSR); err != nil {
		return fmt.Errorf("make directory %q writable: %w", dir.path, err)
	}
	return nil
}

// checkOwner refuses to remove a path another user owns, such as one a
// rootless container created under a subordinate uid.
func (s *store) checkOwner(path string, stat unix.Stat_t) error {
	owner := s.ownerOf(stat)
	if int(owner) == os.Geteuid() {
		return nil
	}
	name := fmt.Sprint("uid ", owner)
	if u, err := user.LookupId(fmt.Sprint(owner)); err == nil {
		name = fmt.Sprintf("%s (uid %d)", u.Username, owner)
	}
	return fmt.Errorf("refusing to remove %q: it is owned by %s, not by you; have that user remove it, then run the command again", path, name)
}

func (s *store) tidyPinParents(e entry, dir *directory) {
	if !e.deadline.IsZero() {
		owner := dir.parent
		if owner.remove() == nil {
			_ = owner.parent.remove()
		}
	}
}

func (s *store) prune(out io.Writer, entries []entry, now time.Time, budget int64) error {
	type measuredEntry struct {
		entry
		size int64
	}
	var candidates []measuredEntry
	var errs []error
	collisions := make(map[string]bool)
	var total int64
	for _, e := range entries {
		if !e.deadline.IsZero() && !now.Before(e.deadline) {
			moved, err := s.move(e, e.id)
			if err == nil {
				e, e.deadline = moved, time.Time{}
			} else {
				errs = append(errs, fmt.Errorf("return expired pin %q to ordinary aging: %w", e.id, err))
				if errors.Is(err, fs.ErrExist) {
					collisions[e.id] = true
				}
			}
		}
		size, err := s.size(e)
		total += size
		if err != nil {
			errs = append(errs, err)
			continue
		}
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
		if collisions[e.id] {
			continue
		}
		if err := s.remove(e.entry); err != nil {
			errs = append(errs, err)
			continue
		}
		total -= e.size
		if _, err := fmt.Fprintln(out, e.id); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	if total > budget {
		errs = append(errs, budgetError(total, budget))
	}
	return errors.Join(errs...)
}

func budgetError(total, budget int64) error {
	return fmt.Errorf("evidence store is over budget (%d bytes, limit %d); run katl-dev-evidence list, then prune or release an entry to free space", total, budget)
}
