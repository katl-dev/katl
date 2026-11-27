package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func newFixture(t *testing.T) (fixtureRepo, string) {
	t.Helper()
	repo := fixtureRepo{t: t, dir: t.TempDir()}
	repo.git("init", "--quiet", "--initial-branch=main")
	repo.git("commit", "--quiet", "--allow-empty", "-m", "root")
	base := repo.commit(repo.git("rev-parse", "HEAD"), map[string]string{"a": "a\n", "b": "b\n", "c": "c\n"})
	return repo, base
}

func fixtureObjects(t *testing.T, repo fixtureRepo) *scratchObjects {
	t.Helper()
	objects, err := newScratchObjects(repo.dir, gittest.Env(os.Environ(), repo.dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	return objects
}

func fixtureEntry(position int, pr queuedPR, state string) queueEntry {
	var e queueEntry
	e.Position = position
	e.State = state
	e.PullRequest.Number = pr.Number
	e.PullRequest.HeadRefOid = pr.Head
	return e
}

func fixturePending(pr queuedPR, base string) prResponse {
	return prResponse{Number: pr.Number, HeadRefOid: pr.Head, BaseRefName: base}
}

func TestPendingConflicts(t *testing.T) {
	repo, base := newFixture(t)
	queued := queuedPR{Number: 1, Head: repo.commit(base, map[string]string{"b": "queued\n"})}
	candidate := queuedPR{Number: 10, Head: repo.commit(base, map[string]string{"a": "candidate\n"})}
	clashes := queuedPR{Number: 20, Head: repo.commit(base, map[string]string{"a": "pending\n"})}
	clean := queuedPR{Number: 21, Head: repo.commit(base, map[string]string{"c": "pending\n"})}
	clashesWithQueue := queuedPR{Number: 22, Head: repo.commit(base, map[string]string{"b": "pending\n"})}
	otherBranch := queuedPR{Number: 23, Head: repo.commit(base, map[string]string{"a": "release\n"})}

	snap := queueSnapshot{
		Branch:  "main",
		BaseOid: base,
		Entries: []queueEntry{fixtureEntry(1, queued, "MERGEABLE")},
		Pending: []prResponse{
			fixturePending(clashes, "main"),
			fixturePending(clean, "main"),
			fixturePending(clashesWithQueue, "main"),
			fixturePending(otherBranch, "release/2026.11"),
			// The candidate itself may already have auto-merge enabled.
			fixturePending(candidate, "main"),
		},
	}

	t.Run("waiting", func(t *testing.T) {
		got, err := pendingConflicts(context.Background(), fixtureObjects(t, repo), snap, candidate)
		if err != nil {
			t.Fatal(err)
		}
		// Only #20 conflicts with the candidate itself: #22 conflicts with
		// the queue, which removes it whatever the candidate does, and #23
		// joins the release branch's queue.
		want := []pendingConflict{{Number: 20, Conflicts: []string{"a"}}}
		if !slices.EqualFunc(got, want, func(a, b pendingConflict) bool {
			return a.Number == b.Number && slices.Equal(a.Conflicts, b.Conflicts)
		}) {
			t.Errorf("pendingConflicts() = %+v, want %+v", got, want)
		}
	})

	t.Run("candidate already queued", func(t *testing.T) {
		snap := snap
		snap.Entries = append(slices.Clone(snap.Entries), fixtureEntry(2, candidate, "MERGEABLE"))
		got, err := pendingConflicts(context.Background(), fixtureObjects(t, repo), snap, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("pendingConflicts() = %+v, want none: pending PRs join behind a queued candidate", got)
		}
	})
}

func TestAdmitPending(t *testing.T) {
	conflicts := []pendingConflict{{Number: 920, Conflicts: []string{".github/workflows/system-extensions.yml"}}}
	tests := []struct {
		name      string
		conflicts []pendingConflict
		allow     bool
		wantErr   error
		wantOut   []string
	}{
		{name: "no conflicts"},
		{
			name:      "refused",
			conflicts: conflicts,
			wantErr:   errConflict,
			wantOut:   []string{"refusing to enqueue #931", "#920", "--allow-pending-conflict"},
		},
		{
			name:      "risk accepted",
			conflicts: conflicts,
			allow:     true,
			wantOut:   []string{"enqueueing #931 anyway", "#920"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := admitPending(&out, 931, tt.conflicts, tt.allow)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("admitPending() error = %v, want %v", err, tt.wantErr)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("admitPending() printed %q, want it to contain %q", out.String(), want)
				}
			}
			if tt.wantOut == nil && out.Len() > 0 {
				t.Errorf("admitPending() printed %q, want nothing", out.String())
			}
		})
	}
}

