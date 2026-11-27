// Command katl-queue manages pull requests in the GitHub merge queue: it shows
// the queue, checks that a pull request merges cleanly over everything queued
// ahead of it, enqueues it, watches the queue, and explains removals.
//
// It is a development tool and is not part of KatlOS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const usage = `usage: katl-queue <command> [arguments]

Commands:
  status            list merge-queue entries and PRs waiting to join
  check PR          simulate merging PR over main and the queue ahead of it,
                    and against each PR waiting to join the queue, in either order
  enqueue PR [--allow-pending-conflict]
                    check PR, then enable auto-merge so it joins the queue;
                    --allow-pending-conflict enqueues it despite a conflict with
                    a PR waiting to join, accepting that the queue removes
                    whichever of the two joins second
  watch [PR...]     report queue state changes until the PRs merge or leave
                    (default: everything queued or with auto-merge enabled)
  watchd [--interval 60s] [--notify]
                    poll GitHub for registered watches and save their results
  wait PR [--timeout D]
                    wait for a merge through watchd
  wait --resume ID [--timeout D]
                    reattach to one wait or replay its recorded verdict (kept 7 days)
                    without re-evaluating it. After every push, run a fresh
                    katl-queue wait PR.
  watches           list registered watches and recent results from watchd
  why PR            explain why a Katl PR was removed from the queue
  review post PR FILE
                    record a review verdict on the pull request
  plan [--milestone M] [--apply | --next]
                    order ready PRs for the queue, holding conflicting ones;
                    --apply makes an autopilot pass, --next lists startable issues
  autopilot [--milestone M] [--apply] [--once] [--interval D]
                    rebase held PRs, enqueue what merges cleanly, dequeue
                    unmergeable entries and explain ejections
  prune-runs [--apply]
                    cancel CI runs of merge groups the queue no longer holds
  ci-cost [--since DATE] [--workflow NAME] [--limit N] [--json]
                    report runner-minutes, runner wait, merge-group wall time
                    and failing jobs across completed workflow runs
  cache [--apply] [--json]
                    report Actions cache usage against .github/cache-policy.json;
                    --apply deletes the entries outside policy

plan and autopilot only report unless given --apply, which needs --milestone.
prune-runs only reports unless given --apply.
cache exits 1 when usage that remains exceeds a family budget or the alert
threshold.

A "Hold: <reason>" line in a PR body holds the PR out of the queue. The
orchestrator adds it while a PR awaits VM gates or a journey, and deletes it
when they pass.

