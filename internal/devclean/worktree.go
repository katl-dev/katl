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
	// MergedPR is the number of the merged pull request that stands in for a
	// remote branch because HEAD is its head commit, or zero when HEAD is on a
	// remote branch.
	MergedPR int
}

// CheckWorktree returns the linked worktree of the repository containing repo
// at path, or an error naming what stops it being removed: it is not a linked
// worktree, it has tracked modifications or staged changes, or its HEAD
// commit is on no remote branch and is not the head of a merged pull request.
// A squash merge leaves the pull request's commits on no remote branch, so
// lookup supplies that second evidence; when it fails, HEAD is not treated as
// safe. It fetches origin first, because pushes made to a URL do not update
// remote-tracking branches.
//
// environ is the environment git runs in.
func CheckWorktree(ctx context.Context, environ []string, lookup PullRequestLookup, repo, path string) (Worktree, error) {
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
	wt := Worktree{Path: abs}
	var head string
	if unpushed = strings.TrimRight(unpushed, "\n"); unpushed != "" {
		out, err := g.run(ctx, abs, "rev-parse", "HEAD")
		if err != nil {
			return Worktree{}, err
		}
		head = strings.TrimSpace(out)
		pr, merged, err := mergedHead(ctx, lookup, head)
		switch {
		case err != nil:
			return Worktree{}, fmt.Errorf("%s has commits on no remote branch and GitHub could not confirm they were merged (%v); push them, or retry when GitHub is reachable:\n%s", abs, err, unpushed)
		case !merged:
			return Worktree{}, fmt.Errorf("%s has commits on no remote branch, and %s is not the head of a merged pull request; push them first:\n%s", abs, head, unpushed)
		}
		wt.MergedPR = pr.Number
	}

	branch, err := g.run(ctx, abs, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		// A detached HEAD has no branch to keep or delete.
		return wt, nil
	}
	wt.Branch = strings.TrimSpace(branch)
	if wt.MergedPR != 0 {
		wt.DeleteBranch, wt.KeepReason = g.mergedBranchDisposition(ctx, repo, wt.Branch, head, wt.MergedPR)
	} else {
		wt.DeleteBranch, wt.KeepReason = g.branchDisposition(ctx, repo, wt.Branch)
	}
	return wt, nil
}

// mergedBranchDisposition reports whether branch can be deleted because its
// tip is exactly the head commit of merged pull request pr.
func (g gitRunner) mergedBranchDisposition(ctx context.Context, repo, branch, prHead string, pr int) (bool, string) {
	tip, err := g.run(ctx, repo, "rev-parse", "refs/heads/"+branch)
	if err == nil && strings.TrimSpace(tip) == prHead {
		return true, ""
	}
	return false, fmt.Sprintf("branch %s is not at the head of merged pull request #%d", branch, pr)
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
