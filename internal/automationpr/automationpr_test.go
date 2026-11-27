package automationpr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

const fakeApp = "katl-automation[bot]"

// fakeSupersedeGH serves open pull requests page by page from pulls-N files,
// keeps posted comments so a later listing returns them, and records branch
// deletions and closures. A delete-fails file makes the next branch deletion
// fail once; a gone-N file makes #N's branch already deleted.
const fakeSupersedeGH = `#!/usr/bin/env bash
set -euo pipefail
api=repos/example/katl
case "$3:$4" in
 GET:$api/pulls\?state=open\&per_page=100\&page=*) cat "$FAKE_DIR/pulls-${4##*page=}" 2>/dev/null || printf '[]' ;;
 GET:$api/issues/*/comments\?per_page=100\&page=1)
   n="${4#$api/issues/}"; n="${n%%/*}"
   if [[ -f "$FAKE_DIR/comment-$n" ]]; then printf '[{"user":{"login":"%s"},%s]' "$FAKE_APP" "$(sed 's/^{//' "$FAKE_DIR/comment-$n")"; else printf '[]'; fi ;;
 POST:$api/issues/*/comments) n="${4#$api/issues/}"; n="${n%%/*}"; cat > "$FAKE_DIR/comment-$n"; echo "$n" >> "$FAKE_DIR/posts" ;;
 DELETE:$api/git/refs/heads/*)
   ref="${4#$api/git/refs/heads/}"
   if [[ -f "$FAKE_DIR/delete-fails" ]]; then rm "$FAKE_DIR/delete-fails"; echo 'gh: Server Error (HTTP 502)' >&2; exit 1; fi
   if [[ -f "$FAKE_DIR/gone-${ref##*/}" ]]; then echo 'gh: Reference does not exist (HTTP 422)' >&2; exit 1; fi
   echo "$ref" >> "$FAKE_DIR/deleted" ;;
 PATCH:$api/pulls/*) cat > "$FAKE_DIR/close-${4##*/}" ;;
 *) echo "unexpected GitHub call $3 $4" >&2; exit 1 ;;
esac
`

type fakePull struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// proposalPull is an open pull request; run 0 leaves its description without
// a run link.
func proposalPull(number int, ref, repository, author string, run int) fakePull {
	var p fakePull
	p.Number, p.Head.Ref, p.Head.Repo.FullName, p.User.Login, p.User.Type = number, ref, repository, author, "Bot"
	if author != fakeApp {
		p.User.Type = "User"
	}
	p.Body = "Pin the builder."
	if run != 0 {
		p.Body += fmt.Sprintf("\n\nPublished by [Builder Image](https://github.com/%s/actions/runs/%d).", repository, run)
	}
	return p
}

