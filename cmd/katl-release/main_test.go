package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const testCommit = "1234567890abcdef1234567890abcdef12345678"

type githubFixture struct {
	t           *testing.T
	run         workflowRun
	artifacts   []artifact
	rerun       bool
	replaced    bool
	corrupt     bool
	provenance  bool
	sourceRef   string
	rootOffline bool
	rootFetches int
	verified    []string
	artifactAPI int
}

// testTrustedRoot stands in for gh attestation trusted-root output.
const testTrustedRoot = `{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}` + "\n"

func newFixture(t *testing.T) *githubFixture {
	return &githubFixture{
		t:         t,
		run:       workflowRun{ID: 42, Attempt: 1, Tag: "v2026.10.2", Commit: testCommit, Event: "push", Path: workflow, Status: "waiting"},
		artifacts: []artifact{{ID: 123, Name: "katl-2026.10.2-x86_64-1234567890ab", Digest: "sha256:archive"}},
		sourceRef: "refs/tags/v2026.10.2",
	}
}

func (f *githubFixture) call(name string, args ...string) ([]byte, error) {
	f.t.Helper()
	if name != "gh" || len(args) == 0 {
		f.t.Fatalf("unexpected command %s %q", name, args)
	}
	if args[0] == "api" {
		if len(args) != 2 {
			f.t.Fatalf("unexpected API mutation: %q", args)
		}
		// Keep GitHub's wire fields independent of the production JSON tags.
		var value any
		switch strings.TrimPrefix(args[1], "repos/"+repository+"/") {
		case "actions/runs/42":
			value = map[string]any{
				"id": f.run.ID, "run_attempt": f.run.Attempt, "head_branch": f.run.Tag,
				"head_sha": f.run.Commit, "event": f.run.Event, "path": f.run.Path,
				"status": f.run.Status, "conclusion": f.run.Conclusion,
			}
		case "actions/runs/42/artifacts?per_page=100&page=1":
			f.artifactAPI++
			var artifacts []map[string]any
			for _, artifact := range f.artifacts {
				artifacts = append(artifacts, map[string]any{
					"id": artifact.ID, "name": artifact.Name, "digest": artifact.Digest, "expired": artifact.Expired,
				})
			}
			value = map[string]any{"artifacts": artifacts}
		case "actions/runs/42/artifacts?per_page=100&page=2":
			f.artifactAPI++
			value = map[string]any{"artifacts": []any{map[string]any{"id": 123, "name": "katl-2026.10.2-x86_64-1234567890ab"}}}
		default:
			f.t.Fatalf("unexpected endpoint: %q", args)
		}
		return json.Marshal(value)
	}
	if reflect.DeepEqual(args[:min(len(args), 2)], []string{"run", "download"}) {
		writeBundle(f.t, args[len(args)-1])
		if f.corrupt {
			if err := os.WriteFile(filepath.Join(args[len(args)-1], "payload"), []byte("xyz"), 0o644); err != nil {
				f.t.Fatal(err)
			}
		}
		if f.rerun {
			f.run.Attempt++
		}
		if f.replaced {
			f.artifacts[0].ID++
		}
		return nil, nil
	}
	if reflect.DeepEqual(args, []string{"attestation", "trusted-root"}) {
		f.rootFetches++
		if f.rootOffline {
			return nil, fmt.Errorf("tuf refresh failed")
		}
		return []byte(testTrustedRoot), nil
	}
	if args[0] == "attestation" {
		joined := strings.Join(args, " ")
		// Every verification must use the root already fetched for this
		// bundle rather than initializing one of its own.
		flag := slices.Index(args, "--custom-trusted-root")
		if flag < 0 || flag+1 >= len(args) {
			f.t.Fatalf("verified without a fetched trusted root: %s", joined)
		}
		if data, err := os.ReadFile(args[flag+1]); err != nil || string(data) != testTrustedRoot || f.rootFetches != 1 {
			f.t.Fatalf("verified against %s after %d trusted-root fetches: %v", args[flag+1], f.rootFetches, err)
		}
		source := slices.Index(args, "--source-ref")
		if source < 0 || source+1 >= len(args) || args[source+1] != f.sourceRef {
			f.t.Fatalf("source ref in %q, want %q", args, f.sourceRef)
		}
		for _, constraint := range []string{"--repo " + repository, "--signer-workflow " + repository + "/" + workflow, "--source-digest " + testCommit} {
			if !strings.Contains(joined, constraint) {
				f.t.Fatalf("missing provenance constraint %s: %s", constraint, joined)
			}
		}
		if f.provenance {
			return nil, fmt.Errorf("untrusted signer")
		}
		f.verified = append(f.verified, filepath.Base(args[2]))
		return nil, nil
	}
	f.t.Fatalf("unexpected GitHub command %q", args)
	return nil, nil
}

