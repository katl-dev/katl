package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gh runs the GitHub CLI for one repository. Every GitHub read and write the
// tool makes goes through it, so the repository is resolved once.
type gh struct {
	owner, name string
}

func runGH(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func newGH(ctx context.Context) (*gh, error) {
	out, err := runGH(ctx, "repo", "view", "--json", "owner,name", "--jq", `.owner.login + "/" + .name`)
	if err != nil {
		return nil, err
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(string(out)), "/")
	if !ok {
		return nil, fmt.Errorf("unexpected repository %q from gh repo view", out)
	}
	return &gh{owner: owner, name: name}, nil
}

func (g *gh) graphql(ctx context.Context, query string, out any) error {
	raw, err := runGH(ctx, "api", "graphql", "-f", "query="+query, "-F", "owner="+g.owner, "-F", "name="+g.name)
	if err != nil {
		return err
	}
	return decodeGraphQL(raw, out)
}

func decodeGraphQL(raw []byte, out any) error {
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GraphQL: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, out)
}

type queueEntry struct {
	Position    int       `json:"position"`
	State       string    `json:"state"`
	EnqueuedAt  time.Time `json:"enqueuedAt"`
	PullRequest struct {
		Number     int    `json:"number"`
		Title      string `json:"title"`
		HeadRefOid string `json:"headRefOid"`
	} `json:"pullRequest"`
}

type pendingPR struct {
	Number           int    `json:"number"`
	Title            string `json:"title"`
	HeadRefOid       string `json:"headRefOid"`
	IsInMergeQueue   bool   `json:"isInMergeQueue"`
	AutoMergeRequest *struct {
		EnabledAt time.Time `json:"enabledAt"`
	} `json:"autoMergeRequest"`
}

// queueSnapshot is the merge queue for the default branch, the branch tip it
// builds on, and open pull requests that will join it once their checks pass.
type queueSnapshot struct {
	Branch  string
	BaseOid string
	Entries []queueEntry
	Pending []pendingPR
}

const snapshotQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name target { oid } }
    mergeQueue {
      entries(first: 100) {
        nodes { position state enqueuedAt pullRequest { number title headRefOid } }
      }
    }
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes { number title headRefOid isInMergeQueue autoMergeRequest { enabledAt } }
    }
  }
}`

func (g *gh) snapshot(ctx context.Context) (queueSnapshot, error) {
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
					Nodes []queueEntry `json:"nodes"`
				} `json:"entries"`
			} `json:"mergeQueue"`
			PullRequests struct {
				Nodes []pendingPR `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, snapshotQuery, &resp); err != nil {
		return queueSnapshot{}, err
	}

	repo := resp.Repository
	snap := queueSnapshot{Branch: repo.DefaultBranchRef.Name, BaseOid: repo.DefaultBranchRef.Target.Oid}
	if repo.MergeQueue == nil {
		return snap, fmt.Errorf("%s/%s has no merge queue on %s", g.owner, g.name, snap.Branch)
	}
	snap.Entries = repo.MergeQueue.Entries.Nodes
	for _, pr := range repo.PullRequests.Nodes {
		if pr.AutoMergeRequest != nil && !pr.IsInMergeQueue {
			snap.Pending = append(snap.Pending, pr)
		}
	}
	return snap, nil
}

// prResponse is the per-PR state watch and why observe. The latest removal
// event carries the merge-group commit it was removed from, and that
// commit's failed check runs explain the removal.
type prResponse struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	HeadRefOid  string `json:"headRefOid"`
	Merged      bool   `json:"merged"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	AutoMergeRequest *struct {
		EnabledAt time.Time `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	MergeQueueEntry *struct {
		Position int    `json:"position"`
		State    string `json:"state"`
	} `json:"mergeQueueEntry"`
	TimelineItems struct {
		Nodes []removalEvent `json:"nodes"`
	} `json:"timelineItems"`
}

type removalEvent struct {
	CreatedAt    time.Time `json:"createdAt"`
	Reason       string    `json:"reason"`
	BeforeCommit *struct {
		Oid         string `json:"oid"`
		CheckSuites struct {
			Nodes []struct {
				WorkflowRun *struct {
					DatabaseID int64  `json:"databaseId"`
					URL        string `json:"url"`
				} `json:"workflowRun"`
				CheckRuns struct {
					Nodes []failedCheck `json:"nodes"`
				} `json:"checkRuns"`
			} `json:"nodes"`
		} `json:"checkSuites"`
	} `json:"beforeCommit"`
}

type failedCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	RunID      int64  `json:"-"`
}

const prFragment = `fragment watched on PullRequest {
  number title state headRefOid merged
  mergeCommit { oid }
  autoMergeRequest { enabledAt }
  mergeQueueEntry { position state }
  timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
    nodes {
      ... on RemovedFromMergeQueueEvent {
        createdAt reason
        beforeCommit {
          oid
          checkSuites(first: 20) {
            nodes {
              workflowRun { databaseId url }
              checkRuns(first: 50, filterBy: {conclusions: [FAILURE, TIMED_OUT, CANCELLED, STARTUP_FAILURE, ACTION_REQUIRED]}) {
                nodes { name conclusion detailsUrl }
              }
            }
          }
        }
      }
    }
  }
}`

// pullRequests reads several pull requests in one query, keyed by number.
func (g *gh) pullRequests(ctx context.Context, numbers []int) (map[int]prResponse, error) {
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&query, "    pr%d: pullRequest(number: %d) { ...watched }\n", n, n)
	}
	query.WriteString("  }\n}\n")
	query.WriteString(prFragment)

	var resp struct {
		Repository map[string]*prResponse `json:"repository"`
	}
	if err := g.graphql(ctx, query.String(), &resp); err != nil {
		return nil, err
	}
	return prsByNumber(resp.Repository, numbers)
}

func prsByNumber(repo map[string]*prResponse, numbers []int) (map[int]prResponse, error) {
	prs := make(map[int]prResponse, len(numbers))
	for _, n := range numbers {
		pr := repo[fmt.Sprintf("pr%d", n)]
		if pr == nil {
			return nil, fmt.Errorf("pull request #%d not found", n)
		}
		prs[n] = *pr
	}
	return prs, nil
}

func (g *gh) pullRequest(ctx context.Context, number int) (prResponse, error) {
	prs, err := g.pullRequests(ctx, []int{number})
	if err != nil {
		return prResponse{}, err
	}
	return prs[number], nil
}

// failedChecks lists the failed check runs on the merge-group commit a
// removal event names.
func (r removalEvent) failedChecks() []failedCheck {
	if r.BeforeCommit == nil {
		return nil
	}
	var checks []failedCheck
	for _, suite := range r.BeforeCommit.CheckSuites.Nodes {
		for _, check := range suite.CheckRuns.Nodes {
			if suite.WorkflowRun != nil {
				check.RunID = suite.WorkflowRun.DatabaseID
			}
			checks = append(checks, check)
		}
	}
	return checks
}

func (g *gh) enableAutoMerge(ctx context.Context, number int) error {
	_, err := runGH(ctx, "pr", "merge", fmt.Sprint(number), "--auto", "--repo", g.owner+"/"+g.name)
	return err
}

func (g *gh) failedRunLog(ctx context.Context, runID int64) ([]byte, error) {
	return runGH(ctx, "run", "view", fmt.Sprint(runID), "--log-failed", "--repo", g.owner+"/"+g.name)
}
