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
	target := releaseDispatch(version, d.Commit)
	var inputs []string
	if d.Reason != "" {
		inputs = []string{"-f", "retry=" + d.Reason}
	}
	run, err := dispatchWorkflow(ctx, r, call, target, inputs,
		fmt.Sprintf("Run scripts/katl-release status %s before dispatching again.", version), waitHint(version))
	if err != nil {
		return err
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
	run, err := r.newestDispatch(ctx, releaseDispatch(version, ""))
	if err != nil {
		return err
	}
	if run.ID == 0 {
		return fmt.Errorf("No Release run for %s. Run scripts/katl-release release %s.", version, version)
	}
	return followRelease(ctx, r, version, run, stdout, stderr)
}

// Release binds the workflow head. Cut release binds its inputs through its
// run title because the cut commit can precede the workflow head on main.
type workflowDispatch struct {
	name, file, ref, head, title string
}

func releaseDispatch(version releaseversion.Version, head string) workflowDispatch {
	return workflowDispatch{name: "Release", file: releasestate.ReleaseWorkflow, ref: releasestate.Branch(version), head: head}
}

// dispatchWorkflow adds recovery to a failed dispatch request, and resume to a
// failure to identify the run that an accepted dispatch started.
func dispatchWorkflow(ctx context.Context, r *reader, call command, target workflowDispatch, inputs []string, recovery, resume string) (restRun, error) {
	before, err := r.newestDispatch(ctx, target)
	if notFound(err) {
		return restRun{}, fmt.Errorf("cannot find the %s workflow. Check that %s is on the repository's default branch: %w", target.name, target.file, err)
	}
	if err != nil {
		return restRun{}, err
	}
	file := path.Base(target.file)
	args := []string{"workflow", "run", file, "--repo", r.repository, "--ref", target.ref}
	// A failed response can still mean GitHub accepted the dispatch.
	// Repeating the write could start a second run.
	output, err := call("gh", append(args, inputs...)...)
	if err != nil {
		return restRun{}, fmt.Errorf("%w\nGitHub may have accepted the dispatch. %s",
			ghFailure("gh workflow run "+file, err, stderrOf(err)), recovery)
	}
	run, err := dispatchedRun(ctx, r, target, before.ID, string(output))
	if err != nil {
		return restRun{}, fmt.Errorf("%w\n%s", err, resume)
	}
	return run, nil
}

func (r *reader) dispatchedRuns(ctx context.Context, target workflowDispatch) ([]restRun, error) {
	runs, err := r.workflowRuns(ctx, target.file, target.ref, "event="+releaseEvent)
	if err != nil {
		return nil, err
	}
	var dispatched []restRun
	for _, run := range runs {
		if target.matchesDispatch(run) {
			dispatched = append(dispatched, run)
		}
	}
	return dispatched, nil
}

func (r *reader) newestDispatch(ctx context.Context, target workflowDispatch) (restRun, error) {
	runs, err := r.dispatchedRuns(ctx, target)
	var newest restRun
	for _, run := range runs {
		if run.ID > newest.ID {
			newest = run
		}
	}
	return newest, err
}

func (target workflowDispatch) matchesDispatch(run restRun) bool {
	return target.hasIdentity(run) && (target.title == "" || run.DisplayTitle == target.title)
}

// hasIdentity checks the fields GitHub fixes when it creates the run.
func (target workflowDispatch) hasIdentity(run restRun) bool {
	return run.ID > 0 && run.Branch == target.ref && run.Path == target.file && run.Event == releaseEvent
}

// identityPending reports a run that GitHub has not finished describing:
// every identity field it reports agrees with the target and some are empty.
func (target workflowDispatch) identityPending(run restRun) bool {
	fields := [][2]string{{run.Branch, target.ref}, {run.Path, target.file}, {run.Event, releaseEvent}}
	if target.head != "" {
		fields = append(fields, [2]string{run.SHA, target.head})
	}
	empty := false
	for _, field := range fields {
		if field[0] == "" {
			empty = true
		} else if field[0] != field[1] {
			return false
		}
	}
	return empty
}

// titlePending reports a run whose title is still empty or the workflow name
// because GitHub has not yet evaluated the workflow's run-name.
func (target workflowDispatch) titlePending(run restRun) bool {
	return target.title != "" && run.DisplayTitle != target.title && (run.DisplayTitle == target.name || run.DisplayTitle == "")
}

// titleConsistent reports a title that matches or may still converge.
func (target workflowDispatch) titleConsistent(run restRun) bool {
	return target.title == "" || run.DisplayTitle == target.title || target.titlePending(run)
}

