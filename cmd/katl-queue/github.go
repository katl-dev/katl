package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	katlHost  = "github.com"
	katlOwner = "katl-dev"
	katlRepo  = "katl"
)

// gh runs the GitHub CLI for one repository. Every GitHub read and write the
// tool makes goes through it, so the repository is resolved once.
type gh struct {
	// viewer is the login of the authenticated user, set when a snapshot is
	// read. It identifies the comments and dequeues katl-queue itself made.
	viewer      string
	owner, name string
	host        string
}

func runGH(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Standard output is returned too: gh api prints a GraphQL response
		// that carries errors, with partial data, before it exits non-zero.
		return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
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

func (g *gh) api(ctx context.Context, args ...string) ([]byte, error) {
	if g.host != "" {
		args = append(args, "--hostname", g.host)
	}
	return runGH(ctx, append([]string{"api"}, args...)...)
}

func (g *gh) repo() string {
	if g.host != "" {
		return g.host + "/" + g.owner + "/" + g.name
	}
	return g.owner + "/" + g.name
}

func (g *gh) graphql(ctx context.Context, query string, out any) error {
	raw, err := g.queryRaw(ctx, query)
	if err != nil {
		return err
	}
	return decodeGraphQL(raw, out)
}

// queryRaw returns the response body. gh exits non-zero for a response that
// carries errors, and that response still has a body, so only a failure
// without one is an error here.
func (g *gh) queryRaw(ctx context.Context, query string) ([]byte, error) {
	raw, err := g.api(ctx, "graphql", "-f", "query="+query, "-F", "owner="+g.owner, "-F", "name="+g.name)
	if err != nil && !json.Valid(raw) {
		return nil, err
	}
	return raw, nil
}

type graphQLErrorItem struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// graphQLError is a response whose errors array is not empty. Data that
// GraphQL resolved beside the errors is still decoded.
type graphQLError struct {
	Errors []graphQLErrorItem
}

func (e *graphQLError) Error() string { return "GraphQL: " + e.Errors[0].Message }

// onlyNotFound reports whether every error is a NOT_FOUND, which a query for
// numbers that may not exist expects.
func (e *graphQLError) onlyNotFound() bool {
	return !slices.ContainsFunc(e.Errors, func(item graphQLErrorItem) bool { return item.Type != "NOT_FOUND" })
}

func decodeGraphQL(raw []byte, out any) error {
	var envelope struct {
		Data   json.RawMessage    `json:"data"`
		Errors []graphQLErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode GraphQL response: %w", err)
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return err
		}
	}
	if len(envelope.Errors) > 0 {
		return &graphQLError{Errors: envelope.Errors}
	}
	return nil
}

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

type queueEntry struct {
	Position    int       `json:"position"`
	State       string    `json:"state"`
	EnqueuedAt  time.Time `json:"enqueuedAt"`
	PullRequest struct {
		ID                      string      `json:"id"`
		Number                  int         `json:"number"`
		Title                   string      `json:"title"`
		HeadRefOid              string      `json:"headRefOid"`
		ClosingIssuesReferences numberNodes `json:"closingIssuesReferences"`
	} `json:"pullRequest"`
}

// queueSnapshot is the merge queue for the default branch, the branch tip it
// builds on, and the open pull requests.
type queueSnapshot struct {
	Viewer  string
	Branch  string
	BaseOid string
	Entries []queueEntry
	// PRs lists every open pull request.
	PRs []prResponse
	// Pending lists those whose auto-merge will put them in the queue once
	// their checks pass.
	Pending []prResponse
}

// Selections shared by every query that reads a pull request, so that all of
// them observe the merge queue the same way. The latest removal event carries
// the merge-group commit it was removed from when removalDetail asks for it.
const (
	prFields = `number title state headRefOid merged isInMergeQueue
  mergeCommit { oid }
  autoMergeRequest { enabledAt }
  mergeQueueEntry { position state }`

	removalOpen = `timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
    nodes { ... on RemovedFromMergeQueueEvent { id createdAt reason actor { login }`
	removalClose = ` } } }`

	removalChecks = `
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
        }`

	// planFields is what planning and autopilot read beyond prFields.
	planFields = `id body isDraft isCrossRepository headRefName baseRefName createdAt
  milestone { title }
  labels(first: 30) { nodes { name } }
  pushes: timelineItems(last: 30, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT, PULL_REQUEST_COMMIT]) {
    nodes { __typename ... on HeadRefForcePushedEvent { createdAt } ... on PullRequestCommit { commit { pushedDate committedDate } } }
  }
  closingIssuesReferences(first: 20) { nodes { number } }`
)

const snapshotQuery = `query($owner: String!, $name: String!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name target { oid } }
    mergeQueue {
      entries(first: 100) {
        nodes { position state enqueuedAt pullRequest { id number title headRefOid closingIssuesReferences(first: 20) { nodes { number } } } }
      }
    }
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
  ` + prFields + `
  ` + planFields + `
  ` + removalOpen + removalClose + `
      }
    }
  }
}`

