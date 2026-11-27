package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
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
	printDecision(stdout, r, d)
	if d.Action == releasestate.Refuse {
		return errors.New(d.Message)
	}

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
	if _, err := call("gh", dispatch...); err != nil {
		return ghFailure("gh workflow run "+path.Base(releasestate.ReleaseWorkflow), err, stderrOf(err))
	}
	run, err := findDispatch(ctx, r, version, before.ID, d.Commit)
	if err != nil {
		return err
	}
	return followRelease(ctx, r, version, run, stdout, stderr)
}

func waitRelease(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader) error {
	if len(args) != 1 {
		return errors.New("usage: scripts/katl-release wait VERSION")
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
		if run.ID > 0 && run.Branch == branch && run.Path == releasestate.ReleaseWorkflow && run.Event == releaseEvent {
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

func findDispatch(ctx context.Context, r *reader, version releaseversion.Version, after int64, commit string) (restRun, error) {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	branch := releasestate.Branch(version)
	recover := fmt.Sprintf("Run scripts/katl-release wait %s.", version)
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
			return restRun{}, false, fmt.Errorf("Multiple Release runs for %s at %s appeared: %s. %s", version, commit, strings.Join(urls, ", "), recover)
		}
		return matching, matching.ID > 0, nil
	}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return run, err
	}
	if moved.ID > 0 {
		return restRun{}, fmt.Errorf("%s moved from %s to %s between the decision and dispatch. The Release run will refuse (row 3) if its commit differs from the branch head when Decide runs: %s. %s",
			branch, commit, moved.SHA, moved.HTMLURL, recover)
	}
	return restRun{}, fmt.Errorf("The dispatch was accepted but no Release run for %s at %s appeared. %s", version, commit, recover)
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
		return err
	}

	ref := releasestate.Ref(version)
	if run.Conclusion != "success" {
		return releaseFailure(ctx, r, ref, run, selected.HTMLURL, jobs)
	}
	d, err := decide(ctx, r, ref, "", "")
	if err != nil {
		return fmt.Errorf("The Release run succeeded, but %w (%s)", err, selected.HTMLURL)
	}
	return confirmRelease(stdout, version, selected, d)
}

func releaseFailure(ctx context.Context, r *reader, ref string, run restRun, url string, jobs []restJob) error {
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
		d, err := decide(ctx, r, ref, run.SHA, "")
		if err != nil {
			return errors.Join(failure, err)
		}
		if d.Action == releasestate.Refuse {
			return errors.Join(failure, errors.New(d.Message))
		}
	}
	return failure
}

func confirmRelease(stdout io.Writer, version releaseversion.Version, run restRun, d releasestate.Decision) error {
	final := d.FinalRecord
	isFinal := false
	if final != nil {
		target, err := runstatus.ParseAttemptURL(final.URL)
		isFinal = err == nil && target.Attempt.Run == run.ID
	}
	switch {
	case isFinal && d.State == "released":
		fmt.Fprintln(stdout, d.Message)
		fmt.Fprintf(stdout, "%s sha256:%s\n", final.Tag, final.Digest)
		return nil
	case !isFinal && d.State == "superseded":
		fmt.Fprintf(stdout, "The Release run succeeded, and a new commit on %s supersedes its candidate: %s\n", releasestate.Branch(version), run.HTMLURL)
		return nil
	case !isFinal && (d.State == "final-start" || d.State == "extensions-already-published") &&
		d.CandidateRecord != nil && d.CandidateRecord.Phase == releasestate.Qualified:
		fmt.Fprintf(stdout, "Published %s sha256:%s\n", d.CandidateRecord.Tag, d.CandidateRecord.Digest)
		return nil
	default:
		message := d.Message
		if message == "" {
			message = d.State
		}
		return fmt.Errorf("The Release run succeeded, but %s (%s)", message, run.HTMLURL)
	}
}
