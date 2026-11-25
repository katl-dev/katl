package devclean

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// UserNS removes a tree from inside the rootless container user namespace,
// where files that builds ran under subuids are owned by a mapped user.
type UserNS interface {
	// RemoveAll deletes path and everything below it that is on the same
	// filesystem as path.
	RemoveAll(ctx context.Context, path string) error
	// Mountinfo returns the namespace's /proc/self/mountinfo, which can list
	// mounts the host cannot see. An error means the namespace is unavailable.
	Mountinfo(ctx context.Context) (string, error)
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

// Mountinfo reads the mount table from inside the user namespace podman
// unshare enters.
func (PodmanUserNS) Mountinfo(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "podman", "unshare", "cat", "/proc/self/mountinfo").Output()
	if err != nil {
		return "", fmt.Errorf("podman unshare: %w", err)
	}
	return string(out), nil
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
// calling user cannot delete. It does not follow symbolic links, refuses a
// worktree that holds a mount point, never touches a path other than the
// worktree, and succeeds only when the directory is gone. Untracked output is
// removed before tracked files and the .git file last, so a partial removal
// can be retried.
func (w Worktree) Remove(ctx context.Context, ns UserNS) error {
	if !w.verified || w.Path != w.root {
		return errors.New("refusing to remove a worktree that CheckWorktree did not verify")
	}
	return w.tree().remove(ctx, ns)
}

func (w Worktree) tree() tree {
	t := newTree(w.root, ".git")
	t.last = w.tracked
	return t
}

// Remove deletes the verified run directory, using ns for files that the
// calling user cannot delete. It does not follow symbolic links, refuses a
// run directory that holds a mount point, never touches a path other than the
// run directory, and succeeds only when it is gone. The ownership marker is removed last, so a
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
	// last names the top-level entries removed only once every other entry
	// is gone.
	last []string

	// removeEntry deletes one file or empty directory.
	removeEntry func(path string) error
	// hostMountinfo returns the host's /proc/self/mountinfo.
	hostMountinfo func() (string, error)
}

func newTree(root, marker string) tree {
	return tree{root: root, marker: marker, removeEntry: os.Remove, hostMountinfo: readHostMountinfo}
}

func readHostMountinfo() (string, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	return string(b), err
}

// mountsUnder returns the mount points in mountinfo that are root or below it.
func mountsUnder(mountinfo, root string) []string {
	var found []string
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point := unescapeMountinfo(fields[4])
		if point == root || strings.HasPrefix(point, root+"/") {
			found = append(found, point)
		}
	}
	return found
}

// unescapeMountinfo decodes the octal escapes, such as \040 for a space, that
// the kernel uses for whitespace and backslashes in mountinfo paths.
func unescapeMountinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// checkNoMounts refuses when a mount point is root or below it. Deleting a
// mount point deletes the contents of the filesystem mounted there, and a bind
// mount keeps the device of its surroundings, so mounts are found in the mount
// tables rather than by comparing devices. Rootless overlay mounts can be
// visible only inside the user namespace, so its table is read too when the
// namespace is available. It returns the namespace the removal may use: when
// the namespace mount table cannot be read, mounts there cannot be ruled out,
// so the namespace step is replaced by one that refuses to delete anything.
func (t tree) checkNoMounts(ctx context.Context, ns UserNS) (UserNS, error) {
	host, err := t.hostMountinfo()
	if err != nil {
		return nil, fmt.Errorf("refusing to remove %s: cannot read the mount table to check for mounted filesystems: %w", t.root, err)
	}
	mounts := mountsUnder(host, t.root)
	inNS, err := ns.Mountinfo(ctx)
	if err != nil {
		ns = unavailableNS{err: err}
	} else {
		mounts = append(mounts, mountsUnder(inNS, t.root)...)
	}
	if len(mounts) == 0 {
		return ns, nil
	}
	slices.Sort(mounts)
	mounts = slices.Compact(mounts)
	return nil, fmt.Errorf("refusing to remove %s: it holds mounted filesystems whose contents would be deleted: %s; unmount them with umount <path> (podman unshare umount <path> for mounts made in the rootless user namespace) and run the command again", t.root, strings.Join(mounts, ", "))
}

// unavailableNS stands in for a user namespace whose mount table could not be
// read, so that nothing is deleted there and the cause is reported.
type unavailableNS struct{ err error }

func (u unavailableNS) RemoveAll(context.Context, string) error {
	return fmt.Errorf("user namespace unavailable: %w", u.err)
}

func (u unavailableNS) Mountinfo(context.Context) (string, error) { return "", u.err }

// remove first deletes the tree as the calling user, which handles read-only
// directories and files owned by other users inside directories the caller
// owns, then asks ns for what remains. Entries named by last are left until
// everything else is gone, so a failure leaves them for a retry. The result is
// judged by looking at the filesystem, never by what either step reports.
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
	ns, err = t.checkNoMounts(ctx, ns)
	if err != nil {
		return err
	}

	// Directories the caller owns may be read-only.
	_ = os.Chmod(root, 0o700)
	var cause error
	for _, stage := range t.stages() {
		for _, entry := range stage {
			t.removePath(entry)
		}
		var rest []string
		for _, entry := range stage {
			if present(entry) {
				rest = append(rest, entry)
			}
		}
		for _, entry := range rest {
			if err := ns.RemoveAll(ctx, entry); err != nil && cause == nil {
				cause = err
			}
		}
		if len(rest) > 0 {
			break
		}
	}

	if len(t.entries()) == 0 {
		_ = t.removeEntry(filepath.Join(root, t.marker))
		_ = t.removeEntry(root)
	}
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return &LeftoverError{Root: root, Paths: t.leftovers(root), Cause: cause}
}

func present(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// stages splits the top-level entries into those removed first and those named
// by last.
func (t tree) stages() [][]string {
	var first, later []string
	for _, entry := range t.entries() {
		if slices.Contains(t.last, filepath.Base(entry)) {
			later = append(later, entry)
		} else {
			first = append(first, entry)
		}
	}
	return [][]string{first, later}
}

// entries lists the top-level entries of the root other than the marker.
func (t tree) entries() []string {
	list, _ := os.ReadDir(t.root)
	var found []string
	for _, entry := range list {
		if entry.Name() != t.marker {
			found = append(found, filepath.Join(t.root, entry.Name()))
		}
	}
	return found
}

// removePath deletes path and, for a directory, what is below it without
// following symbolic links. Failures are ignored: whatever stays is reported
// by the final check.
func (t tree) removePath(path string) {
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.IsDir() {
		// Directories the caller owns may be read-only.
		_ = os.Chmod(path, 0o700)
		list, err := os.ReadDir(path)
		if err != nil {
			return
		}
		for _, entry := range list {
			t.removePath(filepath.Join(path, entry.Name()))
		}
	}
	_ = t.removeEntry(path)
}

// leftovers returns up to maxLeftovers of the innermost entries still present
// under root, other than the marker.
func (t tree) leftovers(root string) []string {
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
