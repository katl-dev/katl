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
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `usage: katl-queue <command> [arguments]

Commands:
  status            list merge-queue entries and PRs waiting to join
  check PR          simulate merging PR over main and the queue ahead of it
  enqueue PR        check PR, then enable auto-merge so it joins the queue
  watch [PR...]     report queue state changes until the PRs merge or leave
                    (default: everything queued or with auto-merge enabled)
  why PR            explain why PR was removed from the queue

Exit status is 1 when check finds a conflict, enqueue refuses, or a watched PR
leaves the queue without merging, and 2 on any other error.
`

// errConflict marks an outcome that has already been reported to the
// operator and only needs a failing exit status.
var errConflict = errors.New("conflict")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, errConflict):
		os.Exit(1)
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "katl-queue:", err)
		os.Exit(2)
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
	interval := time.Minute
	if command == "watch" {
		flags.DurationVar(&interval, "interval", interval, "time between polls")
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
	case "check", "enqueue", "why":
		if err := needOne(); err != nil {
			return err
		}
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
		_, err := check(ctx, g, numbers[0], stdout)
		return err
	case "enqueue":
		return enqueue(ctx, g, numbers[0], stdout)
	case "watch":
		return watch(ctx, g, numbers, interval, stdout, stderr)
	default:
		return why(ctx, g, numbers[0], stdout)
	}
}

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

// check simulates merging the pull request and reports the outcome. It
// returns errConflict, after reporting it, when the simulation conflicts.
func check(ctx context.Context, g *gh, number int, out io.Writer) (queueSnapshot, error) {
	snap, err := g.snapshot(ctx)
	if err != nil {
		return snap, err
	}
	pr, err := g.pullRequest(ctx, number)
	if err != nil {
		return snap, err
	}
	if pr.Merged || pr.State != "OPEN" {
		return snap, fmt.Errorf("#%d is %s", number, strings.ToLower(pr.State))
	}

	queue := make([]queuedPR, 0, len(snap.Entries))
	oids := []string{snap.BaseOid, pr.HeadRefOid}
	for _, e := range snap.Entries {
		queue = append(queue, queuedPR{Number: e.PullRequest.Number, Head: e.PullRequest.HeadRefOid})
		oids = append(oids, e.PullRequest.HeadRefOid)
	}

	objects, err := newScratchObjects(".", os.Environ())
	if err != nil {
		return snap, err
	}
	defer objects.Close()
	if err := objects.fetch(ctx, "origin", oids...); err != nil {
		return snap, err
	}
	sim, err := simulate(ctx, objects, snap.BaseOid, queue, queuedPR{Number: number, Head: pr.HeadRefOid})
	if err != nil {
		return snap, err
	}

	reportSimulation(out, snap, number, pr.HeadRefOid, sim)
	if !sim.clean() {
		return snap, errConflict
	}
	return snap, nil
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

func enqueue(ctx context.Context, g *gh, number int, out io.Writer) error {
	snap, err := check(ctx, g, number, out)
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

	if err := g.enableAutoMerge(ctx, number); err != nil {
		return err
	}

	// GitHub adds the entry asynchronously, and not at all until the pull
	// request's own required checks pass.
	for attempt := 0; attempt < 5; attempt++ {
		pr, err := g.pullRequest(ctx, number)
		if err == nil && pr.MergeQueueEntry != nil {
			fmt.Fprintf(out, "#%d queued at position %d (%s)\n", number, pr.MergeQueueEntry.Position, pr.MergeQueueEntry.State)
			return nil
		}
		if !sleep(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
	fmt.Fprintf(out, "#%d auto-merge enabled; it joins the queue behind %d entries once its required checks pass\n", number, len(snap.Entries))
	return nil
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

	return follow(ctx, numbers, g.pullRequests, interval, out, errOut)
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
		fmt.Fprintln(out, describe(obs))
		if !obs.Merged {
			fmt.Fprintf(out, "#%d has not been removed from the merge queue\n", number)
		}
		return nil
	}

	removal := *obs.Removal
	fmt.Fprintf(out, "#%d removed from the queue at %s: %s\n", number, removal.CreatedAt.Format(time.RFC3339), removal.Reason)
	if obs.InQueue {
		fmt.Fprintf(out, "since then: %s\n", describe(obs))
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
	}
	return nil
}