func (g *gh) snapshot(ctx context.Context) (queueSnapshot, error) {
	var resp struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
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
				Nodes []prResponse `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, snapshotQuery, &resp); err != nil {
		return queueSnapshot{}, err
	}

	g.viewer = resp.Viewer.Login
	repo := resp.Repository
	snap := queueSnapshot{Viewer: resp.Viewer.Login, Branch: repo.DefaultBranchRef.Name, BaseOid: repo.DefaultBranchRef.Target.Oid}
	if repo.MergeQueue == nil {
		return snap, fmt.Errorf("%s/%s has no merge queue on %s", g.owner, g.name, snap.Branch)
	}
	snap.Entries = repo.MergeQueue.Entries.Nodes
	snap.PRs = repo.PullRequests.Nodes
	for _, pr := range snap.PRs {
		if pr.AutoMergeRequest != nil && !pr.IsInMergeQueue {
			snap.Pending = append(snap.Pending, pr)
		}
	}
	return snap, nil
}

// prResponse is the per-PR state watch, why, planning and autopilot observe.
// Which fields are set depends on the selection that read it.
type prResponse struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	HeadRefOid     string `json:"headRefOid"`
	Merged         bool   `json:"merged"`
	IsInMergeQueue bool   `json:"isInMergeQueue"`
	MergeCommit    *struct {
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
	// Pushes lists recent force-pushes and pushed commits.
	Pushes struct {
		Nodes []pushEvent `json:"nodes"`
	} `json:"pushes"`

	ID                      string      `json:"id"`
	Body                    string      `json:"body"`
	IsDraft                 bool        `json:"isDraft"`
	IsCrossRepository       bool        `json:"isCrossRepository"`
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
}

type removalEvent struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Reason    string    `json:"reason"`
	// Actor is who removed the entry: the merge queue itself for failed
	// checks, and the person or tool that dequeued it for a manual removal.
	Actor *struct {
		Login string `json:"login"`
	} `json:"actor"`
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

// mergeRecord reports whether the event is the removal GitHub records, with
// reason "merged", when an entry merges. It is not a removal from the queue.
func (r removalEvent) mergeRecord() bool { return r.Reason == "merged" }

// forFailedChecks reports whether the queue removed the entry because its
// checks failed or timed out.
func (r removalEvent) forFailedChecks() bool {
	return r.Reason == "failed_checks" || r.Reason == "checks_timed_out"
}

type failedCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	RunID      int64  `json:"-"`
}

const prFragment = `fragment watched on PullRequest {
  ` + prFields + `
  ` + removalOpen + removalChecks + removalClose + `
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
	return runGH(ctx, "run", "view", fmt.Sprint(runID), "--log-failed", "--repo", g.repo())
}

// runArtifactLogs downloads the diagnostics artifacts of a run and returns the
// contents of their go test logs by artifact name. Artifacts are untrusted:
// each is extracted into its own new directory and only read as data.
func (g *gh) runArtifactLogs(ctx context.Context, runID int64) (map[string][]byte, error) {
	raw, err := g.api(ctx, "--paginate", fmt.Sprintf("repos/%s/%s/actions/runs/%d/artifacts", g.owner, g.name, runID),
		"--jq", `.artifacts[] | select(.expired | not) | .name`)
	if err != nil {
		return nil, err
	}
	logs := map[string][]byte{}
	for _, name := range diagnosticsArtifacts(string(raw), runID) {
		dir, err := os.MkdirTemp("", "katl-queue-artifact-")
		if err != nil {
			return nil, err
		}
		log, err := g.downloadTestLog(ctx, runID, name, dir)
		os.RemoveAll(dir)
		if err != nil {
			return logs, err
		}
		if log != nil {
			logs[name] = log
		}
	}
	return logs, nil
}

func (g *gh) downloadTestLog(ctx context.Context, runID int64, name, dir string) ([]byte, error) {
	if _, err := runGH(ctx, "run", "download", fmt.Sprint(runID), "--repo", g.repo(), "--name", name, "--dir", dir); err != nil {
		return nil, err
	}
	var log []byte
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !testLogName.MatchString(d.Name()) {
			return err
		}
		data, err := os.ReadFile(path)
		log = append(log, data...)
		return err
	})
	return log, err
}

func decodeJSON(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode gh output: %w", err)
	}
	return nil
}

func (g *gh) disableAutoMerge(ctx context.Context, number int) error {
	_, err := runGH(ctx, "pr", "merge", fmt.Sprint(number), "--disable-auto", "--repo", g.owner+"/"+g.name)
	return err
}

