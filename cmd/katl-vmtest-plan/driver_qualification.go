package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

func (plan *ciPlan) selectDrivers(
	paths []string, event, ref string,
	digest func() (map[string]string, error),
	lookup func(map[string]string, bool) (map[string]driverQualification, error),
) {
	plan.DriverFlavours = []string{}
	plan.DriverDigests = map[string]string{}
	plan.QualifiedDrivers = map[string]string{}
	releasePush := event == "push" && ref != "refs/heads/main"
	mainPush := event == "push" && ref == "refs/heads/main"
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
		// The exact merge-group result also covers a lock change on main.
		if value := plan.DriverDigests[flavour]; value != "" && !releasePush && !forceAll && (!forced[flavour] || mainPush) {
			wanted[flavour] = value
		}
	}
	var qualifications map[string]driverQualification
	var lookupErr error
	if len(wanted) != 0 {
		qualifications, lookupErr = lookup(wanted, mainPush)
		if lookupErr != nil {
			fmt.Fprintf(os.Stderr, "Driver qualification lookup unavailable: %v\n", lookupErr)
		}
	}

	for _, flavour := range kernelFlavours {
		record := qualifications[flavour]
		decision, reason := "run", record.reason
		switch {
		case releasePush:
			decision, reason = "skip", "release-branch-push"
		case forceAll:
			reason = "forced-" + event
		case reason == "same-digest-merge-group-success":
			decision = "skip"
			plan.QualifiedDrivers[flavour] = record.sha
		case forced[flavour]:
			reason = "forced-kernel-lock"
		case plan.DriverDigests[flavour] == "":
			reason = "digest-unavailable"
		case reason == "same-digest-success":
			decision = "skip"
			plan.QualifiedDrivers[flavour] = record.sha
		case reason == "same-digest-qualification-in-progress":
			decision = "skip"
		case reason == "" && lookupErr != nil:
			reason = "lookup-unavailable"
		case reason == "":
			reason = "no-record"
		}
		if decision == "run" {
			plan.DriverFlavours = append(plan.DriverFlavours, flavour)
		}
		digest := plan.DriverDigests[flavour]
		context := ""
		if record.sha != "" {
			context = driverStatusPrefix + flavour + "/" + digest
		}
		line := fmt.Sprintf("driver-qualification flavour=%s digest=%q record_sha=%q record_context=%q record_state=%q producer_run=%q decision=%s reason=%s",
			flavour, digest, record.sha, context, record.status.State, record.status.TargetURL, decision, reason)
		fmt.Fprintln(os.Stderr, line)
		plan.DriverDecisions = append(plan.DriverDecisions, line)
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

type driverQualification struct {
	sha    string
	status driverStatus
	reason string
}

type driverProducer struct {
	event                    string
	jobStatus, jobConclusion string
	startedAt                time.Time
}

func mainDriverQualifications(wanted map[string]string, mainPush bool) (map[string]driverQualification, error) {
	owner, repo, ok := strings.Cut(os.Getenv("GITHUB_REPOSITORY"), "/")
	if !ok || owner == "" || repo == "" {
		return nil, fmt.Errorf("GITHUB_REPOSITORY is unavailable")
	}
	history, err := gitOutput("rev-list", "--first-parent", "refs/remotes/origin/main")
	if err != nil {
		return nil, err
	}
	commits := strings.Fields(string(history))
	var exactSHA string
	if mainPush {
		head, err := gitOutput("rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		exactSHA = strings.TrimSpace(string(head))
		// The remote-tracking ref may already have advanced past this push.
		if i := slices.Index(commits, exactSHA); i >= 0 {
			commits = slices.Delete(commits, i, i+1)
		}
		commits = slices.Insert(commits, 0, exactSHA)
	}

	// Bound cold lookups so the planner can run the gate when evidence is slow.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return findDriverQualifications(commits, wanted, exactSHA, time.Now(), func(commits []string) ([]commitDriverStatuses, error) {
		return githubDriverStatuses(ctx, owner, repo, commits, wanted)
	}, func(sha, flavour string, record driverStatus) (driverProducer, error) {
		return verifyDriverPublisher(ctx, owner+"/"+repo, sha, flavour, record)
	})
}

