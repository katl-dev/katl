package devclean

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// UserNS removes a tree from inside the rootless container user namespace,
// where files that builds ran under subuids are owned by a mapped user.
type UserNS interface {
	// RemoveAll deletes path and everything below it that is on the same
	// filesystem as path.
	RemoveAll(ctx context.Context, path string) error
}

// PodmanUserNS removes trees with podman unshare.
type PodmanUserNS struct{}

// RemoveAll deletes path from inside the user namespace podman unshare enters,
// without crossing into other filesystems.
func (PodmanUserNS) RemoveAll(ctx context.Context, path string) error {
	const script = `find "$1" -xdev -type d -exec chmod u+rwx {} + 2>/dev/null; rm -rf --one-file-system -- "$1"`
	out, err := exec.CommandContext(ctx, "podman", "unshare", "sh", "-c", script, "sh", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman unshare: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LeftoverError reports that a removal left files behind.
type LeftoverError struct {
	Root string
	// Paths are some of the entries still present under Root.
	Paths []string
	// Cause is the first failure of the user namespace removal.
	Cause error
}

func (e *LeftoverError) Error() string {
	msg := fmt.Sprintf("could not remove %s; still present: %s", e.Root, strings.Join(e.Paths, ", "))
	if e.Cause != nil {
		msg += fmt.Sprintf(" (%v)", e.Cause)
	}
	return msg
}

func (e *LeftoverError) Unwrap() error { return e.Cause }

const maxLeftovers = 5

// Remove deletes the verified worktree directory, using ns for files that the
// calling user cannot delete. It does not follow symbolic links or cross
// mount points, never touches a path other than the worktree, and succeeds
// only when the directory is gone. The worktree's .git file is removed last,
// so a partial removal can be retried.
func (w Worktree) Remove(ctx context.Context, ns UserNS) error {
	if !w.verified || w.Path != w.root {
		return errors.New("refusing to remove a worktree that CheckWorktree did not verify")
	}
	return newTree(w.root, ".git").remove(ctx, ns)
}

// Remove deletes the verified run directory, using ns for files that the
// calling user cannot delete. It does not follow symbolic links or cross
// mount points, never touches a path other than the run directory, and
// succeeds only when it is gone. The ownership marker is removed last, so a
// partial removal can be retried.
func (r RunDir) Remove(ctx context.Context, ns UserNS) error {
	if !r.verified || r.Path != r.root {
		return errors.New("refusing to remove a run directory that CheckRunDir did not verify")
	}
	return newTree(r.root, RunDirMarker).remove(ctx, ns)
}

// tree is a directory to delete. Its marker entry is the evidence that the
// directory may be deleted, so it is removed after everything else.
type tree struct {
	root   string
	marker string

	// remove deletes one file or empty directory.
	removeEntry func(path string) error
	// device identifies the filesystem holding an entry.
	device func(fs.FileInfo) uint64
}

func newTree(root, marker string) tree {
	return tree{root: root, marker: marker, removeEntry: os.Remove, device: statDevice}
}

func statDevice(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// remove first deletes the tree as the calling user, which handles read-only
// directories and files owned by other users inside directories the caller
// owns, then asks ns for what remains. The result is judged by looking at the
// filesystem, never by what either step reports.
func (t tree) remove(ctx context.Context, ns UserNS) error {
	root := t.root
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Dir(root) == root {
		return fmt.Errorf("refusing to remove %q: not a clean absolute path below the filesystem root", root)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to remove %s: not a directory", root)
	}

	rootDev := t.device(info)
	t.removeContents(root, rootDev, true)

	var cause error
	if rest := t.entries(root); len(rest) > 0 {
		for _, entry := range rest {
			if err := ns.RemoveAll(ctx, entry); err != nil && cause == nil {
				cause = err
			}
		}
	}

	if len(t.entries(root)) == 0 {
		_ = t.removeEntry(filepath.Join(root, t.marker))
		_ = t.removeEntry(root)
	}
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return &LeftoverError{Root: root, Paths: t.leftovers(root, rootDev), Cause: cause}
}

// entries lists the top-level entries of root other than the marker.
func (t tree) entries(root string) []string {
	list, _ := os.ReadDir(root)
	var found []string
	for _, entry := range list {
		if entry.Name() != t.marker {
			found = append(found, filepath.Join(root, entry.Name()))
		}
	}
	return found
}

// removeContents deletes what is below dir without following symbolic links
// or entering directories on another filesystem. Failures are ignored:
// whatever stays is reported by the final check.
func (t tree) removeContents(dir string, rootDev uint64, top bool) {
	// Directories the caller owns may be read-only.
	_ = os.Chmod(dir, 0o700)
	list, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range list {
		if top && entry.Name() == t.marker {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			if t.device(info) != rootDev {
				continue
			}
			t.removeContents(path, rootDev, false)
		}
		_ = t.removeEntry(path)
	}
}

// leftovers returns up to maxLeftovers of the innermost entries still present
// under root, other than the marker.
func (t tree) leftovers(root string, rootDev uint64) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if path == root || (filepath.Dir(path) == root && entry != nil && entry.Name() == t.marker) {
			return nil
		}
		switch {
		case err != nil:
			// An unreadable directory is a leaf.
			found = append(found, path)
			return fs.SkipDir
		case entry.IsDir():
			info, infoErr := entry.Info()
			if infoErr == nil && t.device(info) != rootDev {
				found = append(found, path)
				return fs.SkipDir
			}
			if list, readErr := os.ReadDir(path); readErr == nil && len(list) == 0 {
				found = append(found, path)
			}
		default:
			found = append(found, path)
		}
		if len(found) >= maxLeftovers {
			return fs.SkipAll
		}
		return nil
	})
	if len(found) == 0 {
		found = []string{root}
	}
	return found
}
