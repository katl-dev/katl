package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func (plan *ciPlan) selectDrivers(
	paths []string, event, ref string,
	digest func() (map[string]string, error),
	lookup func(map[string]string) (map[string]string, error),
) {
	plan.DriverFlavours = []string{}
	plan.DriverDigests = map[string]string{}
	plan.QualifiedDrivers = map[string]string{}
	if event == "push" && ref != "refs/heads/main" {
		return
	}

	forceAll := event == "schedule" || event == "workflow_dispatch"
	forced := map[string]bool{}
	for _, name := range paths {
		if flavour := kernelLockFlavour(name); flavour != "" {
			forced[flavour] = true
		}
	}

	digests, err := digest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Driver input digest unavailable: %v\n", err)
	} else {
		plan.DriverDigests = digests
	}
	wanted := map[string]string{}
	for _, flavour := range kernelFlavours {
		if value := plan.DriverDigests[flavour]; value != "" && !forceAll && !forced[flavour] {
			wanted[flavour] = value
		}
	}
	if len(wanted) != 0 {
		matches, err := lookup(wanted)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Driver qualification lookup unavailable: %v\n", err)
		}
		maps.Copy(plan.QualifiedDrivers, matches)
	}

	for _, flavour := range kernelFlavours {
		if plan.QualifiedDrivers[flavour] == "" {
			plan.DriverFlavours = append(plan.DriverFlavours, flavour)
		}
	}
}

const driverStatusPrefix = "katl/driver-qualification/"

type driverStatus struct {
	State, Description, TargetURL string
	CreatedAt                     time.Time
	Creator                       struct {
		Login, Type string
		DatabaseID  int
	}
}

type commitDriverStatuses struct {
	OID    string
	Status map[string]*driverStatus
}

func mainDriverQualifications(wanted map[string]string) (map[string]string, error) {
	owner, repo, ok := strings.Cut(os.Getenv("GITHUB_REPOSITORY"), "/")
	if !ok || owner == "" || repo == "" {
		return nil, fmt.Errorf("GITHUB_REPOSITORY is unavailable")
	}
	history, err := gitOutput("rev-list", "--first-parent", "refs/remotes/origin/main")
	if err != nil {
		return nil, err
	}

	// A hit near the tip costs one read. Bound the time spent on cold lookups
	// and old history so the plan still runs the gate when evidence is slow.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return findDriverQualifications(strings.Fields(string(history)), wanted, func(commits []string) ([]commitDriverStatuses, error) {
		return githubDriverStatuses(ctx, owner, repo, commits, wanted)
	}, func(sha string, record driverStatus) (bool, error) {
		return verifyDriverPublisher(ctx, owner+"/"+repo, sha, record)
	})
}

func findDriverQualifications(
	history []string, wanted map[string]string,
	readStatuses func([]string) ([]commitDriverStatuses, error),
	verifyPublisher func(string, driverStatus) (bool, error),
) (map[string]string, error) {
	type outcome struct {
		sha    string
		status driverStatus
	}
	latest := map[string]outcome{}
	const batchSize = 100
	for start := 0; start < len(history); start += batchSize {
		commits := history[start:min(start+batchSize, len(history))]
		statuses, err := readStatuses(commits)
		if err != nil {
			return nil, err
		}
		if len(statuses) != len(commits) {
			return nil, fmt.Errorf("incomplete driver status batch")
		}
		for i, commit := range statuses {
			if commit.OID != commits[i] {
				return nil, fmt.Errorf("missing or mismatched driver status for commit %s", commits[i])
			}
			for flavour, digest := range wanted {
				record := commit.Status[flavour]
				if record == nil {
					continue
				}
				if record.CreatedAt.IsZero() {
					return nil, fmt.Errorf("missing creation time for driver status on %s", commit.OID)
				}
				if record.Description != digest {
					// The context is already keyed by the requested digest.
					// An unreadable replacement must not revive older evidence.
					record.State = ""
				}
				previous := latest[flavour].status
				// Main runs can finish out of commit order. In a timestamp
				// tie, pending or failed evidence takes precedence.
				if record.CreatedAt.After(previous.CreatedAt) ||
					(record.CreatedAt.Equal(previous.CreatedAt) && previous.State == "SUCCESS" && record.State != "SUCCESS") {
					latest[flavour] = outcome{commit.OID, *record}
				}
			}
		}
	}
	found := map[string]string{}
	for flavour, result := range latest {
		if result.status.State != "SUCCESS" {
			continue
		}
		verified, err := verifyPublisher(result.sha, result.status)
		if err != nil {
			return nil, err
		}
		if verified {
			found[flavour] = result.sha
		}
	}
	return found, nil
}

