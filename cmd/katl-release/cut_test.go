package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/releaseversion"
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
		{name: "already cut and tagged", version: cutVersion, head: cutCommit, tag: "v2026.12.1-rc.1"},
		{name: "another commit and tagged", version: cutVersion, head: movedMain, tag: "v2026.12.1-rc.1", want: []string{movedMain, "Choose the next version"}},
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
	// create after a race or on a rerun makes a successful cut fail. GitHub
	// can list the refs without a ref it has just created, so an accepted
	// create waits for the ref, while a refused one is checked once.
	const never = -1
	for _, tc := range []struct {
		name, before, after string
		// pending is how many reads after the create still miss the ref.
		pending    int
		writeError error
		want       []string
	}{
		{name: "create", after: cutCommit},
		{name: "create shows late", after: cutCommit, pending: 2},
		{name: "already cut", before: cutCommit},
		{name: "wrong readback", after: movedMain, want: []string{cutCommit, movedMain}},
		{name: "wrong readback shows late", after: movedMain, pending: 2, want: []string{cutCommit, movedMain}},
		{name: "absent readback", pending: never, want: []string{"did not return refs/heads/release/2026.12.1"}},
		{name: "raced to same commit", after: cutCommit, writeError: errors.New("already exists")},
		{name: "raced to other commit", after: movedMain, writeError: errors.New("already exists"), want: []string{cutCommit, movedMain}},
		{name: "creation rejected", writeError: errors.New("ref creation denied"), want: []string{"ref creation denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := cutWorld(t)
				if tc.before != "" {
					f.cutBranch(tc.before)
				}
				r := newTestReader(f)
				created := false
				readbacks := 0
				r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
					if endpoint == followBranch && created {
						readbacks++
						if tc.after != "" && tc.pending != never && readbacks > tc.pending {
							f.cutBranch(tc.after)
						}
					}
					return f.read(ctx, endpoint, page, accept)
				}
				call := func(name string, args ...string) ([]byte, error) {
					want := []string{"gh", "api", "--method", "POST", "repos/katl-dev/katl/git/refs",
						"-f", "ref=refs/heads/release/2026.12.1", "-f", "sha=" + cutCommit}
					if got := append([]string{name}, args...); !slices.Equal(got, want) {
						t.Fatalf("create = %q, want %q", got, want)
					}
					if created {
						t.Fatal("created the ref twice")
					}
					created = true
					return []byte(`{"object":{"sha":"` + cutCommit + `"}}`), tc.writeError
				}

				start := time.Now()
				err := cut(t.Context(), []string{"cut-branch", cutVersion, cutCommit}, io.Discard, io.Discard, r, call)
				elapsed := time.Since(start)

				switch {
				case tc.before != "" && created:
					t.Fatal("created a ref that already existed")
				case tc.before == "" && tc.pending == never:
					if readbacks < 2 || elapsed != readbackWait {
						t.Fatalf("gave up after %d reads in %s; want the %s deadline", readbacks, elapsed, readbackWait)
					}
				case tc.before == "" && readbacks != tc.pending+1:
					t.Fatalf("read the ref %d times after the create; want %d", readbacks, tc.pending+1)
				}
				if len(tc.want) > 0 {
					assertErrorContains(t, err, tc.want...)
				} else if err != nil {
					t.Fatal(err)
				}
			})
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
	// A successful run alone cannot prove the intended branch exists, and
	// GitHub can list the refs without the branch the run has just created.
	// A failed run needs the actual job and run URL for diagnosis.
	const never = -1
	for _, tc := range []struct {
		name, conclusion, head string
		// pending is how many reads after the run still miss the branch.
		pending int
		want    []string
	}{
		{name: "failed job", conclusion: "failure", want: []string{"failed: Create release branch", cutRunURL}},
		{name: "branch shows late", conclusion: "success", head: cutCommit, pending: 2},
		{name: "absent branch", conclusion: "success", pending: never, want: []string{"release/2026.12.1 is missing", cutRunURL}},
		{name: "wrong branch head", conclusion: "success", head: movedMain, want: []string{cutCommit, movedMain, cutRunURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := cutWorld(t)
				f.responses[cutRun] = cutRunAt(9, tc.conclusion)
				listJobs(f, 9, 1, listedJob(1, "Create release branch", "completed", tc.conclusion))
				r := newTestReader(f)
				dispatched := false
				readbacks := 0
				r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
					if endpoint == followBranch && dispatched {
						readbacks++
						if tc.head != "" && tc.pending != never && readbacks > tc.pending {
							f.cutBranch(tc.head)
						}
					}
					return f.read(ctx, endpoint, page, accept)
				}

				var out strings.Builder
				err := cut(t.Context(), []string{"cut", cutVersion, cutCommit}, &out, io.Discard, r, func(string, ...string) ([]byte, error) {
					dispatched = true
					return []byte(cutRunURL), nil
				})

				if tc.want == nil {
					if err != nil || readbacks != tc.pending+1 || !strings.Contains(out.String(), "release/2026.12.1 exists at "+cutCommit) {
						t.Fatalf("cut = %q, %v after %d reads of the branch", out.String(), err, readbacks)
					}
					return
				}
				assertErrorContains(t, err, tc.want...)
				if !strings.Contains(out.String(), cutRunURL) || strings.Contains(out.String(), "Next:") {
					t.Errorf("cut output = %q", out.String())
				}
			})
		})
	}
}

