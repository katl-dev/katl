package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	groupRefPrefix = "gh-readonly-queue/"
	// pruneGrace keeps a run that has only just started out of reach, so a
	// group ref the API does not list yet is never mistaken for a deleted one.
	pruneGrace = 2 * time.Minute
)

// groupRun is a merge_group workflow run that has not finished.
type groupRun struct {
	ID      int64
	Branch  string
	Created time.Time
}

// runSource is the GitHub state prune reads and the one change it makes.
type runSource interface {
	// ActiveGroupRuns lists the unfinished merge_group runs.
	ActiveGroupRuns(ctx context.Context) ([]groupRun, error)
	// GroupRefs lists the merge-group branches that exist, without the
	// refs/heads/ prefix.
	GroupRefs(ctx context.Context) ([]string, error)
	CancelRun(ctx context.Context, id int64) error
}

// supersededRuns selects the runs whose merge group no longer exists. GitHub
// deletes a group's ref when the group is invalidated or merged, and gives
// every rebuilt group a new ref, so a run whose ref is absent is for a group
// the queue will never merge.
func supersededRuns(runs []groupRun, refs []string, now time.Time) []groupRun {
	var out []groupRun
	for _, r := range runs {
		if strings.HasPrefix(r.Branch, groupRefPrefix) && !slices.Contains(refs, r.Branch) && now.Sub(r.Created) >= pruneGrace {
			out = append(out, r)
		}
	}
	return out
}

// pruneRuns cancels the unfinished merge_group runs of groups that no longer
// exist, or reports them when apply is false. It lists the runs before the
// refs: a run exists only once its ref does, so a ref missing from the later
// listing was deleted rather than not yet created. A failure to list either
// cancels nothing, since an unknown queue is not an empty one.
func pruneRuns(ctx context.Context, src runSource, now time.Time, apply bool, out io.Writer) error {
	runs, err := src.ActiveGroupRuns(ctx)
	if err != nil {
		return err
	}
	refs, err := src.GroupRefs(ctx)
	if err != nil {
		return err
	}

	stale := supersededRuns(runs, refs, now)
	if len(stale) == 0 {
		fmt.Fprintf(out, "%d unfinished merge-group runs, all for groups in the queue\n", len(runs))
		return nil
	}

	var errs []error
	for _, r := range stale {
		verb := "would cancel"
		if apply {
			verb = "cancelling"
		}
		fmt.Fprintf(out, "%s run %d: %s no longer exists\n", verb, r.ID, r.Branch)
		if !apply {
			continue
		}
		if err := src.CancelRun(ctx, r.ID); err != nil {
			fmt.Fprintf(out, "  failed: %v\n", err)
			errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (g *gh) repoPath() string { return "repos/" + g.owner + "/" + g.name }

func (g *gh) ActiveGroupRuns(ctx context.Context) ([]groupRun, error) {
	var runs []groupRun
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested"} {
		raw, err := runGH(ctx, "api", g.repoPath()+"/actions/runs?event=merge_group&per_page=100&status="+status)
		if err != nil {
			return nil, err
		}
		found, err := decodeGroupRuns(raw)
		if err != nil {
			return nil, err
		}
		runs = append(runs, found...)
	}
	return runs, nil
}

func decodeGroupRuns(raw []byte) ([]groupRun, error) {
	var resp struct {
		WorkflowRuns []struct {
			ID         int64     `json:"id"`
			HeadBranch string    `json:"head_branch"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode workflow runs: %w", err)
	}
	var runs []groupRun
	for _, r := range resp.WorkflowRuns {
		runs = append(runs, groupRun{ID: r.ID, Branch: r.HeadBranch, Created: r.CreatedAt})
	}
	return runs, nil
}

func (g *gh) GroupRefs(ctx context.Context) ([]string, error) {
	out, err := runGH(ctx, "api", "--paginate", g.repoPath()+"/git/matching-refs/heads/"+groupRefPrefix, "--jq", ".[].ref")
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, ref := range strings.Fields(string(out)) {
		refs = append(refs, strings.TrimPrefix(ref, "refs/heads/"))
	}
	return refs, nil
}

func (g *gh) CancelRun(ctx context.Context, id int64) error {
	_, err := runGH(ctx, "run", "cancel", fmt.Sprint(id), "--repo", g.owner+"/"+g.name)
	return err
}
