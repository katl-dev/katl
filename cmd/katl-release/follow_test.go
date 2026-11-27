package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/katl-dev/katl/internal/releaseversion"
)

const (
	followVersion  = "2026.12.1"
	followRef      = "refs/heads/release/2026.12.1"
	followQuery    = repoPrefix + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&event=workflow_dispatch&per_page=100"
	followURL      = "https://github.com/katl-dev/katl/actions/runs/"
	followTags     = repoPrefix + "git/matching-refs/tags/v2026.12.1?per_page=100"
	followBranch   = repoPrefix + "git/matching-refs/heads/release/2026.12.1?per_page=100"
	followReleases = repoPrefix + "releases?per_page=100"
)

func listedRun(id int64, commit string) map[string]any {
	run := runAttempt(id, 1, commit, "2026-12-01T03:00:00Z")
	run["html_url"] = fmt.Sprint(followURL, id)
	return run
}

func latestRun(f *fakeGitHub, id int64, attempt int, state, conclusion string) {
	run := listedRun(id, readerHead)
	run["run_attempt"], run["status"], run["conclusion"] = attempt, state, conclusion
	f.responses[fmt.Sprintf("%sactions/runs/%d", repoPrefix, id)] = run
}

func listJobs(f *fakeGitHub, id int64, attempt int, jobs ...map[string]any) {
	f.pages[fmt.Sprintf("%sactions/runs/%d/attempts/%d/jobs?per_page=100", repoPrefix, id, attempt)] = []any{
		map[string]any{"jobs": jobs},
	}
}

func listedJob(id int64, name, status, conclusion string) map[string]any {
	return map[string]any{"id": id, "name": name, "status": status, "conclusion": conclusion}
}

func pendingRelease(f *fakeGitHub, id int64, environment string) {
	f.responses[fmt.Sprintf("%sactions/runs/%d/pending_deployments", repoPrefix, id)] = []any{
		map[string]any{"environment": map[string]any{"name": environment}},
	}
}

type readbackFixture struct {
	release     map[string]any
	sumsAsset   map[string]any
	digest      string
	attestation *fakeProvenance
}