// markers lists the markers in every comment on a pull request that the
// authenticated user wrote. Comments from anyone else are ignored, so a
// quoted or forged marker cannot suppress or trigger an action. It needs a
// snapshot to have identified the user.
func (g *gh) markers(ctx context.Context, number int) ([]marker, error) {
	if g.viewer == "" {
		return nil, errors.New("the authenticated user is unknown; read a snapshot first")
	}
	filter := fmt.Sprintf(`.[] | select(.user.login == %q) | .body`, g.viewer)
	out, err := runGH(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/%s/issues/%d/comments", g.owner, g.name, number), "--jq", filter)
	if err != nil {
		return nil, err
	}
	return parseMarkers(string(out)), nil
}

// checkFailures names the checks failing on a pull request's latest commit.
// Required is what the repository's rules require to merge; Optional is
// everything else, which GitHub does not hold the pull request for.
type checkFailures struct {
	Required, Optional []string
}

// pullRequestChecks reads the failing checks of pull requests by number.
// Whether a check is required depends on the pull request, so each is asked
// for under its own number.
func (g *gh) pullRequestChecks(ctx context.Context, numbers []int) (map[int]checkFailures, error) {
	if len(numbers) == 0 {
		return map[int]checkFailures{}, nil
	}
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&query, `    pr%[1]d: pullRequest(number: %[1]d) { commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
      __typename
      ... on CheckRun { name conclusion startedAt completedAt isRequired(pullRequestNumber: %[1]d) }
      ... on StatusContext { context state isRequired(pullRequestNumber: %[1]d) }
    } } } } } } }
`, n)
	}
	query.WriteString("  }\n}\n")

	raw, err := g.queryRaw(ctx, query.String())
	if err != nil {
		return nil, err
	}
	return decodeCheckFailures(raw, numbers)
}

func decodeCheckFailures(raw []byte, numbers []int) (map[int]checkFailures, error) {
	var resp struct {
		Repository map[string]*struct {
			Commits struct {
				Nodes []struct {
					Commit struct {
						StatusCheckRollup *struct {
							Contexts struct {
								Nodes []checkContext `json:"nodes"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"repository"`
	}
	if err := decodeGraphQL(raw, &resp); err != nil {
		return nil, err
	}

	out := map[int]checkFailures{}
	for _, n := range numbers {
		pr := resp.Repository[fmt.Sprintf("pr%d", n)]
		if pr == nil || len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
			continue
		}
		var failures checkFailures
		for _, c := range latestContexts(pr.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes) {
			name, failed := c.Name, slices.Contains([]string{"FAILURE", "TIMED_OUT", "STARTUP_FAILURE"}, c.Conclusion)
			if c.Type == "StatusContext" {
				name, failed = c.Context, c.State == "FAILURE" || c.State == "ERROR"
			}
			switch {
			case !failed:
			case c.IsRequired:
				failures.Required = append(failures.Required, name)
			default:
				failures.Optional = append(failures.Optional, name)
			}
		}
		out[n] = failures
	}
	return out, nil
}

// pushEvent is a force-push, or a commit added to the branch.
type pushEvent struct {
	Type      string    `json:"__typename"`
	CreatedAt time.Time `json:"createdAt"`
	Commit    *struct {
		PushedDate    *time.Time `json:"pushedDate"`
		CommittedDate time.Time  `json:"committedDate"`
	} `json:"commit"`
}

// pushedAfter reports whether the push provably happened after t. A commit
// cannot be pushed before it exists, so a commit committed after t was pushed
// after t; a commit committed earlier says nothing, because it may have been
// pushed late. GitHub often omits the push date.
func (e pushEvent) pushedAfter(t time.Time) bool {
	switch {
	case e.Type == "HeadRefForcePushedEvent":
		return e.CreatedAt.After(t)
	case e.Commit != nil:
		return e.Commit.CommittedDate.After(t) || (e.Commit.PushedDate != nil && e.Commit.PushedDate.After(t))
	}
	return false
}

// checkContext is a check run or commit status on a head.
type checkContext struct {
	Type        string    `json:"__typename"`
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
	Context     string    `json:"context"`
	State       string    `json:"state"`
	IsRequired  bool      `json:"isRequired"`
}

// latestContexts keeps the latest check run of each name. Reruns and
// superseded workflow runs leave older runs of the same name on the commit,
// and GitHub gates on the latest, so older failures must not count.
func latestContexts(all []checkContext) []checkContext {
	latest := func(c checkContext) time.Time {
		if c.StartedAt.After(c.CompletedAt) {
			return c.StartedAt
		}
		return c.CompletedAt
	}
	var out []checkContext
	index := map[string]int{}
	for _, c := range all {
		if c.Type == "StatusContext" {
			out = append(out, c)
			continue
		}
		i, seen := index[c.Name]
		switch {
		case !seen:
			index[c.Name] = len(out)
			out = append(out, c)
		case latest(c).After(latest(out[i])):
			out[i] = c
		}
	}
	return out
}
