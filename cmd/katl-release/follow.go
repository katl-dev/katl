package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/retry"
	"github.com/katl-dev/katl/internal/runstatus"
)

const releaseEvent = "workflow_dispatch"
const decideJob = "Decide"

func release(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader, call command) error {
	usage := errors.New("usage: scripts/katl-release release VERSION [--retry REASON]")
	if len(args) == 0 {
		return usage
	}
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	retryReason := flags.String("retry", "", "reason to retry a failed release build")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.HasPrefix(args[0], "-") || flags.NArg() != 0 {
		return usage
	}

	if err := r.requireReleaseWorkflow(ctx); err != nil {
		return err
	}
	ref := releasestate.RefForVersion(args[0])
	d, err := decide(ctx, r, ref, "", *retryReason)
	if err != nil {
		return err
	}
	if d.Action == releasestate.Refuse {
		return errors.New(d.Message)
	}
	printDecision(stdout, r, d)

	version, _ := releasestate.ParseRef(ref)
	branch := releasestate.Branch(version)
	before, err := r.newestReleaseRun(ctx, branch)
	if err != nil {
		return err
	}
	dispatch := []string{"workflow", "run", path.Base(releasestate.ReleaseWorkflow), "--repo", r.repository, "--ref", branch}
	if d.Reason != "" {
		dispatch = append(dispatch, "-f", "retry="+d.Reason)
	}
	// A failed response can still mean GitHub accepted the dispatch.
	// Repeating the write could start a second run.
	output, err := call("gh", dispatch...)
	if err != nil {
		return fmt.Errorf("%w\nGitHub may have accepted the dispatch. Run scripts/katl-release status %s before dispatching again.",
			ghFailure("gh workflow run "+path.Base(releasestate.ReleaseWorkflow), err, stderrOf(err)), version)
	}
	run, err := dispatchedRun(ctx, r, version, before.ID, d.Commit, string(output))
	if err != nil {
		return resumeWait(version, err)
	}
	return followRelease(ctx, r, version, run, stdout, stderr)
}

func waitRelease(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader) error {
	if len(args) != 1 {
		return errors.New("usage: scripts/katl-release wait VERSION")
	}
	if err := r.requireReleaseWorkflow(ctx); err != nil {
		return err
	}
	ref := releasestate.RefForVersion(args[0])
	version, ok := releasestate.ParseRef(ref)
	if !ok {
		d, err := decide(ctx, r, ref, "", "")
		if err != nil {
			return err
		}
		return errors.New(d.Message)
	}
	run, err := r.newestReleaseRun(ctx, releasestate.Branch(version))
	if err != nil {
		return err
	}
	if run.ID == 0 {
		return fmt.Errorf("No Release run for %s. Run scripts/katl-release release %s.", version, version)
	}
	return followRelease(ctx, r, version, run, stdout, stderr)
}

func (r *reader) dispatchedRuns(ctx context.Context, branch string) ([]restRun, error) {
	runs, err := r.releaseRuns(ctx, branch, "event="+releaseEvent)
	if err != nil {
		return nil, err
	}
	var dispatched []restRun
	for _, run := range runs {
		if run.isReleaseDispatch(branch) {
			dispatched = append(dispatched, run)
		}
	}
	return dispatched, nil
}

func (r *reader) newestReleaseRun(ctx context.Context, branch string) (restRun, error) {
	runs, err := r.dispatchedRuns(ctx, branch)
	var newest restRun
	for _, run := range runs {
		if run.ID > newest.ID {
			newest = run
		}
	}
	return newest, err
}

func (run restRun) isReleaseDispatch(branch string) bool {
	return run.ID > 0 && run.Branch == branch && run.Path == releasestate.ReleaseWorkflow && run.Event == releaseEvent
}

