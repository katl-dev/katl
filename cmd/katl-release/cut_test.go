package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

const (
	cutVersion = "2026.12.1"
	cutCommit  = "1111111111111111111111111111111111111111"
	movedMain  = "2222222222222222222222222222222222222222"
	cutCompare = repoPrefix + "compare/" + cutCommit + "...main?per_page=1"
	cutRuns    = repoPrefix + "actions/workflows/cut-release.yml/runs?branch=main&event=workflow_dispatch&per_page=100"
	cutRun     = repoPrefix + "actions/runs/9"
	cutRunURL  = followURL + "9"
)

func cutWorld(t *testing.T) *fakeGitHub {
	return &fakeGitHub{t: t, failures: map[string][]error{}, responses: map[string]any{
		cutCompare: map[string]string{"status": "identical"},
	}, pages: map[string][]any{
		followTags:   {[]any{map[string]string{"ref": "refs/tags/v2026.12.10"}}},
		followBranch: {[]any{map[string]string{"ref": "refs/heads/release/2026.12.10"}}},
		cutRuns:      {runList()},
	}}
}

func (f *fakeGitHub) cutBranch(sha string) {
	f.pages[followBranch] = []any{[]any{
		map[string]any{"ref": "refs/heads/release/2026.12.1", "object": map[string]string{"sha": sha, "type": "commit"}},
	}}
}

func noGitHubWrite(t *testing.T) command {
	return func(name string, args ...string) ([]byte, error) {
		t.Fatalf("unexpected write %s %q", name, args)
		return nil, nil
	}
}

func TestCutCheck(t *testing.T) {
	// Reusing a version with a tag, accepting an unmerged commit, or
	// creating a series branch would cut the wrong release.
	for _, tc := range []struct {
		name, version, status, tag, head string
		want                             []string
	}{
		{name: "unused final version", version: cutVersion},
		{name: "already cut", version: cutVersion, head: cutCommit},
		{name: "another commit", version: cutVersion, head: movedMain, want: []string{cutCommit, movedMain, "Choose the next version"}},
		{name: "candidate tag", version: cutVersion, tag: "v2026.12.1-rc.1", want: []string{"v2026.12.1-rc.1", "Choose the next version"}},
		{name: "final tag", version: cutVersion, tag: "v2026.12.1", want: []string{"v2026.12.1", "Choose the next version"}},
		{name: "unmerged commit", version: cutVersion, status: "behind", want: []string{"is not reachable from main"}},
		{name: "unknown commit", version: cutVersion, status: "404", want: []string{cutCommit, "could not be found", "Choose a commit from main"}},
		{name: "candidate version", version: "2026.12.1-rc.1", want: []string{"complete final version"}},
		{name: "series", version: "2026.12.x", want: []string{"complete final version"}},
		{name: "tag instead of version", version: "v2026.12.1", want: []string{"complete final version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := cutWorld(t)
			switch tc.status {
			case "404":
				f.responses[cutCompare] = &apiError{status: 404, text: "not found"}
			case "behind":
				f.responses[cutCompare] = map[string]string{"status": tc.status}
			}
			if tc.tag != "" {
				// A matching tag may be buried beyond the first page.
				f.pages[followTags] = append(f.pages[followTags], []any{map[string]string{"ref": "refs/tags/" + tc.tag}})
			}
			if tc.head != "" {
				f.cutBranch(tc.head)
			}
			for _, cmd := range []string{"cut", "cut-branch"} {
				var out strings.Builder
				call := noGitHubWrite(t)
				if len(tc.want) == 0 && tc.head == "" {
					// Reaching the requested write shows the preflight allowed the cut.
					call = func(_ string, _ ...string) ([]byte, error) {
						return nil, errors.New("fixture reached write")
					}
				}
				err := cut(t.Context(), []string{cmd, tc.version, cutCommit}, &out, io.Discard, newTestReader(f), call)
				switch {
				case len(tc.want) > 0:
					assertErrorContains(t, err, tc.want...)
				case tc.head == "":
					assertErrorContains(t, err, "fixture reached write")
				case err != nil || !strings.Contains(out.String(), "release/2026.12.1 is already cut at "+cutCommit):
					t.Fatalf("%s = %q, %v", cmd, out.String(), err)
				}
			}
		})
	}
}

