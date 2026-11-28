package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
)

const fixturePayloadDigest = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
const fixtureNotesDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const fixtureSumsDigest = "sha256:2c9b81e8c0ddcebef4bd0954c29f70382b229aaf12affbf4818d5cabf47307cc"

type releaseRequest struct {
	method, endpoint string
	fields           map[string]string
}

type publicationFixture struct {
	api      *fakeGitHub
	dir      string
	tag      string
	readback map[string]any
	created  int
	onWrite  func(releaseRequest) error
	patch    func(context.Context) error
	onRead   func(endpoint string)
	writes   []releaseRequest
	reads    []string
}

func publicationWorld(t *testing.T, tag string, draft bool) *publicationFixture {
	t.Helper()
	dir := t.TempDir()
	writeBundle(t, dir)
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}

	f := &publicationFixture{dir: dir, tag: tag, created: 42}
	f.readback = map[string]any{"id": 42, "tag_name": tag, "draft": draft, "prerelease": strings.Contains(tag, "-rc."),
		"assets": []map[string]any{
			{"id": 11, "name": "SHA256SUMS", "state": "uploaded", "digest": fixtureSumsDigest},
			{"id": 12, "name": "payload", "state": "uploaded", "digest": fixturePayloadDigest},
			{"id": 13, "name": "RELEASE_NOTES.md", "state": "uploaded", "digest": fixtureNotesDigest},
		}}
	f.api = &fakeGitHub{
		t: t,
		responses: map[string]any{
			repoPrefix + "releases/42": f.readback,
			repoPrefix + "git/ref/tags/" + tag: map[string]any{"ref": "refs/tags/" + tag,
				"object": map[string]string{"sha": readerTag, "type": "tag"}},
			repoPrefix + "git/tags/" + readerTag: map[string]any{"object": map[string]string{"sha": testCommit, "type": "commit"}},
		},
		raw: map[string][]byte{repoPrefix + "releases/assets/11": sums},
	}
	f.list()
	return f
}

const releaseListEndpoint = repoPrefix + "releases?per_page=100"

func (f *publicationFixture) list(releases ...any) {
	other := []any{
		map[string]any{"id": 21, "tag_name": "v2026.12.2", "draft": true},
		map[string]any{"id": 22, "tag_name": "v2026.11.7", "draft": false},
	}
	f.api.pages = map[string][]any{releaseListEndpoint: {other, releases}}
}

func (f *publicationFixture) call(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.api.t.Helper()
	if name != "gh" || len(args) < 2 {
		f.api.t.Fatalf("unexpected command %s %v", name, args)
	}
	if slices.Equal(args, []string{"attestation", "trusted-root"}) {
		return []byte(testTrustedRoot), nil
	}
	if args[0] == "attestation" && args[1] == "verify" {
		at := slices.Index(args, "--source-ref")
		if at < 0 || args[at+1] != "refs/heads/release/2026.12.1" {
			f.api.t.Fatalf("draft must verify branch provenance: %v", args)
		}
		return nil, nil
	}
	if len(args) < 4 || args[0] != "api" || args[1] != "--method" {
		f.api.t.Fatalf("unexpected command %v", args)
	}
	req := releaseRequest{method: args[2], endpoint: args[3], fields: map[string]string{}}
	if strings.HasPrefix(req.endpoint, "https://uploads.github.com/") {
		address, err := url.Parse(req.endpoint)
		if err != nil || strings.TrimPrefix(address.Path, "/") != repoPrefix+"releases/"+strconv.Itoa(f.created)+"/assets" {
			f.api.t.Fatalf("invalid upload endpoint %q: %v", req.endpoint, err)
		}
		index := slices.Index(args, "--input")
		want := filepath.Join(f.dir, address.Query().Get("name"))
		if index < 0 || args[index+1] != want {
			f.api.t.Fatalf("upload = %v, want input %s", args, want)
		}
	} else {
		for i := 4; i < len(args); i += 2 {
			field, value, ok := strings.Cut(args[i+1], "=")
			if (args[i] != "-f" && args[i] != "-F") || !ok {
				f.api.t.Fatalf("unexpected API field %v", args[i:])
			}
			req.fields[field] = value
		}
	}
	f.writes = append(f.writes, req)
	if req.method == "PATCH" && f.patch != nil {
		return nil, f.patch(ctx)
	}
	if f.onWrite != nil {
		if err := f.onWrite(req); err != nil {
			return nil, err
		}
	}
	if req.method == "POST" && req.endpoint == repoPrefix+"releases" {
		return json.Marshal(map[string]any{"id": f.created, "tag_name": f.tag, "draft": true})
	}
	// The PATCH response is intentionally uninformative. Only a fresh GET
	// reveals the persisted release, ref and bytes.
	return nil, nil
}

