package scriptstest

import (
	"errors"
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
	listed := func(f fixture) bool {
		cmd := exec.Command("git", "-C", f.repo, "worktree", "list", "--porcelain")
		cmd.Env = f.env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(out), "worktree "+f.wt+"\n")
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
		if listed(f) {
			t.Fatal("git still lists the removed worktree")
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
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("worktree-clean error = %v; want exit status 2 for a refusal:\n%s", err, out)
		}
		if !strings.Contains(out, "work") {
			t.Fatalf("message does not name the unpushed commit:\n%s", out)
		}
		if !exists(f.wt) || !branchExists(f) {
			t.Fatal("a refused removal deleted the worktree or its branch")
		}
	})

	t.Run("refuses to remove the checkout the script runs from", func(t *testing.T) {
		f := setup(t)
		git(f, f.wt, "push", "--quiet", "origin", "feature")
		copied := filepath.Join(f.wt, "scripts", "worktree-clean")
		if err := os.MkdirAll(filepath.Dir(copied), 0o755); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copied, body, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(copied, f.wt)
		cmd.Env = append(f.env, "KATL_DEVCLEAN="+devclean)

		out, err := cmd.CombinedOutput()

		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("worktree-clean error = %v; want exit status 2 for a refusal:\n%s", err, out)
		}
		if !strings.Contains(string(out), "main checkout or another worktree") {
			t.Fatalf("message does not say where to run from:\n%s", out)
		}
		if !exists(f.wt) || !listed(f) || !branchExists(f) {
			t.Fatal("a refused removal deleted the worktree, its registration or its branch")
		}
	})

	t.Run("reports a partial removal and keeps the branch", func(t *testing.T) {
		f := setup(t)
		git(f, f.wt, "push", "--quiet", "origin", "feature")
		partial := filepath.Join(t.TempDir(), "katl-devclean")
		if err := os.WriteFile(partial, []byte("#!/bin/sh\necho 'incomplete: could not remove x' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(script, f.wt)
		cmd.Env = append(f.env, "KATL_REPO_ROOT="+f.repo, "KATL_DEVCLEAN="+partial)

		out, err := cmd.CombinedOutput()

		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("worktree-clean error = %v; want exit status 1 for a partial removal:\n%s", err, out)
		}
		if !strings.Contains(string(out), "partly removed") || strings.Contains(string(out), "was not removed") {
			t.Fatalf("message does not say the worktree was partly removed:\n%s", out)
		}
		if !branchExists(f) {
			t.Fatal("the branch was deleted although the worktree is still there")
		}
	})
}

// The scripts must see katl-devclean's own exit status, which go run replaces
// with 1.
func TestWorktreeCleanKeepsDevcleanExitStatus(t *testing.T) {
	root := repoRoot(t)
	target := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "scripts", "worktree-clean"), target)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "KATL_REPO_ROOT="+root, "KATL_DEVCLEAN=")

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("worktree-clean error = %v; want exit status 2 for a refusal:\n%s", err, out)
	}
	if !strings.Contains(string(out), "was not removed") || strings.Contains(string(out), "partly removed") || strings.Contains(string(out), "exit status") {
		t.Fatalf("a refusal was reported as something else:\n%s", out)
	}
}

func TestCleanScriptsFailWhenBuildFails(t *testing.T) {
	root := repoRoot(t)
	// The scripts use an installed katl-devclean when there is one.
	var path []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "katl-devclean")); err != nil {
			path = append(path, dir)
		}
	}
	tmp := t.TempDir()
	for _, c := range []struct {
		script string
		args   []string
		env    string
	}{
		{"worktree-clean", []string{filepath.Join(tmp, "wt")}, "KATL_WORKTREE_GO"},
		{"vmtest-clean", []string{tmp}, "KATL_VMTEST_GO"},
	} {
		t.Run(c.script, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", c.script), c.args...)
			cmd.Env = append(os.Environ(), "PATH="+strings.Join(path, ":"), c.env+"=false")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("%s error = %v; want exit status 2\n%s", c.script, err, out)
			}
			if !strings.Contains(string(out), "could not build katl-devclean") {
				t.Fatalf("%s output does not say the build failed:\n%s", c.script, out)
			}
		})
	}
}
