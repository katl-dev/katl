package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"

	"github.com/katl-dev/katl/internal/releasestate"
)

func newGitHubReader(call command) *reader {
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		repo = repository
	}
	self, _ := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	return &reader{server: server, repository: repo, read: ghRead, self: self,
		provenance: &provenance{call: call, repository: repo}}
}

// nextDecision is what the decide job reads from katl-release next.
type nextDecision struct {
	Row          int    `json:"row"`
	State        string `json:"state"`
	Action       string `json:"action"`
	Message      string `json:"message,omitempty"`
	Version      string `json:"version,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Candidate    string `json:"candidate,omitempty"`
	Reason       string `json:"reason,omitempty"`
	BuildRun     int64  `json:"build_run,omitempty"`
	BuildAttempt int    `json:"build_attempt,omitempty"`
	Digest       string `json:"digest,omitempty"`
}

// next decides the dispatched run's step and prints it as JSON. It exits
// zero for every decision, a refusal included; only a failed read fails it.
func next(ctx context.Context, args []string, stdout io.Writer, r *reader) error {
	flags := flag.NewFlagSet("next", flag.ContinueOnError)
	ref := flags.String("ref", os.Getenv("GITHUB_REF"), "dispatched ref")
	dispatch := flags.String("dispatch", os.Getenv("GITHUB_SHA"), "commit the run was dispatched at")
	retryReason := flags.String("retry", "", "retry reason of the dispatch")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *ref == "" || *dispatch == "" {
		return errors.New("usage: scripts/katl-release next [--ref REF] [--dispatch COMMIT] [--retry REASON]")
	}
	d, err := decide(ctx, r, *ref, *dispatch, *retryReason)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(nextDecision{
		Row: d.Row, State: d.State, Action: string(d.Action), Message: d.Message, Version: d.Version, Commit: d.Commit,
		Candidate: d.Candidate, Reason: d.Reason, BuildRun: d.Build.Run, BuildAttempt: d.Build.Number, Digest: d.Digest,
	})
}

func decide(ctx context.Context, r *reader, ref, dispatch, retryReason string) (releasestate.Decision, error) {
	in, err := r.Read(ctx, ref, dispatch, retryReason)
	if err != nil {
		return releasestate.Decision{}, fmt.Errorf("cannot read the release state, so nothing was decided: %w", err)
	}
	return releasestate.Decide(in)
}

// status prints what a dispatch on the version's branch would do now.
func status(ctx context.Context, args []string, stdout io.Writer, r *reader) error {
	if len(args) != 1 {
		return errors.New("usage: scripts/katl-release status VERSION")
	}
	// Without the workflow every run listing fails with GitHub's bare 404.
	_, err := r.get(ctx, r.repo("actions/workflows/"+path.Base(releasestate.ReleaseWorkflow)), false, "")
	if notFound(err) {
		return errors.New("the Release workflow is not installed on this repository")
	}
	if err != nil {
		return fmt.Errorf("cannot read the release state, so nothing was decided: %w", err)
	}
	d, err := decide(ctx, r, "refs/heads/release/"+args[0], "", "")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "State: %s (row %d)\n", d.State, d.Row)
	switch d.Action {
	case releasestate.Refuse:
		fmt.Fprintf(stdout, "Refusal: %s\n", d.Message)
	case releasestate.Final, releasestate.RetryFinal:
		fmt.Fprintf(stdout, "Next: %s builds %s at %s from %s\n", d.Action, d.Version, d.Commit, d.Candidate)
	case releasestate.ResumeCandidate:
		fmt.Fprintf(stdout, "Next: %s publishes %s at %s from %s with sha256:%s\n", d.Action, d.Version, d.Commit,
			d.Build.URL(r.server, r.repository), d.Digest)
	default:
		fmt.Fprintf(stdout, "Next: %s builds %s at %s\n", d.Action, d.Version, d.Commit)
	}
	for _, record := range []struct {
		name    string
		summary *releasestate.RecordSummary
	}{{"Candidate", d.CandidateRecord}, {"Final", d.FinalRecord}} {
		if record.summary == nil {
			continue
		}
		digest := ""
		if record.summary.Digest != "" {
			digest = " sha256:" + record.summary.Digest
		}
		fmt.Fprintf(stdout, "%s record: %s %s%s at %s (%s)\n", record.name, record.summary.Tag, record.summary.Phase, digest,
			record.summary.Commit, record.summary.URL)
	}
	if d.Approval != "" {
		fmt.Fprintf(stdout, "Approval pending: %s\n", d.Approval)
	}
	for _, warning := range d.Warnings {
		fmt.Fprintf(stdout, "Warning: %s\n", warning)
	}
	return nil
}
