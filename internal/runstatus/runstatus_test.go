package runstatus

import (
	"context"
	"errors"
	"testing"
)

func TestCreatorIsActionsBot(t *testing.T) {
	for _, tc := range []struct {
		creator Creator
		want    bool
	}{
		{Creator{Login: "github-actions", Type: "Bot", DatabaseID: 41898282}, true},
		{Creator{Login: "github-actions", Type: "User", DatabaseID: 41898282}, false},
		{Creator{Login: "github-actions", Type: "Bot", DatabaseID: 1}, false},
		{Creator{Login: "github-actions[bot]", Type: "Bot", DatabaseID: 41898282}, false},
	} {
		if got := tc.creator.IsActionsBot(); got != tc.want {
			t.Errorf("%+v.IsActionsBot() = %v, want %v", tc.creator, got, tc.want)
		}
	}
}

func TestRESTCreator(t *testing.T) {
	if got := RESTCreator("github-actions[bot]", "Bot", 41898282); !got.IsActionsBot() {
		t.Errorf("REST's Actions bot = %+v, want the Actions bot", got)
	}
	// Only a bot's login carries the suffix; a user named so stays distinct.
	if got := RESTCreator("github-actions[bot]", "User", 41898282); got.Login != "github-actions[bot]" || got.IsActionsBot() {
		t.Errorf("user = %+v, want the login unchanged and not the bot", got)
	}
}

func TestParseAttemptURL(t *testing.T) {
	for target, want := range map[string]AttemptURL{
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/2": {Server: "https://github.com", Repository: "katl-dev/katl", Attempt: Attempt{Run: 123, Number: 2}},
		// The caller compares server and repository with its own.
		"https://ghe.example.test:8443/fork/katl/actions/runs/7/attempts/1": {Server: "https://ghe.example.test:8443", Repository: "fork/katl", Attempt: Attempt{Run: 7, Number: 1}},
	} {
		got, err := ParseAttemptURL(target)
		if err != nil || got != want {
			t.Errorf("ParseAttemptURL(%q) = %+v, %v, want %+v", target, got, err, want)
		}
		if rendered, err := want.String(); err != nil || rendered != target {
			t.Errorf("%+v.String() = %q, %v, want %q", want, rendered, err, target)
		}
	}

	for _, target := range []string{
		"https://github.com/katl-dev/katl/actions/runs/123",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/0",
		"https://github.com/katl-dev/katl/actions/runs/0123/attempts/1",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/+1",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/1/jobs",
		"https://github.com/katl-dev/katl/actions/runs/123/job/5",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/1/",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/1?check_suite_focus=true",
		"https://github.com/katl-dev/katl/actions/runs/123/attempts/1#summary",
		"http://github.com/katl-dev/katl/actions/runs/123/attempts/1",
		"github.com/katl-dev/katl/actions/runs/123/attempts/1",
		"https://bot@github.com/katl-dev/katl/actions/runs/123/attempts/1",
		"https://github.com/x/katl-dev/katl/actions/runs/123/attempts/1",
		"https://github.com/katl%2Ddev/katl/actions/runs/123/attempts/1",
		"https://github.com/katl-dev/katl/actions/runs/99999999999999999999/attempts/1",
		"",
	} {
		if got, err := ParseAttemptURL(target); err == nil {
			t.Errorf("ParseAttemptURL(%q) = %+v", target, got)
		}
	}
}

func TestOriginProduced(t *testing.T) {
	origin := Origin{Repository: "katl-dev/katl", Workflow: ".github/workflows/release.yml", Events: map[string]func(string) bool{
		"workflow_dispatch": func(branch string) bool { return branch == "release/2026.12.1" },
	}}
	attempt := Attempt{Run: 7, Number: 2}
	good := func() Run {
		run := Run{ID: 7, Attempt: 2, Branch: "release/2026.12.1", SHA: "abc", Path: ".github/workflows/release.yml", Event: "workflow_dispatch"}
		run.Repository.FullName, run.HeadRepository.FullName = "katl-dev/katl", "Katl-Dev/katl"
		return run
	}
	if !origin.Produced(good(), attempt, "abc") {
		t.Fatal("the expected attempt was rejected")
	}
	for name, spoil := range map[string]func(*Run){
		"other attempt":  func(r *Run) { r.Attempt = 1 },
		"other run":      func(r *Run) { r.ID = 8 },
		"other commit":   func(r *Run) { r.SHA = "def" },
		"other workflow": func(r *Run) { r.Path = ".github/workflows/ci.yml" },
		"other event":    func(r *Run) { r.Event = "push" },
		"other branch":   func(r *Run) { r.Branch = "main" },
		"other repo":     func(r *Run) { r.Repository.FullName = "fork/katl" },
		"fork head":      func(r *Run) { r.HeadRepository.FullName = "fork/katl" },
	} {
		run := good()
		spoil(&run)
		if origin.Produced(run, attempt, "abc") {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRead(t *testing.T) {
	var endpoint string
	run, err := Read(context.Background(), func(_ context.Context, e string) ([]byte, error) {
		endpoint = e
		return []byte(`{"id":7,"run_attempt":2,"run_started_at":"2026-12-01T00:00:00Z"}`), nil
	}, "katl-dev/katl", Attempt{Run: 7, Number: 2})
	if err != nil || endpoint != "repos/katl-dev/katl/actions/runs/7/attempts/2" || run.ID != 7 || run.StartedAt.IsZero() {
		t.Fatalf("Read = %+v, %v from %q", run, err, endpoint)
	}
	if _, err := Read(context.Background(), func(context.Context, string) ([]byte, error) { return nil, errors.New("down") }, "katl-dev/katl", Attempt{Run: 7, Number: 2}); err == nil {
		t.Fatal("a failed read returned a run")
	}
}