func TestCutRunTitle(t *testing.T) {
	// The cut follows only a run whose title the workflow renders from the
	// same inputs, so a changed run-name would leave every cut unfollowed.
	// Until GitHub renders it, the title is the workflow name, which the cut
	// must recognise to wait rather than refuse.
	data, err := os.ReadFile(filepath.Join("..", "..", cutWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Name    string `yaml:"name"`
		RunName string `yaml:"run-name"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if target := cutDispatch(releaseversion.Version{}, cutCommit); workflow.Name != target.name {
		t.Fatalf("workflow name %q, want %q", workflow.Name, target.name)
	}
	title := strings.NewReplacer("${{ inputs.version }}", cutVersion, "${{ inputs.revision }}", cutCommit).Replace(workflow.RunName)
	if title == "" || strings.Contains(title, "${{") {
		t.Fatalf("run-name %q renders as %q", workflow.RunName, title)
	}

	f := cutWorld(t)
	run := cutRunAt(9, "success")
	run["display_title"] = title
	f.responses[cutRun] = run
	listJobs(f, 9, 1, listedJob(1, "Create release branch", "completed", "success"))
	err = cut(t.Context(), []string{"cut", cutVersion, cutCommit}, io.Discard, io.Discard, newTestReader(f), func(string, ...string) ([]byte, error) {
		f.cutBranch(cutCommit)
		return []byte(cutRunURL), nil
	})
	if err != nil {
		t.Fatalf("cut did not follow the run titled %q: %v", title, err)
	}
}

func TestCutDispatchedRun(t *testing.T) {
	// GitHub titles a run with the workflow name until it evaluates
	// run-name, and can report a fresh run with empty fields, so a single
	// read can refuse a correct run. Waiting on a non-empty mismatch in a
	// field fixed at creation would only delay refusing another run. Without
	// a URL from gh, the listed run is read again before it is followed.
	const always = math.MaxInt
	for _, tc := range []struct {
		name    string
		pending int
		// blank empties the identity fields while the title is pending.
		blank  bool
		listed bool
		field  string
		value  any
		want   []string
	}{
		{name: "title converges", pending: 2},
		{name: "title never converges", pending: always,
			want: []string{cutRunURL, `still has the workflow name "Cut release" as its title`, "Check the Cut release runs"}},
		{name: "fields fill in", pending: 2, blank: true},
		{name: "fields never fill in", pending: always, blank: true,
			want: []string{"The dispatched run " + cutRunURL, "after " + readbackWait.String(), "Check the Cut release runs"}},
		{name: "wrong run ID", pending: always, field: "id", value: 12},
		{name: "wrong branch", pending: always, field: "head_branch", value: "release/2026.12.1"},
		{name: "wrong branch while fields fill in", pending: always, blank: true, field: "head_branch", value: "release/2026.12.1"},
		{name: "wrong workflow", pending: always, field: "path", value: ".github/workflows/other.yml"},
		{name: "wrong event", pending: always, field: "event", value: "push"},
		{name: "another cut's title", field: "display_title", value: "Cut release 2026.12.2 at " + cutCommit},
		{name: "listed title converges", pending: 2, listed: true},
		{name: "listed title never converges", pending: always, listed: true,
			want: []string{cutRunURL, `still has the workflow name "Cut release" as its title`, "Check the Cut release runs"}},
		{name: "listed run reads differently", pending: always, listed: true, field: "head_branch", value: "release/2026.12.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := cutWorld(t)
				run := cutRunAt(9, "success")
				f.responses[cutRun] = run
				listJobs(f, 9, 1, listedJob(1, "Create release branch", "completed", "success"))
				r := newTestReader(f)
				reads := 0
				r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
					if endpoint == cutRun {
						reads++
						fresh := cutRunAt(9, "success")
						for _, key := range []string{"display_title", "path", "head_branch", "event"} {
							run[key] = fresh[key]
						}
						if reads <= tc.pending {
							run["display_title"] = "Cut release"
							if tc.blank {
								run["path"], run["head_branch"], run["event"] = "", "", ""
							}
						}
						if tc.field != "" {
							run[tc.field] = tc.value
						}
					}
					return f.read(ctx, endpoint, page, accept)
				}

				start := time.Now()
				var out strings.Builder
				err := cut(t.Context(), []string{"cut", cutVersion, cutCommit}, &out, io.Discard, r, func(string, ...string) ([]byte, error) {
					f.cutBranch(cutCommit)
					if tc.listed {
						listed := cutRunAt(9, "success")
						if tc.pending > 0 {
							listed["display_title"] = "Cut release"
						}
						f.pages[cutRuns] = []any{runList(listed)}
						return nil, nil
					}
					return []byte(cutRunURL), nil
				})
				elapsed := time.Since(start)

				switch {
				case tc.field != "":
					assertErrorContains(t, err, "The dispatched run "+cutRunURL, fmt.Sprint(tc.value), "Check the Cut release runs")
					if reads != 1 {
						t.Fatalf("refused after %d reads; want 1", reads)
					}
				case tc.want != nil:
					assertErrorContains(t, err, tc.want...)
					if elapsed < readbackWait || elapsed > readbackWait+time.Second {
						t.Fatalf("refused after %s; want the %s deadline", elapsed, readbackWait)
					}
				case err != nil || !strings.Contains(out.String(), "release/2026.12.1 exists at "+cutCommit):
					t.Fatalf("cut = %q, %v after %d reads", out.String(), err, reads)
				}
			})
		})
	}
}
