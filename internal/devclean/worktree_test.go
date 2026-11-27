package devclean

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
	"github.com/katl-dev/katl/internal/workstationtest"
)

// fixture is a clone of a bare origin with main pushed, plus helpers to add
// linked worktrees.
type fixture struct {
	t      *testing.T
	root   string
	origin string
	repo   string
	env    []string
	lookup fakeLookup
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
	return CheckWorktree(context.Background(), f.env, &f.lookup, f.repo, path)
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
		if got.Tip != f.head(wt) {
			t.Fatalf("Tip = %s; want the commit the branch points at, %s", got.Tip, f.head(wt))
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

	t.Run("a directory under the main checkout that lost its .git", func(t *testing.T) {
		f := newFixture(t)
		nested := filepath.Join(f.repo, "nested")
		f.git(f.repo, "worktree", "add", "--quiet", "-b", "feature", nested, "main")
		if err := os.Remove(filepath.Join(nested, ".git")); err != nil {
			t.Fatal(err)
		}
		f.refused(nested, "not the top level")
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

	t.Run("a staged new file deleted from the working tree", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.git(wt, "add", "new.txt")
		if err := os.Remove(filepath.Join(wt, "new.txt")); err != nil {
			t.Fatal(err)
		}
		f.refused(wt, "new.txt")
	})

	t.Run("a staged modification deleted from the working tree", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		if err := os.WriteFile(filepath.Join(wt, "base.txt"), []byte("edited"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.git(wt, "add", "base.txt")
		if err := os.Remove(filepath.Join(wt, "base.txt")); err != nil {
			t.Fatal(err)
		}
		f.refused(wt, "base.txt")
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

// fakeLookup answers PullRequests with the same fixed result for any commit
// and records the commits asked about.
type fakeLookup struct {
	prs   []PullRequest
	err   error
	asked []string
}

func (l *fakeLookup) PullRequests(_ context.Context, commit string) ([]PullRequest, error) {
	l.asked = append(l.asked, commit)
	return l.prs, l.err
}

func (f *fixture) head(dir string) string {
	f.t.Helper()
	return strings.TrimSpace(f.git(dir, "rev-parse", "HEAD"))
}

func TestCheckWorktreeMergedPullRequest(t *testing.T) {
	t.Run("accepts a squash-merged head and deletes its branch", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.lookup.prs = []PullRequest{{Number: 616, Merged: true, HeadSHA: f.head(wt)}}

		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		if got.MergedPR != 616 || !got.DeleteBranch {
			t.Fatalf("CheckWorktree() = %+v; want PR 616 and the branch deletable", got)
		}
		if len(f.lookup.asked) != 1 || f.lookup.asked[0] != f.head(wt) {
			t.Fatalf("lookup asked about %v; want exactly HEAD", f.lookup.asked)
		}
	})

	t.Run("refuses a later commit that the merged head does not cover", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		merged := f.head(wt)
		f.commit(wt, "b.txt", "b")
		f.lookup.prs = []PullRequest{{Number: 616, Merged: true, HeadSHA: merged}}

		f.refused(wt, "change b.txt")
	})

	t.Run("refuses a pull request that is not merged", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.lookup.prs = []PullRequest{{Number: 616, Merged: false, HeadSHA: f.head(wt)}}

		f.refused(wt, "change a.txt")
	})

	t.Run("refuses when GitHub cannot be asked", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.lookup.err = errors.New("gh: not logged in")

		f.refused(wt, "gh: not logged in")
		f.refused(wt, "change a.txt")
	})

	t.Run("does not ask GitHub about a commit that is on a remote branch", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.commit(wt, "a.txt", "a")
		f.git(wt, "push", "--quiet", "origin", "feature")
		f.lookup.err = errors.New("must not be asked")

		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		if got.MergedPR != 0 || !got.DeleteBranch || len(f.lookup.asked) != 0 {
			t.Fatalf("CheckWorktree() = %+v, asked %v; want the remote-branch path with no lookup", got, f.lookup.asked)
		}
	})
}

func TestMergedBranchDispositionKeepsMovedBranch(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.commit(wt, "a.txt", "a")
	merged := f.head(wt)
	f.commit(wt, "b.txt", "b")
	g := gitRunner{environ: f.env}

	del, reason := g.mergedBranchDisposition("feature", f.head(wt), merged, 616)
	if del || !strings.Contains(reason, "#616") {
		t.Fatalf("mergedBranchDisposition() = %t, %q; want the moved branch kept, naming PR #616", del, reason)
	}
}

func TestWorktreeRemovalCanBeRetried(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	writeFile(t, filepath.Join(wt, "_build", "image.iso"))
	f.git(wt, "push", "--quiet", "origin", "feature")
	// An earlier removal that stopped part way left a tracked file missing.
	if err := os.Remove(filepath.Join(wt, "base.txt")); err != nil {
		t.Fatal(err)
	}

	checked, err := f.check(wt)
	if err != nil {
		t.Fatalf("a partly removed worktree is refused, so removal cannot be retried: %v", err)
	}
	if err := checked.Remove(context.Background(), &fakeNS{act: removeEverything}); err != nil {
		t.Fatal(err)
	}

	if exists(wt) {
		t.Fatal("worktree survived")
	}
	if list := f.git(f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Fatalf("git still lists the removed worktree:\n%s", list)
	}
}

func TestCheckWorktreeAcceptsDeletedTrackedFiles(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	if err := os.Remove(filepath.Join(wt, "base.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := f.check(wt); err != nil {
		t.Fatalf("a worktree missing only tracked files is refused: %v", err)
	}
}

func TestCheckWorktreeIgnoresStaleRegistrations(t *testing.T) {
	f := newFixture(t)
	stale := f.worktree("stale")
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	if _, err := f.check(wt); err != nil {
		t.Fatalf("a listed worktree whose directory is gone blocked removing another: %v", err)
	}
}

func TestWorktreeRemoveDeregisters(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	got, err := f.check(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Remove(context.Background(), &fakeNS{act: removeEverything}); err != nil {
		t.Fatal(err)
	}
	if deleted, why := got.DeleteBranchIfUnmoved(context.Background()); !deleted {
		t.Fatalf("the pushed branch was not deleted: %s", why)
	}

	if list := f.git(f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Fatalf("git still lists the removed worktree:\n%s", list)
	}
	if out := f.git(f.repo, "branch", "--list", "feature"); strings.TrimSpace(out) != "" {
		t.Fatalf("branch survived: %s", out)
	}
	if list := f.git(f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, "prunable") {
		t.Fatalf("a stale registration is left:\n%s", list)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "worktrees", "wt-feature")); !os.IsNotExist(err) {
		t.Fatalf("the registration directory survives: %v", err)
	}
}

func TestWorktreeKeepsBranchThatMoved(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	got, err := f.check(wt)
	if err != nil {
		t.Fatal(err)
	}
	f.commit(wt, "late.txt", "late")
	if err := got.Remove(context.Background(), &fakeNS{act: removeEverything}); err != nil {
		t.Fatal(err)
	}

	if deleted, why := got.DeleteBranchIfUnmoved(context.Background()); deleted || !strings.Contains(why, "moved") {
		t.Fatalf("a branch that moved after it was judged: deleted=%t, reason %q; want kept because it moved", deleted, why)
	}
}

func TestWorktreeCheckNotInUse(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	got, err := f.check(wt)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := f.repo

	for name, c := range map[string]struct{ script, cwd string }{
		"script inside": {filepath.Join(wt, "scripts", "worktree-clean"), elsewhere},
		"cwd inside":    {filepath.Join(elsewhere, "scripts", "worktree-clean"), filepath.Join(wt, "sub")},
		"cwd at root":   {elsewhere, wt},
	} {
		if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wt, "scripts", "worktree-clean"))
		err := got.CheckNotInUse(c.script, c.cwd)
		if err == nil || !strings.Contains(err.Error(), "main checkout or another worktree") {
			t.Fatalf("%s: CheckNotInUse() = %v; want a refusal saying where to run from", name, err)
		}
	}
	if err := got.CheckNotInUse(filepath.Join(elsewhere, "base.txt"), elsewhere); err != nil {
		t.Fatalf("a script and directory outside the worktree were refused: %v", err)
	}
}

