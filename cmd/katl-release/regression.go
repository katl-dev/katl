package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const (
	ciWorkflow      = "ci.yml"
	releaseWorkflow = "release-artifacts.yml"
)
const ciWorkflowPath = ".github/workflows/ci.yml"

var (
	candidateVersion = regexp.MustCompile(`^[0-9]{4}\.[0-9]+\.[0-9]+-rc\.[0-9]+$`)
	finalVersion     = regexp.MustCompile(`^[0-9]{4}\.[0-9]+\.[0-9]+$`)
	fullSHA          = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// regression decides whether a release run repeats the VM regression lanes.
// Only the rc and final versions can skip them, and only when CI has already
// run the full plan to success on the exact commit. Every other outcome, an
// unreadable API included, runs them. A final version also requires a
// successful rc release run at the same commit and fails without one. Only
// tag pushes call it; other triggers always run the lanes.
func regression(args []string, stdout, stderr io.Writer, call command) error {
	if len(args) != 2 {
		return errors.New("usage: scripts/katl-release regression VERSION COMMIT")
	}
	version, commit := args[0], args[1]
	if !fullSHA.MatchString(commit) {
		return errors.New("COMMIT must be the full source commit SHA")
	}
	if err := validateVersion(version, call); err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}

	final := finalVersion.MatchString(version)
	if final {
		rc, err := candidateRun(version, commit, call)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Release candidate %s succeeded at %s: %s\n", rc.Tag, commit, rc.URL)
	}
	if !final && !candidateVersion.MatchString(version) {
		fmt.Fprintf(stderr, "%s is not a release candidate or final version; running the regression lanes.\n", version)
		fmt.Fprintln(stdout, "run")
		return nil
	}

	queued, err := mergedCIRun(commit, call)
	if err != nil {
		fmt.Fprintf(stderr, "Cannot establish that CI passed at %s (%v); running the regression lanes.\n", commit, err)
		fmt.Fprintln(stdout, "run")
		return nil
	}
	if queued == nil {
		fmt.Fprintf(stderr, "No successful merge-queue CI run produced %s; running the regression lanes.\n", commit)
		fmt.Fprintln(stdout, "run")
		return nil
	}
	fmt.Fprintf(stderr, "CI passed at %s in %s; skipping the regression lanes.\n", commit, queued.URL)
	fmt.Fprintln(stdout, "skip")
	return nil
}

// candidateRun finds the successful rc tag run of the final version at the
// commit. A final release rebuilds the bytes but must follow a candidate that
// was built, qualified and published from the same source.
func candidateRun(version, commit string, call command) (workflowRun, error) {
	runs, err := workflowRuns(releaseWorkflow, commit, "push", call)
	if err != nil {
		return workflowRun{}, fmt.Errorf("cannot look up the release candidate of %s: %w", version, err)
	}
	prefix := "v" + version + "-rc."
	for _, run := range runs {
		if strings.HasPrefix(run.Tag, prefix) && candidateVersion.MatchString(strings.TrimPrefix(run.Tag, "v")) && run.Path == workflow {
			return run, nil
		}
	}
	return workflowRun{}, fmt.Errorf("no successful release candidate run of v%[1]s-rc.N exists at %[2]s; tag a candidate at this commit and wait for its release run to succeed, then tag v%[1]s again", version, commit)
}

// mergedCIRun returns the successful merge-queue CI run that built the commit
// as it landed on main, or nil. The merge queue always plans the full VM
// regression and the production qualification, and a run succeeds only when
// the required result job does. Pushes and pull requests plan less, and a pull
// request tested a different merge commit, so neither is accepted.
func mergedCIRun(commit string, call command) (*workflowRun, error) {
	runs, err := workflowRuns(ciWorkflow, commit, "merge_group", call)
	if err != nil {
		return nil, err
	}
	var found *workflowRun
	for _, run := range runs {
		if run.Path == ciWorkflowPath && strings.HasPrefix(run.Tag, "gh-readonly-queue/main/") {
			found = &run
			break
		}
	}
	if found == nil {
		return nil, nil
	}
	// A group that never merged leaves a successful run for a commit that is not on main.
	var comparison struct{ Status string }
	if err := api(call, "compare/main..."+commit, &comparison); err != nil {
		return nil, err
	}
	if comparison.Status != "identical" && comparison.Status != "behind" {
		return nil, fmt.Errorf("%s is not reachable from main", commit)
	}
	return found, nil
}

// workflowRuns lists the successful runs of a workflow for one event at the
// commit. The head_sha filter is the commit itself, so a run of another
// revision is never returned; the checks below re-assert what is trusted.
func workflowRuns(file, commit, event string, call command) ([]workflowRun, error) {
	var response struct {
		Runs []workflowRun `json:"workflow_runs"`
	}
	endpoint := fmt.Sprintf("actions/workflows/%s/runs?head_sha=%s&event=%s&status=success&per_page=100", url.PathEscape(file), commit, event)
	if err := api(call, endpoint, &response); err != nil {
		return nil, err
	}
	var runs []workflowRun
	for _, run := range response.Runs {
		if run.Commit == commit && run.Event == event && run.Status == "completed" && run.Conclusion == "success" {
			runs = append(runs, run)
		}
	}
	return runs, nil
}
