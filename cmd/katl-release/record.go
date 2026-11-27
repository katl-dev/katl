package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/katl-dev/katl/internal/releaserecord"
	"github.com/katl-dev/katl/internal/retry"
)

const recordUsage = "usage: scripts/katl-release record VERSION COMMIT building|failed|qualified [DIGEST]"

// recordAttempts bounds the writes of one record. gh reports every failure
// the same way, so a refused write is retried as well, and the bound keeps
// that short.
const recordAttempts = 5

var recordBackoff = func() retry.Backoff { return retry.NewBackoff(2*time.Second, 30*time.Second) }

// record writes the release record of VERSION on COMMIT for the run attempt
// the Actions environment names, so the workflow never composes a record.
func record(args []string, stderr io.Writer, call command) error {
	if len(args) < 3 || len(args) > 4 {
		return errors.New(recordUsage)
	}
	version, commit, phase := args[0], args[1], releaserecord.Phase(args[2])
	if err := validateVersion(version, call); err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}
	if !finalVersion.MatchString(version) && !candidateVersion.MatchString(version) {
		return fmt.Errorf("%s is neither a release candidate nor a final version", version)
	}
	if !fullSHA.MatchString(commit) {
		return errors.New("COMMIT must be the full commit SHA the version is built from")
	}
	if (phase == releaserecord.Qualified) != (len(args) == 4) {
		return errors.New("give a DIGEST with qualified and only with qualified; " + recordUsage)
	}

	run, err := actionsRun()
	if err != nil {
		return err
	}
	entry := releaserecord.Record{Version: version, Phase: phase, Run: run}
	if len(args) == 4 {
		entry.Digest = args[3]
	}
	status, err := entry.Status()
	if err != nil {
		return err
	}

	// A record is append-only: a repeated write adds an identical status
	// for the same attempt and phase, which leaves the attempt's record as it was.
	endpoint := "repos/" + run.Repository + "/statuses/" + commit
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
			return fmt.Errorf("decode the written status: %w", err)
		}
		if written != status {
			return fmt.Errorf("GitHub stored %+v, not %+v", written, status)
		}
		return nil
	}, func(error) bool { return true })
	if err != nil {
		return fmt.Errorf("write %s on %s: %w", status.Context, commit, err)
	}
	fmt.Fprintf(stderr, "Recorded %s %s on %s: %s\n", status.Context, status.Description, commit, status.TargetURL)
	return nil
}

// actionsRun names the run attempt that is running this command.
func actionsRun() (releaserecord.Run, error) {
	run := releaserecord.Run{Server: os.Getenv("GITHUB_SERVER_URL"), Repository: os.Getenv("GITHUB_REPOSITORY")}
	id, idErr := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, attemptErr := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if run.Server == "" || run.Repository == "" || idErr != nil || attemptErr != nil {
		return run, errors.New("record runs only in GitHub Actions: GITHUB_SERVER_URL, GITHUB_REPOSITORY, GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT name the run attempt it records")
	}
	run.ID, run.Attempt = id, attempt
	return run, nil
}