func (f *publicationFixture) run(action string) error {
	r := newTestReader(f.api)
	r.read = func(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
		f.reads = append(f.reads, endpoint)
		body, err := f.api.read(ctx, endpoint, paginate, accept)
		if f.onRead != nil {
			f.onRead(endpoint)
		}
		return body, err
	}
	return runPublication(f.api.t.Context(), []string{action, strings.TrimPrefix(f.tag, "v"), testCommit, f.dir},
		io.Discard, r, f.call)
}

// Draft replaces interrupted drafts, uploads the verified bundle once, and
// leaves other tags alone. It refuses to replace published bytes or accept
// the wrong uploaded digest.
func TestDraftRelease(t *testing.T) {
	for _, tc := range []struct {
		name, tag, state, wantErr string
		prerelease                string
		wantWrites                bool
	}{
		{name: "new candidate", tag: "v2026.12.1-rc.1", prerelease: "true", wantWrites: true},
		{name: "new final", tag: "v2026.12.1", prerelease: "false", wantWrites: true},
		{name: "interrupted draft", tag: "v2026.12.1", state: "draft", prerelease: "false", wantWrites: true},
		{name: "already published", tag: "v2026.12.1-rc.1", state: "published"},
		{name: "published digest differs", tag: "v2026.12.1", state: "published-different", wantErr: "v2026.12.1 is already published with different contents. Published bytes are never replaced."},
		{name: "upload digest differs", tag: "v2026.12.1", state: "bad-upload", wantErr: "draft of v2026.12.1 does not match", prerelease: "false", wantWrites: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := publicationWorld(t, tc.tag, true)
			if tc.state == "draft" {
				interrupted := map[string]any{"id": 26, "tag_name": tc.tag, "draft": true}
				f.list(interrupted)
				f.api.responses[repoPrefix+"releases/26"] = interrupted
			}
			if strings.HasPrefix(tc.state, "published") {
				f.readback["draft"] = false
				f.list(f.readback)
			}
			assets := f.readback["assets"].([]map[string]any)
			if tc.state == "published-different" {
				assets[0]["digest"] = fixturePayloadDigest
			}
			if tc.state == "bad-upload" {
				assets[1]["digest"] = fixtureNotesDigest
			}

			requireReleaseError(t, f.run("draft"), tc.wantErr)
			var want []string
			if tc.state == "draft" {
				want = append(want, "DELETE "+repoPrefix+"releases/26")
			}
			if tc.wantWrites {
				want = append(want, "POST "+repoPrefix+"releases")
				for _, name := range []string{"RELEASE_NOTES.md", "SHA256SUMS", "payload"} {
					want = append(want, "POST https://uploads.github.com/"+repoPrefix+"releases/42/assets?name="+name)
				}
			}
			var got []string
			for _, write := range f.writes {
				got = append(got, write.method+" "+write.endpoint)
				if write.method == "POST" && write.endpoint == repoPrefix+"releases" {
					fields := map[string]string{"tag_name": tc.tag, "target_commitish": testCommit,
						"name": "KatlOS " + tc.tag[1:], "body": "@" + filepath.Join(f.dir, "RELEASE_NOTES.md"),
						"draft": "true", "prerelease": tc.prerelease}
					if !maps.Equal(write.fields, fields) {
						t.Errorf("draft request = %v, want %v", write.fields, fields)
					}
				}
			}
			// Delete and create must precede the uploads, which can run in any order.
			firstUpload := len(want) - 3
			if tc.wantWrites && len(got) == len(want) {
				slices.Sort(got[firstUpload:])
			}
			if !slices.Equal(got, want) {
				t.Errorf("writes = %v, want %v", got, want)
			}
		})
	}
}

