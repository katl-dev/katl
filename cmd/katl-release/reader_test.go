package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// fakeGitHub answers GET endpoints; paginated reads get the pages as one
// array, and raw holds the bodies of reads that ask for raw content. Like
// GitHub for the Release workflow's token, it refuses the Packages API.
type fakeGitHub struct {
	t         *testing.T
	responses map[string]any
	pages     map[string][]any
	raw       map[string][]byte
	failures  map[string][]error
	// registry serves the extension repositories.
	registry *fakeRegistry
}

func (f *fakeGitHub) read(_ context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
	if strings.HasPrefix(endpoint, "orgs/") && strings.Contains(endpoint, "/packages") {
		return nil, &apiError{status: 400, text: "gh: Invalid argument. (HTTP 400)"}
	}
	// Every commit of the fixtures has readerHead's tree.
	if path, ref, ok := strings.Cut(endpoint, "contents/"); ok {
		if rest, commit, ok := strings.Cut(ref, "?ref="); ok && commit != readerHead {
			endpoint = path + "contents/" + rest + "?ref=" + readerHead
		}
	}
	if queue := f.failures[endpoint]; len(queue) > 0 {
		f.failures[endpoint] = queue[1:]
		return nil, queue[0]
	}
	if body, ok := f.raw[endpoint]; ok {
		if (accept != "application/octet-stream" && accept != "application/vnd.github.raw") || paginate {
			f.t.Errorf("read %s as %q, want its raw content", endpoint, accept)
		}
		return body, nil
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

const repoPrefix = "repos/katl-dev/katl/"

var botCreator = map[string]any{"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}

func runAttempt(id int64, attempt int, sha, started string) map[string]any {
	return map[string]any{
		"id": id, "run_attempt": attempt, "head_branch": "release/2026.12.1", "head_sha": sha, "event": "workflow_dispatch",
		"path": ".github/workflows/release.yml", "status": "completed", "conclusion": "cancelled",
		"run_started_at": started, "repository": map[string]any{"full_name": "katl-dev/katl"},
		"head_repository": map[string]any{"full_name": "katl-dev/katl"},
	}
}

func runsQuery(status string) string {
	return repoPrefix + "actions/workflows/release.yml/runs?branch=release%2F2026.12.1&status=" + status + "&per_page=100"
}

// readerWorld is release/2026.12.1 at readerHead with rc.1 tagged there and
// one cancelled build of it.
func readerWorld(t *testing.T) *fakeGitHub {
	repo := repoPrefix
	none := []any{[]any{}}
	noRuns := []any{map[string]any{"workflow_runs": []any{}}}
	f := &fakeGitHub{t: t, responses: map[string]any{
		repo + "actions/workflows/release.yml":                              map[string]any{"path": ".github/workflows/release.yml"},
		repo + "git/tags/" + readerTag:                                      map[string]any{"object": map[string]any{"sha": readerHead, "type": "commit"}},
		repo + "compare/" + readerHead + "..." + readerHead + "?per_page=1": map[string]any{"status": "identical"},
		repo + "actions/runs/5/attempts/1":                                  runAttempt(5, 1, readerHead, "2026-12-01T00:00:00Z"),
		repo + "actions/runs/5/attempts/2":                                  &apiError{status: 404, text: "gh: Not Found (HTTP 404)"},
		repo + "actions/runs/6/attempts/1":                                  &apiError{status: 404, text: "gh: Not Found (HTTP 404)"},
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
			[]any{map[string]any{"context": "ci", "state": "success", "creator": botCreator}},
			[]any{
				map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "pending", "description": "building",
					"target_url": "https://github.com/katl-dev/katl/actions/runs/5/attempts/1", "creator": botCreator},
				map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "pending", "description": "building",
					"target_url": "https://github.com/katl-dev/katl/actions/runs/6/attempts/1", "creator": botCreator},
			},
		},
		repo + "actions/runs/5/attempts/1/jobs?per_page=100": {map[string]any{"jobs": []any{map[string]any{"name": "Decide", "conclusion": "success"}}},
			map[string]any{"jobs": []any{map[string]any{"name": "Build / Release validation", "conclusion": "cancelled"}}}},
		repo + "actions/runs/5/artifacts?per_page=100": {map[string]any{"artifacts": []any{}}},
		repo + "releases?per_page=100":                 none,
		runsQuery("queued"):                            noRuns,
		runsQuery("in_progress"):                       noRuns,
		runsQuery("waiting"):                           noRuns,
		runsQuery("requested"):                         noRuns,
	}, raw: map[string][]byte{}, failures: map[string][]error{}, registry: newFakeRegistry(t)}

	// drbd9 is the head's only release extension: bird has no recipe. Its
	// repository holds rc.1's bundle and the final's, besides an untagged
	// manifest.
	f.responses[repo+"contents/extensions?ref="+readerHead] = []any{
		map[string]any{"name": "README.md", "type": "file"},
		map[string]any{"name": "bird", "type": "dir"},
	}
	f.responses[repo+"contents/extensions/bird/recipe.json?ref="+readerHead] = &apiError{status: 404, text: "gh: Not Found (HTTP 404)"}
	addRecipe(f, "drbd9", drbd9Repository)
	f.registry.tags[registryPath(drbd9Repository)] = map[string]string{
		"manifest-sha256-" + strings.Repeat("c", 64): "sha256:" + strings.Repeat("c", 64),
		"v2026.12.1-rc.1-lts-x86_64-drbd9-9.3.4":     candidateDRBD9Digest,
		finalDRBD9Tag:                                finalDRBD9Digest,
	}
	return f
}