// qualifiedFixture gives the state machine REST records, artifacts, and a
// Sigstore response for one qualified attempt, whether or not it published.
func qualifiedFixture(t *testing.T, f *fakeGitHub, id int64, attempt int, tag, assetDigest string) readbackFixture {
	t.Helper()
	const start = "2026-12-01T03:00:00Z"
	url := fmt.Sprintf("%s%d/attempts/%d", followURL, id, attempt)
	sums := []byte(assetDigest + "  katlctl\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(sums))
	statuses := repoPrefix + "commits/" + readerHead + "/statuses?per_page=100"
	f.pages[statuses] = append(f.pages[statuses], []any{map[string]any{
		"context": "katl/release/" + tag, "state": "success", "description": "qualified sha256:" + digest,
		"target_url": url, "creator": botCreator,
	}})
	endpoint := fmt.Sprintf("%sactions/runs/%d/attempts/", repoPrefix, id)
	run := runAttempt(id, attempt, readerHead, start)
	run["conclusion"] = "success"
	f.responses[fmt.Sprint(endpoint, attempt)] = run
	f.responses[fmt.Sprint(endpoint, attempt+1)] = &apiError{status: 404, text: "Not Found"}
	listJobs(f, id, attempt, listedJob(1, "Build / Release validation", "completed", "success"))
	f.pages[fmt.Sprintf("%sactions/runs/%d/artifacts?per_page=100", repoPrefix, id)] = []any{map[string]any{
		"artifacts": []any{map[string]any{
			"name": "katl-" + tag[1:] + "-x86_64-111111111111", "created_at": "2026-12-01T03:30:00Z",
			"workflow_run": map[string]any{"id": id},
		}},
	}}

	sumsAsset := map[string]any{"id": id, "name": "SHA256SUMS", "state": "uploaded", "digest": "sha256:" + digest}
	f.raw[fmt.Sprintf("%sreleases/assets/%d", repoPrefix, id)] = sums
	attestation := &fakeProvenance{t: t, sums: sums, output: fmt.Sprintf(`[{"verificationResult":{
		"signature":{"certificate":{
			"buildSignerURI":"https://github.com/katl-dev/katl/.github/workflows/release-artifacts.yml@%s",
			"buildConfigURI":"https://github.com/katl-dev/katl/.github/workflows/release.yml@%s",
			"runInvocationURI":"%s","sourceRepositoryDigest":"%s","sourceRepositoryRef":"%s"}},
		"statement":{"subject":[{"name":"SHA256SUMS","digest":{"sha256":"%s"}}]}}}]`,
		followRef, followRef, url, readerHead, followRef, digest)}
	return readbackFixture{
		release: map[string]any{
			"tag_name": tag, "prerelease": tag != "v2026.12.1", "html_url": "https://github.com/katl-dev/katl/releases/tag/" + tag,
			"assets": []any{sumsAsset, map[string]any{"id": id + 100, "name": "katlctl", "state": "uploaded", "digest": "sha256:" + assetDigest}},
		},
		sumsAsset: sumsAsset, attestation: attestation, digest: digest,
	}
}

func releaseReadback(t *testing.T, publishCandidate, recordFinal, publishFinal bool) (*fakeGitHub, *reader, readbackFixture, readbackFixture) {
	t.Helper()
	f := readerWorld(t)
	f.pages["orgs/katl-dev/packages?package_type=container&per_page=100"] = []any{[]any{}}
	rc := qualifiedFixture(t, f, 7, 1, "v2026.12.1-rc.1", strings.Repeat("c", 64))
	var final readbackFixture
	var releases []any
	if publishCandidate {
		releases = append(releases, rc.release)
	}
	if recordFinal {
		// An attempt of the same run, even if the latest attempt has a
		// different number, identifies this as a final run.
		final = qualifiedFixture(t, f, 8, 1, "v2026.12.1", strings.Repeat("d", 64))
	}
	if publishFinal {
		tags := f.pages[followTags][0].([]any)
		f.pages[followTags][0] = append(tags, map[string]any{
			"ref": "refs/tags/v2026.12.1", "object": map[string]any{"sha": readerHead, "type": "commit"},
		})
		releases = append(releases, final.release)
	}
	f.pages[followReleases] = []any{releases}

	r := newTestReader(f)
	r.provenance = &provenance{repository: "katl-dev/katl", call: func(name string, args ...string) ([]byte, error) {
		if args[1] == "trusted-root" {
			return []byte(testTrustedRoot), nil
		}
		body, err := os.ReadFile(args[2])
		if err != nil {
			t.Fatal(err)
		}
		if string(body) == string(rc.attestation.sums) {
			return rc.attestation.call(name, args...)
		}
		if final.attestation != nil {
			return final.attestation.call(name, args...)
		}
		t.Fatalf("unexpected published checksums: %s", body)
		return nil, nil
	}}
	return f, r, rc, final
}

func TestReleaseRefusals(t *testing.T) {
	// A preflight that only checks the branch, or discards --retry, dispatches
	// runs that the decision refuses.
	for _, tc := range []struct {
		state, version, reason string
		row                    int
		edit                   func(*fakeGitHub)
	}{
		{"not-release-branch", "2026.12.1-rc.1", "", 1, func(*fakeGitHub) {}},
		{"invalid-version", "2026.12.x", "", 1, func(*fakeGitHub) {}},
		{"branch-missing", followVersion, "", 2, func(f *fakeGitHub) { f.pages[followBranch] = []any{[]any{}} }},
		{"run-active", followVersion, "", 13, func(f *fakeGitHub) {
			f.pages[runsQuery("queued")] = []any{runList(listedRun(10, readerHead))}
			f.responses[repoPrefix+"actions/runs/10/pending_deployments"] = []any{}
		}},
		{"approval-pending", followVersion, "", 12, func(f *fakeGitHub) {
			f.pages[runsQuery("waiting")] = []any{runList(listedRun(10, readerHead))}
			pendingRelease(f, 10, "release")
		}},
		{"candidate-failed", followVersion, "", 19, func(*fakeGitHub) {}},
		{"retry-not-applicable", followVersion, "runner lost", 27, func(f *fakeGitHub) { f.pages[followTags] = []any{[]any{}} }},
		{"released", followVersion, "", 14, func(*fakeGitHub) {}},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := readerWorld(t)
			r := newTestReader(f)
			if tc.state == "released" {
				f, r, _, _ = releaseReadback(t, true, true, true)
			}
			tc.edit(f)
			d, err := decide(t.Context(), r, "refs/heads/release/"+tc.version, "", tc.reason)
			if err != nil || d.Row != tc.row || d.Message == "" {
				t.Fatalf("fixture decision = %+v, %v; want refusal row %d", d, err, tc.row)
			}
			args := []string{tc.version}
			if tc.reason != "" {
				args = append(args, "--retry", tc.reason)
			}
			var out strings.Builder
			dispatched := false
			err = release(t.Context(), args, &out, io.Discard, r, func(string, ...string) ([]byte, error) {
				dispatched = true
				return nil, nil
			})
			if dispatched || err == nil || err.Error() != d.Message || !strings.Contains(out.String(), "Refusal: "+d.Message) {
				t.Fatalf("release = %v, dispatched = %t, output = %q; want %q without a dispatch", err, dispatched, out.String(), d.Message)
			}
		})
	}
}

