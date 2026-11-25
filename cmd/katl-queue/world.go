package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	// Removal is the latest removal from the merge queue, if any.
	Removal *removalEvent
	// Pushed is set when someone other than katl-queue pushed to the head
	// branch after Removal.
	Pushed bool
	// Reported lists the removal timestamps already explained by a
	// katl-queue comment.
	Reported []string

	history []historyEvent
}

// queueEntryState is a merge-queue entry and its queue-assigned state.
type queueEntryState struct {
	Number int
	ID     string
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

// marker is a hidden line in a katl-queue comment. It lets a later pass
// recognise what the comment records without parsing its text.
type marker struct {
	Kind, Value string
}

func (m marker) String() string {
	return "<!-- katl-queue:" + m.Kind + " " + m.Value + " -->"
}

var markerLine = regexp.MustCompile(`^<!-- katl-queue:([a-z-]+) (\S+) -->$`)

func parseMarkers(text string) []marker {
	var out []marker
	for _, line := range strings.Split(text, "\n") {
		if m := markerLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, marker{Kind: m[1], Value: m[2]})
		}
	}
	return out
}

func hasMarker(marks []marker, kind, value string) bool {
	return slices.Contains(marks, marker{Kind: kind, Value: value})
}

// ejectionMarker tags a comment that explains a removal, so the same removal
// is explained once.
func ejectionMarker(at time.Time) marker {
	return marker{Kind: "ejection", Value: at.UTC().Format(time.RFC3339)}
}

// eligible reports whether katl-queue may manage the pull request. Release
// branches and pull requests targeting them are never touched, and forks
// cannot be pushed to.
func (w world) eligible(pr openPR) bool {
	return !pr.Draft && !pr.Fork && pr.BaseRef == w.Branch &&
		!strings.HasPrefix(pr.BaseRef, "release/") && !strings.HasPrefix(pr.HeadRef, "release/")
}

// authorPushed reports whether anyone but katl-queue pushed to the branch
// after its latest removal from the queue. History lists removals and pushes
// in order, and a push's commits precede its force-push event. own lists the
// heads katl-queue pushed.
func authorPushed(history []historyEvent, own []string) bool {
	last := -1
	for i, e := range history {
		if e.Commit == nil && e.AfterCommit == nil {
			last = i
		}
	}

	commits := false
	for _, e := range history[last+1:] {
		switch {
		case e.Commit != nil:
			commits = true
		case e.AfterCommit != nil:
			if !slices.Contains(own, e.AfterCommit.Oid) {
				return true
			}
			commits = false
		}
	}
	return commits
}

// settleRemoval reads the markers katl-queue left on the pull request to
// decide which removals are already explained and whether the author has
// acted on the removal since.
func (pr *openPR) settleRemoval(marks []marker) {
	var own []string
	for _, m := range marks {
		switch m.Kind {
		case "ejection":
			pr.Reported = append(pr.Reported, m.Value)
		case "rebased":
			own = append(own, m.Value)
		}
	}
	pr.Pushed = authorPushed(pr.history, own)
	pr.Ejected = pr.Removal.forFailedChecks() && !pr.Pushed
}

