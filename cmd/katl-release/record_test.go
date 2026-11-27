package main

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/retry"
)

const recordDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// statusRequest is one gh api call as the statuses API receives it: the flag
// order on the command line is not part of it.
type statusRequest struct {
	method   string
	endpoint string
	fields   map[string]string
}

// statusAPI stands in for gh: it keeps every request and answers a status
// POST with the fields GitHub echoes, unless fail says to refuse it.
type statusAPI struct {
	requests []statusRequest
	fail     int
	// echo, when set, replaces the answer to a request.
	echo func(fields map[string]string) []byte
}

func (a *statusAPI) call(name string, args ...string) ([]byte, error) {
	if name == "scripts/katl-release-artifacts" {
		return exec.Command(filepath.Join("..", "..", name), args...).CombinedOutput()
	}
	if name != "gh" || len(args) == 0 || args[0] != "api" {
		return nil, errors.New("unexpected command " + name)
	}
	request := statusRequest{method: "GET", fields: map[string]string{}}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--method", "-X":
			i++
			request.method = args[i]
		case "-f", "--raw-field":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			request.fields[key] = value
		default:
			request.endpoint = args[i]
		}
	}
	a.requests = append(a.requests, request)
	if a.fail > 0 {
		a.fail--
		return nil, errors.New("HTTP 502")
	}

	fields := map[string]string{}
	for key, value := range request.fields {
		fields[key] = value
	}
	if a.echo != nil {
		return a.echo(fields), nil
	}
	return echoStatus(fields), nil
}

// echoStatus renders the answer GitHub gives to a status POST.
func echoStatus(fields map[string]string) []byte {
	body := map[string]any{"id": 1, "creator": map[string]string{"login": "github-actions[bot]"}}
	for key, value := range fields {
		body[key] = value
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

func actionsEnv(t *testing.T) {
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_RUN_ID", "9876543210")
	t.Setenv("GITHUB_RUN_ATTEMPT", "3")
	t.Setenv("GITHUB_SHA", testCommit)
	t.Setenv("GITHUB_REF", "refs/heads/release/2026.12.1")
	previous := recordBackoff
	recordBackoff = func() retry.Backoff { return retry.NewBackoff(time.Millisecond, time.Millisecond) }
	t.Cleanup(func() { recordBackoff = previous })
}

func TestRecord(t *testing.T) {
	actionsEnv(t)
	const target = "https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/3"
	for _, tc := range []struct {
		name string
		args []string
		want map[string]string
	}{
		{
			name: "candidate qualified",
			args: []string{"record", "2026.12.1-rc.2", "qualified", recordDigest},
			want: map[string]string{"context": "katl/release/v2026.12.1-rc.2", "state": "success",
				"description": "qualified sha256:" + recordDigest, "target_url": target},
		},
		{
			name: "final building",
			args: []string{"record", "2026.12.1", "building"},
			want: map[string]string{"context": "katl/release/v2026.12.1", "state": "pending",
				"description": "building", "target_url": target},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &statusAPI{}
			if err := run(tc.args, io.Discard, io.Discard, api.call); err != nil {
				t.Fatal(err)
			}
			want := []statusRequest{{method: "POST", endpoint: "repos/katl-dev/katl/statuses/" + testCommit, fields: tc.want}}
			if !reflect.DeepEqual(api.requests, want) {
				t.Fatalf("requests = %+v, want %+v", api.requests, want)
			}
		})
	}
}

func TestRecordRetries(t *testing.T) {
	actionsEnv(t)

	api := &statusAPI{fail: 2}
	if err := run([]string{"record", "2026.12.1", "failed"}, io.Discard, io.Discard, api.call); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 3 || !reflect.DeepEqual(api.requests[0], api.requests[2]) {
		t.Fatalf("requests = %+v, want the same write three times", api.requests)
	}

	api = &statusAPI{fail: recordAttempts}
	if err := run([]string{"record", "2026.12.1", "failed"}, io.Discard, io.Discard, api.call); err == nil {
		t.Fatal("record succeeded although every write failed")
	}
	if len(api.requests) != recordAttempts {
		t.Fatalf("made %d writes, want %d", len(api.requests), recordAttempts)
	}
}

// TestRecordRequiresStoredStatus checks that an answer which does not confirm
// the status as sent fails the command after one write: rewriting the same
// status cannot change what GitHub answers.
func TestRecordRequiresStoredStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		echo func(fields map[string]string) []byte
	}{
		{"description", func(f map[string]string) []byte { f["description"] = "qualified"; return echoStatus(f) }},
		{"context", func(f map[string]string) []byte { f["context"] = "katl/release/v2026.12.2"; return echoStatus(f) }},
		{"state", func(f map[string]string) []byte { f["state"] = "pending"; return echoStatus(f) }},
		{"target", func(f map[string]string) []byte {
			f["target_url"] = "https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/2"
			return echoStatus(f)
		}},
		{"malformed body", func(map[string]string) []byte { return []byte(`{"id":1,"state":`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actionsEnv(t)
			api := &statusAPI{echo: tc.echo}
			if err := run([]string{"record", "2026.12.1", "qualified", recordDigest}, io.Discard, io.Discard, api.call); err == nil {
				t.Fatal("record accepted an answer that does not confirm the status it wrote")
			}
			if len(api.requests) != 1 {
				t.Fatalf("made %d writes, want 1", len(api.requests))
			}
		})
	}
}

func TestRecordRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"qualified without digest", []string{"2026.12.1", "qualified"}, nil},
		{"building with digest", []string{"2026.12.1", "building", recordDigest}, nil},
		{"prefixed digest", []string{"2026.12.1", "qualified", "sha256:" + recordDigest}, nil},
		{"unknown phase", []string{"2026.12.1", "published"}, nil},
		{"commit argument", []string{"2026.12.1", testCommit, "building"}, nil},
		{"development version", []string{"2026.12.1-dev.1", "building"}, nil},
		{"tag instead of version", []string{"v2026.12.1", "building"}, nil},
		{"outside Actions", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_RUN_ATTEMPT": ""}},
		{"repository without owner", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REPOSITORY": "katl"}},
		{"no commit", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_SHA": ""}},
		{"short commit", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_SHA": testCommit[:12]}},
		{"no ref", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REF": ""}},
		{"main", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REF": "refs/heads/main"}},
		{"other version's branch", []string{"2026.12.1-rc.1", "building"}, map[string]string{"GITHUB_REF": "refs/heads/release/2026.12.2"}},
		{"branch with longer version", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REF": "refs/heads/release/2026.12.10"}},
		{"candidate's own branch", []string{"2026.12.1-rc.1", "building"}, map[string]string{"GITHUB_REF": "refs/heads/release/2026.12.1-rc.1"}},
		{"series branch", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REF": "refs/heads/release/2026.12.x"}},
		{"release tag", []string{"2026.12.1", "building"}, map[string]string{"GITHUB_REF": "refs/tags/release/2026.12.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actionsEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			api := &statusAPI{}
			if err := run(append([]string{"record"}, tc.args...), io.Discard, io.Discard, api.call); err == nil {
				t.Fatal("record succeeded")
			}
			if len(api.requests) != 0 {
				t.Fatalf("wrote %+v", api.requests)
			}
		})
	}
}
