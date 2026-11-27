package automationpr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH serves the GitHub API calls Publish makes and records every request
// body by operation, so a test observes the mutations independently of the
// returned commit. Any other call fails.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
printf '%s %s\n' "$3" "$4" >> "$FAKE_DIR/calls"
case "$3:$4" in
 GET:*/git/ref/heads/*)
   [[ -e "$FAKE_DIR/branch" ]] || { echo "${FAKE_REF_ERROR:-HTTP 404}" >&2; exit 1; }
   head=before-merge
   [[ ! -e "$FAKE_DIR/merged" ]] || head=after-merge
   printf '{"object":{"sha":"%s"}}' "$head" ;;
 POST:*/merges) cat > "$FAKE_DIR/merge"; : > "$FAKE_DIR/merged" ;;
 GET:*/contents/*\?ref=after-merge) printf '{"content":"%s"}' "$FAKE_EXISTING" ;;
 GET:*/git/commits/*) printf '{"tree":{"sha":"base-tree"}}' ;;
 POST:*/git/trees) cat > "$FAKE_DIR/tree"; printf '{"sha":"new-tree"}' ;;
 POST:*/git/commits) cat > "$FAKE_DIR/commit"; printf '{"sha":"new-commit"}' ;;
 POST:*/git/refs) cat > "$FAKE_DIR/ref" ;;
 *) echo "unexpected GitHub call $3 $4" >&2; exit 1 ;;
esac
`

func fakeGitHub(t *testing.T, branchExists bool, existing string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	if branchExists {
		if err := os.WriteFile(filepath.Join(dir, "branch"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_EXISTING", base64.StdEncoding.EncodeToString([]byte(existing)))
	return dir
}

func proposal(content string) Proposal {
	return Proposal{
		Repository: "example/katl",
		Base:       strings.Repeat("a", 40),
		Branch:     "automation/test-1",
		Message:    "area: change\n",
		Path:       "dir/file.lock",
		Content:    []byte(content),
		Matches: func(existing []byte) error {
			if string(existing) != content {
				return errors.New("different proposal")
			}
			return nil
		},
	}
}

func decode(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// A new proposal is one commit on the base that writes only its file.
func TestPublishCreates(t *testing.T) {
	dir := fakeGitHub(t, false, "")
	commit, err := Publish(context.Background(), proposal("pin\n"))
	if err != nil {
		t.Fatal(err)
	}
	if commit != "new-commit" {
		t.Fatalf("commit = %q", commit)
	}

	var tree struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct{ Path, Content string }
	}
	decode(t, filepath.Join(dir, "tree"), &tree)
	if tree.BaseTree != "base-tree" || len(tree.Tree) != 1 || tree.Tree[0].Path != "dir/file.lock" || tree.Tree[0].Content != "pin\n" {
		t.Fatalf("tree = %+v", tree)
	}
	var made struct {
		Message string
		Parents []string
	}
	decode(t, filepath.Join(dir, "commit"), &made)
	if made.Message != "area: change\n" || len(made.Parents) != 1 || made.Parents[0] != strings.Repeat("a", 40) {
		t.Fatalf("commit = %+v", made)
	}
	var ref struct{ Ref, SHA string }
	decode(t, filepath.Join(dir, "ref"), &ref)
	if ref.Ref != "refs/heads/automation/test-1" || ref.SHA != "new-commit" {
		t.Fatalf("ref = %+v", ref)
	}
}

// An existing proposal is advanced by merging the base, never replaced, and
// the head to qualify is the one after the merge.
func TestPublishResumes(t *testing.T) {
	dir := fakeGitHub(t, true, "pin\n")
	commit, err := Publish(context.Background(), proposal("pin\n"))
	if err != nil {
		t.Fatal(err)
	}
	if commit != "after-merge" {
		t.Fatalf("commit = %q", commit)
	}
	var merge struct{ Base, Head string }
	decode(t, filepath.Join(dir, "merge"), &merge)
	if merge.Base != "automation/test-1" || merge.Head != strings.Repeat("a", 40) {
		t.Fatalf("merge = %+v", merge)
	}
	for _, mutation := range []string{"tree", "commit", "ref"} {
		if _, err := os.Stat(filepath.Join(dir, mutation)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("resuming created a %s", mutation)
		}
	}
}

// An existing branch with other content is reported, not reused or replaced.
func TestPublishPreservesDifferentProposal(t *testing.T) {
	dir := fakeGitHub(t, true, "other pin\n")
	if _, err := Publish(context.Background(), proposal("pin\n")); err == nil || err.Error() != "different proposal" {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ref")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a different proposal was replaced")
	}
}

// Only a missing branch starts a new proposal; any other lookup failure stops.
func TestPublishStopsOnLookupFailure(t *testing.T) {
	dir := fakeGitHub(t, false, "")
	t.Setenv("FAKE_REF_ERROR", "HTTP 502")
	if _, err := Publish(context.Background(), proposal("pin\n")); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(calls)) != "GET repos/example/katl/git/ref/heads/automation/test-1" {
		t.Fatalf("calls after a failed lookup = %q", calls)
	}
}
