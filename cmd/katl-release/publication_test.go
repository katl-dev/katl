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
	"strings"
	"testing"
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
	writes   []releaseRequest
}

func publicationWorld(t *testing.T, tag string, draft bool) *publicationFixture {
	t.Helper()
	dir := t.TempDir()
	writeBundle(t, dir)
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}

	f := &publicationFixture{dir: dir, tag: tag}
	f.readback = map[string]any{"id": 42, "tag_name": tag, "draft": draft, "prerelease": strings.Contains(tag, "-rc."),
		"assets": []map[string]any{
			{"id": 11, "name": "SHA256SUMS", "state": "uploaded", "digest": fixtureSumsDigest},
			{"id": 12, "name": "payload", "state": "uploaded", "digest": fixturePayloadDigest},
			{"id": 13, "name": "RELEASE_NOTES.md", "state": "uploaded", "digest": fixtureNotesDigest},
		}}
	f.api = &fakeGitHub{
		t:     t,
		pages: map[string][]any{repoPrefix + "releases?per_page=100": {[]any{}}},
		responses: map[string]any{
			repoPrefix + "releases/42": f.readback,
			repoPrefix + "git/ref/tags/" + tag: map[string]any{"ref": "refs/tags/" + tag,
				"object": map[string]string{"sha": readerTag, "type": "tag"}},
			repoPrefix + "git/tags/" + readerTag: map[string]any{"object": map[string]string{"sha": testCommit, "type": "commit"}},
		},
		raw: map[string][]byte{repoPrefix + "releases/assets/11": sums},
	}
	return f
}

func (f *publicationFixture) list(releases ...any) {
	f.api.pages[repoPrefix+"releases?per_page=100"] = []any{[]any{}, releases}
}

func (f *publicationFixture) call(name string, args ...string) ([]byte, error) {
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
		if err != nil || strings.TrimPrefix(address.Path, "/") != repoPrefix+"releases/42/assets" {
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
	if req.method == "POST" && req.endpoint == repoPrefix+"releases" {
		return json.Marshal(map[string]any{"id": 42, "tag_name": f.tag, "draft": true})
	}
	// The PATCH response is intentionally uninformative. Only a fresh GET
	// reveals the persisted release, ref and bytes.
	return nil, nil
}

func (f *publicationFixture) run(action string) error {
	return runPublication(context.Background(), []string{action, strings.TrimPrefix(f.tag, "v"), testCommit, f.dir},
		io.Discard, newTestReader(f.api), f.call)
}

// Draft replaces interrupted drafts, uploads the verified bundle once, and
// refuses to replace published bytes or accept the wrong uploaded digest.
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
				f.list(map[string]any{"id": 26, "tag_name": tc.tag, "draft": true})
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

// Publish sends one request with the bound tag and commit, then checks the
// persisted tag, release and SHA256SUMS independently of the mutation response.
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
			want := map[string]string{"draft": "false", "tag_name": tc.tag, "target_commitish": testCommit}
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
