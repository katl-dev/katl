package reviewrecord_test

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

const (
	reviewedHead = "0123456789abcdef0123456789abcdef01234567"
	reviewedBase = "123456789abcdef0123456789abcdef012345678"
	currentHead  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	currentBase  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

const fixedFile = `{
  "version": 1,
  "pr": 950,
  "head": "0123456789abcdef0123456789abcdef01234567",
  "base": "123456789abcdef0123456789abcdef012345678",
  "verdict": "changes-requested",
  "summary": "The recovery path needs a fix.",
  "findings": [
    {"severity": "note", "title": "Follow-up", "body": "Consider a separate cleanup."},
    {"severity": "major", "path": "internal/worker/worker.go", "title": "Missing retry", "body": "Retry after a transient read error."},
    {"severity": "blocker", "path": "internal/worker/worker.go", "line": 41, "title": "Lost update", "body": "Preserve the previous state on error."},
    {"severity": "minor", "title": "Error message", "body": "Include the operation name."},
    {"severity": "major", "title": "Partial write", "body": "Check the result before continuing."}
  ]
}`

const fixedComment = `## Review: changes-requested (0123456789ab)

The recovery path needs a fix.

### blocker: Lost update (internal/worker/worker.go:41)

Preserve the previous state on error.

### major: Missing retry (internal/worker/worker.go)

Retry after a transient read error.

### major: Partial write

Check the result before continuing.

### minor: Error message

Include the operation name.

### note: Follow-up

Consider a separate cleanup.

<!-- katl-queue:review changes-requested:0123456789abcdef0123456789abcdef01234567:123456789abcdef0123456789abcdef012345678 -->`

func TestRenderVerdict(t *testing.T) {
	file, err := reviewrecord.Decode(strings.NewReader(fixedFile))
	if err != nil {
		t.Fatal(err)
	}

	if got := file.Comment(); got != fixedComment {
		t.Errorf("rendered comment:\n%s\n\nwant:\n%s", got, fixedComment)
	}
}

func TestParseReviewComment(t *testing.T) {
	want := reviewrecord.Record{Verdict: reviewrecord.ChangesRequested, Head: reviewedHead, Base: reviewedBase}

	got, ok := reviewrecord.ParseComment(fixedComment)
	if !ok || got != want {
		t.Errorf("ParseComment = %+v, %t, want %+v, true", got, ok, want)
	}
}

func TestRejectInvalidMarkers(t *testing.T) {
	for _, body := range []string{
		fixedComment + "\nTrailing text",
		"<!-- katl-queue:review approved:abc:" + reviewedBase + " -->",
		"<!-- katl-queue:review unknown:" + reviewedHead + ":" + reviewedBase + " -->",
		"<!-- katl-queue:removal " + reviewedHead + " -->",
	} {
		if record, ok := reviewrecord.ParseComment(body); ok {
			t.Errorf("ParseComment accepted %+v from %q", record, body)
		}
	}
}