func dispatchedRun(ctx context.Context, r *reader, target workflowDispatch, after int64, output string) (restRun, error) {
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
			return restRun{}, fmt.Errorf("gh returned an invalid %s run URL %q", target.name, line)
		}
		return awaitDispatch(ctx, r, target, id, line)
	}

	found, err := findDispatch(ctx, r, target, after)
	if err != nil {
		return restRun{}, err
	}
	return awaitDispatch(ctx, r, target, found.ID, found.HTMLURL)
}

// readbackWait bounds each wait for GitHub to show the result of a write.
const readbackWait = 2 * time.Minute

// awaitReadback reads GitHub after a write until observe reports that the
// write shows, retrying the errors transient classifies, within readbackWait.
func awaitReadback[T any](ctx context.Context, r *reader, observe func(context.Context) (T, bool, error), transient func(error) bool) (T, error) {
	deadline, cancel := context.WithTimeout(ctx, readbackWait)
	defer cancel()
	backoff := r.backoff()
	return retry.Await(deadline, &backoff, observe, transient)
}

// awaitDispatch reads the dispatched run until it exists and its eventually
// consistent display title converges. GitHub can answer 404 for a run it has
// just reported creating, and empty fields while it fills the run in. A
// non-empty mismatch in the fields GitHub fixes at creation, or a title that
// settled on another value, is refused without waiting.
func awaitDispatch(ctx context.Context, r *reader, target workflowDispatch, id int64, url string) (restRun, error) {
	expected := fmt.Sprintf("%s on %s", target.name, target.ref)
	if target.head != "" {
		expected += " at " + target.head
	}
	if target.title != "" {
		expected += fmt.Sprintf(" with title %q", target.title)
	}
	describe := func(run restRun) string {
		return fmt.Sprintf("The dispatched run %s reported ID %d, branch %s, workflow %s, event %s, commit %s and title %q",
			url, run.ID, run.Branch, run.Path, run.Event, run.SHA, run.DisplayTitle)
	}

	run, err := awaitReadback(ctx, r, func(ctx context.Context) (restRun, bool, error) {
		var run restRun
		if err := r.decode(ctx, r.repo(fmt.Sprintf("actions/runs/%d", id)), &run); err != nil {
			return run, false, fmt.Errorf("read dispatched %s run %s: %w", target.name, url, err)
		}
		complete := target.hasIdentity(run) && (target.head == "" || run.SHA == target.head)
		if run.ID == id && target.titleConsistent(run) && (complete || target.identityPending(run)) {
			return run, complete && target.matchesDispatch(run), nil
		}
		return run, false, fmt.Errorf("%s. Expected %s", describe(run), expected)
	}, notFound)
	if !errors.Is(err, context.DeadlineExceeded) {
		return run, err
	}
	switch {
	case run.ID == 0:
		return restRun{}, fmt.Errorf("GitHub still reports the dispatched run %s as not found after %s", url, readbackWait)
	case target.identityPending(run):
		return restRun{}, fmt.Errorf("%s after %s. Expected %s", describe(run), readbackWait, expected)
	case target.titlePending(run):
		return restRun{}, fmt.Errorf("The dispatched run %s still has the workflow name %q as its title after %s. Expected title %q",
			url, run.DisplayTitle, readbackWait, target.title)
	}
	return run, err
}

func resumeWait(version releaseversion.Version, err error) error {
	return fmt.Errorf("%w\n%s", err, waitHint(version))
}

func waitHint(version releaseversion.Version) string {
	return fmt.Sprintf("Run scripts/katl-release wait %s.", version)
}

func findDispatch(ctx context.Context, r *reader, target workflowDispatch, after int64) (restRun, error) {
	var moved restRun
	run, err := awaitReadback(ctx, r, func(ctx context.Context) (restRun, bool, error) {
		runs, err := r.workflowRuns(ctx, target.file, target.ref, "event="+releaseEvent)
		if err != nil {
			return restRun{}, false, err
		}
		var matching restRun
		var urls []string
		seen := map[int64]bool{}
		for _, run := range runs {
			// awaitDispatch waits for a pending title to converge.
			if run.ID <= after || seen[run.ID] || !target.hasIdentity(run) || !target.titleConsistent(run) {
				continue
			}
			seen[run.ID] = true
			if target.head == "" || run.SHA == target.head {
				matching = run
				urls = append(urls, run.HTMLURL)
			} else if run.ID > moved.ID {
				moved = run
			}
		}
		if len(urls) > 1 {
			return restRun{}, false, fmt.Errorf("Multiple %s runs on %s appeared: %s", target.name, target.ref, strings.Join(urls, ", "))
		}
		return matching, matching.ID > 0, nil
	}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return run, err
	}
	if moved.ID > 0 {
		return restRun{}, fmt.Errorf("%s moved from %s to %s between the decision and dispatch. The Release run will refuse (row 3) if its commit differs from the branch head when Decide runs: %s",
			target.ref, target.head, moved.SHA, moved.HTMLURL)
	}
	return restRun{}, fmt.Errorf("The dispatch was accepted but no matching %s run on %s appeared.", target.name, target.ref)
}

