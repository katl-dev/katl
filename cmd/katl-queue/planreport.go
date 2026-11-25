package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

func reportHeld(out io.Writer, p plan) {
	if len(p.Enqueue) > 0 {
		fmt.Fprintf(out, "enqueue order: %s\n", numbersText(p.Enqueue))
	}
	for _, h := range p.Held {
		fmt.Fprintf(out, "held #%d: %s\n", h.Number, h.Reason)
		for _, path := range h.Files {
			fmt.Fprintf(out, "  %s\n", path)
		}
	}
}

// planCommand reports the order to enqueue ready pull requests in and, with
// apply set, enables auto-merge on them in that order.
func planCommand(ctx context.Context, g *gh, milestone string, apply bool, out io.Writer) error {
	w, err := g.world(ctx, milestone)
	if err != nil {
		return err
	}
	objects, err := newScratchObjects(".", os.Environ())
	if err != nil {
		return err
	}
	defer objects.Close()
	objects.config = httpsCredential
	if err := objects.fetch(ctx, g.remoteURL(), worldOids(w)...); err != nil {
		return err
	}

	cands := candidates(w)
	p, err := planFor(ctx, objects, w, cands)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s at %s, %d queued, %d candidates\n", w.Branch, short(w.Base), len(w.Queue), len(cands))
	reportHeld(out, p)
	if len(p.Enqueue) == 0 {
		fmt.Fprintln(out, "nothing to enqueue")
		return nil
	}
	if !apply {
		fmt.Fprintln(out, "dry run: pass --apply to enqueue in this order")
		return nil
	}

	for _, n := range p.Enqueue {
		if i := indexCandidate(cands, n); cands[i].AutoMerge {
			continue
		}
		if err := g.enableAutoMerge(ctx, n); err != nil {
			return err
		}
		waitQueued(ctx, g, n, out)
	}
	return nil
}

func indexCandidate(cands []candidate, n int) int {
	for i, c := range cands {
		if c.Number == n {
			return i
		}
	}
	return -1
}

func nextCommand(ctx context.Context, g *gh, milestone string, out io.Writer) error {
	issues, refs, err := g.milestoneIssues(ctx, milestone)
	if err != nil {
		return err
	}
	ready := startable(issues, refs)
	if len(ready) == 0 {
		fmt.Fprintln(out, "no startable issues")
		return nil
	}
	for _, issue := range ready {
		fmt.Fprintf(out, "#%d\t%s\t%s\n", issue.Number, strings.Join(issue.Labels, ","), issue.Title)
	}
	return nil
}