func fakeSupersede(t *testing.T, pages ...[]fakePull) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeSupersedeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, page := range pages {
		data, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pulls-%d", i+1)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_APP", fakeApp)
	return dir
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// Supersede keeps only the newest proposal the app opened, ordered by the run
// that described each one. Wrong implementations this separates: closing the
// new proposal, closing other automation, fork or human pull requests,
// reading only the first page, letting a rerun of an old run close a newer
// proposal, closing without naming the replacement or leaving the branch, and
// commenting twice when a closure is retried.
func TestSupersede(t *testing.T) {
	const prefix, repository = "automation/b-", "example/katl"

	t.Run("closes older proposals across pages", func(t *testing.T) {
		first := []fakePull{proposalPull(11, "automation/b-new", repository, fakeApp, 200)}
		for n := 100; len(first) < pageSize; n++ {
			first = append(first, proposalPull(n, fmt.Sprintf("automation/kernels-%d", n), repository, fakeApp, 300))
		}
		dir := fakeSupersede(t, first, []fakePull{
			proposalPull(7, "automation/b-old", repository, fakeApp, 100),
			proposalPull(9, "automation/b-fork", "fork/katl", fakeApp, 50),
			proposalPull(12, "automation/b-human", repository, "someone", 50),
			proposalPull(13, "automation/b-unlinked", repository, fakeApp, 0),
		})

		closed, err := Supersede(context.Background(), repository, prefix, "automation/b-new", fakeApp, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(closed) != 1 || closed[0] != 7 {
			t.Fatalf("closed = %v, want [7]", closed)
		}
		var comment struct{ Body string }
		decode(t, filepath.Join(dir, "comment-7"), &comment)
		if comment.Body != "Superseded by #11." {
			t.Fatalf("comment on #7 = %q", comment.Body)
		}
		var state struct{ State string }
		decode(t, filepath.Join(dir, "close-7"), &state)
		if state.State != "closed" {
			t.Fatalf("#7 state = %q", state.State)
		}
		if deleted := lines(t, filepath.Join(dir, "deleted")); len(deleted) != 1 || deleted[0] != "automation/b-old" {
			t.Fatalf("deleted branches = %v", deleted)
		}
		if posts := lines(t, filepath.Join(dir, "posts")); len(posts) != 1 {
			t.Fatalf("comments posted on %v", posts)
		}
	})

	t.Run("rerun of an old run", func(t *testing.T) {
		dir := fakeSupersede(t, []fakePull{
			proposalPull(20, "automation/b-newer", repository, fakeApp, 300),
			proposalPull(21, "automation/b-rerun", repository, fakeApp, 100),
		})

		closed, err := Supersede(context.Background(), repository, prefix, "automation/b-rerun", fakeApp, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(closed) != 1 || closed[0] != 21 {
			t.Fatalf("closed = %v, want only the rerun's own #21", closed)
		}
		var comment struct{ Body string }
		decode(t, filepath.Join(dir, "comment-21"), &comment)
		if comment.Body != "Superseded by #20." {
			t.Fatalf("comment on #21 = %q", comment.Body)
		}
		if _, err := os.Stat(filepath.Join(dir, "close-20")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the newer proposal #20 was closed")
		}
	})

	t.Run("retry after a failed branch deletion", func(t *testing.T) {
		pulls := []fakePull{
			proposalPull(30, "automation/b-old", repository, fakeApp, 100),
			proposalPull(31, "automation/b-new", repository, fakeApp, 200),
		}
		dir := fakeSupersede(t, pulls)
		if err := os.WriteFile(filepath.Join(dir, "delete-fails"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Supersede(context.Background(), repository, prefix, "automation/b-new", fakeApp, 200); err == nil {
			t.Fatal("a failed branch deletion was ignored")
		}
		if _, err := os.Stat(filepath.Join(dir, "close-30")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("#30 was closed although its branch remains")
		}

		closed, err := Supersede(context.Background(), repository, prefix, "automation/b-new", fakeApp, 200)
		if err != nil || len(closed) != 1 || closed[0] != 30 {
			t.Fatalf("retry closed %v: %v", closed, err)
		}
		if posts := lines(t, filepath.Join(dir, "posts")); len(posts) != 1 {
			t.Fatalf("comments posted on %v; want one comment across both runs", posts)
		}
	})

	t.Run("branch already deleted", func(t *testing.T) {
		dir := fakeSupersede(t, []fakePull{
			proposalPull(40, "automation/b-old", repository, fakeApp, 100),
			proposalPull(41, "automation/b-new", repository, fakeApp, 200),
		})
		if err := os.WriteFile(filepath.Join(dir, "gone-b-old"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if closed, err := Supersede(context.Background(), repository, prefix, "automation/b-new", fakeApp, 200); err != nil || len(closed) != 1 {
			t.Fatalf("closed %v: %v", closed, err)
		}
	})

	// Without the app's pull request for this run there is nothing to name, so
	// nothing is closed.
	for _, tc := range []struct {
		name    string
		current fakePull
	}{
		{"no pull request", proposalPull(51, "automation/b-other", repository, fakeApp, 200)},
		{"opened by someone else", proposalPull(51, "automation/b-new", repository, "someone", 200)},
		{"describes another run", proposalPull(51, "automation/b-new", repository, fakeApp, 150)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeSupersede(t, []fakePull{proposalPull(50, "automation/b-old", repository, fakeApp, 100), tc.current})
			if _, err := Supersede(context.Background(), repository, prefix, "automation/b-new", fakeApp, 200); err == nil {
				t.Fatal("superseded without this run's proposal")
			}
			if matches, _ := filepath.Glob(filepath.Join(dir, "close-*")); len(matches) != 0 {
				t.Fatalf("closed %v", matches)
			}
		})
	}
}