func newTestReader(f *fakeGitHub) *reader {
	r := &reader{server: "https://github.com", repository: "katl-dev/katl", read: f.read, delay: time.Millisecond}
	if f.registry != nil {
		r.registry = &http.Client{Transport: f.registry}
	}
	return r
}

func TestReaderRead(t *testing.T) {
	f := readerWorld(t)
	// A transient failure is retried; the read does not report it.
	f.failures[repoPrefix+"releases?per_page=100"] = []error{&apiError{status: 502, text: "gh: Bad Gateway (HTTP 502)"}}

	in, err := newTestReader(f).Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, "")
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
			f.failures[repoPrefix+"releases?per_page=100"] = []error{failure, failure, failure, failure, failure, failure, failure}
			if _, err := newTestReader(f).Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, ""); err == nil {
				t.Fatal("a failed read was taken as no releases")
			}
		})
	}
}

// A 403 that names the rate limit is GitHub's secondary rate limit, which
// passes; any other 403 is a refusal.
func TestReaderRetriesRateLimit(t *testing.T) {
	f := readerWorld(t)
	f.failures[repoPrefix+"releases?per_page=100"] = []error{&apiError{status: 403, text: "gh: You have exceeded a secondary rate limit (HTTP 403)"}}
	if _, err := newTestReader(f).Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, ""); err != nil {
		t.Fatalf("a rate-limited read was not retried: %v", err)
	}
}

// TestReaderAncestry reads a tag one commit behind the head and one ahead of
// it. GitHub's compare answers relative to its base, so a query with base and
// head swapped reads the opposite answer.
func TestReaderAncestry(t *testing.T) {
	other := strings.Repeat("2", 40)
	for _, tc := range []struct {
		name              string
		forward, backward string
		onBranch          bool
		row               int
	}{
		{"tag behind the head", "ahead", "behind", true, 17},
		{"tag ahead of the head", "behind", "ahead", false, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			f.pages[repoPrefix+"git/matching-refs/heads/release/2026.12.1?per_page=100"] = []any{[]any{
				map[string]any{"ref": "refs/heads/release/2026.12.1", "object": map[string]any{"sha": other, "type": "commit"}},
			}}
			delete(f.responses, repoPrefix+"compare/"+readerHead+"..."+readerHead+"?per_page=1")
			f.responses[repoPrefix+"compare/"+readerHead+"..."+other+"?per_page=1"] = map[string]any{"status": tc.forward}
			f.responses[repoPrefix+"compare/"+other+"..."+readerHead+"?per_page=1"] = map[string]any{"status": tc.backward}

			in, err := newTestReader(f).Read(context.Background(), "refs/heads/release/2026.12.1", other, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := in.Ancestry[[2]string{readerHead, other}]; got != tc.onBranch {
				t.Fatalf("rc.1 on the branch = %v, want %v", got, tc.onBranch)
			}
			if d, err := releasestate.Decide(in); err != nil || d.Row != tc.row {
				t.Fatalf("decision row %d (%s), %v, want row %d", d.Row, d.State, err, tc.row)
			}
		})
	}
}