// worldFrom derives the world from a snapshot. A non-empty milestone limits
// the pull requests to that milestone.
func worldFrom(snap queueSnapshot, milestone string) world {
	w := world{Branch: snap.Branch, Base: snap.BaseOid}
	for _, e := range snap.Entries {
		pr := e.PullRequest
		w.Queue = append(w.Queue, queuedPR{Number: pr.Number, Head: pr.HeadRefOid, Closes: pr.ClosingIssuesReferences.numbers()})
		w.Entries = append(w.Entries, queueEntryState{Number: pr.Number, ID: pr.ID, State: e.State})
	}

	for _, n := range snap.PRs {
		obs := observe(n)
		pr := openPR{
			candidate: candidate{
				Number:    n.Number,
				Head:      n.HeadRefOid,
				Created:   n.CreatedAt,
				Closes:    n.ClosingIssuesReferences.numbers(),
				AutoMerge: obs.AutoMerge,
			},
			ID: n.ID, Title: n.Title, HeadRef: n.HeadRefName, BaseRef: n.BaseRefName,
			Draft: n.IsDraft, Fork: n.IsCrossRepository, InQueue: obs.InQueue,
			history: n.History.Nodes,
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
		if obs.Removal != nil && !obs.Removal.mergeRecord() {
			pr.Removal = obs.Removal
		}
		pr.Deps = declaredDeps(n.Body)
		w.PRs = append(w.PRs, pr)
	}
	return w
}

// world reads the queue and open pull requests, and resolves each
// candidate's declared dependencies. A non-empty milestone limits the pull
// requests to that milestone.
func (g *gh) world(ctx context.Context, milestone string) (world, error) {
	snap, err := g.snapshot(ctx)
	if err != nil {
		return world{}, err
	}
	w := worldFrom(snap, milestone)

	for i := range w.PRs {
		pr := &w.PRs[i]
		if !w.eligible(*pr) || pr.InQueue || pr.Removal == nil {
			continue
		}
		marks, err := g.markers(ctx, pr.Number)
		if err != nil {
			return w, err
		}
		pr.settleRemoval(marks)
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
			deps, external, err := g.blockedBy(ctx, n)
			if err != nil {
				return err
			}
			pr.Deps = append(pr.Deps, deps...)
			pr.External = append(pr.External, external...)
		}
		slices.Sort(pr.Deps)
		pr.Deps = slices.Compact(pr.Deps)
		slices.Sort(pr.External)
		pr.External = slices.Compact(pr.External)
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

// blockedBy lists what blocks an issue or pull request: the numbers of
// blockers in this repository, and the blockers elsewhere as owner/name#N,
// whose numbers mean nothing here.
func (g *gh) blockedBy(ctx context.Context, number int) (local []int, external []string, err error) {
	filter := fmt.Sprintf(`.[] | if (.repository_url | ascii_downcase | endswith(%q)) then (.number | tostring) else ((.repository_url | sub(".*/repos/"; "")) + "#" + (.number | tostring)) end`,
		strings.ToLower("/repos/"+g.owner+"/"+g.name))
	out, err := runGH(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by", g.owner, g.name, number), "--jq", filter)
	if err != nil {
		return nil, nil, err
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.Contains(field, "#") {
			external = append(external, field)
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, nil, fmt.Errorf("unexpected blocker %q of #%d", field, number)
		}
		local = append(local, n)
	}
	return local, external, nil
}

// references reads the state of issues and pull requests by number. Numbers
// that do not exist are absent from the result.
func (g *gh) references(ctx context.Context, numbers []int) (map[int]externalRef, error) {
	if len(numbers) == 0 {
		return map[int]externalRef{}, nil
	}
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&query, "    n%d: issueOrPullRequest(number: %d) { __typename ... on Issue { state } ... on PullRequest { state } }\n", n, n)
	}
	query.WriteString("  }\n}\n")

	raw, err := g.queryRaw(ctx, query.String())
	if err != nil {
		return nil, err
	}
	return decodeReferences(raw, numbers)
}

// decodeReferences reads the response to a references query. A number that
// does not exist is absent from the result; any other error fails it.
func decodeReferences(raw []byte, numbers []int) (map[int]externalRef, error) {
	var resp struct {
		Repository map[string]*struct {
			Type  string `json:"__typename"`
			State string `json:"state"`
		} `json:"repository"`
	}
	var gqlErr *graphQLError
	if err := decodeGraphQL(raw, &resp); err != nil && !(errors.As(err, &gqlErr) && gqlErr.onlyNotFound()) {
		return nil, err
	}
	refs := map[int]externalRef{}
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
			if issue.Blockers, issue.External, err = g.blockedBy(ctx, l.Number); err != nil {
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