func TestReleaseDispatch(t *testing.T) {
	// A dropped retry reason leaves the failed candidate refused. An empty
	// -f changes the dispatch args of an ordinary release.
	for _, tc := range []struct {
		name          string
		args, command []string
	}{
		{"retry", []string{"2026.12.1", "--retry", "runner lost during Production VM (lts)"},
			[]string{"gh", "workflow", "run", "release.yml", "--repo", "katl-dev/katl", "--ref", "release/2026.12.1", "-f", "retry=runner lost during Production VM (lts)"}},
		{"plain", []string{"2026.12.1"},
			[]string{"gh", "workflow", "run", "release.yml", "--repo", "katl-dev/katl", "--ref", "release/2026.12.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			if tc.name == "plain" {
				f.pages[followTags] = []any{[]any{}}
			}
			f.pages[followQuery] = []any{runList(listedRun(5, readerHead))}
			latestRun(f, 9, 1, "completed", "cancelled")
			listJobs(f, 9, 1)
			count := 0
			err := release(t.Context(), tc.args, io.Discard, io.Discard, newTestReader(f), func(name string, args ...string) ([]byte, error) {
				count++
				if got := append([]string{name}, args...); !slices.Equal(got, tc.command) {
					t.Fatalf("dispatch = %q; want %q", got, tc.command)
				}
				f.pages[followQuery] = []any{runList(listedRun(5, readerHead), listedRun(9, readerHead))}
				return nil, nil
			})
			if count != 1 || err == nil || !strings.Contains(err.Error(), "was cancelled ("+followURL+"9)") {
				t.Fatalf("dispatch count %d, error = %v; want the outcome of run 9", count, err)
			}
		})
	}
}

func TestFindDispatch(t *testing.T) {
	// A previous run, another ref, or another commit is never adopted. An
	// ambiguous or absent run terminates with a route back into wait.
	version, err := releaseversion.Parse(followVersion)
	if err != nil {
		t.Fatal(err)
	}
	otherCommit := strings.Repeat("2", 40)
	for _, mode := range []string{"match", "absent", "ambiguous", "moved"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := readerWorld(t)
				r := newTestReader(f)
				old, foreign := listedRun(5, readerHead), listedRun(80, readerHead)
				foreign["head_branch"] = "release/2026.12.10"
				wrongPath, wrongEvent := listedRun(81, readerHead), listedRun(82, readerHead)
				wrongPath["path"], wrongEvent["event"] = ".github/workflows/another.yml", "push"
				moved := listedRun(12, otherCommit)
				polls := 0
				r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
					if endpoint == followQuery {
						polls++
						runs := []map[string]any{old, foreign, wrongPath, wrongEvent}
						if mode == "moved" || mode == "match" {
							runs = append(runs, moved)
						}
						if mode == "ambiguous" {
							runs = append(runs, listedRun(9, readerHead), listedRun(10, readerHead))
						}
						if mode == "match" && polls >= 3 {
							runs = append(runs, listedRun(9, readerHead))
						}
						f.pages[followQuery] = []any{runList(runs...)}
					}
					return f.read(ctx, endpoint, page, accept)
				}

				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				defer cancel()
				start := time.Now()
				run, err := findDispatch(ctx, r, version, 6, readerHead)
				if mode == "match" {
					if err != nil || run.ID != 9 || polls != 3 {
						t.Fatalf("run = %d, error = %v after %d polls; want run 9 at poll 3", run.ID, err, polls)
					}
					return
				}
				recover := "Run scripts/katl-release wait 2026.12.1."
				want := map[string][]string{
					"absent":    {"The dispatch was accepted but no Release run for 2026.12.1 at " + readerHead + " appeared. " + recover},
					"ambiguous": {followURL + "9", followURL + "10", recover},
					"moved":     {readerHead, otherCommit, "between the decision and dispatch", "will refuse (row 3)", followURL + "12", recover},
				}
				assertErrorContains(t, err, want[mode]...)
				if elapsed := time.Since(start); mode == "absent" && (elapsed < 100*time.Second || elapsed > 140*time.Second) {
					t.Fatalf("lookup lasted %s, want about two minutes", elapsed)
				}
			})
		})
	}
}

