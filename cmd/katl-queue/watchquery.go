package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/textproto"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	watchBatchSize = 25
	watchQueryOpen = "query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n"
)

type watchHTTP struct {
	status int
	header http.Header
	body   []byte
	stderr string
}

type watchQuery func(context.Context, string) (watchHTTP, error)

func queryWatchGitHub(ctx context.Context, query string) (watchHTTP, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "--hostname", katlHost, "--include",
		"-f", "query="+query, "-F", "owner="+katlOwner, "-F", "name="+katlRepo)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if ctx.Err() != nil {
		return watchHTTP{}, ctx.Err()
	}
	response := watchHTTP{stderr: strings.TrimSpace(stderr.String())}
	if !bytes.HasPrefix(raw, []byte("HTTP/")) {
		if err == nil {
			err = errors.New("missing HTTP status from gh api")
		}
		return response, err
	}

	// gh prints the status and headers followed by the already decoded body.
	buf := bufio.NewReader(bytes.NewReader(raw))
	reader := textproto.NewReader(buf)
	line, err := reader.ReadLine()
	if err != nil {
		return response, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return response, errors.New("invalid HTTP status from gh api")
	}
	response.status, err = strconv.Atoi(fields[1])
	if err != nil {
		return response, err
	}
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		return response, err
	}
	response.header = http.Header(headers)
	response.body, err = io.ReadAll(buf)
	return response, err
}

type watchObject map[string]json.RawMessage