func findDriverQualifications(
	history []string, wanted map[string]string, exactSHA string, now time.Time,
	readStatuses func([]string) ([]commitDriverStatuses, error),
	verifyPublisher func(sha, flavour string, status driverStatus) (driverProducer, error),
) (map[string]driverQualification, error) {
	results := map[string]driverQualification{}
	latest := map[string]driverQualification{}
	const batchSize = 100
	for start := 0; start < len(history); start += batchSize {
		commits := history[start:min(start+batchSize, len(history))]
		statuses, err := readStatuses(commits)
		if err != nil {
			return results, err
		}
		if len(statuses) != len(commits) {
			return results, fmt.Errorf("incomplete driver status batch")
		}
		for i, commit := range statuses {
			if commit.OID != commits[i] {
				return results, fmt.Errorf("missing or mismatched driver status for commit %s", commits[i])
			}
			for flavour, digest := range wanted {
				record := commit.Status[flavour]
				if record == nil || results[flavour].reason != "" {
					continue
				}
				if record.CreatedAt.IsZero() {
					return results, fmt.Errorf("missing creation time for driver status on %s", commit.OID)
				}
				candidate := driverQualification{sha: commit.OID, status: *record}
				if commit.OID == exactSHA && record.State == "SUCCESS" {
					candidate.reason = driverRecordReason(candidate, flavour, digest, exactSHA, now, verifyPublisher)
					if candidate.reason == "same-digest-merge-group-success" {
						results[flavour] = candidate
						continue
					}
				}
				previous := latest[flavour].status
				// Runs can finish out of commit order. In a timestamp tie, prefer
				// evidence that requires rerunning, then pending, then success.
				if record.CreatedAt.After(previous.CreatedAt) ||
					(record.CreatedAt.Equal(previous.CreatedAt) &&
						driverStatusPriority(*record, digest) > driverStatusPriority(previous, digest)) {
					latest[flavour] = candidate
				}
			}
			if len(results) == len(wanted) {
				return results, nil
			}
		}
	}
	for flavour, record := range latest {
		if record.reason == "" {
			record.reason = driverRecordReason(record, flavour, wanted[flavour], exactSHA, now, verifyPublisher)
		}
		results[flavour] = record
	}
	return results, nil
}

func driverStatusPriority(status driverStatus, digest string) int {
	switch {
	case status.Description == digest && status.State == "SUCCESS":
		return 0
	case status.Description == digest && status.State == "PENDING":
		return 1
	default:
		return 2
	}
}

func driverRecordReason(record driverQualification, flavour, digest, exactSHA string, now time.Time,
	verifyPublisher func(sha, flavour string, status driverStatus) (driverProducer, error),
) string {
	if record.status.Description != digest {
		return "record-invalid"
	}
	switch record.status.State {
	case "SUCCESS", "PENDING":
	case "ERROR", "FAILURE":
		return "record-failed"
	default:
		return "record-invalid"
	}
	producer, err := verifyPublisher(record.sha, flavour, record.status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Driver qualification producer unavailable for %s: %v\n", record.sha, err)
		return "producer-unavailable"
	}
	if producer.event == "" {
		return "record-unverified"
	}
	// Main can adopt a merge-group result only when the queue landed the
	// qualified head verbatim. The publisher is already bound to record.sha.
	if exactSHA != "" && producer.event == "merge_group" && record.sha != exactSHA {
		return "merge-group-head-mismatch"
	}
	switch {
	case record.status.State == "SUCCESS":
		if producer.jobStatus != "completed" || producer.jobConclusion != "success" {
			return "producer-job-not-successful"
		}
		if exactSHA != "" && producer.event == "merge_group" {
			return "same-digest-merge-group-success"
		}
		return "same-digest-success"
	case producer.jobConclusion == "" && (producer.jobStatus == "queued" || producer.jobStatus == "in_progress") &&
		!producer.startedAt.IsZero() && !producer.startedAt.After(now) && now.Sub(producer.startedAt) <= driverPendingMaxAge:
		return "same-digest-qualification-in-progress"
	default:
		return "producer-not-live"
	}
}

