package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestWorktreeClean(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "worktree-clean")
	devclean := filepath.Join(t.TempDir(), "katl-devclean")
	build := exec.Command("go", "build", "-o", devclean, "./cmd/katl-devclean")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building katl-devclean: %v\n%s", err, out)
	}

	type fixture struct {
		repo, wt string
		env      []string
	}
	git := func(f fixture, dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = f.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// setup returns a repository with a linked worktree on branch "feature"
	// holding one commit that is not pushed.
	setup := func(t *testing.T) fixture {
		t.Helper()
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		f := fixture{repo: filepath.Join(base, "repo"), wt: filepath.Join(base, "wt")}
		f.env = gittest.Env(os.Environ(), f.repo)
		git(f, base, "init", "--quiet", "--bare", "--initial-branch=main", "origin.git")
		git(f, base, "init", "--quiet", "--initial-branch=main", f.repo)
		git(f, f.repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
		git(f, f.repo, "commit", "--quiet", "--allow-empty", "-m", "base")
		git(f, f.repo, "push", "--quiet", "origin", "main")
		git(f, f.repo, "worktree", "add", "--quiet", "-b", "feature", f.wt, "main")
		git(f, f.wt, "commit", "--quiet", "--allow-empty", "-m", "work")
		return f
	}
	run := func(f fixture) (string, error) {
		cmd := exec.Command(script, f.wt)
		cmd.Env = append(f.env, "KATL_REPO_ROOT="+f.repo, "KATL_DEVCLEAN="+devclean)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	exists := func(path string) bool {
		_, err := os.Lstat(path)
		return err == nil
	}
	branchExists := func(f fixture) bool {
		cmd := exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature")
		cmd.Env = f.env
		return cmd.Run() == nil
	}

	t.Run("removes a pushed worktree with its build output and branch", func(t *testing.T) {
		f := setup(t)
		git(f, f.wt, "push", "--quiet", "origin", "feature")
		readonly := filepath.Join(f.wt, "_build", "tree")
		if err := os.MkdirAll(readonly, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(readonly, "f"), []byte("x"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(readonly, 0o500); err != nil {
			t.Fatal(err)
		}

		if out, err := run(f); err != nil {
			t.Fatalf("worktree-clean failed: %v\n%s", err, out)
		}
		if exists(f.wt) {
			t.Fatal("worktree directory survived")
		}
		if branchExists(f) {
			t.Fatal("pushed branch was kept")
		}
	})

	t.Run("keeps a branch that is on a remote under another name", func(t *testing.T) {
		f := setup(t)
		git(f, f.wt, "push", "--quiet", "origin", "HEAD:refs/heads/other")

		out, err := run(f)
		if err != nil {
			t.Fatalf("worktree-clean failed: %v\n%s", err, out)
		}
		if exists(f.wt) {
			t.Fatal("worktree directory survived")
		}
		if !branchExists(f) || !strings.Contains(out, "kept branch feature") {
			t.Fatalf("branch should be kept with a note:\n%s", out)
		}
	})

	t.Run("refuses an unpushed commit and leaves everything", func(t *testing.T) {
		f := setup(t)

		out, err := run(f)
		if err == nil {
			t.Fatalf("worktree-clean succeeded:\n%s", out)
		}
		if !strings.Contains(out, "work") {
			t.Fatalf("message does not name the unpushed commit:\n%s", out)
		}
		if !exists(f.wt) || !branchExists(f) {
			t.Fatal("a refused removal deleted the worktree or its branch")
		}
	})
}