func TestCutBranchWrite(t *testing.T) {
	// Trusting the POST response misses a conflicting head. Retrying a
	// create after a race or on a rerun makes a successful cut fail.
	for _, tc := range []struct {
		name, before, after string
		writeError          error
		want                []string
	}{
		{name: "create", after: cutCommit},
		{name: "already cut", before: cutCommit},
		{name: "wrong readback", after: movedMain, want: []string{cutCommit, movedMain}},
		{name: "raced to same commit", after: cutCommit, writeError: errors.New("already exists")},
		{name: "raced to other commit", after: movedMain, writeError: errors.New("already exists"), want: []string{cutCommit, movedMain}},
		{name: "creation rejected", writeError: errors.New("ref creation denied"), want: []string{"ref creation denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := cutWorld(t)
			if tc.before != "" {
				f.cutBranch(tc.before)
			}
			r := newTestReader(f)
			var operations []string
			r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
				if endpoint == followBranch {
					operations = append(operations, "read ref")
				}
				return f.read(ctx, endpoint, page, accept)
			}
			call := func(name string, args ...string) ([]byte, error) {
				operations = append(operations, "create ref")
				want := []string{"gh", "api", "--method", "POST", "repos/katl-dev/katl/git/refs",
					"-f", "ref=refs/heads/release/2026.12.1", "-f", "sha=" + cutCommit}
				if got := append([]string{name}, args...); !slices.Equal(got, want) {
					t.Fatalf("create = %q, want %q", got, want)
				}
				if tc.after != "" {
					f.cutBranch(tc.after)
				}
				return []byte(`{"object":{"sha":"` + cutCommit + `"}}`), tc.writeError
			}
			err := cut(t.Context(), []string{"cut-branch", cutVersion, cutCommit}, io.Discard, io.Discard, r, call)
			wantOps := []string{"read ref"}
			if tc.before == "" {
				wantOps = append(wantOps, "create ref", "read ref")
			}
			if !slices.Equal(operations, wantOps) {
				t.Fatalf("operations = %v, want %v", operations, wantOps)
			}
			if len(tc.want) > 0 {
				assertErrorContains(t, err, tc.want...)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func cutRunAt(id int64, conclusion string) map[string]any {
	run := listedRun(id, movedMain)
	run["path"], run["head_branch"] = ".github/workflows/cut-release.yml", "main"
	run["display_title"] = "Cut release 2026.12.1 at " + cutCommit
	run["status"], run["conclusion"] = "completed", conclusion
	return run
}

func TestCutBindsRevision(t *testing.T) {
	// Resolving main again would dispatch or confirm a different commit.
	// With no dispatch URL, the most recent run can belong to another cut.
	synctest.Test(t, func(t *testing.T) {
		f := cutWorld(t)
		r := newTestReader(f)
		ref := repoPrefix + "commits/main"
		f.responses[ref] = map[string]string{"sha": cutCommit}
		f.responses[cutRun] = cutRunAt(9, "success")
		listJobs(f, 9, 1, listedJob(1, "Create release branch", "completed", "success"))
		resolutions := 0
		r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
			body, err := f.read(ctx, endpoint, page, accept)
			if endpoint == ref {
				resolutions++
				f.responses[ref] = map[string]string{"sha": movedMain}
				f.responses[cutCompare] = map[string]string{"status": "ahead"}
			}
			return body, err
		}
		dispatches := 0
		call := func(name string, args ...string) ([]byte, error) {
			dispatches++
			want := []string{"gh", "workflow", "run", "cut-release.yml", "--repo", "katl-dev/katl", "--ref", "main",
				"-f", "version=2026.12.1", "-f", "revision=" + cutCommit}
			if got := append([]string{name}, args...); !slices.Equal(got, want) {
				t.Fatalf("dispatch = %q, want %q", got, want)
			}
			other := cutRunAt(10, "failure")
			other["display_title"] = "Cut release 2026.12.2 at " + cutCommit
			f.pages[cutRuns] = []any{runList(other, cutRunAt(9, "success"))}
			f.cutBranch(cutCommit)
			return nil, nil
		}
		var out strings.Builder
		err := cut(t.Context(), []string{"cut", cutVersion, "main"}, &out, io.Discard, r, call)
		if err != nil || resolutions != 1 || dispatches != 1 {
			t.Fatalf("cut = %v after %d resolutions and %d dispatches", err, resolutions, dispatches)
		}
		for _, line := range []string{"Source: " + cutCommit, cutRunURL, "release/2026.12.1 exists at " + cutCommit,
			"Next: scripts/katl-release release 2026.12.1"} {
			if !strings.Contains(out.String(), line) {
				t.Errorf("output %q needs %q", out.String(), line)
			}
		}
	})
}

func TestCutRunOutcome(t *testing.T) {
	// A successful run alone cannot prove the intended branch exists.
	// A failed run needs the actual job and run URL for diagnosis.
	for _, tc := range []struct {
		name, conclusion, head string
		want                   []string
	}{
		{"failed job", "failure", "", []string{"failed: Create release branch", cutRunURL}},
		{"absent branch", "success", "", []string{"release/2026.12.1 is missing", cutRunURL}},
		{"wrong branch head", "success", movedMain, []string{cutCommit, movedMain, cutRunURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := cutWorld(t)
			f.responses[cutRun] = cutRunAt(9, tc.conclusion)
			listJobs(f, 9, 1, listedJob(1, "Create release branch", "completed", tc.conclusion))
			var out strings.Builder
			err := cut(t.Context(), []string{"cut", cutVersion, cutCommit}, &out, io.Discard, newTestReader(f), func(string, ...string) ([]byte, error) {
				if tc.head != "" {
					f.cutBranch(tc.head)
				}
				return []byte(cutRunURL), nil
			})
			assertErrorContains(t, err, tc.want...)
			if !strings.Contains(out.String(), cutRunURL) || strings.Contains(out.String(), "Next:") {
				t.Errorf("cut output = %q", out.String())
			}
		})
	}
}