// TestReaderAttemptArtifacts lists a run whose two attempts each uploaded a
// bundle. GitHub lists both under the run, so each attempt keeps only what it
// created from its start until the next attempt's.
func TestReaderAttemptArtifacts(t *testing.T) {
	world := func(t *testing.T) *fakeGitHub {
		f := readerWorld(t)
		f.responses[repoPrefix+"actions/runs/5/attempts/2"] = runAttempt(5, 2, readerHead, "2026-12-01T01:00:00Z")
		f.responses[repoPrefix+"actions/runs/5/attempts/3"] = &apiError{status: 404, text: "gh: Not Found (HTTP 404)"}
		f.pages[repoPrefix+"actions/runs/5/attempts/2/jobs?per_page=100"] = []any{map[string]any{"jobs": []any{}}}
		artifact := func(name string, run int, created string) map[string]any {
			return map[string]any{"name": name, "created_at": created, "workflow_run": map[string]any{"id": run}}
		}
		// The boundary artifact was created at the instant attempt 2 started.
		f.pages[repoPrefix+"actions/runs/5/artifacts?per_page=100"] = []any{map[string]any{"artifacts": []any{
			artifact("second", 5, "2026-12-01T01:30:00Z"),
			artifact("boundary", 5, "2026-12-01T01:00:00Z"),
			artifact("first", 5, "2026-12-01T00:30:00Z"),
			artifact("other run", 4, "2026-12-01T00:40:00Z"),
		}}}
		return f
	}
	read := func(f *fakeGitHub, attempts ...runstatus.Attempt) (releasestate.Inputs, error) {
		r := newTestReader(f)
		in := releasestate.Inputs{Attempts: map[runstatus.Attempt]releasestate.Attempt{}}
		for _, attempt := range attempts {
			if err := r.attempt(context.Background(), attempt, &in); err != nil {
				return in, err
			}
		}
		return in, nil
	}
	first, second := runstatus.Attempt{Run: 5, Number: 1}, runstatus.Attempt{Run: 5, Number: 2}

	t.Run("bounded by the next start", func(t *testing.T) {
		in, err := read(world(t), first, second)
		if err != nil {
			t.Fatal(err)
		}
		for attempt, want := range map[runstatus.Attempt][]string{first: {"first"}, second: {"second", "boundary"}} {
			if got := in.Attempts[attempt].Artifacts; !reflect.DeepEqual(got, want) {
				t.Errorf("attempt %s artifacts = %q, want %q", attempt, got, want)
			}
		}
	})

	// A next attempt that cannot be read is not a missing one: taking it as
	// no upper bound would credit attempt 1 with attempt 2's bundle.
	t.Run("next attempt unreadable", func(t *testing.T) {
		f := world(t)
		failure := &apiError{status: 502, text: "gh: Bad Gateway (HTTP 502)"}
		f.failures[repoPrefix+"actions/runs/5/attempts/2"] = slices.Repeat([]error{failure}, readAttempts)
		if in, err := read(f, first); err == nil {
			t.Fatalf("artifacts = %q, want the failed read of attempt 2", in.Attempts[first].Artifacts)
		}
	})

	for _, tc := range []struct{ name, endpoint string }{
		{"own start missing", "actions/runs/5/attempts/1"},
		{"next start missing", "actions/runs/5/attempts/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := world(t)
			f.responses[repoPrefix+tc.endpoint].(map[string]any)["run_started_at"] = nil
			in, err := read(f, first)
			if err == nil || !strings.Contains(err.Error(), "no start time") {
				t.Fatalf("artifacts = %q, %v; want an error naming the missing start time", in.Attempts[first].Artifacts, err)
			}
		})
	}
}

