package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/runstatus"
)

const (
	ciWorkflowPath = ".github/workflows/ci.yml"
	mainQueue      = "gh-readonly-queue/main/"
	fastChecksJob  = "Fast checks"

	// The reusable workflow's job name is prefixed with its caller's job name.
	vmJobPrefix        = "VM (%s) / "
	vmScenarioPrefix   = "VM / "
	kubernetesCandStep = "Resolve Kubernetes candidate"
	kubernetesCandJob  = "images"
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type regressionDecision struct {
	Decision   string   `json:"decision"`
	VMFlavours []string `json:"vm_flavours"`
	Evidence   string   `json:"evidence"`

	// fastChecks is the run whose Fast checks job supplied the evidence.
	fastChecks string
	// reason says why a run decision found no VM evidence.
	reason string
}

func regression(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader) error {
	if len(args) != 2 {
		return errors.New("usage: scripts/katl-release regression VERSION COMMIT")
	}
	version, err := validateVersion(args[0])
	if err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}
	commit := args[1]
	if !fullSHA.MatchString(commit) {
		return errors.New("COMMIT must be the full source commit SHA")
	}

	decision, err := r.decideRegression(ctx, releasestate.Branch(version.Base()), commit)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Fast checks passed in %s\n", decision.fastChecks)
	if decision.Decision == "run" {
		fmt.Fprintf(stderr, "Running VM regression: %s\n", decision.reason)
	}
	return json.NewEncoder(stdout).Encode(decision)
}

// decideRegression requires Fast checks independently of whether CI can also
// supply the VM evidence. A branch run can supply Fast checks when the queue
// run is absent, incomplete, or unreadable.
func (r *reader) decideRegression(ctx context.Context, branch, commit string) (regressionDecision, error) {
	// Bound both the backoff and the gh requests it runs.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	queued, queueErr := r.mergedCIRuns(ctx, commit)
	queue := r.readCIJobs(ctx, queued, len(queued) == 1)
	fast := queue.fastPassed
	if fast == nil {
		runs, branchErr := r.ciRuns(ctx, commit, "branch="+url.QueryEscape(branch), func(run workflowRun) bool {
			return run.Tag == branch && (run.Event == "push" || run.Event == "workflow_dispatch")
		})
		release := r.readCIJobs(ctx, runs, false)
		fast = release.fastPassed
		if fast == nil {
			// A read failure comes first: the evidence it hid could pass.
			for _, err := range []error{queueErr, queue.err, branchErr, release.err} {
				if err != nil {
					return regressionDecision{}, unreadableEvidence(commit, branch, err)
				}
			}
			for _, failed := range []*workflowRun{queue.fastFailed, release.fastFailed} {
				if failed != nil {
					return regressionDecision{}, fmt.Errorf("CI failed Fast checks at %s (%s); fix it or re-run CI, then dispatch Release again", commit, failed.URL)
				}
			}
			return regressionDecision{}, fmt.Errorf("CI has not passed Fast checks at %s on %s; wait for CI, or dispatch CI on %s, then dispatch Release again", commit, branch, branch)
		}
	}

	if queue.vmEvidence != "" {
		return regressionDecision{Decision: "skip", VMFlavours: []string{}, Evidence: queue.vmEvidence, fastChecks: fast.URL}, nil
	}
	var reason string
	switch {
	case queueErr != nil:
		reason = fmt.Sprintf("could not read the merge-queue CI runs: %v", queueErr)
	case len(queued) == 0:
		reason = "no merge-queue CI run tested this commit as it landed on main"
	case len(queued) > 1:
		reason = fmt.Sprintf("%d merge-queue CI runs tested this commit, so none is taken as its VM evidence", len(queued))
	default:
		reason = queue.vmProblem
	}
	return regressionDecision{Decision: "run", VMFlavours: flavour.Supported(), fastChecks: fast.URL, reason: reason}, nil
}

// unreadableEvidence tells a read that may succeed when repeated apart from
// one that will fail the same way again.
func unreadableEvidence(commit, branch string, err error) error {
	if transient(err) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("could not read CI evidence at %s; dispatch Release again: %w", commit, err)
	}
	return fmt.Errorf("CI evidence at %s cannot be read, and reading it again will fail the same way: %w; dispatch CI on %s, then dispatch Release again, or report a katl-release defect if this persists", commit, err, branch)
}

// mergedCIRuns finds the merge-queue CI runs for commits that landed on main.
// An ejected merge group can leave a green run for a commit that never landed.
func (r *reader) mergedCIRuns(ctx context.Context, commit string) ([]workflowRun, error) {
	runs, err := r.ciRuns(ctx, commit, "event=merge_group", func(run workflowRun) bool {
		return run.Event == "merge_group" && strings.HasPrefix(run.Tag, mainQueue) && run.Tag != mainQueue
	})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	status, err := r.compare(ctx, "main", commit)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "identical", "behind":
		return runs, nil
	case "ahead", "diverged":
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected comparison status %q for main and %s", status, commit)
	}
}

