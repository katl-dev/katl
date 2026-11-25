package devclean

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNS records the paths it is asked to remove and runs act on them.
type fakeNS struct {
	calls []string
	act   func(path string) error
}

func (f *fakeNS) RemoveAll(_ context.Context, path string) error {
	f.calls = append(f.calls, path)
	return f.act(path)
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeEverything(path string) error { return os.RemoveAll(path) }

func refuseEverything(string) error { return errors.New("permission denied") }

// hostCannotDelete returns a tree whose calling-user step deletes nothing,
// as when every file below the root belongs to a subuid.
func hostCannotDelete(root string) tree {
	t := newTree(root, RunDirMarker)
	t.removeEntry = func(path string) error {
		if path == root || path == filepath.Join(root, RunDirMarker) {
			return os.Remove(path)
		}
		return errors.New("permission denied")
	}
	return t
}

func TestRemoveRefusesUnverifiedPaths(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "precious", "file")
	writeFile(t, outside)
	ns := &fakeNS{act: removeEverything}

	for name, remove := range map[string]func(string) error{
		"worktree": func(p string) error { return Worktree{Path: p}.Remove(context.Background(), ns) },
		"run dir":  func(p string) error { return RunDir{Path: p, Present: true}.Remove(context.Background(), ns) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := remove(filepath.Dir(outside)); err == nil {
				t.Fatal("Remove accepted a path that no check verified")
			}
			if !exists(outside) || len(ns.calls) != 0 {
				t.Fatalf("a refused removal touched %s (namespace calls: %v)", outside, ns.calls)
			}
		})
	}
}

func TestRemoveDeletesOnlyTheVerifiedPath(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, RunDirParent, "run-1")
	writeFile(t, filepath.Join(run, RunDirMarker))
	victim := filepath.Join(base, "victim")
	writeFile(t, filepath.Join(victim, "file"))
	ns := &fakeNS{act: removeEverything}

	checked, err := CheckRunDir(run)
	if err != nil {
		t.Fatal(err)
	}
	checked.Path = victim

	if err := checked.Remove(context.Background(), ns); err == nil {
		t.Fatal("Remove deleted a path other than the one that was verified")
	}
	if !exists(filepath.Join(victim, "file")) || !exists(run) || len(ns.calls) != 0 {
		t.Fatalf("a refused removal deleted something (namespace calls: %v)", ns.calls)
	}
}

func TestRemoveRefusesRootsThatAreNotCleanAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim", "file")
	writeFile(t, victim)
	ns := &fakeNS{act: removeEverything}

	for _, root := range []string{"/", "relative/dir", base + "/wt/../victim", ""} {
		if err := newTree(root, ".git").remove(context.Background(), ns); err == nil {
			t.Errorf("remove(%q) succeeded", root)
		}
	}
	if !exists(victim) || len(ns.calls) != 0 {
		t.Fatalf("a refused removal touched %s (namespace calls: %v)", victim, ns.calls)
	}
}

func TestRemoveStaysInsideTree(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "file"))
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	root := filepath.Join(base, "wt")
	writeFile(t, filepath.Join(root, "_build", "f"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "_build", "deep-link")); err != nil {
		t.Fatal(err)
	}
	ns := &fakeNS{act: func(string) error { return errors.New("must not be called") }}

	if err := newTree(root, ".git").remove(context.Background(), ns); err != nil {
		t.Fatal(err)
	}

	if exists(root) {
		t.Fatal("tree survived")
	}
	info, err := os.Stat(outside)
	if err != nil || !exists(filepath.Join(outside, "file")) {
		t.Fatalf("removal followed a symbolic link out of the tree: %v", err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("removal changed the mode of a directory outside the tree to %v", info.Mode().Perm())
	}
	if len(ns.calls) != 0 {
		t.Fatalf("namespace was called for a tree the caller could delete: %v", ns.calls)
	}
}