func writeBundle(t *testing.T, dir string) {
	t.Helper()
	// Independently calculated SHA-256 test vectors for "abc" and an empty file.
	for name, data := range map[string]string{
		"payload": "abc", "RELEASE_NOTES.md": "",
		"SHA256SUMS": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  payload\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  RELEASE_NOTES.md\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspect(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*githubFixture)
		want string
	}{
		{"tag", func(f *githubFixture) {}, ""},
		{"branch", func(f *githubFixture) { f.run.Tag = "main" }, "pushed KatlOS tag"},
		{"manual", func(f *githubFixture) { f.run.Event = "workflow_dispatch" }, "pushed KatlOS tag"},
		{"wrong workflow", func(f *githubFixture) { f.run.Path = ".github/workflows/other.yml" }, "pushed KatlOS tag"},
		{"bundle missing", func(f *githubFixture) { f.artifacts = nil }, "unavailable"},
		{"bundle expired", func(f *githubFixture) { f.artifacts[0].Expired = true }, "expired"},
		{"ambiguous", func(f *githubFixture) { f.artifacts = append(f.artifacts, f.artifacts[0]) }, "multiple matching release bundles"},
		{"wrong commit", func(f *githubFixture) { f.run.Commit = strings.Repeat("a", 40) }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t)
			tc.edit(fixture)
			var output bytes.Buffer
			err := run([]string{"inspect", "42"}, &output, io.Discard, fixture.call)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v; want %s", err, tc.want)
				}
				return
			}
			var receipt struct {
				Run struct {
					Attempt int    `json:"run_attempt"`
					Commit  string `json:"head_sha"`
				} `json:"run"`
				Artifact struct {
					ID int64 `json:"id"`
				} `json:"artifact"`
			}
			if err != nil || json.Unmarshal(output.Bytes(), &receipt) != nil || receipt.Artifact.ID != 123 || receipt.Run.Attempt != 1 || receipt.Run.Commit != testCommit {
				t.Fatalf("inspect = %s, %v", output.String(), err)
			}
		})
	}
}

func TestInspectPagination(t *testing.T) {
	fixture := newFixture(t)
	fixture.artifacts = make([]artifact, 100)
	if err := run([]string{"inspect", "42"}, io.Discard, io.Discard, fixture.call); err != nil {
		t.Fatal(err)
	}
	if fixture.artifactAPI != 2 {
		t.Fatal("did not inspect all artifacts")
	}
}

func TestReleaseRunBundle(t *testing.T) {
	// Branch-built bytes attest to the release ref. Accept its final or
	// candidate, and reject foreign runs, versions, commits and ambiguity.
	for _, tc := range []struct {
		name, branch, bundle, want string
		extra                      string
	}{
		{"candidate", "release/2026.12.1", "katl-2026.12.1-rc.2-x86_64-1234567890ab", "", ""},
		{"final", "release/2026.12.1", "katl-2026.12.1-x86_64-1234567890ab", "", ""},
		{"main", "main", "katl-2026.12.1-rc.2-x86_64-1234567890ab", "a dispatched Release run", ""},
		{"other version", "release/2026.12.1", "katl-2026.12.10-rc.2-x86_64-1234567890ab", "release bundle for v2026.12.1 at " + testCommit + " is unavailable", ""},
		{"other stage", "release/2026.12.1", "katl-2026.12.1-beta.2-x86_64-1234567890ab", "unavailable", ""},
		{"other commit", "release/2026.12.1", "katl-2026.12.1-rc.2-x86_64-111111111111", "unavailable", ""},
		{"ambiguous", "release/2026.12.1", "katl-2026.12.1-rc.2-x86_64-1234567890ab", "multiple matching release bundles", "katl-2026.12.1-x86_64-1234567890ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.run.Tag, f.run.Event, f.run.Path = tc.branch, "workflow_dispatch", ".github/workflows/release.yml"
			f.sourceRef = "refs/heads/release/2026.12.1"
			f.artifacts = []artifact{{ID: 123, Name: tc.bundle}}
			if tc.extra != "" {
				f.artifacts = append(f.artifacts, artifact{ID: 124, Name: tc.extra})
			}
			var out bytes.Buffer
			err := run([]string{"inspect", "42"}, &out, io.Discard, f.call)
			if tc.want != "" {
				assertErrorContains(t, err, tc.want)
				return
			}
			if err != nil || !strings.Contains(out.String(), tc.bundle) {
				t.Fatalf("inspect = %v, output = %q; want %s", err, out.String(), tc.bundle)
			}
			dir := filepath.Join(t.TempDir(), "bundle")
			if err := run([]string{"download", "42", dir}, io.Discard, io.Discard, f.call); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.verified, []string{"RELEASE_NOTES.md", "payload", "SHA256SUMS"}) {
				t.Fatalf("verified %q; want all bundle files", f.verified)
			}
		})
	}
}