// Publish sends one request with the bound tag, commit and Latest policy.
// It leaves other tags alone, refuses an ambiguous draft, and checks persisted
// state independently of the mutation response.
func TestPublishRelease(t *testing.T) {
	for _, tc := range []struct {
		name, tag, state, wantErr string
		wantWrite                 bool
	}{
		{name: "final", tag: "v2026.12.1", state: "draft", wantWrite: true},
		{name: "candidate", tag: "v2026.12.1-rc.1", state: "draft", wantWrite: true},
		{name: "tag points elsewhere", tag: "v2026.12.1", state: "wrong-commit", wantWrite: true, wantErr: "published tag v2026.12.1 points to"},
		{name: "SHA256SUMS differs", tag: "v2026.12.1", state: "wrong-bytes", wantWrite: true, wantErr: "published SHA256SUMS of v2026.12.1 differs from the bundle"},
		{name: "still draft", tag: "v2026.12.1", state: "still-draft", wantWrite: true, wantErr: "published release v2026.12.1 does not match"},
		{name: "draft has wrong digest", tag: "v2026.12.1", state: "bad-draft", wantErr: "no unique draft of v2026.12.1 matches the bundle"},
		{name: "two drafts", tag: "v2026.12.1", state: "two-drafts", wantErr: "no unique draft of v2026.12.1 matches the bundle"},
		{name: "already published", tag: "v2026.12.1-rc.1", state: "published"},
		{name: "absent", tag: "v2026.12.1", wantErr: "Run katl-release draft first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := publicationWorld(t, tc.tag, tc.state == "still-draft")
			if tc.state == "published" {
				f.list(f.readback)
			} else if tc.state != "" {
				draft := maps.Clone(f.readback)
				draft["draft"] = true
				f.list(draft)
				if tc.state == "bad-draft" {
					draft["assets"] = []map[string]any{{"id": 11, "name": "SHA256SUMS", "state": "uploaded", "digest": fixtureNotesDigest}}
				}
				if tc.state == "two-drafts" {
					other := maps.Clone(draft)
					other["id"] = 43
					f.list(draft, other)
				}
			}
			if tc.state == "wrong-commit" {
				f.api.responses[repoPrefix+"git/tags/"+readerTag] = map[string]any{"object": map[string]string{"sha": readerHead, "type": "commit"}}
			}
			if tc.state == "wrong-bytes" {
				f.api.raw[repoPrefix+"releases/assets/11"][0] ^= 1
			}

			requireReleaseError(t, f.run("publish"), tc.wantErr)
			if !tc.wantWrite {
				if len(f.writes) != 0 {
					t.Fatalf("unexpected writes: %v", f.writes)
				}
				return
			}
			want := map[string]string{"draft": "false", "tag_name": tc.tag, "target_commitish": testCommit, "make_latest": "legacy"}
			if strings.HasSuffix(tc.tag, "-rc.1") {
				want["make_latest"] = "false"
			}
			if len(f.writes) != 1 || f.writes[0].method != "PATCH" || f.writes[0].endpoint != repoPrefix+"releases/42" ||
				!maps.Equal(f.writes[0].fields, want) {
				t.Fatalf("writes = %v, want one PATCH of release 42 with %v", f.writes, want)
			}
		})
	}
}