func TestRemoveUserNSOnlyForWhatTheCallerCannotDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(root, "ro", "f"))
	if err := os.Chmod(filepath.Join(root, "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	ns := &fakeNS{act: func(string) error { return errors.New("must not be called") }}

	if err := newTree(root, ".git").remove(context.Background(), ns); err != nil {
		t.Fatal(err)
	}

	if exists(root) || len(ns.calls) != 0 {
		t.Fatalf("read-only tree survived or needlessly used the namespace (calls: %v)", ns.calls)
	}
}

func TestRemoveJudgesByTheFilesystem(t *testing.T) {
	setup := func(t *testing.T) string {
		root := filepath.Join(t.TempDir(), "wt")
		writeFile(t, filepath.Join(root, "_build", "mkosi", "image.iso"))
		return root
	}

	t.Run("removed by the namespace", func(t *testing.T) {
		root := setup(t)
		ns := &fakeNS{act: removeEverything}

		if err := hostCannotDelete(root).remove(context.Background(), ns); err != nil {
			t.Fatal(err)
		}
		if exists(root) {
			t.Fatal("tree survived")
		}
	})

	t.Run("namespace reports success but leaves the files", func(t *testing.T) {
		root := setup(t)
		ns := &fakeNS{act: func(string) error { return nil }}

		err := hostCannotDelete(root).remove(context.Background(), ns)

		var left *LeftoverError
		if !errors.As(err, &left) {
			t.Fatalf("remove() = %v; want a LeftoverError", err)
		}
		if !strings.Contains(err.Error(), "image.iso") || !exists(root) {
			t.Fatalf("error does not name the file that was left: %v", err)
		}
	})

	t.Run("namespace fails and says why", func(t *testing.T) {
		root := setup(t)
		ns := &fakeNS{act: func(string) error { return errors.New("podman: not found") }}

		err := hostCannotDelete(root).remove(context.Background(), ns)

		if err == nil || !strings.Contains(err.Error(), "podman: not found") || !strings.Contains(err.Error(), "image.iso") {
			t.Fatalf("remove() = %v; want the leftover paths and the namespace failure", err)
		}
	})
}

func TestRemoveKeepsMarkerUntilEverythingElseIsGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), RunDirParent, "run-1")
	writeFile(t, filepath.Join(root, RunDirMarker))
	writeFile(t, filepath.Join(root, "_build", "image.iso"))
	writeFile(t, filepath.Join(root, "scenarios", "result.json"))

	failing := &fakeNS{act: func(string) error { return errors.New("podman: not found") }}
	if err := hostCannotDelete(root).remove(context.Background(), failing); err == nil {
		t.Fatal("remove succeeded although nothing could be deleted")
	}

	if _, err := CheckRunDir(root); err != nil {
		t.Fatalf("a partly removed run directory is no longer recognised, so a retry would refuse: %v", err)
	}
	working := &fakeNS{act: removeEverything}
	if err := hostCannotDelete(root).remove(context.Background(), working); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if exists(root) {
		t.Fatal("run directory survived the retry")
	}
}

func TestRemoveDoesNotCrossMountPoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(root, "plain", "f"))
	writeFile(t, filepath.Join(root, "mnt", "precious", "f"))
	tr := newTree(root, ".git")
	realDevice := tr.device
	tr.device = func(info fs.FileInfo) uint64 {
		if info.Name() == "mnt" {
			return realDevice(info) + 1
		}
		return realDevice(info)
	}
	ns := &fakeNS{act: func(path string) error {
		if filepath.Base(path) == "mnt" {
			return nil // as rm --one-file-system skips a mount point
		}
		return os.RemoveAll(path)
	}}

	err := tr.remove(context.Background(), ns)

	var left *LeftoverError
	if !errors.As(err, &left) || !strings.Contains(err.Error(), filepath.Join(root, "mnt")) {
		t.Fatalf("remove() = %v; want a LeftoverError naming the mount point", err)
	}
	if !exists(filepath.Join(root, "mnt", "precious", "f")) {
		t.Fatal("removal entered another filesystem")
	}
	if exists(filepath.Join(root, "plain")) {
		t.Fatal("removal gave up on the rest of the tree")
	}
}

func TestRemoveLeftoversNameInnermostEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(root, "a", "b", "file"))
	ns := &fakeNS{act: func(string) error { return nil }}

	err := hostCannotDelete(root).remove(context.Background(), ns)

	var left *LeftoverError
	if !errors.As(err, &left) || len(left.Paths) != 1 || left.Paths[0] != filepath.Join(root, "a", "b", "file") {
		t.Fatalf("remove() = %v; want only the file that is still present", err)
	}
}

// TestRemoveSubuidOwnedFiles removes files that only the rootless user
// namespace can delete, and checks the result without asking the code under test.
func TestRemoveSubuidOwnedFiles(t *testing.T) {
	if err := exec.Command("podman", "unshare", "true").Run(); err != nil {
		t.Skipf("rootless podman user namespace unavailable: %v", err)
	}
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "file"))
	dir := filepath.Join(base, RunDirParent, "run-1")
	writeFile(t, filepath.Join(dir, RunDirMarker))
	writeFile(t, filepath.Join(dir, "result.json"))
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// Fixture only: give a private directory to a subuid, as mkosi builds do.
	setup := `mkdir -m 700 "$1/_build" && touch "$1/_build/image.iso" && chown -R 1:1 "$1/_build" && ln -s "$2" "$1/_build/link"`
	if out, err := exec.Command("podman", "unshare", "sh", "-c", setup, "sh", dir, outside).CombinedOutput(); err != nil {
		t.Fatalf("creating subuid-owned files: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "_build", "image.iso")); err == nil {
		t.Fatal("fixture is readable by the caller, so it does not need the namespace")
	}

	run, err := CheckRunDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	failing := &fakeNS{act: func(string) error { return errors.New("podman: not found") }}
	if err := run.Remove(context.Background(), failing); err == nil {
		t.Fatal("Remove succeeded although the subuid-owned files could not be deleted")
	}
	retry, err := CheckRunDir(dir)
	if err != nil {
		t.Fatalf("a partly removed run directory is refused, so cleanup cannot be retried: %v", err)
	}
	if err := retry.Remove(context.Background(), PodmanUserNS{}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory survived: %v", err)
	}
	if !exists(filepath.Join(outside, "file")) {
		t.Fatal("removal followed a symbolic link out of the run directory")
	}
}
