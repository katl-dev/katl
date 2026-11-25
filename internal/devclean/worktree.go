package devclean

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	// Tip is the commit Branch pointed at when it was judged deletable.
	Tip string

	// root is the path that was verified; Remove deletes nothing else.
	root     string
	verified bool
	// untracked names the paths under root that git does not track, ignored
	// ones included, which build output owned by subuids can block git from
	// deleting.
	untracked []string
	// repo is the checkout git is run from to remove the worktree.
	repo string
	// environ is the environment git runs in.
	environ []string
	// commonDir is the repository's git directory, derived from the
	// repository CheckWorktree was given and never from the worktree, whose
	// own .git is not trusted to name it.
	commonDir string
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
			if errors.Is(err, fs.ErrNotExist) {
				// A stale registration cannot be the target, which exists.
				continue
			}
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
	if top, err := g.run(ctx, repo, "rev-parse", "--show-toplevel"); err == nil {
		if own, err := resolve(strings.TrimSpace(top)); err == nil && own == abs {
			return Worktree{}, fmt.Errorf("%s is the checkout git would run the removal from; run worktree-clean from the main checkout or another worktree", abs)
		}
	}
	if !contains(paths[1:], abs) {
		return Worktree{}, fmt.Errorf("%s is not a linked worktree of %s; see git worktree list", abs, paths[0])
	}

	// A directory without its own .git is judged by whichever repository
	// encloses it, so git's answers are trusted only for a worktree root.
	top, err := g.run(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return Worktree{}, err
	}
	if top, err = resolve(strings.TrimSpace(top)); err != nil || top != abs {
		return Worktree{}, fmt.Errorf("%s is not the top level of its own git worktree (git reports %s); it may be half removed, so remove it yourself", abs, top)
	}

	if _, err := g.run(ctx, repo, "fetch", "origin"); err != nil {
		return Worktree{}, fmt.Errorf("cannot confirm %s is pushed because fetching origin failed: %w", abs, err)
	}
	unpushed, err := g.run(ctx, abs, "log", "--oneline", "HEAD", "--not", "--remotes")
	if err != nil {
		return Worktree{}, err
	}
	wt := Worktree{Path: abs, root: abs, repo: repo, verified: true, environ: environ}
	repoCommon, err := g.run(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Worktree{}, err
	}
	if wt.commonDir, err = resolve(strings.TrimSpace(repoCommon)); err != nil {
		return Worktree{}, err
	}
	if err := g.bindRegistration(ctx, abs, wt.commonDir); err != nil {
		return Worktree{}, err
	}
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

	if branch, err := g.run(ctx, abs, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		wt.Branch = strings.TrimSpace(branch)
		// The decision is made about one commit, so deleting the branch can
		// be conditional on it still being there.
		tip, err := g.run(ctx, repo, "rev-parse", "refs/heads/"+wt.Branch)
		if err != nil {
			return Worktree{}, err
		}
		tip = strings.TrimSpace(tip)
		if wt.MergedPR != 0 {
			wt.DeleteBranch, wt.KeepReason = g.mergedBranchDisposition(wt.Branch, tip, head, wt.MergedPR)
		} else {
			wt.DeleteBranch, wt.KeepReason = g.branchDisposition(ctx, repo, wt.Branch, tip)
		}
		if wt.DeleteBranch {
			wt.Tip = tip
		}
	}

	// Checked last, so that work added while the checks above ran, which
	// includes a fetch and a GitHub lookup, is not deleted.
	status, err := g.run(ctx, abs, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return Worktree{}, err
	}
	// A tracked file that is only missing from the working tree loses nothing,
	// because HEAD holds it. It is what an earlier removal that stopped part
	// way leaves behind, and refusing it would make that removal unrepeatable.
	var dirty []string
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, " D ") {
			dirty = append(dirty, line)
		}
	}
	if len(dirty) > 0 {
		return Worktree{}, fmt.Errorf("%s has tracked modifications or staged changes; commit and push them or discard them yourself:\n%s", abs, strings.Join(dirty, "\n"))
	}

	files, err := g.run(ctx, abs, "ls-files", "-z", "--others", "--directory")
	if err != nil {
		return Worktree{}, err
	}
	for _, file := range strings.Split(files, "\x00") {
		if file != "" {
			wt.untracked = append(wt.untracked, filepath.Join(abs, file))
		}
	}
	return wt, nil
}

