package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestReviewCommand(t *testing.T) {
	dir := t.TempDir()
	const wantComment = "## Review: approved (aaaaaaaaaaaa)\n\nReady to merge.\n\n" +
		"<!-- katl-queue:review approved:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb -->"
	writeComments := func(name, association, author string) {
		t.Helper()
		raw, err := json.Marshal([][]any{
			{},
			{map[string]any{
				"id": 2, "body": wantComment, "author_association": association,
				"user": map[string]string{"login": author},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeComments("before", "COLLABORATOR", "unrelated")
	writeComments("after", "OWNER", "owner")

	sh := workstationtest.Tool(t, "sh")
	script := fmt.Sprintf(`#!%s
case "$1 $2" in
    "api graphql") printf '%%s\n' '{"data":{"viewer":{"login":"poster"},"repository":{"pullRequest":{"state":"OPEN","headRefOid":"%s"}}}}' ;;
    "api repos/katl-dev/katl/compare/%s...%s?per_page=1")
        test "$3 $4" = "--jq .merge_base_commit.sha" || exit 2
        printf '%%s\n' '%s' ;;
    "api --paginate")
        test "$3 $4" = "--slurp repos/katl-dev/katl/issues/950/comments?per_page=100" || exit 2
        if test -f "$REVIEW_FIXTURES/posted"; then
            cat "$REVIEW_FIXTURES/after"
        else
            cat "$REVIEW_FIXTURES/before"
        fi ;;
    "api --method")
        test "$3 $4 $5 $6" = "POST repos/katl-dev/katl/issues/950/comments --input -" || exit 2
        test ! -e "$REVIEW_FIXTURES/posted" || exit 2
        cat > "$REVIEW_FIXTURES/posted" ;;
    *) exit 2 ;;
esac
`, sh, testReviewHead, testReviewBase, testReviewHead, testReviewBase)
	posted := filepath.Join(dir, "posted")
	t.Setenv("REVIEW_FIXTURES", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(verdict, []byte(testApprovedFile), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Posted approved review on PR #950", "Review already recorded on PR #950"} {
		var out, stderr strings.Builder
		err := run(t.Context(), []string{"review", "post", "950", verdict}, &out, &stderr)
		if err != nil || stderr.Len() != 0 || !strings.Contains(out.String(), want) {
			t.Fatalf("run = %q, stderr %q, error %v, want %q", out.String(), stderr.String(), err, want)
		}
	}

	raw, err := os.ReadFile(posted)
	if err != nil {
		t.Fatal(err)
	}
	var postedComment struct{ Body string }
	if err := json.Unmarshal(raw, &postedComment); err != nil {
		t.Fatal(err)
	}
	if postedComment.Body != wantComment {
		t.Errorf("GitHub comment = %q, want %q", postedComment.Body, wantComment)
	}
}
