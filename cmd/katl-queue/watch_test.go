package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The fixtures are watch-query responses. recorded.json was recorded from
// katl-dev/katl; queued.json and removed.json are edits of it giving the
// earlier states those pull requests passed through:
//
//   - #488 is queued (queued.json), removed for failed checks (removed.json),
//     then queued again (recorded.json);
//   - #457 is mergeable (queued.json), then merged (removed.json and
//     recorded.json), which GitHub also records as a removal with reason
//     "merged";
//   - #499 is awaiting checks (queued.json), then unmergeable (removed.json);
//   - #486 is queued throughout.
func loadPoll(t *testing.T, name string) map[int]prResponse {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "watch", name))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Repository map[string]*prResponse `json:"repository"`
	}
	if err := decodeGraphQL(raw, &resp); err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, pr := range resp.Repository {
		numbers = append(numbers, pr.Number)
	}
	prs, err := prsByNumber(resp.Repository, numbers)
	if err != nil {
		t.Fatal(err)
	}
	return prs
}

func TestTransitions(t *testing.T) {
	tests := []struct {
		name       string
		prev, cur  string
		pr         int
		want       []string
		wantSettle bool
	}{
		{
			name: "removed for failed checks",
			prev: "queued.json", cur: "removed.json", pr: 488,
			want: []string{
				"#488 removed from the queue: failed_checks",
				"#488   failure VM (standard) / VM / lifecycle https://github.com/katl-dev/katl/actions/runs/45290515227/job/138105380290",
				"#488   failure CI https://github.com/katl-dev/katl/actions/runs/45290515227/job/138126269689",
			},
			wantSettle: true,
		},
		{
			name: "merged",
			prev: "queued.json", cur: "removed.json", pr: 457,
			want:       []string{"#457 merged as b8c94940df9e"},
			wantSettle: true,
		},
		{
			name: "unmergeable",
			prev: "queued.json", cur: "removed.json", pr: 499,
			want: []string{"#499 UNMERGEABLE at position 3; it conflicts with an entry ahead and will be removed when the queue reaches it"},
		},
		{
			name: "unchanged",
			prev: "queued.json", cur: "removed.json", pr: 486,
		},
		{
			name: "queued again after removal",
			prev: "removed.json", cur: "recorded.json", pr: 488,
			want: []string{"#488 entered the queue at position 7 (QUEUED)"},
		},
		{
			name: "merged entry stays quiet",
			prev: "removed.json", cur: "recorded.json", pr: 457,
			wantSettle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := observe(loadPoll(t, tt.prev)[tt.pr])
			cur := observe(loadPoll(t, tt.cur)[tt.pr])

			if got := transitions(prev, cur); !slices.Equal(got, tt.want) {
				t.Errorf("transitions() =\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if got := cur.settled(); got != tt.wantSettle {
				t.Errorf("settled() = %v, want %v", got, tt.wantSettle)
			}
		})
	}
}

func TestFollow(t *testing.T) {
	polls := []map[int]prResponse{
		loadPoll(t, "queued.json"),
		nil,
		loadPoll(t, "removed.json"),
	}
	var asked [][]int
	poll := func(_ context.Context, numbers []int) (map[int]prResponse, error) {
		asked = append(asked, slices.Clone(numbers))
		next := polls[0]
		polls = polls[1:]
		if next == nil {
			return nil, errors.New("gh: connection reset")
		}
		return next, nil
	}

	var out, errOut bytes.Buffer
	err := follow(context.Background(), []int{488, 457}, poll, 0, &out, &errOut)
	if !errors.Is(err, errConflict) {
		t.Errorf("follow() error = %v, want errConflict because #488 left without merging", err)
	}

	stamp := regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d `)
	want := strings.Join([]string{
		"#488 AWAITING_CHECKS at position 2",
		"#457 MERGEABLE at position 1",
		"#488 removed from the queue: failed_checks",
		"#488   failure VM (standard) / VM / lifecycle https://github.com/katl-dev/katl/actions/runs/45290515227/job/138105380290",
		"#488   failure CI https://github.com/katl-dev/katl/actions/runs/45290515227/job/138126269689",
		"#457 merged as b8c94940df9e",
		"",
	}, "\n")
	if got := stamp.ReplaceAllString(out.String(), ""); got != want {
		t.Errorf("follow() printed:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("follow() did not report the failed poll; stderr:\n%s", errOut.String())
	}
	if len(asked) != 3 {
		t.Errorf("follow() polled %d times, want 3: it should retry the failed poll and stop once both settle", len(asked))
	}
}

func TestErrorExcerpts(t *testing.T) {
	log := strings.Join([]string{
		"VM / lifecycle\tUNKNOWN STEP\t\ufeff2026-11-23T23:54:27.0355261Z Current runner version: '2.338.2'",
		"VM / lifecycle\tUNKNOWN STEP\t2026-11-23T23:55:20.5650162Z error: failed to get pool 'default'",
		"VM / lifecycle\tUNKNOWN STEP\t2026-11-23T23:55:20.5650162Z \x1b[36;1mif failed; then exit 1; fi\x1b[0m",
		"VM / lifecycle\tUNKNOWN STEP\t2026-11-23T23:55:20.5650162Z   KATL_VMTEST_KEEP: failed",
		"VM / lifecycle\tUNKNOWN STEP\t2026-11-23T23:55:20.5650162Z error: failed to get pool 'default'",
		"VM / lifecycle\tUNKNOWN STEP\t2026-11-24T00:10:55.3974587Z ##[error]Process completed with exit code 1.",
		"CI\tRequire selected validation\t2026-11-24T00:39:31.2727824Z --- FAIL: TestSomething (0.01s)",
	}, "\n")

	got := errorExcerpts([]byte(log), 8)
	want := map[string][]string{
		"VM / lifecycle": {"##[error]Process completed with exit code 1."},
		"CI":             {"--- FAIL: TestSomething (0.01s)"},
	}
	for job, lines := range want {
		if !slices.Equal(got[job], lines) {
			t.Errorf("excerpts[%q] = %q, want %q", job, got[job], lines)
		}
	}
	if len(got) != len(want) {
		t.Errorf("excerpts has jobs %v, want %v", got, want)
	}
}
