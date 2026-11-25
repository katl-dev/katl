package main

import (
	"encoding/json"
	"fmt"
	"time"
)

const watchCIStartGrace = 10 * time.Minute

type watchOutcome struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func needsWatchAction(pr int, condition string) *watchOutcome {
	return &watchOutcome{Code: 1, Message: fmt.Sprintf("https://%s/%s/%s/pull/%d %s.", katlHost, katlOwner, katlRepo, pr, condition)}
}

// Only require the fields that prove the particular condition. In particular,
// a nullable field must be explicitly null before it can prove absence.
func evaluateWatch(record *watchRecord, evidence watchEvidence, now time.Time, interval time.Duration) (outcome *watchOutcome, conclusive bool) {
	pr := evidence.pr
	switch string(pr["merged"]) {
	case "true":
		return &watchOutcome{Code: 0, Message: fmt.Sprintf("#%d merged", record.PR)}, true
	case "false":
	default:
		return nil, false
	}

	switch watchString(pr["state"]) {
	case "CLOSED":
		return needsWatchAction(record.PR, "closed without merging"), true
	case "OPEN":
	default:
		return nil, false
	}
	switch queue := pr["mergeQueueEntry"]; {
	case watchString(watchObjectFrom(queue)["state"]) != "":
		record.SeenQueued = true
		return nil, true
	case string(queue) != "null":
		return nil, false
	}
	if record.SeenQueued {
		outcome := needsWatchAction(record.PR, "left the merge queue (observed outside the queue)")
		outcome.Message += fmt.Sprintf(" Run katl-queue why %d for details.", record.PR)
		return outcome, true
	}
	if watchString(pr["mergeable"]) == "CONFLICTING" {
		return needsWatchAction(record.PR, "has a merge conflict"), true
	}
	if watchString(pr["reviewDecision"]) == "CHANGES_REQUESTED" {
		return needsWatchAction(record.PR, "has changes requested in review"), true
	}
	commit := watchHeadCommit(pr)
	state := watchString(watchObjectFrom(commit["statusCheckRollup"])["state"])
	if state == "FAILURE" || state == "ERROR" {
		return needsWatchAction(record.PR, failingWatchChecks(commit)), true
	}
	autoMerge := pr["autoMergeRequest"]
	if string(autoMerge) == "null" && !now.Before(record.Registered.Add(interval).Add(interval)) {
		return needsWatchAction(record.PR, "is outside the queue and auto-merge is off"), true
	}
	if !evidence.failed && !record.HeadSince.IsZero() && !now.Before(record.HeadSince.Add(watchCIStartGrace)) &&
		watchHeadHasNoCI(commit) {
		return needsWatchAction(record.PR, fmt.Sprintf("no CI started for head %s; close and reopen the PR or push to retrigger", record.Head)), true
	}
	return nil, string(autoMerge) == "null" || watchObjectFrom(autoMerge) != nil
}

func watchHeadCommit(pr watchObject) watchObject {
	head := watchString(pr["headRefOid"])
	if head == "" {
		return nil
	}
	var nodes []json.RawMessage
	commits := watchObjectFrom(pr["commits"])
	if json.Unmarshal(commits["nodes"], &nodes) != nil || len(nodes) != 1 {
		return nil
	}
	node := watchObjectFrom(nodes[0])
	commit := watchObjectFrom(node["commit"])
	if watchString(commit["oid"]) != head {
		return nil
	}
	return commit
}

func watchHeadHasNoCI(commit watchObject) bool {
	// GitHub can return a null rollup for a commit without any checks or
	// statuses. Missing fields and resolver errors cannot prove absence.
	if string(commit["statusCheckRollup"]) == "null" && string(commit["checkDetails"]) == "null" {
		return true
	}
	contexts := watchObjectFrom(watchObjectFrom(commit["checkDetails"])["contexts"])
	return string(contexts["checkRunCount"]) == "0" && string(contexts["statusContextCount"]) == "0"
}
