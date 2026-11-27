package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/runstatus"
)

var (
	readerHead = strings.Repeat("1", 40)
	readerTag  = strings.Repeat("a", 40)
)

// fakeGitHub answers GET endpoints; paginated reads get the pages as one array.
type fakeGitHub struct {
	t         *testing.T
	responses map[string]any
	pages     map[string][]any
	failures  map[string][]error
}

func (f *fakeGitHub) read(_ context.Context, endpoint string, paginate bool, _ string) ([]byte, error) {
	if queue := f.failures[endpoint]; len(queue) > 0 {
		f.failures[endpoint] = queue[1:]
		return nil, queue[0]
	}
	if paginate {
		pages, ok := f.pages[endpoint]
		if !ok {
			f.t.Errorf("unexpected paginated read %s", endpoint)
			return nil, &apiError{status: 404, text: "not found"}
		}
		return json.Marshal(pages)
	}
	value, ok := f.responses[endpoint]
	if !ok {
		f.t.Errorf("unexpected read %s", endpoint)
		return nil, &apiError{status: 404, text: "not found"}
	}
	if err, isErr := value.(error); isErr {
		return nil, err
	}
	return json.Marshal(value)
}

// fakeParse stands in for internal/releaserecord.
func fakeParse(context, state, description, target string) (*releasestate.Record, error) {
	attempt, ok := runstatus.ParseAttemptURL("https://github.com", "katl-dev/katl", target)
	if !ok || state != "pending" || description != "building" {
		return nil, errors.New("not a record")
	}
	return &releasestate.Record{Tag: strings.TrimPrefix(context, "katl/release/"), Phase: releasestate.Building, Attempt: attempt}, nil
}

