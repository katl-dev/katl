package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/katl-dev/katl/internal/releaseversion"
)

const (
	ciWorkflow      = "ci.yml"
	releaseWorkflow = "release-artifacts.yml"
)
const ciWorkflowPath = ".github/workflows/ci.yml"

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// regression decides whether a release run repeats the VM regression lanes.
// Only the rc and final versions can skip them, and only when CI has already
// run the VM regression to success on the exact commit. Every other outcome, an
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
	parsed, err := validateVersion(version)
	if err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}

	final := parsed.IsFinal()
	if final {
		rc, err := candidateRun(version, commit, call)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Release candidate %s succeeded at %s: %s\n", rc.Tag, commit, rc.URL)
	}
	if !final && parsed.Stage != releaseversion.Candidate {
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
	for _, run := range runs {
		tag, err := releaseversion.ParseTag(run.Tag)
		if err == nil && tag.Stage == releaseversion.Candidate && tag.Base().String() == version && run.Path == workflow {
			return run, nil
		}
	}
	return workflowRun{}, fmt.Errorf("no successful release candidate run of v%[1]s-rc.N exists at %[2]s; tag a candidate at this commit, wait for the candidate run to succeed, then re-run this workflow run, or tag a new -rc.N at this commit", version, commit)
}

// mergedCIRun returns the successful merge-queue CI run that built the commit
// as it landed on main, or nil. A merge-queue run plans no VM suite for a
// change that selects none, such as documentation, and still succeeds, so the
// run counts only when its jobs show both flavours ran the VM regression to
// success on the default Kubernetes payload. Pushes and pull requests plan
// less, and a pull request tested a different merge commit, so neither is
// accepted.
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
	if err := requireVMRegression(*found, call); err != nil {
		return nil, err
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

const (
	// Job names of the reusable hosted-vmtests.yml as listed under the CI run:
	// the caller job "VM (<flavour>)", then "/", then the called job's name.
	vmJobPrefix        = "VM (%s) / "
	vmScenarioPrefix   = "VM / "
	kubernetesCandStep = "Resolve Kubernetes candidate"
	kubernetesCandJob  = "images"
)

var vmFlavours = []string{"standard", "lts"}

type runJobs struct {
	Total int `json:"total_count"`
	Jobs  []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		Steps      []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"steps"`
	} `json:"jobs"`
}

// requireVMRegression fails unless every VM job of both flavours in the latest
// attempt of the run succeeded, at least one scenario ran per flavour, and no
// presubmit Kubernetes candidate replaced the default payload that the release
// lanes build. A skipped, missing or unlisted job is not evidence.
func requireVMRegression(run workflowRun, call command) error {
	var jobs runJobs
	endpoint := fmt.Sprintf("actions/runs/%d/jobs?filter=latest&per_page=100", run.ID)
	if err := api(call, endpoint, &jobs); err != nil {
		return err
	}
	if jobs.Total > len(jobs.Jobs) {
		return fmt.Errorf("CI run %d lists %d jobs, more than one page", run.ID, jobs.Total)
	}
	for _, flavour := range vmFlavours {
		prefix := fmt.Sprintf(vmJobPrefix, flavour)
		scenarios, images := 0, 0
		for _, job := range jobs.Jobs {
			if !strings.HasPrefix(job.Name, prefix) {
				continue
			}
			if job.Conclusion != "success" {
				return fmt.Errorf("CI job %q concluded %q", job.Name, job.Conclusion)
			}
			called := strings.TrimPrefix(job.Name, prefix)
			if strings.HasPrefix(called, vmScenarioPrefix) {
				scenarios++
			}
			if called != kubernetesCandJob {
				continue
			}
			images++
			skipped := false
			for _, step := range job.Steps {
				if step.Name == kubernetesCandStep {
					skipped = step.Conclusion == "skipped"
					break
				}
			}
			if !skipped {
				return fmt.Errorf("CI job %q tested a Kubernetes candidate, not the default payload", job.Name)
			}
		}
		if scenarios == 0 || images == 0 {
			return fmt.Errorf("CI run %d did not run the %s VM images and scenarios", run.ID, flavour)
		}
	}
	return nil
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