func TestWaitFailures(t *testing.T) {
	// A completed failed run needs the latest attempt's failed jobs, in
	// listing order, or its cancellation. A skipped job is not a failure.
	for _, concl := range []string{"failure", "cancelled", "refused"} {
		t.Run(concl, func(t *testing.T) {
			f := readerWorld(t)
			f.pages[followQuery] = []any{runList(listedRun(10, readerHead), listedRun(11, readerHead))}
			switch concl {
			case "failure":
				latestRun(f, 11, 2, "completed", "failure")
				listJobs(f, 11, 2, listedJob(2, "Build / Production VM (lts)", "completed", "failure"),
					listedJob(3, "Publish", "completed", "skipped"),
					listedJob(4, "Record qualification", "completed", "timed_out"))
			case "cancelled":
				latestRun(f, 11, 2, "completed", "cancelled")
				listJobs(f, 11, 2)
			case "refused":
				latestRun(f, 11, 2, "completed", "failure")
				listJobs(f, 11, 2, listedJob(1, "Decide", "completed", "failure"))
			}

			err := waitRelease(t.Context(), []string{followVersion}, io.Discard, io.Discard, newTestReader(f))
			want := map[string]string{
				"failure":   "Build / Production VM (lts), Record qualification",
				"cancelled": "was cancelled",
				"refused":   "v2026.12.1-rc.1 did not qualify at 111111111111: the run was cancelled",
			}
			assertErrorContains(t, err, want[concl], followURL+"11")
			if strings.Contains(err.Error(), "Publish") || (concl == "refused" && !strings.Contains(err.Error(), "Decide")) {
				t.Fatalf("unexpected failed jobs: %v", err)
			}
		})
	}
}

func TestWaitNoRun(t *testing.T) {
	f := readerWorld(t)
	f.pages[followQuery] = []any{runList()}
	err := waitRelease(t.Context(), []string{followVersion}, io.Discard, io.Discard, newTestReader(f))
	assertErrorContains(t, err, "No Release run for 2026.12.1", "Run scripts/katl-release release 2026.12.1.")
}

func TestWaitApproval(t *testing.T) {
	// Repeated job and approval observations do not repeat progress or let
	// the command exit before publication. A different environment is not
	// the release approval.
	for _, environment := range []string{"release", "staging"} {
		t.Run(environment, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, r, _, final := releaseReadback(t, true, true, true)
				f.pages[followQuery] = []any{runList(listedRun(8, readerHead))}
				pendingRelease(f, 8, environment)

				const pollRun = repoPrefix + "actions/runs/8"
				const pending = pollRun + "/pending_deployments"
				polls, pendingReads := 0, 0
				r.read = func(ctx context.Context, endpoint string, page bool, accept string) ([]byte, error) {
					switch endpoint {
					case pollRun:
						polls++
						if polls <= 3 {
							latestRun(f, 8, 2, "waiting", "")
							listJobs(f, 8, 2, listedJob(30, "Publish", "in_progress", ""))
						} else {
							latestRun(f, 8, 2, "completed", "success")
							listJobs(f, 8, 2, listedJob(30, "Publish", "completed", "success"))
						}
					case pending:
						pendingReads++
					}
					return f.read(ctx, endpoint, page, accept)
				}
				var out, progress strings.Builder
				err := waitRelease(t.Context(), []string{followVersion}, &out, &progress, r)
				if err != nil || polls != 4 || pendingReads == 0 || !strings.Contains(out.String(), final.digest) {
					t.Fatalf("wait = %v, polls = %d, pending reads = %d, stdout = %q; want the final publication", err, polls, pendingReads, out.String())
				}
				want := "Publish: started\n"
				if environment == "release" {
					want += "v2026.12.1 is waiting for approval: " + followURL + "8\n"
				}
				want += "Publish: success\n"
				if progress.String() != want {
					t.Fatalf("progress = %q, want %q", progress.String(), want)
				}
			})
		})
	}
}