// fakeProvenance answers gh attestation for one SHA256SUMS: it verifies
// only that content, for the record's commit on the release branch. Each
// subcommand first fails with its queued failures, in order.
type fakeProvenance struct {
	t        *testing.T
	sums     []byte
	output   string
	failures map[string][]error
	verified int
}

func (p *fakeProvenance) call(name string, args ...string) ([]byte, error) {
	if name != "gh" || len(args) < 2 || args[0] != "attestation" {
		p.t.Fatalf("unexpected command %s %q", name, args)
	}
	if queue := p.failures[args[1]]; len(queue) > 0 {
		p.failures[args[1]] = queue[1:]
		return nil, queue[0]
	}
	if args[1] == "trusted-root" {
		return []byte(testTrustedRoot), nil
	}
	content, err := os.ReadFile(args[2])
	if err != nil || string(content) != string(p.sums) {
		p.t.Fatalf("verified %s with content %q (%v), want the published SHA256SUMS", args[2], content, err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--repo katl-dev/katl", "--signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml",
		"--source-ref refs/heads/release/2026.12.1", "--source-digest " + readerHead, "--format json"} {
		if !strings.Contains(joined, want) {
			p.t.Fatalf("gh %s lacks %s", joined, want)
		}
	}
	p.verified++
	return []byte(p.output), nil
}

// ghExit is gh exiting non-zero after printing stderr, as the command
// runner reports it.
func ghExit(stderr string) error { return &exec.ExitError{Stderr: []byte(stderr)} }

// TestReaderReleases publishes rc.1 from a qualified build. The reader
// downloads its SHA256SUMS, verifies its attestation and skips the draft and
// the release of another version.
func TestReaderReleases(t *testing.T) {
	sums := []byte(strings.Repeat("c", 64) + "  katlctl\n")
	sum := sha256.Sum256(sums)
	digest := hex.EncodeToString(sum[:])
	invocation := "https://github.com/katl-dev/katl/actions/runs/7/attempts/1"
	branch := "@refs/heads/release/2026.12.1"
	verifiedJSON := `[{"verificationResult":{
		"signature":{"certificate":{
			"buildSignerURI":"https://github.com/katl-dev/katl/.github/workflows/release-artifacts.yml` + branch + `",
			"buildConfigURI":"https://github.com/katl-dev/katl/.github/workflows/release.yml` + branch + `",
			"runInvocationURI":"` + invocation + `",
			"sourceRepositoryDigest":"` + readerHead + `",
			"sourceRepositoryRef":"refs/heads/release/2026.12.1"}},
		"statement":{"subject":[{"name":"SHA256SUMS","digest":{"sha256":"` + digest + `"}},{"name":"katlctl","digest":{"sha256":"` + strings.Repeat("c", 64) + `"}}]}}}]`

	world := func(t *testing.T) (*reader, *fakeProvenance) {
		f := readerWorld(t)
		statuses := f.pages[repoPrefix+"commits/"+readerHead+"/statuses?per_page=100"]
		f.pages[repoPrefix+"commits/"+readerHead+"/statuses?per_page=100"] = append([]any{[]any{
			map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "success", "description": "qualified sha256:" + digest,
				"target_url": invocation, "creator": botCreator},
		}}, statuses...)
		qualified := runAttempt(7, 1, readerHead, "2026-12-01T02:00:00Z")
		qualified["conclusion"] = "success"
		f.responses[repoPrefix+"actions/runs/7/attempts/1"] = qualified
		f.responses[repoPrefix+"actions/runs/7/attempts/2"] = &apiError{status: 404, text: "gh: Not Found (HTTP 404)"}
		f.pages[repoPrefix+"actions/runs/7/attempts/1/jobs?per_page=100"] = []any{map[string]any{"jobs": []any{
			map[string]any{"name": "Build / Release validation", "conclusion": "success"},
		}}}
		f.pages[repoPrefix+"actions/runs/7/artifacts?per_page=100"] = []any{map[string]any{"artifacts": []any{
			map[string]any{"name": "katl-2026.12.1-rc.1-x86_64-111111111111", "created_at": "2026-12-01T02:30:00Z", "workflow_run": map[string]any{"id": 7}},
		}}}
		release := func(tag string, draft bool, sumsAsset int) map[string]any {
			return map[string]any{"tag_name": tag, "draft": draft, "prerelease": true, "html_url": "https://github.com/katl-dev/katl/releases/tag/" + tag,
				"assets": []any{
					map[string]any{"id": sumsAsset, "name": "SHA256SUMS", "state": "uploaded", "digest": "sha256:" + digest},
					map[string]any{"id": 12, "name": "katlctl", "state": "uploaded", "digest": "sha256:" + strings.Repeat("c", 64)},
				}}
		}
		// Downloading the draft's or the other version's SHA256SUMS fails the test.
		f.pages[repoPrefix+"releases?per_page=100"] = []any{
			[]any{release("v2026.12.1-rc.1", true, 98), release("v2026.12.10", false, 99)},
			[]any{release("v2026.12.1-rc.1", false, 11)},
		}
		f.raw[repoPrefix+"releases/assets/11"] = sums

		attestations := &fakeProvenance{t: t, sums: sums, output: verifiedJSON, failures: map[string][]error{}}
		r := newTestReader(f)
		r.provenance = &provenance{call: attestations.call, repository: "katl-dev/katl"}
		return r, attestations
	}

	attestationURL := " (https://api.github.com/repos/katl-dev/katl/attestations/sha256:" + digest + ")"
	offline := ghExit("Get \"https://tuf-repo-cdn.sigstore.dev/timestamp.json\": dial tcp: i/o timeout")
	for _, tc := range []struct {
		name, command string
		failure       error
	}{
		{name: "verified"},
		{"trusted root retried", "trusted-root", offline},
		{"verify retried", "verify", offline},
		// A gateway error must get another attempt instead of stopping the decision.
		{"verify gateway error retried", "verify", ghExit("Error: HTTP 502: Bad Gateway" + attestationURL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, attestations := world(t)
			if tc.failure != nil {
				attestations.failures[tc.command] = []error{tc.failure}
			}
			in, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(in.Releases) != 1 || !reflect.DeepEqual(in.Releases["v2026.12.1-rc.1"].Sums, map[string]string{"katlctl": strings.Repeat("c", 64)}) {
				t.Fatalf("releases = %+v, want rc.1's published release with its SHA256SUMS content", in.Releases)
			}
			want := []releasestate.Attestation{{
				Subjects:       []string{digest, strings.Repeat("c", 64)},
				SignerWorkflow: "https://github.com/katl-dev/katl/.github/workflows/release-artifacts.yml" + branch,
				ConfigWorkflow: "https://github.com/katl-dev/katl/.github/workflows/release.yml" + branch,
				Invocation:     invocation, SourceDigest: readerHead, SourceRef: "refs/heads/release/2026.12.1",
			}}
			if got := in.Attestations[digest]; attestations.verified != 1 || !reflect.DeepEqual(got, want) {
				t.Fatalf("attestations = %+v after %d verifications, want %+v from one", got, attestations.verified, want)
			}

			// The final's extension reference holds the green candidate at row 22,
			// which is past the check that the pre-release matches its record.
			if d, err := releasestate.Decide(in); err != nil || d.Row != 22 {
				t.Fatalf("decision row %d (%s): %s, %v, want row 22", d.Row, d.State, d.Message, err)
			}
		})
	}

	// An attestation that was not verified stops the decision. A refusal by
	// gh says the release must not be published; any other failure says only
	// that the verification could not run.
	unverified := "could not verify the attestation of the published SHA256SUMS of v2026.12.1-rc.1"
	unattested := "the published SHA256SUMS of v2026.12.1-rc.1 has no valid attestation from release-artifacts.yml at " + readerHead + " on refs/heads/release/2026.12.1"
	for _, tc := range []struct {
		name  string
		spoil func(*fakeProvenance)
		want  string
	}{
		{"verify refused", func(p *fakeProvenance) {
			p.failures["verify"] = []error{ghExit("Error: verifying with issuer \"sigstore.dev\": no matching attestations")}
		}, unattested},
		// An authentication or permission error must not look like a rejected attestation.
		{"verify bad credentials", func(p *fakeProvenance) {
			p.failures["verify"] = []error{ghExit("Error: HTTP 401: Bad credentials" + attestationURL)}
		}, unverified},
		{"verify forbidden", func(p *fakeProvenance) {
			p.failures["verify"] = []error{ghExit("Error: HTTP 403: Resource not accessible by integration" + attestationURL)}
		}, unverified},
		{"verify forbidden with parenthesized status", func(p *fakeProvenance) {
			p.failures["verify"] = []error{ghExit("gh: Resource not accessible by integration (HTTP 403)")}
		}, unverified},
		{"verify offline", func(p *fakeProvenance) { p.failures["verify"] = slices.Repeat([]error{offline}, readAttempts) }, unverified},
		{"gh missing", func(p *fakeProvenance) {
			p.failures["verify"] = []error{&exec.Error{Name: "gh", Err: exec.ErrNotFound}}
		}, unverified},
		{"trusted root offline", func(p *fakeProvenance) { p.failures["trusted-root"] = slices.Repeat([]error{offline}, readAttempts) }, unverified},
		{"trusted root refused", func(p *fakeProvenance) {
			p.failures["trusted-root"] = []error{ghExit("Error: failed to verify TUF metadata: invalid signature")}
		}, unverified},
		{"output not JSON", func(p *fakeProvenance) { p.output = "✓ Verification succeeded!" }, unverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, attestations := world(t)
			tc.spoil(attestations)
			d, err := decide(context.Background(), r, "refs/heads/release/2026.12.1", readerHead, "")
			if err == nil || d.Row != 0 || d.Action != "" {
				t.Fatalf("decision row %d (%s), %v; want no decision", d.Row, d.Action, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			other := map[string]string{unverified: unattested, unattested: unverified}[tc.want]
			if strings.Contains(err.Error(), other) {
				t.Fatalf("err = %v, also claims %q", err, other)
			}
		})
	}
}

func TestReaderActive(t *testing.T) {
	run := func(id int, created string) map[string]any {
		return map[string]any{"id": id, "html_url": fmt.Sprintf("https://github.com/katl-dev/katl/actions/runs/%d", id), "created_at": created}
	}
	for _, tc := range []struct {
		name        string
		environment string
		approval    bool
	}{
		{"waiting in release", "release", true},
		{"waiting elsewhere", "staging", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			// The deciding run is the oldest; the oldest other run is the active one.
			f.pages[runsQuery("queued")] = []any{map[string]any{"workflow_runs": []any{run(902, "2026-12-01T00:02:00Z")}}}
			f.pages[runsQuery("in_progress")] = []any{map[string]any{"workflow_runs": []any{run(900, "2026-12-01T00:00:00Z")}}}
			f.pages[runsQuery("waiting")] = []any{map[string]any{"workflow_runs": []any{run(901, "2026-12-01T00:01:00Z")}}}
			f.responses[repoPrefix+"actions/runs/901/pending_deployments"] = []any{map[string]any{"environment": map[string]any{"name": tc.environment}}}
			r := newTestReader(f)
			r.self = 900

			in, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, "")
			if err != nil {
				t.Fatal(err)
			}
			want := releasestate.ActiveRun{URL: "https://github.com/katl-dev/katl/actions/runs/901", ApprovalPending: tc.approval}
			if in.Active == nil || *in.Active != want {
				t.Fatalf("active = %+v, want %+v", in.Active, want)
			}
		})
	}
}

func TestTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"server error", &apiError{status: 502}, true},
		{"rate limit", &apiError{status: 429}, true},
		{"secondary rate limit", &apiError{status: 403, text: "API rate limit exceeded (HTTP 403)"}, true},
		{"refused", &apiError{status: 403, text: "Resource not accessible (HTTP 403)"}, false},
		{"not found", &apiError{status: 404}, false},
		{"network", &apiError{network: true}, true},
		{"no status", &apiError{text: "gh: unknown flag"}, false},
		{"wrapped network", fmt.Errorf("read run attempt: %w", &apiError{network: true}), true},
		{"not gh", errors.New("decode: unexpected end of JSON input"), false},
	} {
		if got := transient(tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGHFailure(t *testing.T) {
	for _, tc := range []struct {
		name, stderr string
		status       int
		network      bool
	}{
		// Both spellings must carry an HTTP status into the retry and refusal checks.
		{name: "api status", stderr: "gh: Not Found (HTTP 404)", status: 404},
		{name: "attestation status", stderr: "Error: HTTP 401: Bad credentials (https://api.github.com/repos/katl-dev/katl/attestations/sha256:deadbeef)", status: 401},
		// Timeouts without an HTTP response must be eligible for retry.
		{name: "context deadline", stderr: "Get \"https://api.github.com\": context deadline exceeded", network: true},
		{name: "client timeout", stderr: "Get \"https://api.github.com\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)", network: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ghFailure("gh", ghExit(tc.stderr), tc.stderr)
			var api *apiError
			if !errors.As(err, &api) {
				t.Fatalf("expected an API error, got %v", err)
			}
			if api.status != tc.status || api.network != tc.network {
				t.Fatalf("status = %d, network = %v, want status = %d, network = %v", api.status, api.network, tc.status, tc.network)
			}
		})
	}
}

// TestGHRead classifies the failures of a real gh process: a stand-in gh
// on PATH fails as gh does.
func TestGHRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   bool
	}{
		{"not authenticated", "echo 'To get started with GitHub CLI, please run:  gh auth login' >&2; exit 4", false},
		{"offline", "echo 'error connecting to api.github.com' >&2; exit 1", true},
		{"server error", "echo 'gh: Bad Gateway (HTTP 502)' >&2; exit 1", true},
		{"other failure", "echo 'unknown flag: --slurp' >&2; exit 1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			_, err := ghRead(context.Background(), "repos/katl-dev/katl/releases", false, "")
			if err == nil || transient(err) != tc.want {
				t.Fatalf("err = %v, transient %v, want a failure with transient %v", err, transient(err), tc.want)
			}
		})
	}

	t.Run("gh missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := ghRead(context.Background(), "repos/katl-dev/katl/releases", false, "")
		if !errors.Is(err, exec.ErrNotFound) || transient(err) {
			t.Fatalf("err = %v, want a permanent exec.ErrNotFound", err)
		}
	})
}

