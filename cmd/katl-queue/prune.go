package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	SHA     string
	Created time.Time
}

// groupRef is a merge-group branch and the commit it points at.
type groupRef struct {
	Name string // without the refs/heads/ prefix
	SHA  string
}

// errRunCompleted reports that a run finished before it could be cancelled.
var errRunCompleted = errors.New("run already completed")

// runSource is the GitHub state prune reads and the one change it makes.
type runSource interface {
	// ActiveGroupRuns lists the unfinished merge_group runs.
	ActiveGroupRuns(ctx context.Context) ([]groupRun, error)
	// GroupRefs lists the merge-group branches that exist.
	GroupRefs(ctx context.Context) ([]groupRef, error)
	// QueueEntries counts the entries of the merge queue.
	QueueEntries(ctx context.Context) (int, error)
	// CancelRun returns errRunCompleted when the run had already finished.
	CancelRun(ctx context.Context, id int64) error
}

// supersededRuns selects the runs whose merge group no longer exists. GitHub
// deletes a group's ref when the group is invalidated or merged, and gives
// every rebuilt group a new ref, so a run whose ref is absent, or present at a
// different commit, is for a group the queue will never merge.
func supersededRuns(runs []groupRun, refs []groupRef, now time.Time) []groupRun {
	live := make(map[string]string, len(refs))
	for _, ref := range refs {
		live[ref.Name] = ref.SHA
	}

	var out []groupRun
	for _, r := range runs {
		if !strings.HasPrefix(r.Branch, groupRefPrefix) || now.Sub(r.Created) < pruneGrace {
			continue
		}
		if sha, ok := live[r.Branch]; !ok || sha != r.SHA {
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
	if len(refs) == 0 && len(stale) > 0 {
		// An empty listing beside a populated queue means the listing is
		// wrong, and every live group would look superseded.
		entries, err := src.QueueEntries(ctx)
		if err != nil {
			return err
		}
		if entries > 0 {
			return fmt.Errorf("no merge-group refs are listed but the merge queue holds %d entries; cancelling nothing", entries)
		}
	}
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
		err := src.CancelRun(ctx, r.ID)
		if errors.Is(err, errRunCompleted) {
			fmt.Fprintf(out, "  run %d had already completed\n", r.ID)
		} else if err != nil {
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
		raw, err := g.api(ctx, g.repoPath()+"/actions/runs?event=merge_group&per_page=100&status="+status)
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
			HeadSHA    string    `json:"head_sha"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode workflow runs: %w", err)
	}
	var runs []groupRun
	for _, r := range resp.WorkflowRuns {
		runs = append(runs, groupRun{ID: r.ID, Branch: r.HeadBranch, SHA: r.HeadSHA, Created: r.CreatedAt})
	}
	return runs, nil
}

func (g *gh) GroupRefs(ctx context.Context) ([]groupRef, error) {
	raw, err := g.api(ctx, "--paginate", g.repoPath()+"/git/matching-refs/heads/"+groupRefPrefix)
	if err != nil {
		return nil, err
	}
	return decodeGroupRefs(raw)
}

// decodeGroupRefs parses the matching-refs listing. --paginate prints one JSON
// array per page, so the input may hold several.
func decodeGroupRefs(raw []byte) ([]groupRef, error) {
	var refs []groupRef
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var page []struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("decode group refs: %w", err)
		}
		for _, r := range page {
			refs = append(refs, groupRef{Name: strings.TrimPrefix(r.Ref, "refs/heads/"), SHA: r.Object.SHA})
		}
	}
	return refs, nil
}

func (g *gh) QueueEntries(ctx context.Context) (int, error) {
	snap, err := g.snapshot(ctx)
	return len(snap.Entries), err
}

func (g *gh) CancelRun(ctx context.Context, id int64) error {
	_, err := runGH(ctx, "run", "cancel", fmt.Sprint(id), "--repo", g.repo())
	if err == nil {
		return nil
	}

	// A run can finish between the listing and the cancel, which GitHub
	// refuses with a 409. Re-read the run rather than match the message.
	raw, readErr := g.api(ctx, fmt.Sprintf("%s/actions/runs/%d", g.repoPath(), id), "--jq", ".status")
	if readErr == nil && strings.TrimSpace(string(raw)) == "completed" {
		return errRunCompleted
	}
	return err
}