func TestWaitReadFailure(t *testing.T) {
	// The follower leaves retries to the reader and gives up when they are
	// exhausted, even if the workflow itself is still running.
	synctest.Test(t, func(t *testing.T) {
		f := readerWorld(t)
		f.pages[followQuery] = []any{runList(listedRun(11, readerHead))}
		latestRun(f, 11, 1, "in_progress", "")
		endpoint := repoPrefix + "actions/runs/11"
		f.failures[endpoint] = slices.Repeat([]error{&apiError{status: 502, text: "Bad Gateway (HTTP 502)"}}, readAttempts)
		err := waitRelease(t.Context(), []string{followVersion}, io.Discard, io.Discard, newTestReader(f))
		assertErrorContains(t, err, "Bad Gateway (HTTP 502)")
		if len(f.failures[endpoint]) != 0 {
			t.Fatal("reader did not retry transient failures")
		}
	})
}

func TestWaitCandidateReadback(t *testing.T) {
	// A green run alone cannot confirm a candidate. The state machine must
	// accept its actual release, or recognize the branch moved afterward.
	for _, tc := range []struct {
		name, failure string
	}{
		{"published", ""},
		{"extensions", ""},
		{"superseded", ""},
		{"changed digest", "The GitHub pre-release v2026.12.1-rc.1 disagrees with what the release action recorded"},
		{"missing release", "candidate-incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r, rc, _ := releaseReadback(t, tc.name != "missing release", false, false)
			f.pages[followQuery] = []any{runList(listedRun(7, readerHead))}
			latestRun(f, 7, 1, "completed", "success")
			switch tc.name {
			case "changed digest":
				rc.sumsAsset["digest"] = "sha256:" + strings.Repeat("e", 64)
			case "extensions":
				original := readerWorld(t)
				endpoint := "orgs/katl-dev/packages?package_type=container&per_page=100"
				f.pages[endpoint] = original.pages[endpoint]
			case "superseded":
				other := strings.Repeat("2", 40)
				f.pages[followBranch] = []any{[]any{map[string]any{"ref": followRef, "object": map[string]any{"sha": other, "type": "commit"}}}}
				f.responses[repoPrefix+"compare/"+readerHead+"..."+other+"?per_page=1"] = map[string]any{"status": "ahead"}
			}
			var out strings.Builder
			err := waitRelease(t.Context(), []string{followVersion}, &out, io.Discard, r)
			if tc.failure != "" {
				assertErrorContains(t, err, "The Release run succeeded, but "+tc.failure, followURL+"7")
				return
			}
			want := "Published v2026.12.1-rc.1 sha256:" + rc.digest
			if tc.name == "superseded" {
				want = "a new commit on release/2026.12.1 supersedes its candidate"
			}
			if err != nil || !strings.Contains(out.String(), want) {
				t.Fatalf("wait = %v, output = %q; want %q", err, out.String(), want)
			}
		})
	}
}

func TestWaitFinalReadback(t *testing.T) {
	// The final record can name an earlier attempt of the selected run. A
	// qualified build alone does not establish that the final was published.
	for _, tc := range []struct {
		name, failure  string
		tag, published bool
	}{
		{"published", "", true, true},
		{"tag only", "The GitHub release v2026.12.1 disagrees with what the release action recorded", true, false},
		{"no final tag", "final-unapproved", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r, _, final := releaseReadback(t, true, true, tc.tag)
			if tc.tag && !tc.published {
				f.pages[followReleases][0] = f.pages[followReleases][0].([]any)[:1]
			}
			f.pages[followQuery] = []any{runList(listedRun(8, readerHead))}
			latestRun(f, 8, 3, "completed", "success")
			listJobs(f, 8, 3)
			var out strings.Builder
			err := waitRelease(t.Context(), []string{followVersion}, &out, io.Discard, r)
			if tc.failure != "" {
				assertErrorContains(t, err, "The Release run succeeded, but "+tc.failure, followURL+"8")
				return
			}
			if err != nil || !strings.Contains(out.String(), final.digest) ||
				!strings.Contains(out.String(), "https://github.com/katl-dev/katl/releases/tag/v2026.12.1.") {
				t.Fatalf("wait = %v, output = %q; want the final release URL and digest", err, out.String())
			}
		})
	}
}

func assertErrorContains(t *testing.T, err error, fragments ...string) {
	t.Helper()
	for _, want := range fragments {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v; want %q", err, want)
		}
	}
}