func watchObjectFrom(raw json.RawMessage) watchObject {
	var fields watchObject
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

func watchString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

type watchGraphQLError struct {
	Type       string `json:"type"`
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"extensions"`
}

func (e *watchGraphQLError) UnmarshalJSON(raw []byte) error {
	type errorFields watchGraphQLError
	var fields errorFields
	if json.Unmarshal(raw, &fields) == nil && !slices.ContainsFunc(fields.Path, invalidWatchPathSegment) {
		*e = watchGraphQLError(fields)
		return nil
	}

	// Keep an identifiable path for backoff, but discard malformed
	// metadata that could otherwise assert absence, denial or throttling.
	var path []any
	json.Unmarshal(watchObjectFrom(raw)["path"], &path)
	*e = watchGraphQLError{Path: path}
	return nil
}

func invalidWatchPathSegment(segment any) bool {
	switch value := segment.(type) {
	case string:
		return value == ""
	case float64:
		return value < 0 || math.Trunc(value) != value
	default:
		return true
	}
}

func (e watchGraphQLError) kind() string {
	for _, kind := range []string{e.Type, e.Extensions.Type, e.Extensions.Code} {
		if kind != "" {
			return kind
		}
	}
	return ""
}

func (e watchGraphQLError) forbidden() bool {
	return slices.Contains([]string{"FORBIDDEN", "UNAUTHORIZED", "UNAUTHENTICATED", "INSUFFICIENT_SCOPES"}, e.kind())
}

type watchGraphQL struct {
	Data struct {
		Repository watchObject `json:"repository"`
	} `json:"data"`
	Errors  []watchGraphQLError `json:"errors"`
	Message string              `json:"message"`
}

type watchQueryIssue struct {
	cause    string
	operator bool
	limited  bool
	retryAt  time.Time
}

func readWatchQuery(response watchHTTP, transportErr error, now time.Time) (watchGraphQL, watchQueryIssue) {
	var envelope watchGraphQL
	decodeErr := json.Unmarshal(response.body, &envelope)
	message := envelope.Message
	for _, item := range envelope.Errors {
		if message == "" {
			message = item.Message
		}
	}
	if message == "" {
		message = response.stderr
	}
	if message == "" {
		message = fmt.Sprintf("HTTP %d", response.status)
	}

	remaining := response.header.Get("X-RateLimit-Remaining")
	retryAfter := response.header.Get("Retry-After")
	limited := response.status == http.StatusTooManyRequests || remaining == "0" ||
		(retryAfter != "" && response.status == http.StatusForbidden) ||
		strings.Contains(strings.ToLower(envelope.Message), "rate limit") ||
		(response.status != http.StatusOK && strings.Contains(strings.ToLower(message), "rate limit"))
	for _, item := range envelope.Errors {
		limited = limited || item.kind() == "RATE_LIMITED" || item.kind() == "RATELIMITED" ||
			strings.Contains(strings.ToLower(item.Message), "rate limit")
	}
	if limited {
		issue := watchQueryIssue{limited: true, cause: "GitHub rate limit: " + message}
		if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds >= 0 {
			issue.retryAt = now.Add(time.Duration(min(seconds, int64(math.MaxInt64/time.Second))) * time.Second)
		} else if date, err := http.ParseTime(retryAfter); err == nil {
			issue.retryAt = date
		}
		if remaining == "0" {
			if seconds, err := strconv.ParseInt(response.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				reset := time.Unix(seconds, 0)
				if reset.After(issue.retryAt) {
					issue.retryAt = reset
				}
			}
		}
		if response.status != http.StatusOK || transportErr != nil || decodeErr != nil {
			return watchGraphQL{}, issue
		}
		return envelope, issue
	}

	if transportErr != nil {
		if errors.Is(transportErr, exec.ErrNotFound) {
			return watchGraphQL{}, watchQueryIssue{cause: "GitHub CLI gh is unavailable", operator: true}
		}
		if response.status == 0 && (strings.Contains(response.stderr, "gh auth login") ||
			strings.Contains(response.stderr, "GH_TOKEN environment variable")) {
			return watchGraphQL{}, watchQueryIssue{cause: "GitHub authentication required: " + response.stderr, operator: true}
		}
		if response.status == 0 {
			return watchGraphQL{}, watchQueryIssue{cause: fmt.Sprintf("GitHub transport: %v %s", transportErr, response.stderr)}
		}
	}
	switch response.status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return watchGraphQL{}, watchQueryIssue{cause: fmt.Sprintf("cannot access GitHub (%d): %s", response.status, message), operator: true}
	case http.StatusOK:
	default:
		return watchGraphQL{}, watchQueryIssue{cause: fmt.Sprintf("GitHub returned HTTP %d: %s", response.status, message)}
	}
	if transportErr != nil {
		return watchGraphQL{}, watchQueryIssue{cause: fmt.Sprintf("read GitHub response: %v", transportErr)}
	}
	if decodeErr != nil {
		return watchGraphQL{}, watchQueryIssue{cause: fmt.Sprintf("decode GitHub response: %v", decodeErr)}
	}
	for _, item := range envelope.Errors {
		if len(item.Path) == 0 && item.forbidden() ||
			len(item.Path) == 1 && item.Path[0] == "repository" && (item.forbidden() || item.kind() == "NOT_FOUND") {
			return envelope, watchQueryIssue{cause: "cannot access GitHub: " + item.Message, operator: true}
		}
	}
	return envelope, watchQueryIssue{}
}

func pullWatchQuery(numbers []int) string {
	var query strings.Builder
	query.WriteString(watchQueryOpen)
	// GitHub can null the optional details alias on a resolver error while
	// leaving the separate rollup state available for the verdict.
	for _, n := range numbers {
		fmt.Fprintf(&query, `    pr%d: pullRequest(number: %d) {
      state merged mergeable reviewDecision
      autoMergeRequest { enabledAt }
      mergeQueueEntry { state }
      headRefOid
      commits(last: 1) { nodes { commit {
        oid
        statusCheckRollup { state }
        checkDetails: statusCheckRollup { contexts(first: 50) {
          checkRunCount statusContextCount
          pageInfo { hasNextPage }
          nodes {
            ... on CheckRun { name conclusion }
            ... on StatusContext { context state }
          }
        } }
      } } }
    }
`, n, n)
	}
	query.WriteString("  }\n}\n")
	return query.String()
}

type watchEvidence struct {
	pr       watchObject
	absent   bool
	operator string
	failed   bool
}

func (envelope watchGraphQL) evidence(number int) watchEvidence {
	alias := fmt.Sprintf("pr%d", number)
	pr := watchObjectFrom(envelope.Data.Repository[alias])
	result := watchEvidence{pr: pr}
	for _, item := range envelope.Errors {
		if len(item.Path) < 2 || item.Path[0] != "repository" || item.Path[1] != alias {
			continue
		}
		if len(item.Path) == 2 {
			switch {
			case item.kind() == "NOT_FOUND" && pr == nil:
				result.absent = true
			case item.forbidden():
				result.operator = "cannot observe the pull request on GitHub: " + item.Message
			default:
				result.failed = true
			}
			continue
		}
		result.failed = true
		// Null propagated from a resolver error is missing evidence. A
		// nested error can leave a usable object for another decision.
		if field, ok := item.Path[2].(string); ok && (len(item.Path) == 3 || string(pr[field]) == "null") {
			delete(pr, field)
		}
	}
	return result
}

func failingWatchChecks(commit watchObject) string {
	rollup := watchObjectFrom(commit["checkDetails"])
	contexts := watchObjectFrom(rollup["contexts"])
	var nodes []json.RawMessage
	if json.Unmarshal(contexts["nodes"], &nodes) != nil {
		return "a check failed"
	}
	var names []string
	for _, raw := range nodes {
		node := watchObjectFrom(raw)
		switch watchString(node["state"]) {
		case "FAILURE", "ERROR":
			if name := watchString(node["context"]); name != "" {
				names = append(names, name)
			}
		}
		switch watchString(node["conclusion"]) {
		case "ACTION_REQUIRED", "CANCELLED", "FAILURE", "STALE", "STARTUP_FAILURE", "TIMED_OUT":
			if name := watchString(node["name"]); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return "a check failed"
	}
	slices.Sort(names)
	names = slices.Compact(names)
	page := watchObjectFrom(contexts["pageInfo"])
	if string(page["hasNextPage"]) == "true" {
		return "failing checks include: " + strings.Join(names, ", ")
	}
	return "failing checks: " + strings.Join(names, ", ")
}
