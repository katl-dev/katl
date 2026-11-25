package devclean

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree is a linked git worktree that may be removed.
type Worktree struct {
	// Path is the worktree's absolute path.
	Path string
	// Branch is the checked-out local branch, empty for a detached HEAD.
	Branch string
	// DeleteBranch reports whether Branch holds nothing that is not already
	// merged into origin/main or contained in its remote branch.
	DeleteBranch bool
	// KeepReason explains why Branch is kept when DeleteBranch is false.
	KeepReason string
}

// CheckWorktree returns the linked worktree of the repository containing repo
// at path, or an error naming what stops it being removed: it is not a linked
// worktree, it has tracked modifications or staged changes, or its HEAD
// commit is on no remote branch. It fetches origin first, because pushes made
// to a URL do not update remote-tracking branches.
//
// environ is the environment git runs in.
func CheckWorktree(ctx context.Context, environ []string, repo, path string) (Worktree, error) {
	g := gitRunner{environ: environ}

	abs, err := resolve(path)
	if err != nil {
		return Worktree{}, err
	}
	listing, err := g.run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return Worktree{}, err
	}
	var paths []string
	for _, line := range strings.Split(listing, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			resolved, err := resolve(p)
			if err != nil {
				return Worktree{}, err
			}
			paths = append(paths, resolved)
		}
	}
	if len(paths) == 0 {
		return Worktree{}, errors.New("git lists no worktrees")
	}
	if abs == paths[0] {
		return Worktree{}, fmt.Errorf("%s is the main worktree and is never removed", abs)
	}
	if !contains(paths[1:], abs) {
		return Worktree{}, fmt.Errorf("%s is not a linked worktree of %s; see git worktree list", abs, paths[0])
	}

	dirty, err := g.run(ctx, abs, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return Worktree{}, err
	}
	if dirty = strings.TrimRight(dirty, "\n"); dirty != "" {
		return Worktree{}, fmt.Errorf("%s has tracked modifications or staged changes; commit and push them or discard them yourself:\n%s", abs, dirty)
	}

	if _, err := g.run(ctx, repo, "fetch", "origin"); err != nil {
		return Worktree{}, fmt.Errorf("cannot confirm %s is pushed because fetching origin failed: %w", abs, err)
	}
	unpushed, err := g.run(ctx, abs, "log", "--oneline", "HEAD", "--not", "--remotes")
	if err != nil {
		return Worktree{}, err
	}
	if unpushed = strings.TrimRight(unpushed, "\n"); unpushed != "" {
		return Worktree{}, fmt.Errorf("%s has commits on no remote branch; push them first:\n%s", abs, unpushed)
	}

	wt := Worktree{Path: abs}
	branch, err := g.run(ctx, abs, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		// A detached HEAD has no branch to keep or delete.
		return wt, nil
	}
	wt.Branch = strings.TrimSpace(branch)
	wt.DeleteBranch, wt.KeepReason = g.branchDisposition(ctx, repo, wt.Branch)
	return wt, nil
}

// branchDisposition reports whether branch can be deleted without losing a
// commit that is on neither origin/main nor the same-named remote branch.
func (g gitRunner) branchDisposition(ctx context.Context, repo, branch string) (bool, string) {
	ref := "refs/heads/" + branch
	if g.isAncestor(ctx, repo, ref, "refs/remotes/origin/main") {
		return true, ""
	}
	remotes, err := g.run(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch)
	if err == nil {
		for _, remote := range strings.Fields(remotes) {
			if g.isAncestor(ctx, repo, ref, remote) {
				return true, ""
			}
		}
	}
	return false, fmt.Sprintf("branch %s is neither merged into origin/main nor contained in a remote branch of the same name", branch)
}

type gitRunner struct {
	environ []string
}

func (g gitRunner) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = g.environ
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

func (g gitRunner) isAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	_, err := g.run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// resolve returns path as an absolute path with symbolic links resolved, so
// that paths compare equal however they were spelled.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return resolved, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