// An upload error may leave an asset behind. Retrying just the upload can
// collide with that asset, and giving up leaves the publication unfinished.
func TestDraftRestartsAfterUploadFailure(t *testing.T) {
	f := publicationWorld(t, "v2026.12.1", true)
	var failedUpload string
	f.onWrite = func(req releaseRequest) error {
		switch {
		case failedUpload == "" && strings.HasPrefix(req.endpoint, "https://uploads.github.com/"+repoPrefix+"releases/42/assets?name="):
			failedUpload = req.endpoint
			f.list(map[string]any{"id": 42, "tag_name": f.tag, "draft": true})
			return ghExit("gh: Bad Gateway (HTTP 502)")
		case req.method == "DELETE" && req.endpoint == repoPrefix+"releases/42":
			f.created = 43
			delete(f.api.responses, repoPrefix+"releases/42")
			f.readback["id"] = 43
			f.api.responses[repoPrefix+"releases/43"] = f.readback
		}
		return nil
	}

	requireReleaseError(t, f.run("draft"), "")
	if failedUpload == "" {
		t.Fatal("the first draft never attempted an upload")
	}
	want := []string{
		"POST " + repoPrefix + "releases",
		"POST " + failedUpload,
		"DELETE " + repoPrefix + "releases/42",
		"POST " + repoPrefix + "releases",
		"POST https://uploads.github.com/" + repoPrefix + "releases/43/assets?name=RELEASE_NOTES.md",
		"POST https://uploads.github.com/" + repoPrefix + "releases/43/assets?name=SHA256SUMS",
		"POST https://uploads.github.com/" + repoPrefix + "releases/43/assets?name=payload",
	}
	var got []string
	for _, req := range f.writes {
		got = append(got, req.method+" "+req.endpoint)
	}
	if len(got) == len(want) {
		slices.Sort(got[4:])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	if !slices.Contains(f.reads, repoPrefix+"releases/43") {
		t.Error("the replacement draft was never read back")
	}
}

// A lost PATCH response can precede either state. A blind retry can PATCH an
// already published release, while giving up leaves a verified draft behind.
// A replacement draft has a different identity and must never be patched.
func TestPublishReconcilesPatchFailure(t *testing.T) {
	for _, tc := range []struct {
		name, afterPatch, wantErr string
		wantPatches               int
	}{
		{name: "already published", afterPatch: "published", wantPatches: 1},
		{name: "same draft", afterPatch: "draft", wantPatches: 2},
		{name: "replacement draft", afterPatch: "replacement", wantPatches: 1, wantErr: "HTTP 502"},
		{name: "permanent error", afterPatch: "draft", wantPatches: 1, wantErr: "HTTP 422"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := publicationWorld(t, "v2026.12.1", false)
			draft := maps.Clone(f.readback)
			draft["draft"] = true
			f.list(draft)
			f.onWrite = func(req releaseRequest) error {
				if req.method != "PATCH" || req.endpoint != repoPrefix+"releases/42" {
					t.Fatalf("unexpected write: %v", req)
				}
				if len(f.writes) == 2 {
					if len(f.reads) < 2 || f.reads[len(f.reads)-1] != releaseListEndpoint {
						t.Fatal("the draft was patched again before its state was checked")
					}
					return nil
				}
				switch tc.afterPatch {
				case "published":
					f.list(f.readback)
				case "replacement":
					draft["id"] = 43
				}
				if tc.name == "permanent error" {
					return ghExit("gh: Validation failed (HTTP 422)")
				}
				return ghExit("gh: Bad Gateway (HTTP 502)")
			}

			requireReleaseError(t, f.run("publish"), tc.wantErr)
			if len(f.writes) != tc.wantPatches {
				t.Errorf("PATCH requests = %d, want %d", len(f.writes), tc.wantPatches)
			}
			if len(f.reads) < 2 || f.reads[1] != releaseListEndpoint {
				t.Errorf("the failed PATCH did not trigger a fresh release list: %v", f.reads)
			}
			if tc.wantErr == "" && !slices.Contains(f.reads, repoPrefix+"releases/assets/11") {
				t.Error("successful publication was never confirmed from its published bytes")
			}
		})
	}
}

func TestPublishReconcilesPatchTimeout(t *testing.T) {
	for _, landed := range []bool{true, false} {
		name := "draft still pending"
		if landed {
			name = "already published"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := publicationWorld(t, "v2026.12.1", true)
				f.list(f.readback)
				f.patch = func(ctx context.Context) error {
					if landed || len(f.writes) == 2 {
						f.readback["draft"] = false
					}
					if len(f.writes) == 1 {
						<-ctx.Done()
						return ghExit("signal: killed")
					}
					return nil
				}

				requireReleaseError(t, f.run("publish"), "")
				want := 2
				if landed {
					want = 1
				}
				if len(f.writes) != want || !slices.Contains(f.reads, repoPrefix+"releases/assets/11") {
					t.Fatalf("writes = %v, reads = %v, want %d PATCH requests and the published checksums", f.writes, f.reads, want)
				}
			})
		})
	}
}

