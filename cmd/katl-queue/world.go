package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// openPR is an open pull request as the planner and autopilot see it.
type openPR struct {
	candidate
	ID         string
	Title      string
	HeadRef    string
	BaseRef    string
	Milestone  string
	Draft      bool
	Fork       bool
	InQueue    bool
	NeedsLabel bool // carries the needs-rebase label
	Removal    *removalMark
	// Reported lists the removal timestamps already explained by a
	// katl-queue comment.
	Reported []string
}

// removalMark is the latest removal from the merge queue.
type removalMark struct {
	At     time.Time
	Reason string
}

// queueEntryState is a merge-queue entry and its queue-assigned state.
type queueEntryState struct {
	Number int
	State  string
}

// world is everything planning and autopilot decide from.
type world struct {
	Branch  string
	Base    string
	Queue   []queuedPR
	Entries []queueEntryState
	PRs     []openPR
	Refs    map[int]externalRef
}

const needsRebaseLabel = "needs-rebase"

// ejectionMarker tags a comment that explains a removal, so the same removal
// is explained once.
func ejectionMarker(at time.Time) string {
	return "<!-- katl-queue:ejection " + at.UTC().Format(time.RFC3339) + " -->"
}

// eligible reports whether katl-queue may manage the pull request. Release
// branches and pull requests targeting them are never touched, and forks
// cannot be pushed to.
func (w world) eligible(pr openPR) bool {
	return !pr.Draft && !pr.Fork && pr.BaseRef == w.Branch &&
		!strings.HasPrefix(pr.BaseRef, "release/") && !strings.HasPrefix(pr.HeadRef, "release/")
}

const worldQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name target { oid } }
    mergeQueue {
      entries(first: 100) {
        nodes { position state pullRequest { number headRefOid closingIssuesReferences(first: 20) { nodes { number } } } }
      }
    }
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
        id number title body isDraft isCrossRepository isInMergeQueue
        headRefOid headRefName baseRefName createdAt
        milestone { title }
        labels(first: 30) { nodes { name } }
        closingIssuesReferences(first: 20) { nodes { number } }
        autoMergeRequest { enabledAt }
        commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }
        comments(last: 50) { nodes { body } }
        timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
          nodes { ... on RemovedFromMergeQueueEvent { createdAt reason } }
        }
      }
    }
  }
}`

type numberNodes struct {
	Nodes []struct {
		Number int `json:"number"`
	} `json:"nodes"`
}

func (n numberNodes) numbers() []int {
	var out []int
	for _, node := range n.Nodes {
		out = append(out, node.Number)
	}
	return out
}

// world reads the queue and open pull requests, and resolves each
// candidate's declared dependencies. A non-empty milestone limits the pull
// requests to that milestone.
func (g *gh) world(ctx context.Context, milestone string) (world, error) {
	var resp struct {
		Repository struct {
			DefaultBranchRef struct {
				Name   string `json:"name"`
				Target struct {
					Oid string `json:"oid"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
			MergeQueue *struct {
				Entries struct {
					Nodes []struct {
						State       string `json:"state"`
						PullRequest struct {
							Number                  int         `json:"number"`
							HeadRefOid              string      `json:"headRefOid"`
							ClosingIssuesReferences numberNodes `json:"closingIssuesReferences"`
						} `json:"pullRequest"`
					} `json:"nodes"`
				} `json:"entries"`
			} `json:"mergeQueue"`
			PullRequests struct {
				Nodes []struct {
					ID                      string      `json:"id"`
					Number                  int         `json:"number"`
					Title                   string      `json:"title"`
					Body                    string      `json:"body"`
					IsDraft                 bool        `json:"isDraft"`
					IsCrossRepository       bool        `json:"isCrossRepository"`
					IsInMergeQueue          bool        `json:"isInMergeQueue"`
					HeadRefOid              string      `json:"headRefOid"`
					HeadRefName             string      `json:"headRefName"`
					BaseRefName             string      `json:"baseRefName"`
					CreatedAt               time.Time   `json:"createdAt"`
					ClosingIssuesReferences numberNodes `json:"closingIssuesReferences"`
					Milestone               *struct {
						Title string `json:"title"`
					} `json:"milestone"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					AutoMergeRequest *struct{} `json:"autoMergeRequest"`
					Commits          struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
								CommittedDate time.Time `json:"committedDate"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
					Comments struct {
						Nodes []struct {
							Body string `json:"body"`
						} `json:"nodes"`
					} `json:"comments"`
					TimelineItems struct {
						Nodes []struct {
							CreatedAt time.Time `json:"createdAt"`
							Reason    string    `json:"reason"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, worldQuery, &resp); err != nil {
		return world{}, err
	}

	repo := resp.Repository
	w := world{Branch: repo.DefaultBranchRef.Name, Base: repo.DefaultBranchRef.Target.Oid}
	if repo.MergeQueue == nil {
		return w, fmt.Errorf("%s/%s has no merge queue on %s", g.owner, g.name, w.Branch)
	}
	for _, e := range repo.MergeQueue.Entries.Nodes {
		pr := e.PullRequest
		w.Queue = append(w.Queue, queuedPR{Number: pr.Number, Head: pr.HeadRefOid, Closes: pr.ClosingIssuesReferences.numbers()})
		w.Entries = append(w.Entries, queueEntryState{Number: pr.Number, State: e.State})
	}

	for _, n := range repo.PullRequests.Nodes {
		pr := openPR{
			candidate: candidate{
				Number:    n.Number,
				Head:      n.HeadRefOid,
				Created:   n.CreatedAt,
				Closes:    n.ClosingIssuesReferences.numbers(),
				AutoMerge: n.AutoMergeRequest != nil,
			},
			ID: n.ID, Title: n.Title, HeadRef: n.HeadRefName, BaseRef: n.BaseRefName,
			Draft: n.IsDraft, Fork: n.IsCrossRepository, InQueue: n.IsInMergeQueue,
		}
		if n.Milestone != nil {
			pr.Milestone = n.Milestone.Title
		}
		if milestone != "" && pr.Milestone != milestone {
			continue
		}
		for _, l := range n.Labels.Nodes {
			pr.Labels = append(pr.Labels, l.Name)
		}
		pr.NeedsLabel = slices.Contains(pr.Labels, needsRebaseLabel)
		if len(n.Commits.Nodes) > 0 {
			if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
				pr.ChecksFailed = rollup.State == "FAILURE" || rollup.State == "ERROR"
			}
		}
		if len(n.TimelineItems.Nodes) > 0 {
			last := n.TimelineItems.Nodes[len(n.TimelineItems.Nodes)-1]
			pr.Removal = &removalMark{At: last.CreatedAt, Reason: last.Reason}
			if last.Reason != "merged" && len(n.Commits.Nodes) > 0 {
				pr.Ejected = !n.Commits.Nodes[0].Commit.CommittedDate.After(last.CreatedAt)
			}
		}
		for _, c := range n.Comments.Nodes {
			for _, line := range strings.Split(c.Body, "\n") {
				if rest, ok := strings.CutPrefix(line, "<!-- katl-queue:ejection "); ok {
					pr.Reported = append(pr.Reported, strings.TrimSuffix(rest, " -->"))
				}
			}
		}
		pr.Deps = declaredDeps(n.Body)
		w.PRs = append(w.PRs, pr)
	}

	if err := g.loadDependencies(ctx, &w); err != nil {
		return w, err
	}
	return w, nil
}

// loadDependencies adds the native blocked-by dependencies of each eligible
// pull request and of the issues it closes, and reads the state of every
// dependency that is not a candidate.
func (g *gh) loadDependencies(ctx context.Context, w *world) error {
	for i := range w.PRs {
		pr := &w.PRs[i]
		if !w.eligible(*pr) || pr.InQueue {
			continue
		}
		for _, n := range append([]int{pr.Number}, pr.Closes...) {
			deps, err := g.blockedBy(ctx, n)
			if err != nil {
				return err
			}
			pr.Deps = append(pr.Deps, deps...)
		}
		slices.Sort(pr.Deps)
		pr.Deps = slices.Compact(pr.Deps)
	}

	var missing []int
	open := map[int]bool{}
	for _, pr := range w.PRs {
		open[pr.Number] = true
	}
	for _, pr := range w.PRs {
		for _, dep := range pr.Deps {
			if !open[dep] && !slices.Contains(missing, dep) {
				missing = append(missing, dep)
			}
		}
	}
	refs, err := g.references(ctx, missing)
	if err != nil {
		return err
	}
	w.Refs = refs
	return nil
}

func (g *gh) blockedBy(ctx context.Context, number int) ([]int, error) {
	out, err := runGH(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by", g.owner, g.name, number), "--jq", ".[].number")
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, field := range strings.Fields(string(out)) {
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("unexpected blocker %q of #%d", field, number)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

// references reads the state of issues and pull requests by number. Numbers
// that do not exist are absent from the result.
func (g *gh) references(ctx context.Context, numbers []int) (map[int]externalRef, error) {
	refs := map[int]externalRef{}
	if len(numbers) == 0 {
		return refs, nil
	}
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&query, "    n%d: issueOrPullRequest(number: %d) { __typename ... on Issue { state } ... on PullRequest { state } }\n", n, n)
	}
	query.WriteString("  }\n}\n")

	var resp struct {
		Repository map[string]*struct {
			Type  string `json:"__typename"`
			State string `json:"state"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, query.String(), &resp); err != nil {
		// A missing number fails the whole query, so retry one by one.
		if len(numbers) == 1 {
			return refs, nil
		}
		for _, n := range numbers {
			one, err := g.references(ctx, []int{n})
			if err != nil {
				return nil, err
			}
			for k, v := range one {
				refs[k] = v
			}
		}
		return refs, nil
	}
	for _, n := range numbers {
		if r := resp.Repository[fmt.Sprintf("n%d", n)]; r != nil {
			refs[n] = externalRef{Pull: r.Type == "PullRequest", State: r.State}
		}
	}
	return refs, nil
}

// milestoneIssues lists the open issues of a milestone with their blockers.
func (g *gh) milestoneIssues(ctx context.Context, milestone string) ([]issueInfo, map[int]externalRef, error) {
	args := []string{"issue", "list", "--repo", g.owner + "/" + g.name, "--state", "open", "--limit", "500",
		"--json", "number,title,assignees,labels,createdAt"}
	if milestone != "" {
		args = append(args, "--milestone", milestone)
	}
	raw, err := runGH(ctx, args...)
	if err != nil {
		return nil, nil, err
	}
	var listed []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		CreatedAt time.Time `json:"createdAt"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := decodeJSON(raw, &listed); err != nil {
		return nil, nil, err
	}

	var issues []issueInfo
	var blockers []int
	for _, l := range listed {
		issue := issueInfo{Number: l.Number, Title: l.Title, Created: l.CreatedAt, Assigned: len(l.Assignees) > 0}
		for _, label := range l.Labels {
			issue.Labels = append(issue.Labels, label.Name)
		}
		if !issue.Assigned {
			if issue.Blockers, err = g.blockedBy(ctx, l.Number); err != nil {
				return nil, nil, err
			}
			blockers = append(blockers, issue.Blockers...)
		}
		issues = append(issues, issue)
	}
	slices.Sort(blockers)
	refs, err := g.references(ctx, slices.Compact(blockers))
	return issues, refs, err
}
