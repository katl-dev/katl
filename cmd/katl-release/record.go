package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/releaserecord"
	"github.com/katl-dev/katl/internal/retry"
)

const recordUsage = "usage: scripts/katl-release record VERSION building|failed|qualified [DIGEST]"

// recordAttempts bounds the writes of one record. gh reports every failure
// the same way, so a refused write is retried as well, and the bound keeps
// that short.
const recordAttempts = 5

var recordBackoff = func() retry.Backoff { return retry.NewBackoff(2*time.Second, 30*time.Second) }

// errUnconfirmed marks an answer from GitHub that does not confirm the status
// as sent. Writing the same status again cannot change that answer, so it is
// never retried.
var errUnconfirmed = errors.New("GitHub did not confirm the status as sent")

// record writes the release record of VERSION for the run attempt the Actions
// environment names, on the commit that attempt runs at, so the workflow
// never composes a record. Trust step 3 accepts a record only on its run
// attempt's head_sha and from a run on release/<final version>, so the commit
// comes from GITHUB_SHA and the version must belong to GITHUB_REF.
func record(args []string, stderr io.Writer, call command) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New(recordUsage)
	}
	version, phase := args[0], releaserecord.Phase(args[1])
	if err := validateVersion(version, call); err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}
	if !finalVersion.MatchString(version) && !candidateVersion.MatchString(version) {
		return fmt.Errorf("%s is neither a release candidate nor a final version", version)
	}
	if (phase == releaserecord.Qualified) != (len(args) == 3) {
		return errors.New("give a DIGEST with qualified and only with qualified; " + recordUsage)
	}

	attempt, err := actionsAttempt()
	if err != nil {
		return err
	}
	final, _, _ := strings.Cut(version, "-")
	if branch := "refs/heads/release/" + final; attempt.ref != branch {
		return fmt.Errorf("%s is recorded only by a run on %s, and this run is on %q", version, branch, attempt.ref)
	}
	entry := releaserecord.Record{Version: version, Phase: phase, Run: attempt.run}
	if len(args) == 3 {
		entry.Digest = args[2]
	}
	status, err := entry.Status()
	if err != nil {
		return err
	}

	// A record is append-only: a repeated write adds an identical status
	// for the same attempt and phase, which leaves the attempt's record as it was.
	commit := attempt.commit
	endpoint := "repos/" + attempt.run.Repository + "/statuses/" + commit
	backoff := recordBackoff()
	err = backoff.DoAttempts(context.Background(), recordAttempts, func(context.Context) error {
		out, err := call("gh", "api", "--method", "POST", endpoint,
			"-f", "context="+status.Context, "-f", "state="+status.State,
			"-f", "description="+status.Description, "-f", "target_url="+status.TargetURL)
		if err != nil {
			fmt.Fprintf(stderr, "Writing %s on %s failed: %v\n", status.Context, commit, err)
			return err
		}
		var written releaserecord.Status
		if err := json.Unmarshal(out, &written); err != nil {
			return fmt.Errorf("%w: decode the answer: %v", errUnconfirmed, err)
		}
		if written != status {
			return fmt.Errorf("%w: GitHub stored %+v, not %+v", errUnconfirmed, written, status)
		}
		return nil
	}, func(err error) bool { return !errors.Is(err, errUnconfirmed) })
	if err != nil {
		return fmt.Errorf("write %s on %s: %w", status.Context, commit, err)
	}
	fmt.Fprintf(stderr, "Recorded %s %s on %s: %s\n", status.Context, status.Description, commit, status.TargetURL)
	return nil
}

// actionsRunAttempt is the run attempt running this command, with the commit
// and ref it runs at.
type actionsRunAttempt struct {
	run    releaserecord.Run
	commit string
	ref    string
}

func actionsAttempt() (actionsRunAttempt, error) {
	attempt := actionsRunAttempt{
		run:    releaserecord.Run{Server: os.Getenv("GITHUB_SERVER_URL"), Repository: os.Getenv("GITHUB_REPOSITORY")},
		commit: os.Getenv("GITHUB_SHA"),
		ref:    os.Getenv("GITHUB_REF"),
	}
	id, idErr := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	number, numberErr := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if attempt.run.Server == "" || attempt.run.Repository == "" || idErr != nil || numberErr != nil || !fullSHA.MatchString(attempt.commit) {
		return attempt, errors.New("record runs only in GitHub Actions: GITHUB_SERVER_URL, GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT and GITHUB_SHA name the run attempt it records and its commit")
	}
	attempt.run.ID, attempt.run.Attempt = id, number
	return attempt, nil
}
