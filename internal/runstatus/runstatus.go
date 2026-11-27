// Package runstatus decides whether a commit status was written by a GitHub
// Actions run attempt that its caller trusts: the status's creator is the
// GitHub Actions bot, its target names one run attempt of this repository,
// and that attempt ran the expected workflow for the status's commit.
package runstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// actionsBotID is the database ID of the GitHub Actions bot account. GitHub
// names it github-actions[bot] in REST and github-actions in GraphQL; a user
// with a similar name is a different actor.
const actionsBotID = 41898282

// Creator is the account that created a status, with the GraphQL login.
type Creator struct {
	Login, Type string
	DatabaseID  int
}

// IsActionsBot reports whether the creator is the GitHub Actions bot, the
// only writer of a status a workflow token creates.
func (c Creator) IsActionsBot() bool {
	return c.Type == "Bot" && c.Login == "github-actions" && c.DatabaseID == actionsBotID
}

// Attempt names one attempt of a workflow run.
type Attempt struct {
	Run    int64
	Number int
}

func (a Attempt) String() string { return fmt.Sprintf("%d/attempts/%d", a.Run, a.Number) }

// URL is the attempt's page, which is also the target a record names.
func (a Attempt) URL(server, repository string) string {
	return server + "/" + repository + "/actions/runs/" + a.String()
}

var attemptPath = regexp.MustCompile(`^([1-9][0-9]*)/attempts/([1-9][0-9]*)$`)

// ParseAttemptURL reads a run-attempt URL of the repository on the server. It
// accepts nothing else, so a target cannot name another repository's run.
func ParseAttemptURL(server, repository, target string) (Attempt, bool) {
	marker, ok := strings.CutPrefix(target, server+"/"+repository+"/actions/runs/")
	if !ok {
		return Attempt{}, false
	}
	match := attemptPath.FindStringSubmatch(marker)
	if match == nil {
		return Attempt{}, false
	}
	run, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return Attempt{}, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil {
		return Attempt{}, false
	}
	return Attempt{Run: run, Number: number}, true
}

// Run is one run attempt as GitHub reports it.
type Run struct {
	ID         int64     `json:"id"`
	Attempt    int       `json:"run_attempt"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"run_started_at"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// Read fetches the attempt through get, which takes a REST endpoint relative
// to the API root and returns the response body.
func Read(ctx context.Context, get func(context.Context, string) ([]byte, error), repository string, attempt Attempt) (Run, error) {
	body, err := get(ctx, AttemptEndpoint(repository, attempt))
	if err != nil {
		return Run{}, fmt.Errorf("read run attempt %s: %w", attempt, err)
	}
	var run Run
	if err := json.Unmarshal(body, &run); err != nil {
		return Run{}, fmt.Errorf("decode run attempt %s: %w", attempt, err)
	}
	return run, nil
}

// AttemptEndpoint is the REST endpoint of the attempt.
func AttemptEndpoint(repository string, attempt Attempt) string {
	return "repos/" + repository + "/actions/runs/" + attempt.String()
}

// Origin is the producer a caller trusts: a workflow of one repository and,
// for each event the caller accepts, the branches it accepts for that event.
type Origin struct {
	Repository string
	Workflow   string
	Events     map[string]func(branch string) bool
}

// Produced reports whether run is the attempt named and ran the origin's
// workflow, for this repository with this repository as its head, at commit.
func (o Origin) Produced(run Run, attempt Attempt, commit string) bool {
	if run.ID != attempt.Run || run.Attempt != attempt.Number || run.SHA != commit || run.Path != o.Workflow ||
		!strings.EqualFold(run.Repository.FullName, o.Repository) || !strings.EqualFold(run.HeadRepository.FullName, o.Repository) {
		return false
	}
	branch, ok := o.Events[run.Event]
	return ok && branch(run.Branch)
}
