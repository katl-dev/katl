package main

import (
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

// statusAPI stands in for gh: it keeps every request and answers a status
// POST with the fields GitHub echoes, unless fail says to refuse it.
type statusAPI struct {
	requests [][]string
	fail     int
	echo     func(fields map[string]string)
}

func (a *statusAPI) call(name string, args ...string) ([]byte, error) {
	if name == "scripts/katl-release-artifacts" {
		return exec.Command(filepath.Join("..", "..", name), args...).CombinedOutput()
	}
	a.requests = append(a.requests, append([]string{name}, args...))
	if a.fail > 0 {
		a.fail--
		return nil, errors.New("HTTP 502")
	}
	fields := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" {
			key, value, _ := strings.Cut(args[i+1], "=")
			fields[key] = value
		}
	}
	if a.echo != nil {
		a.echo(fields)
	}
	return []byte(`{"id":1,"state":"` + fields["state"] + `","description":"` + fields["description"] +
		`","target_url":"` + fields["target_url"] + `","context":"` + fields["context"] + `","creator":{"login":"github-actions[bot]"}}`), nil
}

func actionsEnv(t *testing.T) {
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_RUN_ID", "9876543210")
	t.Setenv("GITHUB_RUN_ATTEMPT", "3")
	previous := recordBackoff
	recordBackoff = func() retry.Backoff { return retry.NewBackoff(time.Millisecond, time.Millisecond) }
	t.Cleanup(func() { recordBackoff = previous })
}

func TestRecord(t *testing.T) {
	actionsEnv(t)
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "candidate qualified",
			args: []string{"record", "2026.12.1-rc.2", testCommit, "qualified", recordDigest},
			want: []string{"gh", "api", "--method", "POST", "repos/katl-dev/katl/statuses/" + testCommit,
				"-f", "context=katl/release/v2026.12.1-rc.2", "-f", "state=success",
				"-f", "description=qualified sha256:" + recordDigest,
				"-f", "target_url=https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/3"},
		},
		{
			name: "final building",
			args: []string{"record", "2026.12.1", testCommit, "building"},
			want: []string{"gh", "api", "--method", "POST", "repos/katl-dev/katl/statuses/" + testCommit,
				"-f", "context=katl/release/v2026.12.1", "-f", "state=pending", "-f", "description=building",
				"-f", "target_url=https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/3"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &statusAPI{}
			if err := run(tc.args, io.Discard, io.Discard, api.call); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(api.requests, [][]string{tc.want}) {
				t.Fatalf("requests = %q, want %q", api.requests, tc.want)
			}
		})
	}
}

func TestRecordRetries(t *testing.T) {
	actionsEnv(t)

	api := &statusAPI{fail: 2}
	if err := run([]string{"record", "2026.12.1", testCommit, "failed"}, io.Discard, io.Discard, api.call); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 3 || !reflect.DeepEqual(api.requests[0], api.requests[2]) {
		t.Fatalf("requests = %q, want the same write three times", api.requests)
	}

	api = &statusAPI{fail: recordAttempts}
	if err := run([]string{"record", "2026.12.1", testCommit, "failed"}, io.Discard, io.Discard, api.call); err == nil {
		t.Fatal("record succeeded although every write failed")
	}
	if len(api.requests) != recordAttempts {
		t.Fatalf("made %d writes, want %d", len(api.requests), recordAttempts)
	}
}

func TestRecordRequiresStoredStatus(t *testing.T) {
	actionsEnv(t)
	api := &statusAPI{echo: func(fields map[string]string) { fields["description"] = "qualified" }}
	if err := run([]string{"record", "2026.12.1", testCommit, "qualified", recordDigest}, io.Discard, io.Discard, api.call); err == nil {
		t.Fatal("record accepted a stored status that differs from the one it wrote")
	}
}

func TestRecordRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"qualified without digest", []string{"2026.12.1", testCommit, "qualified"}, nil},
		{"building with digest", []string{"2026.12.1", testCommit, "building", recordDigest}, nil},
		{"prefixed digest", []string{"2026.12.1", testCommit, "qualified", "sha256:" + recordDigest}, nil},
		{"unknown phase", []string{"2026.12.1", testCommit, "published"}, nil},
		{"development version", []string{"2026.12.1-dev.1", testCommit, "building"}, nil},
		{"tag instead of version", []string{"v2026.12.1", testCommit, "building"}, nil},
		{"short commit", []string{"2026.12.1", testCommit[:12], "building"}, nil},
		{"outside Actions", []string{"2026.12.1", testCommit, "building"}, map[string]string{"GITHUB_RUN_ATTEMPT": ""}},
		{"repository without owner", []string{"2026.12.1", testCommit, "building"}, map[string]string{"GITHUB_REPOSITORY": "katl"}},
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
				t.Fatalf("wrote %q", api.requests)
			}
		})
	}
}