func TestDownload(t *testing.T) {
	for _, mode := range []string{"verified", "rerun", "replaced", "corrupt", "untrusted", "existing directory"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.rerun = mode == "rerun"
			fixture.replaced = mode == "replaced"
			fixture.corrupt = mode == "corrupt"
			fixture.provenance = mode == "untrusted"
			dir := filepath.Join(t.TempDir(), "candidate")
			if mode == "existing directory" {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var receipt bytes.Buffer
			err := run([]string{"download", "42", dir}, &receipt, io.Discard, fixture.call)
			if mode == "verified" {
				if err != nil || !strings.Contains(receipt.String(), `"id": 123`) {
					t.Fatalf("download = %s, %v", receipt.String(), err)
				}
				if !reflect.DeepEqual(fixture.verified, []string{"RELEASE_NOTES.md", "payload", "SHA256SUMS"}) || fixture.rootFetches != 1 {
					t.Fatalf("verified assets = %v after %d trusted-root fetches", fixture.verified, fixture.rootFetches)
				}
				return
			}
			if err == nil || receipt.Len() != 0 {
				t.Fatalf("failed verification produced a receipt: %s, %v", receipt.String(), err)
			}
			want := map[string]string{"rerun": "candidate changed", "replaced": "candidate changed", "corrupt": "checksum mismatch", "untrusted": "provenance verification failed", "existing directory": "create fresh download directory"}[mode]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v; want %s", err, want)
			}
			if mode != "existing directory" {
				if _, err := os.Stat(filepath.Join(dir, "payload")); err != nil {
					t.Fatal("failed download did not retain diagnostic evidence:", err)
				}
			}
		})
	}
}

func TestVerify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tag    string
		commit string
		edit   func(*githubFixture)
		want   string
	}{
		{"verified", "v2026.10.2", testCommit, func(f *githubFixture) {}, ""},
		{"root unavailable", "v2026.10.2", testCommit, func(f *githubFixture) { f.rootOffline = true }, "fetch Sigstore trusted root"},
		{"untrusted", "v2026.10.2", testCommit, func(f *githubFixture) { f.provenance = true }, "provenance verification failed for RELEASE_NOTES.md"},
		{"branch", "main", testCommit, func(f *githubFixture) {}, "release tag"},
		{"noncanonical tag", "v2026.13.0", testCommit, func(f *githubFixture) {}, "not a KatlOS release"},
		{"short commit", "v2026.10.2", testCommit[:12], func(f *githubFixture) {}, "full source commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t)
			tc.edit(fixture)
			dir := t.TempDir()
			writeBundle(t, dir)

			err := run([]string{"verify", tc.tag, tc.commit, dir}, io.Discard, io.Discard, fixture.call)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v; want %s", err, tc.want)
				}
				if len(fixture.verified) != 0 {
					t.Fatalf("accepted assets despite failure: %v", fixture.verified)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(fixture.verified, []string{"RELEASE_NOTES.md", "payload", "SHA256SUMS"}) || fixture.rootFetches != 1 {
				t.Fatalf("verify = %v; assets %v after %d trusted-root fetches", err, fixture.verified, fixture.rootFetches)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 3 {
				t.Fatalf("verification changed the bundle: %v, %v", entries, err)
			}
		})
	}
}