func dispatchedRun(ctx context.Context, r *reader, version releaseversion.Version, after int64, commit, output string) (restRun, error) {
	// gh prints the run URL on its own line when the dispatch response has one.
	prefix := r.server + "/" + r.repository + "/actions/runs/"
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "https://") && !strings.HasPrefix(line, "http://") {
			continue
		}
		number, ok := strings.CutPrefix(line, prefix)
		id, err := strconv.ParseInt(number, 10, 64)
		if !ok || err != nil || id <= 0 || strconv.FormatInt(id, 10) != number {
			return restRun{}, fmt.Errorf("gh returned an invalid Release run URL %q", line)
		}

		var run restRun
		if err := r.decode(ctx, r.repo(fmt.Sprintf("actions/runs/%d", id)), &run); err != nil {
			return restRun{}, fmt.Errorf("read dispatched Release run %s: %w", line, err)
		}
		branch := releasestate.Branch(version)
		if run.ID != id || !run.isReleaseDispatch(branch) || run.SHA != commit {
			return restRun{}, fmt.Errorf("The dispatched run %s reported ID %d, branch %s, workflow %s, event %s and commit %s. Expected Release on %s at %s",
				line, run.ID, run.Branch, run.Path, run.Event, run.SHA, branch, commit)
		}
		return run, nil
	}
	return findDispatch(ctx, r, version, after, commit)
}

func resumeWait(version releaseversion.Version, err error) error {
	return fmt.Errorf("%w\nRun scripts/katl-release wait %s.", err, version)
}

func findDispatch(ctx context.Context, r *reader, version releaseversion.Version, after int64, commit string) (restRun, error) {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	branch := releasestate.Branch(version)
	backoff := retry.NewBackoff(2*time.Second, 15*time.Second)
	var moved restRun
	run, err := retry.Await(deadline, &backoff, func(ctx context.Context) (restRun, bool, error) {
		runs, err := r.dispatchedRuns(ctx, branch)
		if err != nil {
			return restRun{}, false, err
		}
		var matching restRun
		var urls []string
		seen := map[int64]bool{}
		for _, run := range runs {
			if run.ID <= after || seen[run.ID] {
				continue
			}
			seen[run.ID] = true
			if run.SHA == commit {
				matching = run
				urls = append(urls, run.HTMLURL)
			} else if run.ID > moved.ID {
				moved = run
			}
		}
		if len(urls) > 1 {
			return restRun{}, false, fmt.Errorf("Multiple Release runs for %s at %s appeared: %s", version, commit, strings.Join(urls, ", "))
		}
		return matching, matching.ID > 0, nil
	}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return run, err
	}
	if moved.ID > 0 {
		return restRun{}, fmt.Errorf("%s moved from %s to %s between the decision and dispatch. The Release run will refuse (row 3) if its commit differs from the branch head when Decide runs: %s",
			branch, commit, moved.SHA, moved.HTMLURL)
	}
	return restRun{}, fmt.Errorf("The dispatch was accepted but no Release run for %s at %s appeared.", version, commit)
}

type releaseProgress struct {
	attempt  int
	jobs     map[int64]restJob
	approval bool
}

func (p *releaseProgress) reportJobs(stderr io.Writer, attempt int, jobs []restJob) {
	if attempt != p.attempt {
		p.attempt = attempt
		p.jobs = make(map[int64]restJob)
	}
	for _, job := range jobs {
		previous := p.jobs[job.ID]
		switch {
		case job.Conclusion != "" && job.Conclusion != previous.Conclusion:
			fmt.Fprintf(stderr, "%s: %s\n", job.Name, job.Conclusion)
		case job.Status == "in_progress" && previous.Status != job.Status:
			fmt.Fprintf(stderr, "%s: started\n", job.Name)
		}
		p.jobs[job.ID] = job
	}
}

func followRelease(ctx context.Context, r *reader, version releaseversion.Version, selected restRun, stdout, stderr io.Writer) error {
	fmt.Fprintln(stdout, selected.HTMLURL)
	var progress releaseProgress
	var jobs []restJob
	backoff := retry.NewBackoff(3*time.Second, time.Minute)
	run, err := retry.Await(ctx, &backoff, func(ctx context.Context) (restRun, bool, error) {
		var run restRun
		if err := r.decode(ctx, r.repo(fmt.Sprintf("actions/runs/%d", selected.ID)), &run); err != nil {
			return run, false, err
		}
		if run.ID != selected.ID || run.Attempt < 1 {
			return run, false, fmt.Errorf("GitHub did not report the latest attempt of Release run %d: %s", selected.ID, selected.HTMLURL)
		}
		attempt := runstatus.Attempt{Run: run.ID, Number: run.Attempt}
		var err error
		jobs, err = r.jobs(ctx, attempt)
		if err != nil {
			return run, false, err
		}
		progress.reportJobs(stderr, run.Attempt, jobs)
		if run.Status == "waiting" && !progress.approval {
			pending, err := r.approvalPending(ctx, run.ID)
			if err != nil {
				return run, false, err
			}
			if pending {
				fmt.Fprintf(stderr, "%s is waiting for approval: %s\n", version.Tag(), selected.HTMLURL)
				progress.approval = true
			}
		}
		return run, run.Status == "completed", nil
	}, nil)
	if err != nil {
		return resumeWait(version, err)
	}

	ref := releasestate.Ref(version)
	// The run detail is terminal even if the active-run listing still includes it.
	readback := *r
	readback.self = run.ID
	if run.Conclusion != "success" {
		return releaseFailure(ctx, &readback, version, run, selected.HTMLURL, jobs, stderr)
	}
	d, err := decide(ctx, &readback, ref, "", "")
	if err != nil {
		return resumeWait(version, fmt.Errorf("The Release run succeeded, but %w (%s)", err, selected.HTMLURL))
	}
	return confirmRelease(stdout, stderr, r, version, run, d)
}

