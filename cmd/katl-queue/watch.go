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
	Merged      bool
	MergeCommit string
	Closed      bool
	AutoMerge   bool
	InQueue     bool
	Position    int
	QueueState  string
	Removal     *removalEvent
}

func observe(pr prResponse) observation {
	obs := observation{
		Number:    pr.Number,
		Title:     pr.Title,
		Merged:    pr.Merged,
		Closed:    pr.State == "CLOSED",
		AutoMerge: pr.AutoMergeRequest != nil,
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

// settled reports whether the pull request has left the queue for good: it
// merged, closed, or is neither queued nor waiting to be.
func (o observation) settled() bool {
	return o.Merged || o.Closed || (!o.InQueue && !o.AutoMerge)
}

// removedSince reports a removal newer than prev's. GitHub also records a
// removal, with reason "merged", when an entry merges; that is not one.
func (o observation) removedSince(prev observation) bool {
	if o.Merged || o.Removal == nil || o.Removal.Reason == "merged" {
		return false
	}
	return prev.Removal == nil || o.Removal.CreatedAt.After(prev.Removal.CreatedAt)
}

// describe is the line watch prints when it first sees a pull request.
func describe(o observation) string {
	switch {
	case o.Merged:
		return fmt.Sprintf("#%d merged as %s", o.Number, short(o.MergeCommit))
	case o.Closed:
		return fmt.Sprintf("#%d closed without merging", o.Number)
	case o.InQueue:
		return fmt.Sprintf("#%d %s at position %d", o.Number, o.QueueState, o.Position)
	case o.AutoMerge:
		return fmt.Sprintf("#%d auto-merge enabled, waiting for checks to join the queue", o.Number)
	case o.Removal != nil:
		return fmt.Sprintf("#%d not queued; last removed %s: %s", o.Number, o.Removal.CreatedAt.Format(time.RFC3339), o.Removal.Reason)
	default:
		return fmt.Sprintf("#%d not queued and auto-merge is off", o.Number)
	}
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
// errConflict when any pull request settled without merging.
func follow(ctx context.Context, numbers []int, poll func(context.Context, []int) (map[int]prResponse, error), interval time.Duration, out, errOut io.Writer) error {
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
				lines := []string{describe(cur)}
				if prev, ok := seen[n]; ok {
					lines = transitions(prev, cur)
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