Exit status is 1 when check finds a conflict, enqueue refuses, or a watched PR
leaves the queue without merging, and 2 on any other error.
wait exits 0 when the PR merges, 1 when it needs action, and 2 when there
is no verdict. A timeout only ends the client invocation.
`

// errConflict marks an outcome that has already been reported to the
// operator and only needs a failing exit status.
var errConflict = errors.New("conflict")

func main() {
	signals := []os.Signal{os.Interrupt}
	if len(os.Args) > 1 && (os.Args[1] == "watchd" || os.Args[1] == "wait") {
		signals = append(signals, syscall.SIGTERM)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	code := commandStatus(run(ctx, os.Args[1:], os.Stdout, os.Stderr), os.Stderr)
	stop()
	os.Exit(code)
}

func commandStatus(err error, stderr io.Writer) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errConflict):
		return 1
	default:
		fmt.Fprintln(stderr, "katl-queue:", err)
		return 2
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	command, args := args[0], args[1:]
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Fprint(stdout, usage)
		return nil
	}

	flags := flag.NewFlagSet("katl-queue "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	if command == "ci-cost" {
		return ciCostCommand(ctx, flags, args, stdout)
	}
	if command == "review" {
		return reviewCommand(ctx, args, stdout)
	}
	if command == "cache" {
		return cacheCommand(ctx, flags, args, stdout, stderr)
	}
	interval := time.Minute
	var (
		milestone                string
		applyChanges, next, once bool
		notify, allowPending     bool
		resume                   string
		timeout                  time.Duration
	)
	switch command {
	case "watch", "watchd":
		flags.DurationVar(&interval, "interval", interval, "time between polls")
		if command == "watchd" {
			flags.BoolVar(&notify, "notify", false, "send desktop notifications for merges and failures")
		}
	case "enqueue":
		flags.BoolVar(&allowPending, allowPendingFlag, false, "enqueue despite a conflict with a PR waiting to join the queue")
		// flag stops at the first positional argument. Accept PR before flags.
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			args = append(slices.Clone(args[1:]), args[0])
		}
	case "wait":
		flags.StringVar(&resume, "resume", "", "reattach to one attempt without re-evaluating its verdict; after every push, start a fresh katl-queue wait PR")
		flags.DurationVar(&timeout, "timeout", 0, "maximum wait time (0 waits indefinitely)")
		// flag stops at the first positional argument. Accept PR before flags.
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			args = append(slices.Clone(args[1:]), args[0])
		}
	case "plan":
		flags.StringVar(&milestone, "milestone", "", "limit to this milestone")
		flags.BoolVar(&applyChanges, "apply", false, "make the changes an autopilot pass would")
		flags.BoolVar(&next, "next", false, "list issues that can be started")
	case "prune-runs":
		flags.BoolVar(&applyChanges, "apply", false, "cancel the runs instead of reporting them")
	case "autopilot":
		flags.StringVar(&milestone, "milestone", "", "limit to this milestone")
		flags.BoolVar(&applyChanges, "apply", false, "make the changes instead of reporting them")
		flags.BoolVar(&once, "once", false, "run one pass and exit")
		flags.DurationVar(&interval, "interval", interval, "time between passes")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	numbers, err := parseNumbers(flags.Args())
	if err != nil {
		return err
	}

	needOne := func() error {
		if len(numbers) != 1 {
			return fmt.Errorf("%s takes exactly one pull request number", command)
		}
		return nil
	}
	switch command {
	case "status", "watch":
		if command == "status" && len(numbers) > 0 {
			return errors.New("status takes no arguments")
		}
	case "prune-runs":
		if len(numbers) > 0 {
			return errors.New("prune-runs takes no arguments")
		}
	case "plan", "autopilot":
		if len(numbers) > 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		if next && applyChanges {
			return errors.New("--next only lists issues; it cannot be combined with --apply")
		}
		if err := applyScope(command, milestone, applyChanges); err != nil {
			return err
		}
	case "check", "enqueue":
		if err := needOne(); err != nil {
			return err
		}
	case "why":
		if err := needOne(); err != nil {
			return err
		}
		return why(ctx, &gh{host: katlHost, owner: katlOwner, name: katlRepo}, numbers[0], stdout)
	case "watchd":
		if len(numbers) > 0 {
			return errors.New("watchd takes no arguments")
		}
		if interval <= 0 {
			return errors.New("--interval must be positive")
		}
		return watchd(ctx, interval, notify, stdout)
	case "wait":
		if timeout < 0 {
			return errors.New("--timeout cannot be negative")
		}
		if flagWasSet(flags, "resume") {
			if resume == "" || len(numbers) != 0 {
				return errors.New("--resume takes a watch ID and cannot be combined with a PR")
			}
			return waitForWatch(ctx, dialWatch, watchRequest{Resume: resume}, timeout, stdout, stderr)
		}
		if err := needOne(); err != nil {
			return err
		}
		if !validPRNumber(numbers[0]) {
			return errors.New("pull request number must be between 1 and 2147483647")
		}
		return waitForWatch(ctx, dialWatch, watchRequest{PR: numbers[0]}, timeout, stdout, stderr)
	case "watches":
		if len(numbers) > 0 {
			return errors.New("watches takes no arguments")
		}
		return listWatches(ctx, stdout)
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}

	g, err := newGH(ctx)
	if err != nil {
		return err
	}
	switch command {
	case "status":
		return status(ctx, g, stdout)
	case "check":
		_, pending, err := check(ctx, g, numbers[0], stdout)
		if err == nil && len(pending) > 0 {
			return errConflict
		}
		return err
	case "enqueue":
		return enqueue(ctx, g, numbers[0], allowPending, stdout)
	case "watch":
		return watch(ctx, g, numbers, interval, stdout, stderr)
	case "plan":
		if next {
			return nextCommand(ctx, g, milestone, stdout)
		}
		return planCommand(ctx, g, milestone, applyChanges, stdout)
	case "prune-runs":
		return pruneRuns(ctx, g, time.Now(), applyChanges, stdout)
	default:
		return autopilot(ctx, g, milestone, !applyChanges, once, interval, stdout)
	}
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func validPRNumber(n int) bool { return n >= 1 && n <= math.MaxInt32 }

func parseNumbers(args []string) ([]int, error) {
	var numbers []int
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a pull request number", arg)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

func status(ctx context.Context, g *gh, out io.Writer) error {
	snap, err := g.snapshot(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	fmt.Fprintf(out, "%s at %s, %d queued\n", snap.Branch, short(snap.BaseOid), len(snap.Entries))
	if len(snap.Entries) > 0 {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "POS\tSTATE\tPR\tHEAD\tENQUEUED\tTITLE")
		for _, e := range snap.Entries {
			fmt.Fprintf(w, "%d\t%s\t#%d\t%s\t%s\t%s\n", e.Position, e.State, e.PullRequest.Number,
				short(e.PullRequest.HeadRefOid), since(e.EnqueuedAt, now), e.PullRequest.Title)
		}
		w.Flush()
	}

	if len(snap.Pending) > 0 {
		fmt.Fprintf(out, "\nauto-merge enabled, waiting for checks to join the queue:\n")
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PR\tHEAD\tENABLED\tTITLE")
		for _, pr := range snap.Pending {
			fmt.Fprintf(w, "#%d\t%s\t%s\t%s\n", pr.Number, short(pr.HeadRefOid), since(pr.AutoMergeRequest.EnabledAt, now), pr.Title)
		}
		w.Flush()
	}
	return nil
}

func since(t, now time.Time) string {
	age := now.Sub(t).Truncate(time.Minute)
	text := strings.TrimSuffix(age.String(), "0s")
	if age < time.Minute {
		text = "<1m"
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format("2006-01-02 15:04Z"), text)
}

// check simulates merging the pull request over the queue and against each
// pull request waiting to join it, and reports the outcome. It returns
// errConflict, after reporting it, when the pull request conflicts with the
// base or the queue, and otherwise the conflicts with waiting pull requests.
func check(ctx context.Context, g *gh, number int, out io.Writer) (queueSnapshot, []pendingConflict, error) {
	snap, err := g.snapshot(ctx)
	if err != nil {
		return snap, nil, err
	}
	pr, err := g.pullRequest(ctx, number)
	if err != nil {
		return snap, nil, err
	}
	if pr.Merged || pr.State != "OPEN" {
		return snap, nil, fmt.Errorf("#%d is %s", number, strings.ToLower(pr.State))
	}

	objects, err := queueObjects(ctx, snap, pr.HeadRefOid)
	if err != nil {
		return snap, nil, err
	}
	defer objects.Close()
	candidate := queuedPR{Number: number, Head: pr.HeadRefOid}
	sim, err := simulate(ctx, objects, snap.BaseOid, queueOf(snap), candidate)
	if err != nil {
		return snap, nil, err
	}
	reportSimulation(out, snap, number, pr.HeadRefOid, sim)
	if !sim.clean() {
		return snap, nil, errConflict
	}

	pending, err := pendingConflicts(ctx, objects, snap, candidate)
	if err != nil {
		return snap, nil, err
	}
	reportPendingConflicts(out, number, pending)
	return snap, pending, nil
}

func reportSimulation(out io.Writer, snap queueSnapshot, number int, head string, sim simulation) {
	over := snap.Branch + " " + short(snap.BaseOid)
	for _, n := range sim.Applied {
		over += fmt.Sprintf(" + #%d", n)
	}
	fmt.Fprintf(out, "simulated #%d (%s) over %s\n", number, short(head), over)
	for _, n := range sim.Evicted {
		fmt.Fprintf(out, "note: #%d conflicts with an entry ahead of it; the queue will remove it, so it was left out\n", n)
	}

	switch {
	case sim.clean():
		fmt.Fprintf(out, "#%d merges cleanly\n", number)
		return
	case sim.ConflictWith == 0:
		fmt.Fprintf(out, "#%d conflicts with %s; rebase it onto origin/%s:\n", number, snap.Branch, snap.Branch)
	default:
		position := 0
		for _, e := range snap.Entries {
			if e.PullRequest.Number == sim.ConflictWith {
				position = e.Position
			}
		}
		fmt.Fprintf(out, "#%d conflicts with #%d (queue position %d):\n", number, sim.ConflictWith, position)
	}
	for _, path := range sim.Conflicts {
		fmt.Fprintf(out, "  %s\n", path)
	}
}

func enqueue(ctx context.Context, g *gh, number int, allowPending bool, out io.Writer) error {
	snap, pending, err := check(ctx, g, number, out)
	if errors.Is(err, errConflict) {
		fmt.Fprintf(out, "refusing to enqueue #%d\n", number)
	}
	if err != nil {
		return err
	}
	for _, e := range snap.Entries {
		if e.PullRequest.Number == number {
			fmt.Fprintf(out, "#%d is already queued at position %d (%s)\n", number, e.Position, e.State)
			return nil
		}
	}
	if err := admitPending(out, number, pending, allowPending); err != nil {
		return err
	}

	if err := g.enableAutoMerge(ctx, number); err != nil {
		return err
	}

	if !waitQueued(ctx, g, number, out) && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// waitQueued gives GitHub a moment to add the entry, which it does
// asynchronously and not at all until the pull request's own required checks
// pass.
func waitQueued(ctx context.Context, g *gh, number int, out io.Writer) bool {
	for attempt := 0; attempt < 5; attempt++ {
		pr, err := g.pullRequest(ctx, number)
		if err == nil && pr.MergeQueueEntry != nil {
			fmt.Fprintf(out, "#%d queued at position %d (%s)\n", number, pr.MergeQueueEntry.Position, pr.MergeQueueEntry.State)
			return true
		}
		if !sleep(ctx, 2*time.Second) {
			return false
		}
	}
	fmt.Fprintf(out, "#%d auto-merge enabled; it joins the queue once its required checks pass\n", number)
	return false
}

func watch(ctx context.Context, g *gh, numbers []int, interval time.Duration, out, errOut io.Writer) error {
	if len(numbers) == 0 {
		snap, err := g.snapshot(ctx)
		if err != nil {
			return err
		}
		for _, e := range snap.Entries {
			numbers = append(numbers, e.PullRequest.Number)
		}
		for _, pr := range snap.Pending {
			numbers = append(numbers, pr.Number)
		}
		if len(numbers) == 0 {
			fmt.Fprintln(out, "nothing is queued or waiting to join the queue")
			return nil
		}
	}

	explain := func(ctx context.Context, number int) ([]string, error) {
		var report strings.Builder
		if err := reportUnmergeable(ctx, g, number, &report); err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimSuffix(report.String(), "\n"), "\n"), nil
	}
	return follow(ctx, numbers, g.pullRequests, explain, interval, out, errOut)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func why(ctx context.Context, g *gh, number int, out io.Writer) error {
	pr, err := g.pullRequest(ctx, number)
	if err != nil {
		return err
	}
	obs := observe(pr)
	if obs.Merged || obs.Removal == nil {
		for _, line := range describe(obs) {
			fmt.Fprintln(out, line)
		}
		whyUnmergeable(ctx, g, obs, out)
		if !obs.Merged {
			fmt.Fprintf(out, "#%d has not been removed from the merge queue\n", number)
		}
		return nil
	}

	removal := *obs.Removal
	fmt.Fprintf(out, "#%d removed from the queue at %s: %s\n", number, removal.CreatedAt.Format(time.RFC3339), removal.Reason)
	if obs.InQueue || obs.blocked() {
		for _, line := range describe(obs) {
			fmt.Fprintf(out, "since then: %s\n", line)
		}
		whyUnmergeable(ctx, g, obs, out)
	}
	checks := removal.failedChecks()
	if len(checks) == 0 {
		if removal.BeforeCommit != nil {
			fmt.Fprintf(out, "no failed checks on merge-group commit %s\n", short(removal.BeforeCommit.Oid))
		}
		return nil
	}

	var runs []int64
	byRun := map[int64][]failedCheck{}
	for _, c := range checks {
		if _, ok := byRun[c.RunID]; !ok {
			runs = append(runs, c.RunID)
		}
		byRun[c.RunID] = append(byRun[c.RunID], c)
	}
	for _, run := range runs {
		var excerpts map[string][]string
		if run != 0 {
			log, err := g.failedRunLog(ctx, run)
			if err != nil {
				fmt.Fprintf(out, "could not read the failed log of run %d: %v\n", run, err)
			}
			excerpts = errorExcerpts(log, 8)
		}
		needArtifacts := run != 0 && !slices.ContainsFunc(slices.Collect(maps.Values(excerpts)), explainsTestFailure)
		for _, c := range byRun[run] {
			fmt.Fprintf(out, "\n%s %s\n  %s\n", strings.ToLower(c.Conclusion), c.Name, c.DetailsURL)
			for _, line := range excerpts[c.Name] {
				fmt.Fprintf(out, "  | %s\n", line)
			}
			delete(excerpts, c.Name)
		}
		// Jobs whose log name differs from their check name.
		var rest []string
		for job := range excerpts {
			rest = append(rest, job)
		}
		sort.Strings(rest)
		for _, job := range rest {
			fmt.Fprintf(out, "\n%s\n", job)
			for _, line := range excerpts[job] {
				fmt.Fprintf(out, "  | %s\n", line)
			}
		}
		if needArtifacts {
			reportArtifactFailures(ctx, g, run, out)
		}
	}
	return nil
}

// whyUnmergeable names the entry ahead that an unmergeable entry conflicts
// with. Naming it needs a checkout to simulate in, so a failure is reported
// rather than ending the explanation.
func whyUnmergeable(ctx context.Context, g *gh, obs observation, out io.Writer) {
	if !obs.unmergeable() {
		return
	}
	if err := reportUnmergeable(ctx, g, obs.Number, out); err != nil {
		fmt.Fprintf(out, "could not name the entry #%d conflicts with; run why from the Katl checkout: %v\n", obs.Number, err)
	}
}

// reportArtifactFailures prints go test failures from the diagnostics
// artifacts of a run, which hold the test log that the job log lacks.
func reportArtifactFailures(ctx context.Context, g *gh, run int64, out io.Writer) {
	logs, err := g.runArtifactLogs(ctx, run)
	if err != nil {
		fmt.Fprintf(out, "could not read the diagnostics artifacts of run %d: %v\n", run, err)
	}
	for _, name := range slices.Sorted(maps.Keys(logs)) {
		lines := testLogExcerpt(logs[name], 8)
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(out, "\ngo test failures in artifact %s\n", name)
		for _, line := range lines {
			fmt.Fprintf(out, "  | %s\n", line)
		}
	}
}

// applyScope refuses an --apply that would change pull requests without a
// milestone to confine it: a pass pushes to branches and dequeues entries, which
// should never reach every open pull request by default.
func applyScope(command, milestone string, apply bool) error {
	if apply && milestone == "" {
		return fmt.Errorf("%s --apply changes pull requests, so it needs --milestone to say which; run it without --apply to see what it would do", command)
	}
	return nil
}
