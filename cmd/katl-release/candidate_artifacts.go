package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/runstatus"
)

// candidateArtifacts binds a resume to artifact IDs in the recorded attempt.
// Artifact names can recur in another attempt of the same workflow run.
func candidateArtifacts(ctx context.Context, args []string, stdout io.Writer, r *reader) error {
	if len(args) != 4 {
		return errors.New("usage: scripts/katl-release candidate-artifacts VERSION COMMIT RUN ATTEMPT")
	}
	version, err := publicationVersion(args[0])
	if err != nil {
		return err
	}
	commit := args[1]
	id, idErr := strconv.ParseInt(args[2], 10, 64)
	number, numberErr := strconv.Atoi(args[3])
	if version.Stage != releaseversion.Candidate || !fullSHA.MatchString(commit) || idErr != nil || id <= 0 || numberErr != nil || number <= 0 {
		return errors.New("give a candidate version, full commit SHA, and positive run and attempt numbers")
	}
	attempt := runstatus.Attempt{Run: id, Number: number}
	get := func(ctx context.Context, endpoint string) ([]byte, error) { return r.get(ctx, endpoint, false, "") }
	run, err := runstatus.Read(ctx, get, r.repository, attempt)
	if err != nil {
		return err
	}
	if run.ID != attempt.Run || run.Attempt != attempt.Number || run.SHA != commit {
		return fmt.Errorf("run attempt %s does not match the recorded candidate commit %s", attempt, commit)
	}
	artifacts, err := r.attemptArtifacts(ctx, attempt, run.StartedAt)
	if err != nil {
		return err
	}

	bundle := releasestate.BundleName(version, commit)
	names := map[string]string{bundle: "bundle"}
	for _, track := range flavour.Supported() {
		names["katl-extension-bundles-"+track+"-"+commit] = track
	}
	ids := make(map[string]int64, len(names))
	for _, artifact := range artifacts {
		key := names[artifact.Name]
		if key == "" || artifact.Expired || artifact.ID <= 0 {
			continue
		}
		if ids[key] != 0 {
			return fmt.Errorf("run attempt %s has multiple artifacts named %s", attempt, artifact.Name)
		}
		ids[key] = artifact.ID
	}
	if ids["bundle"] == 0 {
		return fmt.Errorf("The bundle of %s from %s has expired, so its publication cannot be finished. Push a commit to %s; the next dispatch then tags %s.",
			version.Tag(), attempt.URL(r.server, r.repository), releasestate.Branch(version.Base()), version.Candidate(version.Number+1).Tag())
	}
	for _, track := range flavour.Supported() {
		if ids[track] == 0 {
			return fmt.Errorf("the %s extension inputs for %s from %s are unavailable. Push a commit to %s to qualify the next candidate.",
				track, version.Tag(), attempt.URL(r.server, r.repository), releasestate.Branch(version.Base()))
		}
	}
	return json.NewEncoder(stdout).Encode(ids)
}