// bindRegistration checks that the .git of the worktree at abs belongs to
// abs: it must name the repository's own git directory, and the worktree
// directory it names must point back at abs. A .git copied from another
// checkout, or aimed into another repository, fails one of these, and
// anything read through it, such as HEAD, would describe somebody else's
// worktree.
func (g gitRunner) bindRegistration(ctx context.Context, abs, commonDir string) error {
	own, err := g.run(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if own, err = resolve(strings.TrimSpace(own)); err != nil || own != commonDir {
		return fmt.Errorf("%s: its .git belongs to the repository %s, not %s; remove it yourself", abs, own, commonDir)
	}
	adminDir, err := g.run(ctx, abs, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	adminDir = strings.TrimSpace(adminDir)
	back, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return fmt.Errorf("%s: cannot read the registration %s points to: %w", abs, adminDir, err)
	}
	owner, err := resolve(filepath.Dir(strings.TrimSpace(string(back))))
	if err != nil || owner != abs {
		return fmt.Errorf("%s: its .git points to the worktree registration %s, which belongs to %s; it may be a copy of another worktree's .git, so remove it yourself", abs, adminDir, owner)
	}
	return nil
}

// mergedBranchDisposition reports whether branch can be deleted because its
// tip is exactly the head commit of merged pull request pr.
func (gitRunner) mergedBranchDisposition(branch, tip, prHead string, pr int) (bool, string) {
	if tip == prHead {
		return true, ""
	}
	return false, fmt.Sprintf("branch %s is not at the head of merged pull request #%d", branch, pr)
}

// branchDisposition reports whether branch can be deleted without losing a
// commit that is on neither origin/main nor the same-named remote branch.
func (g gitRunner) branchDisposition(ctx context.Context, repo, branch, tip string) (bool, string) {
	if g.isAncestor(ctx, repo, tip, "refs/remotes/origin/main") {
		return true, ""
	}
	remotes, err := g.run(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch)
	if err == nil {
		for _, remote := range strings.Fields(remotes) {
			if g.isAncestor(ctx, repo, tip, remote) {
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

// Remove deletes the verified worktree. Build output that the calling user
// cannot delete is cleared first, through ns, under the same mount check as
// any removal; git then deletes the worktree and its registration itself, from
// the repository it was given and not from anything the worktree's .git says.
// It succeeds only when the directory is gone.
func (w Worktree) Remove(ctx context.Context, ns UserNS) error {
	if !w.verified || w.Path != w.root || w.commonDir == "" {
		return errors.New("refusing to remove a worktree that CheckWorktree did not verify")
	}
	t := newTree(w.root, ".git")
	ns, err := t.checkNoMounts(ctx, ns)
	if err != nil {
		return err
	}
	var cause error
	for _, path := range w.untracked {
		if !strings.HasPrefix(path, w.root+"/") {
			return fmt.Errorf("refusing to remove %s: it is outside %s", path, w.root)
		}
		t.removePath(path)
		if present(path) {
			if err := ns.RemoveAll(ctx, path); err != nil && cause == nil {
				cause = err
			}
		}
	}
	for _, path := range w.untracked {
		if present(path) {
			return &LeftoverError{Root: w.root, Paths: t.leftovers(w.root), Cause: cause}
		}
	}

	if _, err := (gitRunner{environ: w.environ}).run(ctx, w.repo, "worktree", "remove", "--force", w.root); err != nil && present(w.root) {
		return &LeftoverError{Root: w.root, Paths: t.leftovers(w.root), Cause: err}
	}
	return nil
}

// DeleteBranchIfUnmoved deletes the worktree's branch when it was judged
// deletable and still points at the commit that was judged. It reports
// whether it did and, when it did not, why. Call it after Remove, so the
// removed worktree no longer holds the branch.
func (w Worktree) DeleteBranchIfUnmoved(ctx context.Context) (bool, string) {
	if !w.DeleteBranch || w.Branch == "" {
		return false, ""
	}
	g := gitRunner{environ: w.environ}
	gitDir := "--git-dir=" + w.commonDir
	listing, err := g.run(ctx, w.commonDir, gitDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Sprintf("kept branch %s: cannot tell whether it is checked out: %v", w.Branch, err)
	}
	var path string
	for _, line := range strings.Split(listing, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
		}
		if line == "branch refs/heads/"+w.Branch {
			return false, fmt.Sprintf("kept branch %s: checked out in %s", w.Branch, path)
		}
	}
	ref := "refs/heads/" + w.Branch
	if _, err := g.run(ctx, w.commonDir, gitDir, "update-ref", "-d", ref, w.Tip); err != nil {
		current, showErr := g.run(ctx, w.commonDir, gitDir, "rev-parse", "--verify", "--quiet", ref)
		if showErr == nil && strings.TrimSpace(current) != w.Tip {
			return false, fmt.Sprintf("kept branch %s: it moved after it was checked", w.Branch)
		}
		return false, fmt.Sprintf("kept branch %s: could not delete it: %v", w.Branch, err)
	}
	return true, ""
}

// CheckNotInUse refuses removal when the worktree contains the running script
// or the caller's working directory, which removal would pull out from under
// the caller.
func (w Worktree) CheckNotInUse(script, cwd string) error {
	for _, p := range []string{script, cwd} {
		resolved, err := resolve(p)
		if err != nil {
			continue
		}
		if resolved == w.root || strings.HasPrefix(resolved, w.root+"/") {
			return fmt.Errorf("%s contains %s, which is in use; run worktree-clean from the main checkout or another worktree", w.root, resolved)
		}
	}
	return nil
}