func TestCheckWorktreeRefusesForeignGitFile(t *testing.T) {
	// copyGitFile gives target the .git file of source, as copying a
	// checkout over a removed one would.
	copyGitFile := func(t *testing.T, f *fixture, target, content string) {
		t.Helper()
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, ".git"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a .git copied from a live worktree", func(t *testing.T) {
		f := newFixture(t)
		live := f.worktree("live")
		stale := f.worktree("stale")
		f.git(live, "push", "--quiet", "origin", "live")
		gitFile, err := os.ReadFile(filepath.Join(live, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		copyGitFile(t, f, stale, string(gitFile))

		_, err = f.check(stale)
		if err == nil || !strings.Contains(err.Error(), "belongs to") {
			t.Fatalf("CheckWorktree() error = %v; want a refusal naming the registration's real owner", err)
		}

		if list := f.git(f.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, live) {
			t.Fatalf("the live worktree lost its registration:\n%s", list)
		}
		if out := f.git(live, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(out) != "live" {
			t.Fatalf("live HEAD = %q; want live", out)
		}
		if out := f.git(f.repo, "branch", "--list", "live"); strings.TrimSpace(out) == "" {
			t.Fatal("the live worktree's branch is gone")
		}
	})

	t.Run("a .git pointing into another repository", func(t *testing.T) {
		f := newFixture(t)
		other := newFixture(t)
		foreign := other.worktree("foreign")
		stale := f.worktree("stale")
		gitFile, err := os.ReadFile(filepath.Join(foreign, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		copyGitFile(t, f, stale, string(gitFile))

		_, err = f.check(stale)
		if err == nil || !strings.Contains(err.Error(), "belongs to the repository") {
			t.Fatalf("CheckWorktree() error = %v; want a refusal naming the other repository", err)
		}
		if list := other.git(other.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, foreign) {
			t.Fatalf("the other repository's worktree lost its registration:\n%s", list)
		}
	})
}

func TestWorktreeKeepsBranchCheckedOutElsewhere(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	got, err := f.check(wt)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.root, "other")
	f.git(f.repo, "worktree", "add", "--quiet", "--force", other, "feature")
	if err := got.Remove(context.Background(), &fakeNS{act: removeEverything}); err != nil {
		t.Fatal(err)
	}

	deleted, why := got.DeleteBranchIfUnmoved(context.Background())
	if deleted || !strings.Contains(why, "checked out in "+other) {
		t.Fatalf("DeleteBranchIfUnmoved() = %t, %q; want the branch kept because it is checked out in %s", deleted, why, other)
	}
	if out := f.git(f.repo, "branch", "--list", "feature"); strings.TrimSpace(out) == "" {
		t.Fatal("a branch checked out elsewhere was deleted")
	}
}

func TestCheckWorktreeRefusesItsOwnCheckout(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")

	_, err := CheckWorktree(context.Background(), f.env, &f.lookup, wt, wt)
	if err == nil || !strings.Contains(err.Error(), "main checkout or another worktree") {
		t.Fatalf("CheckWorktree() error = %v; want a refusal to remove the checkout it runs from", err)
	}
}

func TestWorktreeRemovesSubuidOwnedOutput(t *testing.T) {
	requireRootlessPodman(t)
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	build := filepath.Join(wt, "_build", "mkosi")
	script := `mkdir -p "$1/sub" && touch "$1/sub/file" && chown 1:1 "$1/sub" "$1/sub/file" && chmod 0500 "$1/sub"`
	if out, err := exec.Command("podman", "unshare", "sh", "-c", script, "sh", build).CombinedOutput(); err != nil {
		t.Skipf("cannot create subuid-owned output: %v: %s", err, out)
	}
	got, err := f.check(wt)
	if err != nil {
		t.Fatal(err)
	}

	if err := got.Remove(context.Background(), PodmanUserNS{}); err != nil {
		t.Fatal(err)
	}

	if exists(wt) {
		t.Fatal("worktree survived")
	}
	if list := f.git(f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, wt) {
		t.Fatalf("git still lists the removed worktree:\n%s", list)
	}
}

func TestCheckWorktreeRefusesLocked(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")
	f.git(f.repo, "worktree", "lock", wt)
	writeFile(t, filepath.Join(wt, "_build", "image.iso"))

	f.refused(wt, "git worktree unlock "+wt)

	if !exists(filepath.Join(wt, "_build", "image.iso")) {
		t.Fatal("a refused worktree lost its untracked output")
	}
}

func TestCheckWorktreeRefusesRegistrationOutsideWorktrees(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("feature")
	f.git(wt, "push", "--quiet", "origin", "feature")

	// A copy of the registration at a path that is not under
	// <common>/worktrees, still naming the repository and the worktree.
	real := filepath.Join(f.repo, ".git", "worktrees", "wt-feature")
	admin := filepath.Join(f.root, "admin")
	if err := os.Mkdir(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HEAD", "gitdir", "index"} {
		b, err := os.ReadFile(filepath.Join(real, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(admin, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(filepath.Join(f.repo, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.refused(wt, "not a worktree registration")
}

// failingGit puts a git first in PATH that runs the real one, except that
// `worktree remove` runs on and then fails.
func failingGit(t *testing.T, onRemove string) {
	t.Helper()
	real := workstationtest.Tool(t, "git")
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a; do last=$a; done\ncase \" $* \" in\n*\" worktree remove \"*) " + onRemove + "; exit 1;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWorktreeRemoveReportsGitFailure(t *testing.T) {
	t.Run("files left after the registration is gone", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.git(wt, "push", "--quiet", "origin", "feature")
		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		// Real git fails to remove a worktree it no longer lists.
		if err := os.RemoveAll(filepath.Join(f.repo, ".git", "worktrees", "wt-feature")); err != nil {
			t.Fatal(err)
		}

		err = got.Remove(context.Background(), &fakeNS{act: removeEverything})

		var left *LeftoverError
		if err == nil || errors.As(err, &left) || !strings.Contains(err.Error(), "delete "+wt+" yourself") {
			t.Fatalf("Remove() error = %v; want a manual next step, not a request to run again", err)
		}
	})

	t.Run("directory gone but still registered", func(t *testing.T) {
		f := newFixture(t)
		wt := f.worktree("feature")
		f.git(wt, "push", "--quiet", "origin", "feature")
		got, err := f.check(wt)
		if err != nil {
			t.Fatal(err)
		}
		failingGit(t, `mv "$last" "$last.moved"`)

		err = got.Remove(context.Background(), &fakeNS{act: removeEverything})

		if err == nil || !strings.Contains(err.Error(), "worktree prune") {
			t.Fatalf("Remove() error = %v; want it to say the registration remains and how to prune it", err)
		}
	})
}
