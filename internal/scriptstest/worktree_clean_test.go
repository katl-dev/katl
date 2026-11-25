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
	devclean := buildCommand(t, t.TempDir(), "katl-devclean")

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

// pathWithoutDevclean is PATH without the directories holding an installed
// katl-devclean, which the scripts would use instead of building one.
func pathWithoutDevclean() string {
	var path []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "katl-devclean")); err != nil {
			path = append(path, dir)
		}
	}
	return strings.Join(path, string(os.PathListSeparator))
}

func TestCleanScriptsFailWhenBuildFails(t *testing.T) {
	root := repoRoot(t)
	path := pathWithoutDevclean()
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
			cmd.Env = append(os.Environ(), "PATH="+path, c.env+"=false")
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

// Go recognises a repository only by a .git directory, so in a linked
// worktree, whose .git is a file, it stamps builds from whatever .git
// directory lies above the checkout. The scripts must build katl-devclean
// regardless of what that directory holds; here it is not a repository at
// all, which makes git exit 128.
func TestCleanScriptsBuildBelowUnrelatedGitDir(t *testing.T) {
	root := repoRoot(t)
	path := pathWithoutDevclean()

	enclosing := t.TempDir()
	if err := os.Mkdir(filepath.Join(enclosing, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(enclosing, "checkout")
	for name, content := range map[string]string{
		"go.mod": "module example.invalid/katl\n\ngo 1.21\n",
		// The stub records that it ran and refuses, so the scripts stop there.
		"cmd/katl-devclean/main.go": `package main

import (
	"os"
	"strings"
)

func main() {
	_ = os.WriteFile(os.Getenv("KATL_TEST_DEVCLEAN_LOG"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
	os.Exit(2)
}
`,
	} {
		file := filepath.Join(checkout, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		script  string
		command string
	}{
		{"worktree-clean", "remove worktree"},
		{"vmtest-clean", "run-dir"},
	} {
		t.Run(c.script, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "devclean.log")
			cmd := exec.Command(filepath.Join(root, "scripts", c.script), t.TempDir())
			cmd.Env = append(os.Environ(),
				"PATH="+path,
				"KATL_REPO_ROOT="+checkout,
				"KATL_DEVCLEAN=",
				"KATL_TEST_DEVCLEAN_LOG="+log,
				"GIT_CEILING_DIRECTORIES="+filepath.Dir(enclosing),
				"GOFLAGS=",
				"GOWORK=off",
			)

			out, _ := cmd.CombinedOutput()

			ran, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("%s did not run the katl-devclean it built: %v\n%s", c.script, err, out)
			}
			if !strings.HasPrefix(string(ran), c.command+" ") {
				t.Fatalf("katl-devclean ran with %q, want %s", ran, c.command)
			}
		})
	}
}