func TestPublishWithDifferentReleaseID(t *testing.T) {
	for _, tc := range []struct {
		name, wantErr     string
		badChecksums      bool
		staleDraftListing bool
		omittedDraft      bool
	}{
		{name: "confirmed publication"},
		{name: "draft omitted during reconciliation", omittedDraft: true},
		{name: "failed confirmation", badChecksums: true, wantErr: "published SHA256SUMS"},
		{name: "stale draft listing", staleDraftListing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := publicationWorld(t, "v2026.12.1", true)
			draft := f.readback
			published := maps.Clone(draft)
			published["id"], published["draft"] = 43, false
			f.list(draft)
			f.api.responses[repoPrefix+"releases/43"] = published
			if tc.badChecksums {
				f.api.raw[repoPrefix+"releases/assets/11"][0] ^= 1
			}
			f.onWrite = func(req releaseRequest) error {
				switch {
				case req.method == "PATCH" && req.endpoint == repoPrefix+"releases/42":
					f.list(published, draft)
					if tc.omittedDraft {
						f.list(published)
					}
					if tc.staleDraftListing {
						changed := maps.Clone(draft)
						changed["tag_name"] = "v2026.12.2"
						f.api.responses[repoPrefix+"releases/42"] = changed
					}
					return ghExit("gh: Bad Gateway (HTTP 502)")
				case req.method == "DELETE" && req.endpoint == repoPrefix+"releases/42":
					if tc.badChecksums || tc.staleDraftListing || !slices.Contains(f.reads, repoPrefix+"releases/assets/11") {
						t.Fatalf("deleted draft without confirming publication and draft identity: %v", f.reads)
					}
					delete(f.api.responses, req.endpoint)
					f.list(published)
				default:
					t.Fatalf("unexpected write: %v", req)
				}
				return nil
			}

			requireReleaseError(t, f.run("publish"), tc.wantErr)
			if _, draftExists := f.api.responses[repoPrefix+"releases/42"]; draftExists != (tc.badChecksums || tc.staleDraftListing) {
				t.Fatalf("draft 42 still exists = %v", draftExists)
			}
			if !tc.badChecksums && !tc.staleDraftListing {
				requireReleaseError(t, f.run("publish"), "")
				if len(f.writes) != 2 {
					t.Fatalf("writes = %v, want one PATCH and one DELETE across both runs", f.writes)
				}
			}
		})
	}
}

// After a timed-out PATCH, a replacement or second draft means the original
// is no longer the sole candidate, so a retry could publish the wrong
// release. The command stops and says how to reconcile.
func TestPublishTimeoutWithOtherDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed func(draft, other map[string]any) []any
	}{
		{name: "replacement draft", listed: func(_, other map[string]any) []any { return []any{other} }},
		{name: "second draft", listed: func(draft, other map[string]any) []any { return []any{draft, other} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := publicationWorld(t, "v2026.12.1", true)
				other := maps.Clone(f.readback)
				other["id"] = 43
				f.list(f.readback)
				f.patch = func(ctx context.Context) error {
					f.list(tc.listed(f.readback, other)...)
					<-ctx.Done()
					return ghExit("signal: killed")
				}

				hint := "rerun `scripts/katl-release publish 2026.12.1 " + testCommit + " " + f.dir + "` to reconcile"
				requireReleaseError(t, f.run("publish"), hint)
				if len(f.writes) != 1 {
					t.Fatalf("writes = %v, want a single PATCH", f.writes)
				}
			})
		})
	}
}

