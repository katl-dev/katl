package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
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
	State       string
	Description string
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
		return githubDriverStatuses(ctx, owner, repo, commits)
	})
}

func findDriverQualifications(history []string, wanted map[string]string, readStatuses func([]string) ([]commitDriverStatuses, error)) (map[string]string, error) {
	remaining := maps.Clone(wanted)
	found := map[string]string{}
	const batchSize = 40
	for start := 0; start < len(history) && len(remaining) > 0; start += batchSize {
		commits := history[start:min(start+batchSize, len(history))]
		statuses, err := readStatuses(commits)
		if err != nil {
			return found, err
		}
		if len(statuses) != len(commits) {
			return nil, fmt.Errorf("incomplete driver status batch")
		}
		for i, commit := range statuses {
			if commit.OID != commits[i] {
				return nil, fmt.Errorf("missing or mismatched driver status for commit %s", commits[i])
			}
			for flavour, digest := range remaining {
				record := commit.Status[flavour]
				if record == nil || record.Description != digest {
					continue
				}
				if record.State == "SUCCESS" {
					found[flavour] = commit.OID
				}
				delete(remaining, flavour)
			}
		}
	}
	return found, nil
}

func githubDriverStatuses(ctx context.Context, owner, repo string, commits []string) ([]commitDriverStatuses, error) {
	var query strings.Builder
	query.WriteString("query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) {")
	for i, commit := range commits {
		fmt.Fprintf(&query, "c%d: object(oid: %q) { ... on Commit { oid status {", i, commit)
		for _, flavour := range kernelFlavours {
			fmt.Fprintf(&query, "%s: context(name: %q) { state description }", flavour, driverStatusPrefix+flavour)
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
