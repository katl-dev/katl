package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/queuemarker"
	"github.com/katl-dev/katl/internal/retry"
	"github.com/katl-dev/katl/internal/reviewrecord"
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
	// readDelay is the first delay before a read is retried; zero uses
	// defaultReadDelay.
	readDelay time.Duration
}

func runGH(ctx context.Context, args ...string) ([]byte, error) {
	return runGHWithInput(ctx, "", args...)
}

func runGHWithInput(ctx context.Context, input string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Standard output is returned too: gh api prints a GraphQL response
		// that carries errors, with partial data, before it exits non-zero.
		return out, &ghError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

func newGH() *gh {
	return &gh{host: katlHost, owner: katlOwner, name: katlRepo}
}

func (g *gh) api(ctx context.Context, args ...string) ([]byte, error) {
	return g.apiWithInput(ctx, "", args...)
}

func (g *gh) apiWithInput(ctx context.Context, input string, args ...string) ([]byte, error) {
	return runGHWithInput(ctx, input, append(append([]string{"api"}, args...), "--hostname", g.host)...)
}

// ghError is a failed gh invocation. It keeps gh's standard error, which is
// where gh reports the HTTP status of a failed request.
type ghError struct {
	args   []string
	err    error
	stderr string
}

func (e *ghError) Error() string {
	return fmt.Sprintf("gh %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *ghError) Unwrap() error { return e.err }

var transientHTTPStatus = regexp.MustCompile(`\(HTTP 5\d\d\)`)

// transientGH reports whether a gh failure is a GitHub server error or a
// dropped connection, which a later identical request can succeed past.
func transientGH(err error) bool {
	var failed *ghError
	if !errors.As(err, &failed) {
		return false
	}
	if transientHTTPStatus.MatchString(failed.stderr) {
		return true
	}
	for _, network := range []string{"connection reset", "i/o timeout", "TLS handshake timeout", "unexpected EOF"} {
		if strings.Contains(failed.stderr, network) {
			return true
		}
	}
	return false
}

// notFoundGH reports a gh failure that GitHub answered with 404.
func notFoundGH(err error) bool {
	var failed *ghError
	return errors.As(err, &failed) && strings.Contains(failed.stderr, "(HTTP 404)")
}

// readbackWait bounds each wait for GitHub to show the result of a write.
const readbackWait = 2 * time.Minute

// awaitReadback reads GitHub after a write until observe reports that the
// write shows, retrying the errors transient classifies, within readbackWait.
func (g *gh) awaitReadback(ctx context.Context, observe func(context.Context) (bool, error), transient func(error) bool) error {
	deadline, cancel := context.WithTimeout(ctx, readbackWait)
	defer cancel()
	backoff := retry.NewBackoff(cmp.Or(g.readDelay, defaultReadDelay), readDelayLimit)
	_, err := retry.Await(deadline, &backoff, func(ctx context.Context) (struct{}, bool, error) {
		shown, err := observe(ctx)
		return struct{}{}, shown, err
	}, transient)
	return err
}

const (
	defaultReadDelay = 2 * time.Second
	readDelayLimit   = 30 * time.Second
	readAttempts     = 6
)

// read makes a REST read, which is safe to repeat, and retries transient
// failures with jittered exponential backoff. A paginated read restarts from
// its first page.
func (g *gh) read(ctx context.Context, args ...string) ([]byte, error) {
	backoff := retry.NewBackoff(cmp.Or(g.readDelay, defaultReadDelay), readDelayLimit)
	var out []byte
	err := backoff.DoAttempts(ctx, readAttempts, func(ctx context.Context) error {
		var err error
		out, err = g.api(ctx, args...)
		return err
	}, transientGH)
	return out, err
}

func (g *gh) repo() string {
	return g.host + "/" + g.owner + "/" + g.name
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
	// HeadChecks is the latest commit and its checks, which watch and why
	// read.
	HeadChecks headCommits `json:"headChecks"`

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

// failedRequiredChecks lists the required checks that failed on the head. A
// latest commit other than the head proves nothing about it.
func (pr prResponse) failedRequiredChecks() []failedCheck {
	nodes := pr.HeadChecks.Nodes
	if len(nodes) != 1 || nodes[0].Commit.Oid != pr.HeadRefOid || nodes[0].Commit.StatusCheckRollup == nil {
		return nil
	}
	required, _ := unsuccessfulChecks(nodes[0].Commit.StatusCheckRollup.Contexts.Nodes)
	return required
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
		fmt.Fprintf(&query, "    pr%d: pullRequest(number: %d) { ...watched headChecks: %s }\n", n, n, headChecks(n))
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
	_, err := runGH(ctx, "pr", "merge", fmt.Sprint(number), "--auto", "--repo", g.repo())
	return err
}

func (g *gh) failedRunLog(ctx context.Context, runID int64) ([]byte, error) {
	return runGH(ctx, "run", "view", fmt.Sprint(runID), "--log-failed", "--repo", g.repo())
}

func (g *gh) workflowRef(ctx context.Context, name string) (string, error) {
	ext := path.Ext(name)
	if _, err := strconv.ParseInt(name, 10, 64); err == nil || ext == ".yml" || ext == ".yaml" {
		return name, nil
	}

	raw, err := g.read(ctx, "--paginate", "--slurp", g.repoPath()+"/actions/workflows?per_page=100")
	if err != nil {
		return "", err
	}
	var pages []struct {
		Workflows []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil {
		return "", fmt.Errorf("decode workflows: %w", err)
	}
	var ref string
	for _, page := range pages {
		for _, workflow := range page.Workflows {
			if workflow.Name != name {
				continue
			}
			if ref != "" {
				return "", fmt.Errorf("multiple workflows named %q; use a workflow filename or ID", name)
			}
			ref = strconv.FormatInt(workflow.ID, 10)
		}
	}
	if ref == "" {
		return "", fmt.Errorf("no workflow named %q; use a workflow filename, display name, or ID", name)
	}
	return ref, nil
}

// completedRuns lists the completed runs created in the window, of every
// workflow unless the window names one.
func (g *gh) completedRuns(ctx context.Context, window ciCostWindow) ([]ciRun, error) {
	endpoint := g.repoPath() + "/actions/runs?per_page=100&status=completed"
	if window.Workflow != "" {
		ref, err := g.workflowRef(ctx, window.Workflow)
		if err != nil {
			return nil, err
		}
		endpoint = g.repoPath() + "/actions/workflows/" + url.PathEscape(ref) + "/runs?per_page=100&status=completed"
	}
	return g.runsCreated(ctx, endpoint, window.Since, window.Until)
}

// runSearchLimit is the most runs GitHub returns for a listing filtered by
// status or creation time, however many match.
const runSearchLimit = 1000

// runsCreated lists the runs created from since to until inclusive. A range
// that reaches the search limit may be truncated, so it is split in two at a
// whole second, which partitions it because creation times have one-second
// resolution.
func (g *gh) runsCreated(ctx context.Context, endpoint string, since, until time.Time) ([]ciRun, error) {
	created := since.Format(time.RFC3339) + ".." + until.Format(time.RFC3339)
	raw, err := g.read(ctx, "--paginate", "--slurp", endpoint+"&created="+url.QueryEscape(created))
	if err != nil {
		return nil, err
	}
	var pages []struct {
		Runs []ciRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil {
		return nil, fmt.Errorf("decode workflow runs: %w", err)
	}
	var runs []ciRun
	for _, page := range pages {
		runs = append(runs, page.Runs...)
	}
	if len(runs) < runSearchLimit {
		return runs, nil
	}

	if until.Sub(since) < time.Second {
		return nil, fmt.Errorf("GitHub lists at most %d runs created at %s", runSearchLimit, since.Format(time.RFC3339))
	}
	middle := since.Add(until.Sub(since) / 2).Truncate(time.Second)
	first, err := g.runsCreated(ctx, endpoint, since, middle)
	if err != nil {
		return nil, err
	}
	second, err := g.runsCreated(ctx, endpoint, middle.Add(time.Second), until)
	if err != nil {
		return nil, err
	}
	return append(first, second...), nil
}

func (g *gh) runJobs(ctx context.Context, runID int64) ([]ciJob, error) {
	// filter=all includes earlier attempts when a workflow was rerun.
	endpoint := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100&filter=all", g.repoPath(), runID)
	raw, err := g.read(ctx, "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, err
	}
	var pages []struct {
		Jobs []ciJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil {
		return nil, fmt.Errorf("decode workflow jobs: %w", err)
	}
	var jobs []ciJob
	for _, page := range pages {
		jobs = append(jobs, page.Jobs...)
	}
	return jobs, nil
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
	_, err := runGH(ctx, "pr", "merge", fmt.Sprint(number), "--disable-auto", "--repo", g.repo())
	return err
}

func (g *gh) reviewTarget(ctx context.Context, number int) (reviewTarget, error) {
	query := fmt.Sprintf(`query($owner: String!, $name: String!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: %d) { state headRefOid }
  }
}`, number)
	raw, err := g.read(ctx, "graphql", "-f", "query="+query, "-F", "owner="+g.owner, "-F", "name="+g.name)
	if err != nil {
		return reviewTarget{}, err
	}
	var resp struct {
		Viewer struct {
			Login string
		}
		Repository struct {
			PullRequest *prResponse
		}
	}
	if err := decodeGraphQL(raw, &resp); err != nil {
		return reviewTarget{}, err
	}
	pr := resp.Repository.PullRequest
	if pr == nil {
		return reviewTarget{}, fmt.Errorf("pull request #%d not found; check the PR number and repository", number)
	}
	return reviewTarget{state: pr.State, head: pr.HeadRefOid, viewer: resp.Viewer.Login}, nil
}

func (g *gh) mergeBase(ctx context.Context, base, head string) (string, error) {
	endpoint := fmt.Sprintf("%s/compare/%s...%s?per_page=1", g.repoPath(), base, head)
	out, err := g.read(ctx, endpoint, "--jq", ".merge_base_commit.sha")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if oid == "" || oid == "null" {
		return "", errors.New("GitHub comparison returned no merge base")
	}
	return oid, nil
}

// comments reads issue comments, which GitHub also uses for the PR conversation.
func (g *gh) comments(ctx context.Context, number int) ([]reviewrecord.Comment, error) {
	raw, err := g.read(ctx, "--paginate", "--slurp", fmt.Sprintf("%s/issues/%d/comments?per_page=100", g.repoPath(), number))
	if err != nil {
		return nil, err
	}
	var pages [][]struct {
		ID                int64  `json:"id"`
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
		User              struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := decodeJSON(raw, &pages); err != nil {
		return nil, err
	}
	var comments []reviewrecord.Comment
	for _, page := range pages {
		for _, c := range page {
			comments = append(comments, reviewrecord.Comment{
				ID: c.ID, Author: c.User.Login, Body: c.Body,
				Member: c.AuthorAssociation == "MEMBER" || c.AuthorAssociation == "OWNER",
			})
		}
	}
	return comments, nil
}

func (g *gh) comment(ctx context.Context, number int, body string) error {
	// PR conversations and issues share the issue comment API.
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{body})
	if err != nil {
		return err
	}
	_, err = g.apiWithInput(ctx, string(payload), "--method", "POST", fmt.Sprintf("%s/issues/%d/comments", g.repoPath(), number), "--input", "-")
	return err
}

// markers lists the markers in every comment on a pull request that the
// authenticated user wrote. Comments from anyone else are ignored, so a
// quoted or forged marker cannot suppress or trigger an action. It needs a
// snapshot to have identified the user.
func (g *gh) markers(ctx context.Context, number int) ([]queuemarker.Marker, error) {
	if g.viewer == "" {
		return nil, errors.New("the authenticated user is unknown; read a snapshot first")
	}
	comments, err := g.comments(ctx, number)
	if err != nil {
		return nil, err
	}
	var markers []queuemarker.Marker
	for _, c := range comments {
		if c.Author == g.viewer {
			markers = append(markers, queuemarker.Parse(c.Body)...)
		}
	}
	return markers, nil
}

// checkFailures names the checks failing on a pull request's latest commit.
// Required is what the repository's rules require to merge; Optional is
// everything else, which GitHub does not hold the pull request for.
type checkFailures struct {
	Required, Optional []string
}

// pullRequestChecks reads the failing checks of pull requests by number.
func (g *gh) pullRequestChecks(ctx context.Context, numbers []int) (map[int]checkFailures, error) {
	if len(numbers) == 0 {
		return map[int]checkFailures{}, nil
	}
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&query, "    pr%d: pullRequest(number: %d) { commits: %s }\n", n, n, headChecks(n))
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
			Commits headCommits `json:"commits"`
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
		required, optional := unsuccessfulChecks(pr.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes)
		// Planning counts only checks that ran to a failure, not cancelled ones.
		ran := func(c failedCheck) bool { return c.Conclusion != "CANCELLED" }
		var failures checkFailures
		for _, c := range required {
			if ran(c) {
				failures.Required = append(failures.Required, c.Name)
			}
		}
		for _, c := range optional {
			if ran(c) {
				failures.Optional = append(failures.Optional, c.Name)
			}
		}
		out[n] = failures
	}
	return out, nil
}

// headChecks selects the checks on a pull request's latest commit. Whether a
// check is required depends on the pull request, so the selection names it.
func headChecks(number int) string {
	return fmt.Sprintf(`commits(last: 1) { nodes { commit { oid statusCheckRollup { contexts(first: 100) { nodes {
      __typename
      ... on CheckRun { name conclusion startedAt completedAt detailsUrl isRequired(pullRequestNumber: %[1]d) }
      ... on StatusContext { context state targetUrl isRequired(pullRequestNumber: %[1]d) }
    } } } } } }`, number)
}

// headCommits is what headChecks reads.
type headCommits struct {
	Nodes []struct {
		Commit struct {
			Oid               string `json:"oid"`
			StatusCheckRollup *struct {
				Contexts struct {
					Nodes []checkContext `json:"nodes"`
				} `json:"contexts"`
			} `json:"statusCheckRollup"`
		} `json:"commit"`
	} `json:"nodes"`
}

// unsuccessfulChecks picks the latest checks that concluded unsuccessfully,
// split by whether the repository's rules require them to merge. A failed
// required check keeps GitHub from adding the pull request to the queue.
func unsuccessfulChecks(all []checkContext) (required, optional []failedCheck) {
	for _, c := range latestContexts(all) {
		check := failedCheck{Name: c.Name, Conclusion: c.Conclusion, DetailsURL: c.DetailsURL}
		failed := slices.Contains([]string{"FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "CANCELLED"}, c.Conclusion)
		if c.Type == "StatusContext" {
			check = failedCheck{Name: c.Context, Conclusion: c.State, DetailsURL: c.TargetURL}
			failed = c.State == "FAILURE" || c.State == "ERROR"
		}
		switch {
		case !failed:
		case c.IsRequired:
			required = append(required, check)
		default:
			optional = append(optional, check)
		}
	}
	return required, optional
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
	DetailsURL  string    `json:"detailsUrl"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
	Context     string    `json:"context"`
	State       string    `json:"state"`
	TargetURL   string    `json:"targetUrl"`
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