func TestNextAndStatus(t *testing.T) {
	r := newTestReader(readerWorld(t))
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

func TestStatusWithoutReleaseWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"not installed", &apiError{status: 404, text: "gh: Not Found (HTTP 404)"}, "the Release workflow is not installed on this repository"},
		{"unreadable", &apiError{status: 403, text: "gh: Resource not accessible (HTTP 403)"}, "cannot read the release state, so nothing was decided: gh: Resource not accessible (HTTP 403)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			f.responses[repoPrefix+"actions/workflows/release.yml"] = tc.failure
			var out strings.Builder
			err := status(context.Background(), []string{"2026.12.1"}, &out, newTestReader(f))
			if err == nil || err.Error() != tc.want || out.Len() != 0 {
				t.Fatalf("status = %v, printed %q; want only the error %q", err, out.String(), tc.want)
			}
		})
	}

	// Only the workflow read means the workflow is missing; a 404 anywhere
	// else fails the read as before.
	f := readerWorld(t)
	f.failures[runsQuery("queued")] = []error{&apiError{status: 404, text: "gh: Not Found (HTTP 404)"}}
	err := status(context.Background(), []string{"2026.12.1"}, &strings.Builder{}, newTestReader(f))
	if err == nil || !strings.HasPrefix(err.Error(), "cannot read the release state") {
		t.Fatalf("status = %v, want the failed read", err)
	}
}