func TestReportPendingConflicts(t *testing.T) {
	var out bytes.Buffer
	reportPendingConflicts(&out, 931, []pendingConflict{{Number: 920, Conflicts: []string{".github/workflows/system-extensions.yml"}}})
	want := "#931 conflicts with #920, which has auto-merge enabled and has not joined the queue; whichever joins second will be removed:\n" +
		"  .github/workflows/system-extensions.yml\n"
	if out.String() != want {
		t.Errorf("reportPendingConflicts() printed %q, want %q", out.String(), want)
	}
}

func TestExplainUnmergeable(t *testing.T) {
	repo, base := newFixture(t)
	first := queuedPR{Number: 1, Head: repo.commit(base, map[string]string{"c": "first\n"})}
	touchesB := queuedPR{Number: 2, Head: repo.commit(base, map[string]string{"b": "second\n"})}
	clashesWithSecond := queuedPR{Number: 3, Head: repo.commit(base, map[string]string{"b": "third\n"})}
	touchesA := queuedPR{Number: 4, Head: repo.commit(base, map[string]string{"a": "fourth\n"})}
	clashesWithFourth := queuedPR{Number: 5, Head: repo.commit(base, map[string]string{"a": "fifth\n"})}

	tests := []struct {
		name   string
		queue  []queuedPR
		number int
		want   string
	}{
		{
			name:   "conflict with an entry ahead",
			queue:  []queuedPR{first, touchesB, clashesWithSecond},
			number: 3,
			want:   "#3 conflicts with #2 (queue position 2):\n  b\n",
		},
		{
			// An entry behind is built on top of this one, so it cannot be
			// what makes this one unmergeable.
			name:   "conflict only with an entry behind",
			queue:  []queuedPR{first, clashesWithFourth, touchesA},
			number: 5,
			want:   "#5 merges cleanly\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := queueSnapshot{Branch: "main", BaseOid: base}
			for i, pr := range tt.queue {
				snap.Entries = append(snap.Entries, fixtureEntry(i+1, pr, "MERGEABLE"))
			}
			var out bytes.Buffer
			if err := explainUnmergeable(context.Background(), fixtureObjects(t, repo), snap, tt.number, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(out.String(), tt.want) {
				t.Errorf("explainUnmergeable() printed:\n%s\nwant it to end with:\n%s", out.String(), tt.want)
			}
		})
	}

	t.Run("not queued", func(t *testing.T) {
		snap := queueSnapshot{Branch: "main", BaseOid: base, Entries: []queueEntry{fixtureEntry(1, first, "MERGEABLE")}}
		if err := explainUnmergeable(context.Background(), fixtureObjects(t, repo), snap, 3, &bytes.Buffer{}); err == nil {
			t.Error("explainUnmergeable() of a PR outside the queue succeeded, want an error")
		}
	})
}

func TestFollowExplainsUnmergeable(t *testing.T) {
	queued := func(state, head string) prResponse {
		pr := prResponse{Number: 920, State: "OPEN", HeadRefOid: head, IsInMergeQueue: true}
		pr.MergeQueueEntry = &struct {
			Position int    `json:"position"`
			State    string `json:"state"`
		}{Position: 2, State: state}
		return pr
	}
	polls := []prResponse{
		queued("AWAITING_CHECKS", "aaa"),
		queued("UNMERGEABLE", "aaa"),
		queued("UNMERGEABLE", "aaa"),
		queued("UNMERGEABLE", "bbb"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poll := func(context.Context, []int) (map[int]prResponse, error) {
		if len(polls) == 0 {
			return nil, context.Canceled
		}
		next := polls[0]
		polls = polls[1:]
		if len(polls) == 0 {
			cancel()
		}
		return map[int]prResponse{920: next}, nil
	}
	var explained []int
	explain := func(_ context.Context, n int) ([]string, error) {
		explained = append(explained, n)
		return []string{"#920 conflicts with #931 (queue position 1):", "  .github/workflows/system-extensions.yml"}, nil
	}

	var out bytes.Buffer
	if err := follow(ctx, []int{920}, poll, explain, 0, &out, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("follow() error = %v, want context.Canceled", err)
	}

	// The cause is named when the entry becomes unmergeable and again when
	// a new head becomes unmergeable, not on every poll in between.
	stamp := regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d `)
	want := strings.Join([]string{
		"#920 AWAITING_CHECKS at position 2",
		"#920 UNMERGEABLE at position 2; it conflicts with an entry ahead and will be removed when the queue reaches it",
		"#920 conflicts with #931 (queue position 1):",
		"  .github/workflows/system-extensions.yml",
		"#920 conflicts with #931 (queue position 1):",
		"  .github/workflows/system-extensions.yml",
		"",
	}, "\n")
	if got := stamp.ReplaceAllString(out.String(), ""); got != want {
		t.Errorf("follow() printed:\n%s\nwant:\n%s", got, want)
	}
	if !slices.Equal(explained, []int{920, 920}) {
		t.Errorf("explain called for %v, want [920 920]", explained)
	}
}
