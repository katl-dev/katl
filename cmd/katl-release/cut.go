package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
)

const cutWorkflow = ".github/workflows/cut-release.yml"

func cut(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader, call command) error {
	if len(args) != 3 || args[2] == "" {
		return fmt.Errorf("usage: scripts/katl-release %s VERSION REVISION", args[0])
	}
	version, err := releaseversion.Parse(args[1])
	if err != nil || !version.IsFinal() {
		return fmt.Errorf("version %q must be a complete final version in YYYY.M.PATCH form, such as 2026.12.1", args[1])
	}

	commit := args[2]
	inWorkflow := args[0] == "cut-branch"
	if !fullSHA.MatchString(commit) {
		if inWorkflow {
			return errors.New("REVISION must be the full source commit SHA")
		}
		commit, err = resolveCommit(commit, func(endpoint string, value any) error {
			return r.decode(ctx, r.repo(endpoint), value)
		})
		if err != nil {
			return fmt.Errorf("resolve %q on GitHub. Choose a commit from main: %w", args[2], err)
		}
	}
	fmt.Fprintf(stdout, "Source: %s\n", commit)
	already, err := checkCut(ctx, r, version, commit)
	if err != nil {
		return err
	}
	branch := releasestate.Branch(version)
	if already {
		fmt.Fprintf(stdout, "%s is already cut at %s.\n", branch, commit)
		return nil
	}

	if inWorkflow {
		if err := createReleaseBranch(ctx, r, call, version, commit); err != nil {
			return err
		}
	} else {
		if err := dispatchCut(ctx, r, call, version, commit, stdout, stderr); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "%s exists at %s.\n", branch, commit)
	if !inWorkflow {
		fmt.Fprintf(stdout, "Next: scripts/katl-release release %s\n", version)
	}
	return nil
}

// checkCut returns whether the branch already exists at the selected commit,
// or an error when this version and commit cannot be used for a cut.
func checkCut(ctx context.Context, r *reader, version releaseversion.Version, commit string) (bool, error) {
	landed, err := r.isAncestor(ctx, commit, "main")
	if notFound(err) {
		return false, fmt.Errorf("commit %s could not be found on GitHub. Choose a commit from main: %w", commit, err)
	}
	if err != nil {
		return false, err
	}
	if !landed {
		return false, fmt.Errorf("%s is not reachable from main. Choose a commit from main", commit)
	}

	tags, err := r.refs(ctx, "tags/"+version.Tag())
	if err != nil {
		return false, err
	}
	for _, ref := range tags {
		tag := strings.TrimPrefix(ref.Ref, "refs/tags/")
		if releasestate.IsVersionTag(version, tag) {
			return false, fmt.Errorf("tag %s already exists for %s. Choose the next version", tag, version)
		}
	}
	return cutBranchExists(ctx, r, version, commit)
}

func cutBranchExists(ctx context.Context, r *reader, version releaseversion.Version, commit string) (bool, error) {
	head, err := r.refCommit(ctx, releasestate.Ref(version))
	if err != nil || head == "" {
		return false, err
	}
	if head != commit {
		return false, fmt.Errorf("%s exists at %s, but the requested commit is %s. Choose the next version",
			releasestate.Branch(version), head, commit)
	}
	return true, nil
}

func createReleaseBranch(ctx context.Context, r *reader, call command, version releaseversion.Version, commit string) error {
	endpoint := r.repo("git/refs")
	_, writeErr := call("gh", "api", "--method", "POST", endpoint, "-f", "ref="+releasestate.Ref(version), "-f", "sha="+commit)
	// A concurrent cut or a lost response can leave the ref in place. Check
	// the actual ref after the single create request, including on error.
	exists, err := cutBranchExists(ctx, r, version, commit)
	if !exists && writeErr != nil {
		writeErr = ghFailure("create "+releasestate.Ref(version), writeErr, stderrOf(writeErr))
		return errors.Join(writeErr, err)
	}
	if err == nil && !exists {
		return fmt.Errorf("GitHub did not return %s after creating it. Inspect the run before retrying", releasestate.Ref(version))
	}
	return err
}

func dispatchCut(ctx context.Context, r *reader, call command, version releaseversion.Version, commit string, stdout, stderr io.Writer) error {
	target := workflowDispatch{name: "Cut release", file: cutWorkflow, ref: "main",
		title: fmt.Sprintf("Cut release %s at %s", version, commit)}
	inputs := []string{"-f", "version=" + version.String(), "-f", "revision=" + commit}
	selected, err := dispatchWorkflow(ctx, r, call, target, inputs, "Check the Cut release runs in GitHub Actions before dispatching again.")
	if err != nil {
		return err
	}
	run, jobs, err := followWorkflow(ctx, r, target.name, "", selected, stdout, stderr)
	if err != nil {
		return fmt.Errorf("follow Cut release run %s: %w", selected.HTMLURL, err)
	}
	if run.Conclusion != "success" {
		return workflowFailure(target.name, run, jobs)
	}

	exists, err := cutBranchExists(ctx, r, version, commit)
	if err != nil {
		return fmt.Errorf("Cut release run %s succeeded, but %w", run.HTMLURL, err)
	}
	if !exists {
		return fmt.Errorf("Cut release run %s succeeded, but %s is missing. Inspect the run before retrying",
			run.HTMLURL, releasestate.Branch(version))
	}
	return nil
}
