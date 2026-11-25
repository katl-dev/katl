package devclean

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitHubSlug(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:katl-dev/katl.git":        "katl-dev/katl",
		"https://github.com/katl-dev/katl.git":    "katl-dev/katl",
		"https://github.com/katl-dev/katl":        "katl-dev/katl",
		"ssh://git@github.com/katl-dev/katl.git":  "katl-dev/katl",
		"https://token@github.com/katl-dev/katl/": "katl-dev/katl",
	} {
		got, err := githubSlug(url)
		if err != nil || got != want {
			t.Errorf("githubSlug(%q) = %q, %v; want %q", url, got, err, want)
		}
	}
	for _, url := range []string{"/srv/origin.git", "git@example.com:katl-dev/katl.git"} {
		if got, err := githubSlug(url); err == nil {
			t.Errorf("githubSlug(%q) = %q; want an error", url, got)
		}
	}
}

func TestGitHubLookupDecodesPullRequests(t *testing.T) {
	f := newFixture(t)
	f.git(f.repo, "remote", "set-url", "origin", "git@github.com:katl-dev/katl.git")
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		`[ "$*" = "api repos/katl-dev/katl/commits/abc/pulls" ] || exit 3` + "\n" +
		`echo '[{"number":1,"merged_at":null,"head":{"sha":"aaa"}},{"number":2,"merged_at":"2026-11-01T00:00:00Z","head":{"sha":"bbb"}}]'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	env := os.Environ()

	got, err := GitHubLookup{Environ: env, Repo: f.repo}.PullRequests(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	want := []PullRequest{{Number: 1, Merged: false, HeadSHA: "aaa"}, {Number: 2, Merged: true, HeadSHA: "bbb"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("PullRequests() = %+v; want %+v", got, want)
	}
}

func TestGitHubLookupFailsWithoutGitHub(t *testing.T) {
	f := newFixture(t) // origin is a local path
	if _, err := (GitHubLookup{Environ: f.env, Repo: f.repo}).PullRequests(context.Background(), "abc"); err == nil {
		t.Fatal("PullRequests() succeeded for a non-GitHub origin")
	}
}
