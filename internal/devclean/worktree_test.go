package devclean

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

// fixture is a clone of a bare origin with main pushed, plus helpers to add
// linked worktrees.
type fixture struct {
	t      *testing.T
	root   string
	origin string
	repo   string
	env    []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	f.env = gittest.Env(os.Environ(), f.repo)

	f.git(root, "init", "--quiet", "--bare", "--initial-branch=main", f.origin)
	f.git(root, "init", "--quiet", "--initial-branch=main", f.repo)
	f.git(f.repo, "remote", "add", "origin", f.origin)
	f.commit(f.repo, "base.txt", "base")
	f.git(f.repo, "push", "--quiet", "origin", "main")
	f.git(f.repo, "fetch", "--quiet", "origin")
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *fixture) commit(dir, file, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(dir, "add", file)
	f.git(dir, "commit", "--quiet", "-m", "change "+file)
}

// worktree adds a linked worktree on a new branch cut from main.
func (f *fixture) worktree(branch string) string {
	f.t.Helper()
	path := filepath.Join(f.root, "wt-"+branch)
	f.git(f.repo, "worktree", "add", "--quiet", "-b", branch, path, "main")
	return path
}

func (f *fixture) check(path string) (Worktree, error) {
	f.t.Helper()
	return CheckWorktree(context.Background(), f.env, f.repo, path)
}

func (f *fixture) refused(path, want string) {
	f.t.Helper()
	_, err := f.check(path)
	if err == nil || !strings.Contains(err.Error(), want) {
		f.t.Fatalf("CheckWorktree(%s) error = %v; want it to contain %q", path, err, want)
	}
}

func TestCheckWorktreeAccepts(t *testing.T) {
	t.Run("a worktree whose commits are pushed to its remote branch", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.git(wt, "push", "--quiet", "origin", "feature")

		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != wt || got.Branch != "feature" || !got.DeleteBranch {
			t.Fatalf("CheckWorktree() = %+v; want feature branch deletable", got)
		}
	})

	t.Run("a push made to a URL that leaves the tracking branch stale", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.git(wt, "push", "--quiet", f.origin, "HEAD:refs/heads/feature")

		got, err := f.check(wt)
		if err != nil {
			t.Fatalf("a pushed worktree was refused: %v", err)
		}
		if !got.DeleteBranch {
			t.Fatalf("DeleteBranch = false (%s); want true", got.KeepReason)
		}
	})

	t.Run("untracked build output does not block removal", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		if err := os.MkdirAll(filepath.Join(wt, "_build"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "_build", "out"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := f.check(wt); err != nil {
			t.Fatalf("untracked output blocked removal: %v", err)
		}
	})

	t.Run("a branch merged into origin/main is deletable without a remote branch", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.git(wt, "push", "--quiet", "origin", "feature:main")

		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		if !got.DeleteBranch {
			t.Fatalf("DeleteBranch = false (%s); want true", got.KeepReason)
		}
	})

	t.Run("a detached HEAD has no branch", func(t *testing.T) {
		f := newFixture(t)
		wt := filepath.Join(f.root, "wt-detached")
		f.git(f.repo, "worktree", "add", "--quiet", "--detach", wt, "main")

		got, err := f.check(wt)
		if err != nil || got.Branch != "" || got.DeleteBranch {
			t.Fatalf("CheckWorktree() = %+v, %v; want no branch", got, err)
		}
	})
}

func TestCheckWorktreeKeepsBranchNotOnItsRemote(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.commit(wt, "a.txt", "a")
	// The commit is on a remote branch, but not on one named like the local branch.
	f.git(wt, "push", "--quiet", "origin", "HEAD:refs/heads/other")

	got, err := f.check(wt)
	if err != nil {
		t.Fatalf("a pushed worktree was refused: %v", err)
	}
	if got.DeleteBranch || !strings.Contains(got.KeepReason, "feature") {
		t.Fatalf("CheckWorktree() = %+v; want the branch kept with a reason naming it", got)
	}
}

func TestCheckWorktreeRefuses(t *testing.T) {
	t.Run("the main worktree", func(t *testing.T) {
		f := newFixture(t)
		f.refused(f.repo, "main worktree")
	})

	t.Run("a directory that is not a worktree", func(t *testing.T) {
		f := newFixture(t)
		other := filepath.Join(f.root, "other")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		f.refused(other, "not a linked worktree")
	})

	t.Run("a worktree of a different repository", func(t *testing.T) {
		f := newFixture(t)
		stranger := filepath.Join(f.root, "stranger")
		f.git(f.root, "init", "--quiet", stranger)
		f.refused(stranger, "not a linked worktree")
	})

	t.Run("a subdirectory of a worktree", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		sub := filepath.Join(wt, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		f.refused(sub, "not a linked worktree")
	})

	t.Run("a tracked modification", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		if err := os.WriteFile(filepath.Join(wt, "base.txt"), []byte("edited"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.refused(wt, "base.txt")
	})

	t.Run("a staged new file", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.git(wt, "add", "new.txt")
		f.refused(wt, "new.txt")
	})

	t.Run("an unpushed commit, naming it", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.refused(wt, "change a.txt")
	})

	t.Run("a commit pushed only after the branch moved on", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.git(wt, "push", "--quiet", "origin", "feature")
		f.commit(wt, "b.txt", "b")
		f.refused(wt, "change b.txt")
	})

	t.Run("a missing origin", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.git(f.repo, "remote", "remove", "origin")
		f.refused(wt, "fetching origin failed")
	})
}
