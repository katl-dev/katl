package devclean

import (
	"context"
	"errors"
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

func failRemoveAll(string) error { return errors.New("permission denied") }

func TestRemoveRefusesUnverifiedPaths(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "precious", "file")
	writeFile(t, outside)
	ns := &fakeNS{act: func(path string) error { return os.RemoveAll(path) }}

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

func TestRemoveRefusesRootsThatAreNotCleanAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim", "file")
	writeFile(t, victim)
	ns := &fakeNS{act: func(path string) error { return os.RemoveAll(path) }}

	for _, root := range []string{"/", "relative/dir", base + "/wt/../victim", ""} {
		if err := removeTree(context.Background(), root, os.RemoveAll, ns); err == nil {
			t.Errorf("removeTree(%q) succeeded", root)
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
	root := filepath.Join(base, "wt")
	writeFile(t, filepath.Join(root, "_build", "f"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	ns := &fakeNS{act: func(path string) error { return os.RemoveAll(path) }}

	if err := removeTree(context.Background(), root, failRemoveAll, ns); err != nil {
		t.Fatal(err)
	}

	if exists(root) {
		t.Fatal("tree survived")
	}
	if !exists(filepath.Join(outside, "file")) {
		t.Fatal("removal followed a symbolic link out of the tree")
	}
	if len(ns.calls) != 1 || ns.calls[0] != root {
		t.Fatalf("namespace was asked to remove %v; want only %s", ns.calls, root)
	}
}

func TestRemoveUserNSOnlyForWhatTheCallerCannotDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(root, "ro", "f"))
	if err := os.Chmod(filepath.Join(root, "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	ns := &fakeNS{act: func(string) error { return errors.New("must not be called") }}

	if err := removeTree(context.Background(), root, os.RemoveAll, ns); err != nil {
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
		ns := &fakeNS{act: func(path string) error { return os.RemoveAll(path) }}

		if err := removeTree(context.Background(), root, failRemoveAll, ns); err != nil {
			t.Fatal(err)
		}
		if exists(root) {
			t.Fatal("tree survived")
		}
	})

	t.Run("namespace reports success but leaves the files", func(t *testing.T) {
		root := setup(t)
		ns := &fakeNS{act: func(string) error { return nil }}

		err := removeTree(context.Background(), root, failRemoveAll, ns)

		var left *LeftoverError
		if !errors.As(err, &left) {
			t.Fatalf("removeTree() = %v; want a LeftoverError", err)
		}
		if !strings.Contains(err.Error(), "_build") || !exists(root) {
			t.Fatalf("error does not name what was left: %v", err)
		}
	})

	t.Run("namespace fails and says why", func(t *testing.T) {
		root := setup(t)
		ns := &fakeNS{act: func(string) error { return errors.New("podman: not found") }}

		err := removeTree(context.Background(), root, failRemoveAll, ns)

		if err == nil || !strings.Contains(err.Error(), "podman: not found") || !strings.Contains(err.Error(), "_build") {
			t.Fatalf("removeTree() = %v; want the leftover paths and the namespace failure", err)
		}
	})
}

// TestRemoveSubuidOwnedFiles removes files that only the rootless user
// namespace can delete, and checks the result without asking the code under test.
func TestRemoveSubuidOwnedFiles(t *testing.T) {
	if err := exec.Command("podman", "unshare", "true").Run(); err != nil {
		t.Skipf("rootless podman user namespace unavailable: %v", err)
	}
	dir := filepath.Join(t.TempDir(), RunDirParent, "run-1")
	writeFile(t, filepath.Join(dir, "result.json"))
	// Fixture only: give a private directory to a subuid, as mkosi builds do.
	setup := `mkdir -m 700 "$1/_build" && touch "$1/_build/image.iso" && chown -R 1:1 "$1/_build"`
	if out, err := exec.Command("podman", "unshare", "sh", "-c", setup, "sh", dir).CombinedOutput(); err != nil {
		t.Fatalf("creating subuid-owned files: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "_build", "image.iso")); err == nil {
		t.Fatal("fixture is readable by the caller, so it does not need the namespace")
	}

	run, err := CheckRunDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Remove(context.Background(), PodmanUserNS{}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory survived: %v", err)
	}
}
