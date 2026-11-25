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
)

// UserNS removes a tree from inside the rootless container user namespace,
// where files that builds ran under subuids are owned by a mapped user.
type UserNS interface {
	RemoveAll(ctx context.Context, path string) error
}

// PodmanUserNS removes trees with podman unshare.
type PodmanUserNS struct{}

// RemoveAll deletes path from inside the user namespace podman unshare enters.
func (PodmanUserNS) RemoveAll(ctx context.Context, path string) error {
	const script = `chmod -R u+rwX -- "$1" 2>/dev/null; rm -rf -- "$1"`
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
	// Cause is the last failure of the user namespace removal.
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
// calling user cannot delete. It does not follow symbolic links, never touches
// a path other than the worktree, and succeeds only when the directory is gone.
func (w Worktree) Remove(ctx context.Context, ns UserNS) error {
	if !w.verified {
		return errors.New("refusing to remove a worktree that CheckWorktree did not verify")
	}
	return removeTree(ctx, w.Path, os.RemoveAll, ns)
}

// Remove deletes the verified run directory, using ns for files that the
// calling user cannot delete. It does not follow symbolic links, never touches
// a path other than the run directory, and succeeds only when it is gone.
func (r RunDir) Remove(ctx context.Context, ns UserNS) error {
	if !r.verified {
		return errors.New("refusing to remove a run directory that CheckRunDir did not verify")
	}
	return removeTree(ctx, r.Path, os.RemoveAll, ns)
}

// removeTree first deletes root as the calling user, which handles read-only
// directories and files owned by other users inside directories the caller
// owns, then asks ns for what remains. The result is judged by looking at the
// filesystem, never by what either step reports.
func removeTree(ctx context.Context, root string, removeAll func(string) error, ns UserNS) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Dir(root) == root {
		return fmt.Errorf("refusing to remove %q: not a clean absolute path below the filesystem root", root)
	}

	makeWritable(root)
	_ = removeAll(root)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	cause := ns.RemoveAll(ctx, root)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return &LeftoverError{Root: root, Paths: leftovers(root), Cause: cause}
}

// makeWritable lets the caller delete read-only directories it owns. Errors
// are ignored: whatever stays unremovable is reported by the final check.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if entry != nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}

func leftovers(root string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if path != root {
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
