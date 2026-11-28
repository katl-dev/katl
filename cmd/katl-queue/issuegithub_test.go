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

func TestIssueCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INTAKE_FIXTURE", dir)
	sh := workstationtest.Tool(t, "sh")
	script := fmt.Sprintf(`#!%s
set -eu
case "$1 $2" in
  "api --paginate")
    case "$4" in
      'repos/katl-dev/katl/issues?state=all&per_page=100') cat "$INTAKE_FIXTURE/issues.json" ;;
      'repos/katl-dev/katl/labels?per_page=100') printf '[[{"name":"enhancement"},{"name":"area: dev-tooling"},{"name":"priority: p1"}]]\n' ;;
      *) exit 2 ;;
    esac ;;
  "issue create")
    test "$3 $4 $7 $8" = "--repo github.com/katl-dev/katl --body-file -"
    cat > "$INTAKE_FIXTURE/created.md"
    printf 'https://github.com/katl-dev/katl/issues/20\n' ;;
  "api --method")
    test "$3 $4 $5 $6" = "POST repos/katl-dev/katl/issues/2/comments --input -"
    cat > "$INTAKE_FIXTURE/comment.json" ;;
  *) exit 2 ;;
esac
`, sh)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"draft.md": "The queue repeats GitHub repository resolution when posting a review." + criteria,
		"issues.json": `[[{"number":2,"title":"Repository lookup ownership","body":"The queue repeats GitHub repository resolution when posting a review.","state":"open"},
{"number":3,"title":"Repository lookup ownership","body":"The queue repeats GitHub repository resolution when posting a review.","state":"open","pull_request":{"url":"https://example.org/pulls/3"}}]]`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	args := []string{
		"issue", "new", "--title", "Repository lookup belongs to one owner", "--body-file", filepath.Join(dir, "draft.md"),
		"--kind", "enhancement", "--area", "dev-tooling", "--priority", "p1",
	}
	var out, stderr strings.Builder
	err := run(t.Context(), args, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "strong open matches #2 require") || strings.Contains(out.String(), "#3") {
		t.Fatalf("initial request: %v\n%s%s", err, &out, &stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "created.md")); !os.IsNotExist(err) {
		t.Fatalf("refused request reached GitHub creation: %v", err)
	}
	out.Reset()
	if err := run(t.Context(), append(args, "--supersedes", "2"), &out, &stderr); err != nil {
		t.Fatalf("confirmed request: %v\n%s%s", err, &out, &stderr)
	}
	created, err := os.ReadFile(filepath.Join(dir, "created.md"))
	if err != nil || !strings.Contains(string(created), "Supersedes: #2") || !strings.Contains(out.String(), "Created #20.") {
		t.Fatalf("created issue = %q, %v; output %s", created, err, &out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "comment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var comment struct{ Body string }
	if err := json.Unmarshal(raw, &comment); err != nil || comment.Body != "Superseded by #20." {
		t.Fatalf("supersession comment = %s, %v", raw, err)
	}
}