func githubDriverStatuses(ctx context.Context, owner, repo string, commits []string, wanted map[string]string) ([]commitDriverStatuses, error) {
	var query strings.Builder
	query.WriteString("query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) {")
	for i, commit := range commits {
		fmt.Fprintf(&query, "c%d: object(oid: %q) { ... on Commit { oid status {", i, commit)
		for _, flavour := range kernelFlavours {
			if digest := wanted[flavour]; digest != "" {
				fmt.Fprintf(&query, "%s: context(name: %q) { state description targetUrl createdAt creator { login type: __typename ... on Bot { databaseId } } }",
					flavour, driverStatusPrefix+flavour+"/"+digest)
			}
		}
		query.WriteString("} } }")
	}
	query.WriteString("} }")

	command := exec.CommandContext(ctx, "gh", "api", "graphql",
		"-f", "owner="+owner, "-f", "repo="+repo, "-f", "query="+query.String())
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read GitHub commit statuses: %w", err)
	}
	var response struct {
		Data struct {
			Repository map[string]commitDriverStatuses
		}
		Errors []json.RawMessage
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("decode GitHub commit statuses: %w", err)
	}
	if len(response.Errors) != 0 || len(response.Data.Repository) != len(commits) {
		return nil, fmt.Errorf("incomplete GitHub commit status response")
	}

	statuses := make([]commitDriverStatuses, len(commits))
	for i := range commits {
		statuses[i] = response.Data.Repository[fmt.Sprintf("c%d", i)]
	}
	return statuses, nil
}

var driverRunPath = regexp.MustCompile(`^([1-9][0-9]*)/attempts/([1-9][0-9]*)$`)

func verifyDriverPublisher(ctx context.Context, repo, sha string, status driverStatus) (bool, error) {
	// GitHub exposes this account as github-actions[bot] in REST and
	// github-actions in GraphQL. A user with a similar name is a different actor.
	if status.Creator.Type != "Bot" || status.Creator.Login != "github-actions" || status.Creator.DatabaseID != 41898282 {
		return false, nil
	}
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	marker, ok := strings.CutPrefix(status.TargetURL, server+"/"+repo+"/actions/runs/")
	if !ok || !driverRunPath.MatchString(marker) {
		return false, nil
	}

	command := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/actions/runs/"+marker)
	output, err := command.Output()
	if err != nil {
		return false, fmt.Errorf("verify driver qualification run: %w", err)
	}
	var run struct {
		ID          int64
		Attempt     int    `json:"run_attempt"`
		Branch      string `json:"head_branch"`
		SHA         string `json:"head_sha"`
		Path, Event string
		Repository  struct {
			FullName string `json:"full_name"`
		}
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	if err := json.Unmarshal(output, &run); err != nil {
		return false, fmt.Errorf("decode driver qualification run: %w", err)
	}
	return marker == fmt.Sprintf("%d/attempts/%d", run.ID, run.Attempt) &&
		run.Branch == "main" && run.SHA == sha && run.Path == ".github/workflows/ci.yml" &&
		strings.EqualFold(run.Repository.FullName, repo) && strings.EqualFold(run.HeadRepository.FullName, repo) &&
		(run.Event == "push" || run.Event == "schedule" || run.Event == "workflow_dispatch"), nil
}
