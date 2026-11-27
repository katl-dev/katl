// Package runstatus decides whether a commit status was written by a GitHub
// Actions run attempt that its caller trusts: the status's creator is the
// GitHub Actions bot, its target names one run attempt of this repository,
// and that attempt ran the expected workflow for the status's commit.
package runstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

// RESTCreator is the creator as REST reports it: a bot's login there carries
// a [bot] suffix that GraphQL and Creator omit.
func RESTCreator(login, kind string, id int) Creator {
	if kind == "Bot" {
		login = strings.TrimSuffix(login, "[bot]")
	}
	return Creator{Login: login, Type: kind, DatabaseID: id}
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

// AttemptURL is a run attempt with the server and repository whose page names
// it.
type AttemptURL struct {
	// Server is exactly https://host or https://host:port.
	Server string
	// Repository is owner/name.
	Repository string
	Attempt    Attempt
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validName(name string) bool {
	return namePattern.MatchString(name) && name != "." && name != ".."
}

// String renders the attempt's page, failing for a server, repository or
// attempt that is not of the form ParseAttemptURL accepts.
func (u AttemptURL) String() (string, error) {
	server, err := url.Parse(u.Server)
	if err != nil || server.Scheme != "https" || server.Host == "" || strings.HasSuffix(server.Host, ":") ||
		server.Scheme+"://"+server.Host != u.Server {
		return "", fmt.Errorf("server %q is not exactly https://host or https://host:port", u.Server)
	}
	owner, name, ok := strings.Cut(u.Repository, "/")
	if !ok || !validName(owner) || !validName(name) {
		return "", fmt.Errorf("repository %q is not owner/name", u.Repository)
	}
	if u.Attempt.Run <= 0 || u.Attempt.Number <= 0 {
		return "", fmt.Errorf("run %d attempt %d is not a positive run ID and attempt", u.Attempt.Run, u.Attempt.Number)
	}
	return u.Attempt.URL(u.Server, u.Repository), nil
}

// ParseAttemptURL reads exactly the URL that String renders. The caller
// compares the server and repository with its own, so a target cannot name
// another repository's run.
func ParseAttemptURL(target string) (AttemptURL, error) {
	invalid := fmt.Errorf("target URL %q is not a run attempt URL", target)
	parsed, err := url.Parse(target)
	if err != nil {
		return AttemptURL{}, invalid
	}
	segments := strings.Split(parsed.Path, "/")
	if len(segments) != 8 || segments[0] != "" || segments[3] != "actions" || segments[4] != "runs" || segments[6] != "attempts" {
		return AttemptURL{}, invalid
	}
	run, runErr := strconv.ParseInt(segments[5], 10, 64)
	number, numberErr := strconv.Atoi(segments[7])
	if runErr != nil || numberErr != nil {
		return AttemptURL{}, invalid
	}
	u := AttemptURL{Server: parsed.Scheme + "://" + parsed.Host, Repository: segments[1] + "/" + segments[2], Attempt: Attempt{Run: run, Number: number}}

	// Rendering the parsed URL again rejects everything the segments alone do
	// not: another scheme, credentials, a query, escapes, signs and leading
	// zeros.
	if rendered, err := u.String(); err != nil || rendered != target {
		return AttemptURL{}, invalid
	}
	return u, nil
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