type workflowProgress struct {
	attempt  int
	jobs     map[int64]restJob
	approval bool
}

func (p *workflowProgress) reportJobs(stderr io.Writer, attempt int, jobs []restJob) {
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

func followWorkflow(ctx context.Context, r *reader, name, approvalTag string, selected restRun, stdout, stderr io.Writer) (restRun, []restJob, error) {
	fmt.Fprintln(stdout, selected.HTMLURL)
	var progress workflowProgress
	var jobs []restJob
	backoff := retry.NewBackoff(3*time.Second, time.Minute)
	run, err := retry.Await(ctx, &backoff, func(ctx context.Context) (restRun, bool, error) {
		var run restRun
		if err := r.decode(ctx, r.repo(fmt.Sprintf("actions/runs/%d", selected.ID)), &run); err != nil {
			return run, false, err
		}
		if run.ID != selected.ID || run.Attempt < 1 {
			return run, false, fmt.Errorf("GitHub did not report the latest attempt of %s run %d: %s", name, selected.ID, selected.HTMLURL)
		}
		attempt := runstatus.Attempt{Run: run.ID, Number: run.Attempt}
		var err error
		jobs, err = r.jobs(ctx, attempt)
		if err != nil {
			return run, false, err
		}
		progress.reportJobs(stderr, run.Attempt, jobs)
		if approvalTag != "" && run.Status == "waiting" && !progress.approval {
			pending, err := r.approvalPending(ctx, run.ID)
			if err != nil {
				return run, false, err
			}
			if pending {
				fmt.Fprintf(stderr, "%s is waiting for approval: %s\n", approvalTag, selected.HTMLURL)
				progress.approval = true
			}
		}
		return run, run.Status == "completed", nil
	}, nil)
	run.HTMLURL = selected.HTMLURL
	return run, jobs, err
}

func followRelease(ctx context.Context, r *reader, version releaseversion.Version, selected restRun, stdout, stderr io.Writer) error {
	run, jobs, err := followWorkflow(ctx, r, "Release", version.Tag(), selected, stdout, stderr)
	if err != nil {
		return resumeWait(version, err)
	}

	ref := releasestate.Ref(version)
	// The run detail is terminal even if the active-run listing still includes it.
	readback := *r
	readback.self = run.ID
	if run.Conclusion != "success" {
		return releaseFailure(ctx, &readback, version, run, jobs, stderr)
	}
	d, err := decide(ctx, &readback, ref, "", "")
	if err != nil {
		return resumeWait(version, fmt.Errorf("The Release run succeeded, but %w (%s)", err, selected.HTMLURL))
	}
	return confirmRelease(stdout, stderr, r, version, run, d)
}

func workflowFailure(name string, run restRun, jobs []restJob) error {
	var failed []string
	for _, job := range jobs {
		if job.Conclusion == "failure" || job.Conclusion == "timed_out" {
			failed = append(failed, job.Name)
		}
	}
	outcome := "concluded " + run.Conclusion
	switch {
	case run.Conclusion == "cancelled":
		outcome = "was cancelled"
	case len(failed) > 0:
		outcome = "failed: " + strings.Join(failed, ", ")
	}
	return fmt.Errorf("%s run %s (%s)", name, outcome, run.HTMLURL)
}

func releaseFailure(ctx context.Context, r *reader, version releaseversion.Version, run restRun, jobs []restJob, stderr io.Writer) error {
	failure := workflowFailure("Release", run, jobs)
	for _, job := range jobs {
		if job.Name != decideJob || (job.Conclusion != "failure" && job.Conclusion != "timed_out") {
			continue
		}
		d, err := decide(ctx, r, releasestate.Ref(version), "", "")
		if err != nil {
			return resumeWait(version, errors.Join(failure, fmt.Errorf("read the current release state: %w", err)))
		}
		fmt.Fprintln(stderr, "Current release decision without a retry reason:")
		printDecision(stderr, r, d)
		if d.Action != releasestate.Refuse {
			fmt.Fprintln(stderr, "The run's refusal no longer applies.")
		}
		break
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