func TestChecksums(t *testing.T) {
	for _, mode := range []string{"valid", "tampered", "missing", "extra", "duplicate", "traversal", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeBundle(t, dir)
			var err error
			switch mode {
			case "tampered":
				err = os.WriteFile(filepath.Join(dir, "payload"), []byte("xyz"), 0o644)
			case "missing":
				err = os.Remove(filepath.Join(dir, "payload"))
			case "extra":
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0o644)
			case "duplicate", "traversal":
				name := "payload"
				if mode == "traversal" {
					name = "../payload"
				}
				file, openErr := os.OpenFile(filepath.Join(dir, "SHA256SUMS"), os.O_APPEND|os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = fmt.Fprintf(file, "%s  %s\n", strings.Repeat("a", 64), name)
				if closeErr := file.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			case "symlink":
				err = os.Symlink("payload", filepath.Join(dir, "link"))
			}
			if err != nil {
				t.Fatal(err)
			}

			names, err := verifyChecksums(dir)
			if mode == "valid" {
				if err != nil || !reflect.DeepEqual(names, []string{"RELEASE_NOTES.md", "payload"}) {
					t.Fatalf("verify = %v, %v", names, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid bundle")
			}
		})
	}
}

func TestEquivalent(t *testing.T) {
	const (
		abc   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
		empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)
	for _, tc := range []struct {
		name     string
		bundle   map[string]string
		standard string
		lts      string
		omitLTS  bool
		extra    string // an additional NAME=standard-evidence argument
		want     string
	}{
		{name: "identical", standard: abc + "  payload\n", lts: abc + "  payload\n"},
		{name: "rendered notes differ", standard: abc + "  payload\n" + strings.Repeat("b", 64) + "  RELEASE_NOTES.md\n", lts: abc + "  payload\n"},
		{name: "differing file", standard: abc + "  payload\n", lts: strings.Repeat("a", 64) + "  payload\n", want: "payload (lts) differs from qualified bytes"},
		{name: "unqualified bundle file", bundle: map[string]string{"extra": ""}, standard: abc + "  payload\n", lts: abc + "  payload\n", want: "published file was not qualified: extra"},
		{name: "qualified file missing from bundle", standard: abc + "  payload\n" + empty + "  other\n", lts: abc + "  payload\n", want: "other (standard) was qualified but is missing from the bundle"},
		{name: "unqualified provenance", bundle: map[string]string{"PROVENANCE.md": ""}, standard: abc + "  payload\n", lts: abc + "  payload\n", want: "published file was not qualified: PROVENANCE.md"},
		{name: "unknown flavour", standard: abc + "  payload\n", lts: abc + "  payload\n", extra: "beta", want: `unknown flavour "beta"`},
		{name: "duplicate flavour", standard: abc + "  payload\n", lts: abc + "  payload\n", extra: "standard", want: "duplicate qualified checksums for standard"},
		{name: "missing flavour", standard: abc + "  payload\n", omitLTS: true, want: "no qualified checksums for lts"},
		{name: "malformed evidence", standard: abc + "  payload\n", lts: "not a checksum\n", want: "qualified checksums for lts: invalid SHA256SUMS entry"},
		{name: "unreadable evidence", standard: abc + "  payload\n", lts: "-", want: "qualified checksums for lts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeBundle(t, bundle)
			sums := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  payload\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  RELEASE_NOTES.md\n"
			for name := range tc.bundle {
				if err := os.WriteFile(filepath.Join(bundle, name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				sums += empty + "  " + name + "\n"
			}
			if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
				t.Fatal(err)
			}

			evidence := t.TempDir()
			var args []string
			for _, flavour := range []struct{ name, content string }{{"standard", tc.standard}, {"lts", tc.lts}} {
				name, content := flavour.name, flavour.content
				path := filepath.Join(evidence, name)
				if name == "lts" && tc.omitLTS {
					continue
				}
				if content != "-" {
					if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				args = append(args, name+"="+path)
			}
			if tc.extra != "" {
				args = append(args, tc.extra+"="+filepath.Join(evidence, "standard"))
			}

			err := run(append([]string{"equivalent", bundle}, args...), io.Discard, io.Discard, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("equivalent = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEquivalentRequiresValidBundle(t *testing.T) {
	bundle := t.TempDir()
	writeBundle(t, bundle)
	if err := os.WriteFile(filepath.Join(bundle, "payload"), []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(evidence, []byte("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"equivalent", bundle, "standard=" + evidence, "lts=" + evidence}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch: payload") {
		t.Fatalf("equivalent = %v", err)
	}
}

func TestChecksumsCommand(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir)
	if err := run([]string{"checksums", dir}, io.Discard, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"checksums", dir}, io.Discard, io.Discard, nil); err == nil {
		t.Fatal("accepted tampered bundle")
	}
}
