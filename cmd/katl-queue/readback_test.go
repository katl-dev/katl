package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GitHub can answer 404 for an issue it has just created, so linking it as a
// sub-issue waits for the issue to show. A refusal is not read again.
func TestAddSubIssueReadback(t *testing.T) {
	const read = "api repos/katl-dev/katl/issues/20 --jq .id --hostname github.com"
	const link = "api --method POST repos/katl-dev/katl/issues/2/sub_issues -F sub_issue_id=9020 --hostname github.com"
	for _, tc := range []struct {
		name, failure string
		failures      int
		wantErr       string
		want          []string
	}{
		{name: "found after 404s", failure: "gh: Not Found (HTTP 404)", failures: 2, want: []string{read, read, read, link}},
		{name: "refused", failure: "gh: Resource not accessible by integration (HTTP 403)", failures: 1 << 30,
			wantErr: "HTTP 403", want: []string{read}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := fakeGHCases(t, `
  "`+read+`")
    if [ "$(grep -c 'issues/20 ' "$log")" -le `+strconv.Itoa(tc.failures)+` ]; then echo '`+tc.failure+`' >&2; exit 1; fi
    echo 9020 ;;
  "`+link+`") ;;`)
			g := newGH()
			g.readDelay = time.Millisecond

			err := g.addSubIssue(t.Context(), 2, 20)

			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("addSubIssue = %v, want %q", err, tc.wantErr)
			}
			if got := logged(); !slices.Equal(got, tc.want) {
				t.Fatalf("gh calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// GitHub adds a merge-queue entry asynchronously after auto-merge is enabled,
// so the enqueue looks again until the entry shows.
func TestWaitQueuedReadback(t *testing.T) {
	logged := fakeGHCases(t, `
  "api graphql"*)
    if [ "$(grep -c '^api graphql' "$log")" -le 2 ]; then entry=null; else entry='{"position":3,"state":"QUEUED"}'; fi
    printf '{"data":{"repository":{"pr7":{"number":7,"mergeQueueEntry":%s}}}}\n' "$entry" ;;`)
	g := newGH()
	g.readDelay = time.Millisecond
	var out strings.Builder

	queued := waitQueued(t.Context(), g, 7, &out)

	// The logged query spans several lines; each read logs one "api graphql".
	reads := 0
	for _, line := range logged() {
		if strings.HasPrefix(line, "api graphql") {
			reads++
		}
	}
	if !queued || out.String() != "#7 queued at position 3 (QUEUED)\n" || reads != 3 {
		t.Fatalf("waitQueued = %t, %q after %d reads", queued, out.String(), reads)
	}
}