// Hosted image builds and the DRBD journey typically finish within an hour.
// Allow 90 minutes for slower attempts while capping the wait below the
// two-hour driver job timeout. A late status cannot extend an attempt's lease.
const driverPendingMaxAge = 90 * time.Minute

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

func verifyDriverPublisher(ctx context.Context, repo, sha, flavour string, status driverStatus) (driverProducer, error) {
	// GitHub exposes this account as github-actions[bot] in REST and
	// github-actions in GraphQL. A user with a similar name is a different actor.
	if status.Creator.Type != "Bot" || status.Creator.Login != "github-actions" || status.Creator.DatabaseID != 41898282 {
		return driverProducer{}, nil
	}
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	marker, ok := strings.CutPrefix(status.TargetURL, server+"/"+repo+"/actions/runs/")
	if !ok || !driverRunPath.MatchString(marker) {
		return driverProducer{}, nil
	}

	attempt := "repos/" + repo + "/actions/runs/" + marker
	command := exec.CommandContext(ctx, "gh", "api", attempt)
	output, err := command.Output()
	if err != nil {
		return driverProducer{}, fmt.Errorf("verify driver qualification run: %w", err)
	}
	var run struct {
		ID          int64
		Attempt     int    `json:"run_attempt"`
		Branch      string `json:"head_branch"`
		SHA         string `json:"head_sha"`
		Path, Event string
		StartedAt   time.Time `json:"run_started_at"`
		Repository  struct {
			FullName string `json:"full_name"`
		}
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	if err := json.Unmarshal(output, &run); err != nil {
		return driverProducer{}, fmt.Errorf("decode driver qualification run: %w", err)
	}
	if marker != fmt.Sprintf("%d/attempts/%d", run.ID, run.Attempt) ||
		run.SHA != sha || run.Path != ".github/workflows/ci.yml" ||
		!strings.EqualFold(run.Repository.FullName, repo) || !strings.EqualFold(run.HeadRepository.FullName, repo) {
		return driverProducer{}, nil
	}
	switch run.Event {
	case "push", "schedule", "workflow_dispatch":
		if run.Branch != "main" {
			return driverProducer{}, nil
		}
	case "merge_group":
		if branch, ok := strings.CutPrefix(run.Branch, "gh-readonly-queue/main/"); !ok || branch == "" {
			return driverProducer{}, nil
		}
	default:
		return driverProducer{}, nil
	}
	command = exec.CommandContext(ctx, "gh", "api", attempt+"/jobs?per_page=100", "--paginate", "--slurp")
	output, err = command.Output()
	if err != nil {
		return driverProducer{}, fmt.Errorf("list driver qualification jobs: %w", err)
	}
	var pages []struct {
		Jobs []struct {
			Name, Status, Conclusion string
		}
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return driverProducer{}, fmt.Errorf("decode driver qualification jobs: %w", err)
	}

	// Match the executable job in ci.yml and kernel-qualification.yml. Its
	// display name binds the tested digest independently of the status writer.
	name := fmt.Sprintf("Drivers (%s) / drivers (%s)", flavour, status.Description)
	var producer driverProducer
	for _, page := range pages {
		for _, job := range page.Jobs {
			if job.Name != name {
				continue
			}
			if producer.event != "" {
				return driverProducer{}, fmt.Errorf("ambiguous driver qualification job %q", name)
			}
			producer = driverProducer{event: run.Event, jobStatus: job.Status, jobConclusion: job.Conclusion, startedAt: run.StartedAt}
		}
	}
	return producer, nil
}