// ciRuns includes all conclusions so a failed Fast checks job can be
// distinguished from missing or pending CI. Recheck the identity GitHub
// returned before reading any of its jobs. A listing that is not found is
// unreadable, not empty.
func (r *reader) ciRuns(ctx context.Context, commit, query string, matches func(workflowRun) bool) ([]workflowRun, error) {
	type runsPage struct {
		Runs []workflowRun `json:"workflow_runs"`
	}
	endpoint := r.repo("actions/workflows/" + path.Base(ciWorkflowPath) + "/runs?head_sha=" + commit + "&" + query + "&per_page=100")
	var runs []workflowRun
	incomplete := false
	err := pages(ctx, r, endpoint, func(page runsPage) {
		incomplete = incomplete || page.Runs == nil
		for _, run := range page.Runs {
			if run.Commit == commit && run.Path == ciWorkflowPath && matches(run) {
				runs = append(runs, run)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if incomplete {
		return nil, fmt.Errorf("incomplete CI run list from %s", endpoint)
	}
	return runs, nil
}

type ciEvidence struct {
	// fastPassed is a completed run whose Fast checks job succeeded,
	// whatever the run's own conclusion.
	fastPassed *workflowRun
	fastFailed *workflowRun
	vmEvidence string
	// vmProblem says why checked runs supplied no VM evidence.
	vmProblem string
	err       error
}

func (r *reader) readCIJobs(ctx context.Context, runs []workflowRun, checkVM bool) ciEvidence {
	var evidence ciEvidence
	for _, run := range runs {
		if run.Status != "completed" {
			evidence.vmProblem = fmt.Sprintf("merge-queue CI run %s has not completed", run.URL)
			continue
		}
		jobs, err := r.ciJobs(ctx, runstatus.Attempt{Run: run.ID, Number: run.Attempt})
		if err == nil {
			var fast string
			fast, err = fastChecksConclusion(jobs)
			switch fast {
			case "failure", "timed_out":
				if evidence.fastFailed == nil {
					evidence.fastFailed = &run
				}
				evidence.vmProblem = fmt.Sprintf("merge-queue CI run %s failed Fast checks", run.URL)
				continue
			case "success":
				evidence.fastPassed = &run
			}
		}
		if err != nil {
			if evidence.err == nil {
				evidence.err = err
			}
			evidence.vmProblem = fmt.Sprintf("could not read the jobs of merge-queue CI run %s: %v", run.URL, err)
			continue
		}
		if checkVM {
			if run.Conclusion != "success" {
				evidence.vmProblem = fmt.Sprintf("merge-queue CI run %s concluded %q", run.URL, run.Conclusion)
			} else if err := requireVMRegression(jobs, vmJobPrefix); err != nil {
				evidence.vmProblem = fmt.Sprintf("merge-queue CI run %s supplied no complete VM regression: %v", run.URL, err)
			} else {
				evidence.vmEvidence = run.URL
			}
		}
		if evidence.fastPassed != nil {
			break
		}
	}
	return evidence
}

type ciJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Steps      []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// ciJobs binds the job list to the attempt in the workflow run listing.
// Looking up "latest" separately could read a re-run that started in between.
func (r *reader) ciJobs(ctx context.Context, attempt runstatus.Attempt) ([]ciJob, error) {
	if attempt.Run < 1 || attempt.Number < 1 {
		return nil, fmt.Errorf("CI run did not identify its attempt: %s", attempt)
	}
	type jobsPage struct {
		Total int     `json:"total_count"`
		Jobs  []ciJob `json:"jobs"`
	}
	var jobs []ciJob
	total, incomplete := -1, false
	err := pages(ctx, r, runstatus.AttemptEndpoint(r.repository, attempt)+"/jobs?per_page=100", func(page jobsPage) {
		incomplete = incomplete || page.Jobs == nil || (total >= 0 && page.Total != total)
		total = page.Total
		jobs = append(jobs, page.Jobs...)
	})
	if err != nil {
		return nil, err
	}
	// A completed attempt's job list does not change between pages.
	if incomplete || total != len(jobs) {
		return nil, fmt.Errorf("incomplete CI job list for run attempt %s", attempt)
	}
	return jobs, nil
}

func fastChecksConclusion(jobs []ciJob) (string, error) {
	var found *ciJob
	for _, job := range jobs {
		if job.Name != fastChecksJob {
			continue
		}
		if found != nil {
			return "", errors.New("CI listed multiple Fast checks jobs in one attempt")
		}
		found = &job
	}
	if found == nil || (found.Status != "" && found.Status != "completed") {
		return "", nil
	}
	return found.Conclusion, nil
}

// requireVMRegression checks the complete job list of one attempt. Both
// flavours must have a successful images job using the default Kubernetes
// payload and at least one successful scenario.
func requireVMRegression(jobs []ciJob, jobPrefix string) error {
	for _, kernelFlavour := range flavour.Supported() {
		prefix := fmt.Sprintf(jobPrefix, kernelFlavour)
		seen := make(map[string]bool)
		scenarios := 0
		for _, job := range jobs {
			if job.Name == "" {
				return errors.New("CI listed a job without a name")
			}
			called, ok := strings.CutPrefix(job.Name, prefix)
			if !ok {
				continue
			}
			if seen[called] {
				return fmt.Errorf("CI listed multiple jobs named %q", job.Name)
			}
			seen[called] = true
			if job.Conclusion != "success" || (job.Status != "" && job.Status != "completed") {
				return fmt.Errorf("CI job %q concluded %q with status %q", job.Name, job.Conclusion, job.Status)
			}
			if strings.HasPrefix(called, vmScenarioPrefix) && called != vmScenarioPrefix {
				scenarios++
			}
			if called != kubernetesCandJob {
				continue
			}
			skipped := 0
			for _, step := range job.Steps {
				if step.Name != kubernetesCandStep {
					continue
				}
				if step.Conclusion != "skipped" {
					return fmt.Errorf("CI job %q ran %q", job.Name, kubernetesCandStep)
				}
				skipped++
			}
			if skipped != 1 {
				return fmt.Errorf("CI job %q did not record exactly one skipped %q step", job.Name, kubernetesCandStep)
			}
		}
		if scenarios == 0 || !seen[kubernetesCandJob] {
			return fmt.Errorf("CI did not run the %s VM images and scenarios", kernelFlavour)
		}
	}
	return nil
}