func readerWorld(t *testing.T) *fakeGitHub {
	repo := "repos/katl-dev/katl/"
	none := []any{[]any{}}
	noRuns := []any{map[string]any{"workflow_runs": []any{}}}
	f := &fakeGitHub{t: t, responses: map[string]any{
		repo + "git/tags/" + readerTag:                                      map[string]any{"object": map[string]any{"sha": readerHead, "type": "commit"}},
		repo + "compare/" + readerHead + "..." + readerHead + "?per_page=1": map[string]any{"status": "identical"},
		repo + "actions/runs/5/attempts/1": map[string]any{
			"id": 5, "run_attempt": 1, "head_branch": "release/2026.12.1", "head_sha": readerHead, "event": "workflow_dispatch",
			"path": ".github/workflows/release.yml", "status": "completed", "conclusion": "cancelled",
			"run_started_at": "2026-12-01T00:00:00Z", "repository": map[string]any{"full_name": "katl-dev/katl"},
			"head_repository": map[string]any{"full_name": "katl-dev/katl"},
		},
		repo + "actions/runs/6/attempts/1": &apiError{status: 404, text: "gh: Not Found (HTTP 404)"},
	}, pages: map[string][]any{
		// GitHub matches refs by prefix, so the branch of 2026.12.10 is listed too.
		repo + "git/matching-refs/heads/release/2026.12.1?per_page=100": {[]any{
			map[string]any{"ref": "refs/heads/release/2026.12.1", "object": map[string]any{"sha": readerHead, "type": "commit"}},
			map[string]any{"ref": "refs/heads/release/2026.12.10", "object": map[string]any{"sha": strings.Repeat("9", 40), "type": "commit"}},
		}},
		repo + "git/matching-refs/tags/v2026.12.1?per_page=100": {[]any{
			map[string]any{"ref": "refs/tags/v2026.12.1-rc.1", "object": map[string]any{"sha": readerTag, "type": "tag"}},
			map[string]any{"ref": "refs/tags/v2026.12.10", "object": map[string]any{"sha": strings.Repeat("9", 40), "type": "commit"}},
		}},
		// The record is on the second page, after a newer status of another context.
		repo + "commits/" + readerHead + "/statuses?per_page=100": {
			[]any{map[string]any{"context": "ci", "state": "success", "creator": map[string]any{"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}}},
			[]any{
				map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "pending", "description": "building",
					"target_url": "https://github.com/katl-dev/katl/actions/runs/5/attempts/1",
					"creator":    map[string]any{"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}},
				map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "pending", "description": "building",
					"target_url": "https://github.com/katl-dev/katl/actions/runs/6/attempts/1",
					"creator":    map[string]any{"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}},
			},
		},
		repo + "actions/runs/5/attempts/1/jobs?per_page=100": {map[string]any{"jobs": []any{map[string]any{"name": "Decide", "conclusion": "success"}}},
			map[string]any{"jobs": []any{map[string]any{"name": "Build / Release validation", "conclusion": "cancelled"}}}},
		repo + "actions/runs/5/artifacts?per_page=100": {map[string]any{"artifacts": []any{}}},
		repo + "releases?per_page=100":                 none,
		repo + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&status=queued&per_page=100":      noRuns,
		repo + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&status=in_progress&per_page=100": noRuns,
		repo + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&status=waiting&per_page=100":     noRuns,
		repo + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&status=requested&per_page=100":   noRuns,
		"orgs/katl-dev/packages?package_type=container&per_page=100":                                           {[]any{map[string]any{"name": "katl/extensions/drbd9"}, map[string]any{"name": "kubernetes"}}},
		"orgs/katl-dev/packages/container/katl%2Fextensions%2Fdrbd9/versions?per_page=100": {[]any{
			map[string]any{"name": "sha256:aa", "metadata": map[string]any{"container": map[string]any{"tags": []any{"v2026.12.1-rc.1-lts-x86_64-drbd9-9.3.4"}}}},
		}, []any{
			map[string]any{"name": "sha256:bb", "metadata": map[string]any{"container": map[string]any{"tags": []any{"v2026.12.1-lts-x86_64-drbd9-9.3.4"}}}},
		}},
	}, failures: map[string][]error{}}
	return f
}

func TestReaderRead(t *testing.T) {
	f := readerWorld(t)
	// A transient failure is retried; the read does not report it.
	f.failures["repos/katl-dev/katl/releases?per_page=100"] = []error{&apiError{status: 502, text: "gh: Bad Gateway (HTTP 502)"}}
	r := &reader{server: "https://github.com", repository: "katl-dev/katl", read: f.read, parse: fakeParse, delay: time.Millisecond}

	in, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, "")
	if err != nil {
		t.Fatal(err)
	}
	if in.Head != readerHead || len(in.Tags) != 1 || in.Tags[0] != (releasestate.Tag{Name: "v2026.12.1-rc.1", Commit: readerHead}) {
		t.Fatalf("head %s, tags %+v: want release/2026.12.1 and the peeled rc.1 only", in.Head, in.Tags)
	}
	statuses := in.Statuses[readerHead]
	if len(statuses) != 2 || statuses[0].Record == nil || !statuses[0].Creator.IsActionsBot() {
		t.Fatalf("statuses = %+v, want both rc.1 records from the second page with the bot's GraphQL login", statuses)
	}
	if attempt := in.Attempts[runstatus.Attempt{Run: 5, Number: 1}]; len(attempt.Jobs) != 2 || attempt.Run.ID != 5 {
		t.Fatalf("attempt = %+v, want run 5 with jobs from both pages", attempt)
	}
	if _, read := in.Attempts[runstatus.Attempt{Run: 6, Number: 1}]; read {
		t.Fatal("a run that does not exist was read as an attempt")
	}
	if len(in.ExtensionReferences) != 1 || !strings.Contains(in.ExtensionReferences[0], ":v2026.12.1-lts-") {
		t.Fatalf("extension references = %q, want the final's only", in.ExtensionReferences)
	}

	d, err := releasestate.Decide(in)
	if err != nil || d.Row != 19 || !strings.Contains(d.Message, "the run was cancelled (https://github.com/katl-dev/katl/actions/runs/5/attempts/1)") {
		t.Fatalf("decision = %+v, %v, want row 19 for the cancelled build", d, err)
	}
}

func TestReaderFailsWithoutAnAnswer(t *testing.T) {
	for name, failure := range map[string]error{
		"refused":      &apiError{status: 403, text: "gh: Resource not accessible (HTTP 403)"},
		"stays broken": &apiError{status: 502, text: "gh: Bad Gateway (HTTP 502)"},
	} {
		t.Run(name, func(t *testing.T) {
			f := readerWorld(t)
			f.failures["repos/katl-dev/katl/releases?per_page=100"] = []error{failure, failure, failure, failure, failure, failure, failure}
			r := &reader{server: "https://github.com", repository: "katl-dev/katl", read: f.read, parse: fakeParse, delay: time.Millisecond}
			if _, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, ""); err == nil {
				t.Fatal("a failed read was taken as no releases")
			}
		})
	}
}

func TestReaderWithoutRecordParser(t *testing.T) {
	f := readerWorld(t)
	r := &reader{server: "https://github.com", repository: "katl-dev/katl", read: f.read, parse: parseRecord, delay: time.Millisecond}
	if _, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, ""); !errors.Is(err, errRecordsUnreadable) {
		t.Fatalf("err = %v, want the read to stop at the first record", err)
	}
}

func TestNextAndStatus(t *testing.T) {
	r := &reader{server: "https://github.com", repository: "katl-dev/katl", read: readerWorld(t).read, parse: fakeParse, delay: time.Millisecond}
	var out strings.Builder
	if err := next(context.Background(), []string{"--ref", "refs/heads/release/2026.12.1", "--dispatch", readerHead}, &out, r); err != nil {
		t.Fatal(err)
	}
	var decision nextDecision
	if err := json.Unmarshal([]byte(out.String()), &decision); err != nil || decision.Action != "refuse" || decision.State != "candidate-failed" || decision.Message == "" {
		t.Fatalf("next printed %q (%v), want the candidate-failed refusal as JSON", out.String(), err)
	}

	out.Reset()
	r.read = readerWorld(t).read
	if err := status(context.Background(), []string{"2026.12.1"}, &out, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"State: candidate-failed (row 19)\n",
		"Refusal: v2026.12.1-rc.1 did not qualify at 111111111111: the run was cancelled",
		"Candidate record: v2026.12.1-rc.1 building at " + readerHead + " (https://github.com/katl-dev/katl/actions/runs/5/attempts/1)\n",
		"Warning: Ignored status katl/release/v2026.12.1-rc.1 on 111111111111: run attempt 6/attempts/1 is not a dispatched Release run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
}