// A rerun after publication removes leftover drafts of the same tag, but
// only those that are still drafts of that tag when read by ID.
func TestPublishRemovesLeftoverDrafts(t *testing.T) {
	for _, tc := range []struct {
		name, byID, wantErr string
		wantDelete          bool
	}{
		{name: "leftover draft", byID: "draft", wantDelete: true},
		{name: "published since listing", byID: "published"},
		{name: "invalid ID", byID: "zero", wantErr: "GitHub returned release ID 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := publicationWorld(t, "v2026.12.1", true)
			draft := f.readback
			published := maps.Clone(draft)
			published["id"], published["draft"] = 43, false
			f.list(published, draft)
			f.api.responses[repoPrefix+"releases/43"] = published
			switch tc.byID {
			case "published":
				changed := maps.Clone(draft)
				changed["draft"] = false
				f.api.responses[repoPrefix+"releases/42"] = changed
			case "zero":
				f.api.responses[repoPrefix+"releases/42"] = map[string]any{"tag_name": f.tag, "draft": true}
			}

			requireReleaseError(t, f.run("publish"), tc.wantErr)
			var want []releaseRequest
			if tc.wantDelete {
				want = []releaseRequest{{method: "DELETE", endpoint: repoPrefix + "releases/42", fields: map[string]string{}}}
			}
			if !slices.EqualFunc(f.writes, want, func(a, b releaseRequest) bool { return a.method == b.method && a.endpoint == b.endpoint }) {
				t.Fatalf("writes = %v, want %v", f.writes, want)
			}
		})
	}
}

// A listed draft that was published before draft could remove it is never
// deleted. The next attempt lists it and completes as already published.
func TestDraftKeepsReleasePublishedSinceListing(t *testing.T) {
	f := publicationWorld(t, "v2026.12.1", false)
	draft := maps.Clone(f.readback)
	draft["draft"] = true
	f.list(draft)
	f.onRead = func(endpoint string) {
		if endpoint == repoPrefix+"releases/42" {
			f.list(f.readback)
		}
	}

	requireReleaseError(t, f.run("draft"), "")
	if len(f.writes) != 0 {
		t.Fatalf("writes = %v, want none", f.writes)
	}
}

// Looking up the tag only once can turn a successful publish into an error
// while the new ref is still propagating.
func TestPublishWaitsForTagRef(t *testing.T) {
	f := publicationWorld(t, "v2026.12.1", false)
	draft := maps.Clone(f.readback)
	draft["draft"] = true
	f.list(draft)
	ref := repoPrefix + "git/ref/tags/" + f.tag
	f.api.failures = map[string][]error{ref: {&apiError{status: 404, text: "gh: Not Found (HTTP 404)"}}}

	requireReleaseError(t, f.run("publish"), "")
	if len(f.writes) != 1 || f.writes[0].method != "PATCH" {
		t.Errorf("writes = %v, want a single PATCH", f.writes)
	}
}

func requireReleaseError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// A branch-built candidate must be verified against its final version's
// branch, while tag-push bundles keep the tag ref.
func TestVerifyReleaseSource(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		flags     []string
	}{
		{name: "branch", ref: "refs/heads/release/2026.12.1", flags: []string{"--branch"}},
		{name: "tag", ref: "refs/tags/v2026.12.1-rc.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBundle(t, dir)
			var files []string
			call := func(name string, args ...string) ([]byte, error) {
				if name == "gh" && slices.Equal(args, []string{"attestation", "trusted-root"}) {
					return []byte(testTrustedRoot), nil
				}
				at := slices.Index(args, "--source-ref")
				if name != "gh" || len(args) < 4 || args[0] != "attestation" || args[1] != "verify" ||
					at < 0 || args[at+1] != tc.ref {
					t.Fatalf("provenance command = %s %v, want source ref %s", name, args, tc.ref)
				}
				files = append(files, filepath.Base(args[2]))
				return nil, nil
			}

			args := append([]string{"verify"}, tc.flags...)
			args = append(args, "v2026.12.1-rc.1", testCommit, dir)
			if err := run(args, io.Discard, io.Discard, call); err != nil {
				t.Fatal(err)
			}
			if want := []string{"RELEASE_NOTES.md", "payload", "SHA256SUMS"}; !slices.Equal(files, want) {
				t.Errorf("verified files = %v, want %v", files, want)
			}
		})
	}
}
