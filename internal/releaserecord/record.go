// Package releaserecord owns the format of release records: the commit
// statuses through which a Release run attempt states what it reached for a
// candidate or final version. docs/internal/release-state-machine.md defines
// the format. Every writer renders records and every reader parses them
// through this package.
//
// A record says nothing about who wrote it. Whether a parsed status is
// trusted, and whether its version is a release version, is for the caller
// to establish.
package releaserecord

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Phase is how far a build attempt got.
type Phase string

const (
	// Building is written before the candidate's tag is created or the final
	// build starts.
	Building Phase = "building"
	// Failed is written when the attempt ends before it qualified.
	Failed Phase = "failed"
	// Qualified is written after Release validation succeeded and the
	// bundle was verified, before anything is pushed or published.
	Qualified Phase = "qualified"
)

// state is the commit status state each phase is stored with.
var state = map[Phase]string{
	Building:  "pending",
	Failed:    "failure",
	Qualified: "success",
}

const (
	contextPrefix = "katl/release/"
	digestPrefix  = "sha256:"
)

// ErrNotRecord reports a status whose context is outside katl/release/, which
// is not a release record at all rather than a malformed one.
var ErrNotRecord = errors.New("not a release record")

var (
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Run names the run attempt that builds the bytes and writes the record.
//
// A parsed Run is only what the target URL says. The caller compares Server
// and Repository with its own server and repository (trust step 2), checks
// the named run attempt (step 3) and checks the status's creator (step 1).
type Run struct {
	// Server is the GitHub server URL, such as https://github.com.
	Server string
	// Repository is owner/name.
	Repository string
	ID         int64
	Attempt    int
}

// URL renders the run attempt's URL, which is a record's target URL.
func (r Run) URL() (string, error) {
	// The server must be exactly scheme://host[:port] as Parse renders it
	// back from a target URL, so that every rendered record parses to itself.
	server, err := url.Parse(r.Server)
	if err != nil || server.Scheme != "https" || server.Host == "" || strings.HasSuffix(server.Host, ":") ||
		server.Scheme+"://"+server.Host != r.Server {
		return "", fmt.Errorf("server %q is not exactly https://host or https://host:port", r.Server)
	}
	owner, name, ok := strings.Cut(r.Repository, "/")
	if !ok || !validName(owner) || !validName(name) {
		return "", fmt.Errorf("repository %q is not owner/name", r.Repository)
	}
	if r.ID <= 0 || r.Attempt <= 0 {
		return "", fmt.Errorf("run %d attempt %d is not a positive run ID and attempt", r.ID, r.Attempt)
	}
	return fmt.Sprintf("%s/%s/actions/runs/%d/attempts/%d", r.Server, r.Repository, r.ID, r.Attempt), nil
}

func validName(name string) bool {
	return namePattern.MatchString(name) && name != "." && name != ".."
}

// Record is what one build attempt reached for one release version.
type Record struct {
	// Version is the release version without the v prefix: a candidate such
	// as 2026.12.1-rc.2 or a final such as 2026.12.1. A parsed Version is the
	// opaque text after v in the context. Readers look a record up by
	// Context(v) for a version v from internal/releaseversion rather than
	// trusting a parsed Version.
	Version string
	Phase   Phase
	// Digest is the SHA-256 of the bundle's SHA256SUMS as 64 lowercase
	// hexadecimal digits. Only a Qualified record has one.
	Digest string
	Run    Run
}

// Status is a commit status as GitHub's REST API names its fields.
type Status struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// Context is the status context of a version's record: katl/release/v
// followed by the candidate or final version.
func Context(version string) string {
	return contextPrefix + "v" + version
}

// Status renders the record as the commit status that stores it.
func (r Record) Status() (Status, error) {
	if r.Version == "" {
		return Status{}, errors.New("record has no version")
	}
	target, err := r.Run.URL()
	if err != nil {
		return Status{}, err
	}
	description, err := r.description()
	if err != nil {
		return Status{}, err
	}
	return Status{Context: Context(r.Version), State: state[r.Phase], Description: description, TargetURL: target}, nil
}

func (r Record) description() (string, error) {
	switch r.Phase {
	case Building, Failed:
		if r.Digest != "" {
			return "", fmt.Errorf("a %s record has no digest", r.Phase)
		}
		return string(r.Phase), nil
	case Qualified:
		if !digestPattern.MatchString(r.Digest) {
			return "", fmt.Errorf("digest %q is not 64 lowercase hexadecimal digits", r.Digest)
		}
		return string(Qualified) + " " + digestPrefix + r.Digest, nil
	default:
		return "", fmt.Errorf("unknown phase %q", r.Phase)
	}
}

// Parse reads a commit status as a record. It returns ErrNotRecord for a
// context outside katl/release/, and another error for a status in that
// namespace that is not exactly a record.
//
// Parse checks the format only (trust step 4 and the form of step 2). The
// caller compares Run.Server and Run.Repository with its own server and
// repository (step 2) and checks the status's creator (step 1). The parsed
// Version is opaque text: readers look a record up by Context(v) for a
// version v from internal/releaseversion, never by trusting Version.
func Parse(status Status) (Record, error) {
	rest, ok := strings.CutPrefix(status.Context, contextPrefix)
	if !ok {
		return Record{}, ErrNotRecord
	}
	version, ok := strings.CutPrefix(rest, "v")
	if !ok || version == "" {
		return Record{}, fmt.Errorf("context %q does not name a version", status.Context)
	}

	record := Record{Version: version}
	if digest, ok := strings.CutPrefix(status.Description, string(Qualified)+" "+digestPrefix); ok {
		record.Phase, record.Digest = Qualified, digest
	} else if status.Description == string(Building) || status.Description == string(Failed) {
		record.Phase = Phase(status.Description)
	} else {
		return Record{}, fmt.Errorf("%s: description %q is not a release phase", status.Context, status.Description)
	}
	if record.Phase == Qualified && !digestPattern.MatchString(record.Digest) {
		return Record{}, fmt.Errorf("%s: digest %q is not 64 lowercase hexadecimal digits", status.Context, record.Digest)
	}
	if status.State != state[record.Phase] {
		return Record{}, fmt.Errorf("%s: state %q does not go with phase %s", status.Context, status.State, record.Phase)
	}

	run, err := parseRun(status.TargetURL)
	if err != nil {
		return Record{}, fmt.Errorf("%s: %w", status.Context, err)
	}
	record.Run = run
	return record, nil
}

// parseRun accepts exactly the URL that Run.URL renders.
func parseRun(target string) (Run, error) {
	invalid := fmt.Errorf("target URL %q is not a run attempt URL", target)
	parsed, err := url.Parse(target)
	if err != nil {
		return Run{}, invalid
	}
	segments := strings.Split(parsed.Path, "/")
	if len(segments) != 8 || segments[0] != "" || segments[3] != "actions" || segments[4] != "runs" || segments[6] != "attempts" {
		return Run{}, invalid
	}
	id, idErr := strconv.ParseInt(segments[5], 10, 64)
	attempt, attemptErr := strconv.Atoi(segments[7])
	if idErr != nil || attemptErr != nil {
		return Run{}, invalid
	}
	run := Run{
		Server:     parsed.Scheme + "://" + parsed.Host,
		Repository: segments[1] + "/" + segments[2],
		ID:         id,
		Attempt:    attempt,
	}

	// Rendering the parsed run again rejects everything the segments alone
	// do not: another scheme, credentials, a query, escapes, signs and
	// leading zeros.
	if rendered, err := run.URL(); err != nil || rendered != target {
		return Run{}, invalid
	}
	return run, nil
}