func releaseFailure(ctx context.Context, r *reader, version releaseversion.Version, run restRun, url string, jobs []restJob, stderr io.Writer) error {
	var failed []string
	var refused bool
	for _, job := range jobs {
		if job.Conclusion == "failure" || job.Conclusion == "timed_out" {
			failed = append(failed, job.Name)
			refused = refused || job.Name == decideJob
		}
	}
	outcome := "concluded " + run.Conclusion
	switch {
	case run.Conclusion == "cancelled":
		outcome = "was cancelled"
	case len(failed) > 0:
		outcome = "failed: " + strings.Join(failed, ", ")
	}
	failure := fmt.Errorf("Release run %s (%s)", outcome, url)
	if refused {
		d, err := decide(ctx, r, releasestate.Ref(version), "", "")
		if err != nil {
			return resumeWait(version, errors.Join(failure, fmt.Errorf("read the current release state: %w", err)))
		}
		fmt.Fprintln(stderr, "Current release decision without a retry reason:")
		printDecision(stderr, r, d)
		if d.Action != releasestate.Refuse {
			fmt.Fprintln(stderr, "The run's refusal no longer applies.")
		}
	}
	return failure
}

func confirmRelease(stdout, stderr io.Writer, r *reader, version releaseversion.Version, run restRun, d releasestate.Decision) error {
	final := d.FinalRecord
	isFinal := false
	if final != nil {
		target, err := runstatus.ParseAttemptURL(final.URL)
		isFinal = err == nil && target.Attempt.Run == run.ID
	}
	rc := d.CandidateRecord
	switch {
	case isFinal && d.State == releasestate.StateReleased:
		fmt.Fprintln(stdout, d.Message)
		fmt.Fprintf(stdout, "%s sha256:%s\n", final.Tag, final.Digest)
		return nil
	case !isFinal && d.State == releasestate.StateSuperseded:
		fmt.Fprintf(stdout, "The Release run succeeded, and a new commit on %s supersedes its candidate: %s\n", releasestate.Branch(version), run.HTMLURL)
		return nil
	case !isFinal && (d.State == releasestate.StateFinalStart || d.State == releasestate.StateExtensionsAlreadyPublished) &&
		rc != nil && rc.Phase == releasestate.Qualified:
		if rc.Commit != run.SHA {
			return fmt.Errorf("The Release run succeeded at %s (%s). The current candidate %s is at %s. Run scripts/katl-release status %s.",
				run.SHA, run.HTMLURL, rc.Tag, rc.Commit, version)
		}
		fmt.Fprintf(stdout, "Published %s sha256:%s\n", rc.Tag, rc.Digest)
		if d.Message != "" {
			fmt.Fprintf(stderr, "Warning: %s\n", d.Message)
		}
		return nil
	case d.State == releasestate.StateCandidateIncomplete || d.State == releasestate.StateFinalUnapproved:
		printDecision(stderr, r, d)
		return fmt.Errorf("The Release run succeeded (%s). v%s qualified and still awaits publication. Run scripts/katl-release release %s to finish publication.",
			run.HTMLURL, d.Version, version)
	case d.Action == releasestate.Refuse:
		return fmt.Errorf("The Release run succeeded, but %s (%s)", d.Message, run.HTMLURL)
	}

	printDecision(stderr, r, d)
	return fmt.Errorf("The Release run succeeded (%s), but publication could not be confirmed. Run scripts/katl-release status %s", run.HTMLURL, version)
}
