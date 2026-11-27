package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// observation is the merge-queue lifecycle state of one pull request at one
// poll.
type observation struct {
	Number      int
	Title       string
	Head        string
	Merged      bool
	MergeCommit string
	Closed      bool
	AutoMerge   bool
	InQueue     bool
	Position    int
	QueueState  string
	Removal     *removalEvent
	// FailedRequired lists the required checks that failed on the head,
	// which keep the pull request out of the queue until the head changes.
	FailedRequired []failedCheck
}

func observe(pr prResponse) observation {
	obs := observation{
		Number:         pr.Number,
		Title:          pr.Title,
		Head:           pr.HeadRefOid,
		Merged:         pr.Merged,
		Closed:         pr.State == "CLOSED",
		AutoMerge:      pr.AutoMergeRequest != nil,
		FailedRequired: pr.failedRequiredChecks(),
	}
	if pr.MergeCommit != nil {
		obs.MergeCommit = pr.MergeCommit.Oid
	}
	if pr.MergeQueueEntry != nil {
		obs.InQueue = true
		obs.Position = pr.MergeQueueEntry.Position
		obs.QueueState = pr.MergeQueueEntry.State
	}
	if n := len(pr.TimelineItems.Nodes); n > 0 {
		removal := pr.TimelineItems.Nodes[n-1]
		obs.Removal = &removal
	}
	return obs
}

// blocked reports whether failed required checks keep the pull request out
// of the queue.
func (o observation) blocked() bool {
	return !o.Merged && !o.Closed && !o.InQueue && len(o.FailedRequired) > 0
}

// unmergeable reports whether the queue found the entry conflicts with one
// ahead of it.
func (o observation) unmergeable() bool {
	return o.InQueue && o.QueueState == "UNMERGEABLE"
}

// settled reports whether the pull request has left the queue for good: it
// merged, closed, cannot join until its head is fixed, or is neither queued
// nor waiting to be.
func (o observation) settled() bool {
	return o.Merged || o.Closed || o.blocked() || (!o.InQueue && !o.AutoMerge)
}

// removedSince reports a removal newer than prev's. GitHub also records a
// removal when an entry merges, which is not one (see mergeRecord).
func (o observation) removedSince(prev observation) bool {
	if o.Merged || o.Removal == nil || o.Removal.mergeRecord() {
		return false
	}
	return prev.Removal == nil || o.Removal.CreatedAt.After(prev.Removal.CreatedAt)
}

// describe is what watch prints when it first sees a pull request.
func describe(o observation) []string {
	switch {
	case o.Merged:
		return []string{fmt.Sprintf("#%d merged as %s", o.Number, short(o.MergeCommit))}
	case o.Closed:
		return []string{fmt.Sprintf("#%d closed without merging", o.Number)}
	case o.InQueue:
		return []string{fmt.Sprintf("#%d %s at position %d", o.Number, o.QueueState, o.Position)}
	case o.blocked():
		return blockedLines(o)
	case o.AutoMerge:
		return []string{fmt.Sprintf("#%d auto-merge enabled, waiting for checks to join the queue", o.Number)}
	case o.Removal != nil:
		return []string{fmt.Sprintf("#%d not queued; last removed %s: %s", o.Number, o.Removal.CreatedAt.Format(time.RFC3339), o.Removal.Reason)}
	default:
		return []string{fmt.Sprintf("#%d not queued and auto-merge is off", o.Number)}
	}
}

func blockedLines(o observation) []string {
	lines := []string{fmt.Sprintf("#%d required checks failed on head %s; it cannot join the queue until the head is fixed", o.Number, short(o.Head))}
	for _, check := range o.FailedRequired {
		lines = append(lines, fmt.Sprintf("#%d   %s %s %s", o.Number, strings.ToLower(check.Conclusion), check.Name, check.DetailsURL))
	}
	return lines
}

// transitions returns the lines watch prints for the change from prev to cur.
func transitions(prev, cur observation) []string {
	n := cur.Number
	var lines []string
	if cur.removedSince(prev) {
		lines = append(lines, removalLines(n, *cur.Removal)...)
	}

	switch {
	case cur.Merged && !prev.Merged:
		lines = append(lines, fmt.Sprintf("#%d merged as %s", n, short(cur.MergeCommit)))
	case cur.Closed && !prev.Closed:
		lines = append(lines, fmt.Sprintf("#%d closed without merging", n))
	case cur.InQueue && !prev.InQueue:
		lines = append(lines, fmt.Sprintf("#%d entered the queue at position %d (%s)", n, cur.Position, cur.QueueState))
	case cur.InQueue && cur.QueueState != prev.QueueState:
		line := fmt.Sprintf("#%d %s at position %d", n, cur.QueueState, cur.Position)
		if cur.QueueState == "UNMERGEABLE" {
			line += "; it conflicts with an entry ahead and will be removed when the queue reaches it"
		}
		lines = append(lines, line)
	case cur.blocked() && !prev.blocked():
		lines = append(lines, blockedLines(cur)...)
	case !cur.InQueue && prev.InQueue && !cur.removedSince(prev):
		lines = append(lines, fmt.Sprintf("#%d left the queue", n))
	case !cur.InQueue && !cur.AutoMerge && prev.AutoMerge && !prev.InQueue:
		lines = append(lines, fmt.Sprintf("#%d auto-merge disabled before it joined the queue", n))
	}
	return lines
}

func removalLines(n int, r removalEvent) []string {
	reason := r.Reason
	if reason == "" {
		reason = "no reason given"
	}
	lines := []string{fmt.Sprintf("#%d removed from the queue: %s", n, reason)}
	for _, check := range r.failedChecks() {
		lines = append(lines, fmt.Sprintf("#%d   %s %s %s", n, strings.ToLower(check.Conclusion), check.Name, check.DetailsURL))
	}
	return lines
}

// follow polls the pull requests and prints a line for each state change
// until every one has settled. A failed poll is reported and retried, so a
// transient GitHub or network failure does not end the watch. It returns
// errConflict when any pull request settled without merging. When a pull
// request becomes unmergeable, explain, if set, says what it conflicts with.
func follow(ctx context.Context, numbers []int, poll func(context.Context, []int) (map[int]prResponse, error), explain func(context.Context, int) ([]string, error), interval time.Duration, out, errOut io.Writer) error {
	seen := map[int]observation{}
	unmerged := false
	for {
		prs, err := poll(ctx, numbers)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(errOut, "%s katl-queue: %v; retrying in %s\n", stamp(), err, interval)
		} else {
			var active []int
			for _, n := range numbers {
				cur := observe(prs[n])
				lines := describe(cur)
				prev, ok := seen[n]
				if ok {
					lines = transitions(prev, cur)
				}
				if explain != nil && cur.unmergeable() && (!ok || !prev.unmergeable() || prev.Head != cur.Head) {
					cause, err := explain(ctx, n)
					if err != nil {
						fmt.Fprintf(errOut, "%s katl-queue: could not name the entry #%d conflicts with: %v\n", stamp(), n, err)
					}
					lines = append(lines, cause...)
				}
				for _, line := range lines {
					fmt.Fprintf(out, "%s %s\n", stamp(), line)
				}
				seen[n] = cur

				switch {
				case !cur.settled():
					active = append(active, n)
				case !cur.Merged:
					unmerged = true
				}
			}
			numbers = active
		}

		if len(numbers) == 0 {
			if unmerged {
				return errConflict
			}
			return nil
		}
		if !sleep(ctx, interval) {
			return ctx.Err()
		}
	}
}

func stamp() string {
	return time.Now().Format("15:04:05")
}

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
