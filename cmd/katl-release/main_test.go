package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testCommit = "1234567890abcdef1234567890abcdef12345678"

type githubFixture struct {
	t           *testing.T
	run         workflowRun
	artifacts   []artifact
	refs        []map[string]string
	protected   bool
	rerun       bool
	replaced    bool
	corrupt     bool
	provenance  bool
	verified    []string
	artifactAPI int
}

func newFixture(t *testing.T) *githubFixture {
	return &githubFixture{
		t:         t,
		run:       workflowRun{ID: 42, Attempt: 1, Tag: "v2026.10.2", Commit: testCommit, Event: "push", Path: workflow, Status: "waiting"},
		artifacts: []artifact{{ID: 123, Name: "katl-2026.10.2-x86_64-1234567890ab", Digest: "sha256:archive"}},
		protected: true,
	}
}

func (f *githubFixture) call(name string, args ...string) ([]byte, error) {
	f.t.Helper()
	if name == "scripts/katl-release-artifacts" {
		return exec.Command(filepath.Join("..", "..", name), args...).CombinedOutput()
	}
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
		case "commits/main":
			value = map[string]string{"sha": testCommit}
		case "git/matching-refs/tags/v2026.10.2":
			value = f.refs
		case "environments/release":
			rules := []any{}
			if f.protected {
				rules = append(rules, map[string]any{"type": "required_reviewers", "reviewers": []any{map[string]any{"type": "User"}}})
			}
			value = map[string]any{"protection_rules": rules}
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
	if args[0] == "attestation" {
		joined := strings.Join(args, " ")
		for _, constraint := range []string{"--repo " + repository, "--signer-workflow " + repository + "/" + workflow, "--source-ref refs/tags/v2026.10.2", "--source-digest " + testCommit} {
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

func TestPrepare(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*githubFixture)
		want string
	}{
		{"available", func(f *githubFixture) {}, ""},
		{"prefix tag", func(f *githubFixture) { f.refs = []map[string]string{{"ref": "refs/tags/v2026.10.20"}} }, ""},
		{"used tag", func(f *githubFixture) { f.refs = []map[string]string{{"ref": "refs/tags/v2026.10.2"}} }, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t)
			tc.edit(fixture)
			var output bytes.Buffer
			err := run([]string{"prepare", "2026.10.2"}, &output, io.Discard, fixture.call)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v; want %s", err, tc.want)
				}
				return
			}
			if err != nil || !strings.Contains(output.String(), testCommit) || !strings.Contains(output.String(), "v2026.10.2") {
				t.Fatalf("prepare = %s, %v", output.String(), err)
			}
		})
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
		{"ambiguous", func(f *githubFixture) { f.artifacts = append(f.artifacts, f.artifacts[0]) }, "multiple final bundles"},
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
				if !reflect.DeepEqual(fixture.verified, []string{"RELEASE_NOTES.md", "payload", "SHA256SUMS"}) {
					t.Fatalf("verified assets = %v", fixture.verified)
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
