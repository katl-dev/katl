package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const allowPendingFlag = "allow-pending-conflict"

// queueOf lists the merge queue entries of snap in queue order.
func queueOf(snap queueSnapshot) []queuedPR {
	queue := make([]queuedPR, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		queue = append(queue, queuedPR{Number: e.PullRequest.Number, Head: e.PullRequest.HeadRefOid})
	}
	return queue
}

// pendingPRs lists the pull requests whose auto-merge will put them in this
// queue once their checks pass. Those bound for another branch join a
// different queue.
func pendingPRs(snap queueSnapshot) []queuedPR {
	var pending []queuedPR
	for _, pr := range snap.Pending {
		if pr.BaseRefName == snap.Branch {
			pending = append(pending, queuedPR{Number: pr.Number, Head: pr.HeadRefOid})
		}
	}
	return pending
}

// queueObjects fetches the base, queued and pending heads of snap and the
// extra commits into a throwaway object store.
func (g *gh) queueObjects(ctx context.Context, snap queueSnapshot, extra ...string) (*scratchObjects, error) {
	oids := append([]string{snap.BaseOid}, extra...)
	for _, pr := range append(queueOf(snap), pendingPRs(snap)...) {
		oids = append(oids, pr.Head)
	}
	slices.Sort(oids)
	oids = slices.Compact(oids)

	objects, err := newScratchObjects(".", os.Environ())
	if err != nil {
		return nil, err
	}
	if err := objects.fetch(ctx, g.remoteURL(), oids...); err != nil {
		objects.Close()
		return nil, err
	}
	return objects, nil
}

// pendingConflict is a pull request waiting to join the queue that conflicts
// with the candidate, so the queue will remove whichever of the two joins
// second.
type pendingConflict struct {
	Number    int
	Conflicts []string
}

// pendingConflicts simulates the candidate against each pull request waiting
// to join the queue. Which of the two joins first depends on when their checks
// finish, so both orders are simulated over the current queue. A pending pull
// request that conflicts with the queue itself is the queue's to remove and is
// not reported. A candidate already queued is ahead of every pending pull
// request, so a conflict removes the pending one and none is reported.
func pendingConflicts(ctx context.Context, objects *scratchObjects, snap queueSnapshot, candidate queuedPR) ([]pendingConflict, error) {
	queue := queueOf(snap)
	if slices.ContainsFunc(queue, func(q queuedPR) bool { return q.Number == candidate.Number }) {
		return nil, nil
	}

	var found []pendingConflict
	for _, pending := range pendingPRs(snap) {
		if pending.Number == candidate.Number {
			continue
		}
		for _, order := range [][2]queuedPR{{pending, candidate}, {candidate, pending}} {
			ahead, behind := order[0], order[1]
			sim, err := simulate(ctx, objects, snap.BaseOid, append(slices.Clone(queue), ahead), behind)
			if err != nil {
				return nil, err
			}
			if !sim.clean() && sim.ConflictWith == ahead.Number {
				found = append(found, pendingConflict{Number: pending.Number, Conflicts: sim.Conflicts})
				break
			}
		}
	}
	return found, nil
}

func reportPendingConflicts(out io.Writer, number int, conflicts []pendingConflict) {
	for _, c := range conflicts {
		fmt.Fprintf(out, "#%d conflicts with #%d, which has auto-merge enabled and has not joined the queue; whichever joins second will be removed:\n", number, c.Number)
		for _, path := range c.Conflicts {
			fmt.Fprintf(out, "  %s\n", path)
		}
	}
}

// admitPending decides whether enqueue may go ahead despite conflicts with
// pull requests waiting to join the queue. It returns errConflict, after
// reporting the refusal, unless allow accepts the risk.
func admitPending(out io.Writer, number int, conflicts []pendingConflict, allow bool) error {
	if len(conflicts) == 0 {
		return nil
	}
	var names []string
	for _, c := range conflicts {
		names = append(names, fmt.Sprintf("#%d", c.Number))
	}
	if allow {
		fmt.Fprintf(out, "enqueueing #%d anyway: --%s accepts that the queue removes whichever of #%d and %s joins second\n",
			number, allowPendingFlag, number, strings.Join(names, ", "))
		return nil
	}
	fmt.Fprintf(out, "refusing to enqueue #%d: rebase it after %s merges, or pass --%s to accept that the queue removes whichever joins second\n",
		number, strings.Join(names, ", "), allowPendingFlag)
	return errConflict
}

// explainUnmergeable reports which entry ahead of a queued pull request it
// conflicts with, and in which files, by simulating it over the entries ahead
// of it.
func explainUnmergeable(ctx context.Context, objects *scratchObjects, snap queueSnapshot, number int, out io.Writer) error {
	queue := queueOf(snap)
	i := slices.IndexFunc(queue, func(q queuedPR) bool { return q.Number == number })
	if i < 0 {
		return fmt.Errorf("#%d is not in the merge queue", number)
	}
	sim, err := simulate(ctx, objects, snap.BaseOid, queue, queue[i])
	if err != nil {
		return err
	}
	reportSimulation(out, snap, number, queue[i].Head, sim)
	return nil
}

// reportUnmergeable reads the queue and reports which entry ahead of the
// queued pull request it conflicts with.
func reportUnmergeable(ctx context.Context, g *gh, number int, out io.Writer) error {
	snap, err := g.snapshot(ctx)
	if err != nil {
		return err
	}
	objects, err := g.queueObjects(ctx, snap)
	if err != nil {
		return err
	}
	defer objects.Close()
	return explainUnmergeable(ctx, objects, snap, number, out)
}
