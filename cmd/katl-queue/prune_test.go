package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	groupA    = "gh-readonly-queue/main/pr-595-aaaa"
	groupB    = "gh-readonly-queue/main/pr-584-bbbb"
	groupBOld = "gh-readonly-queue/main/pr-584-0000"
	groupCOld = "gh-readonly-queue/main/pr-554-1111"
	otherRef  = "release/v1"
)

var pruneNow = time.Date(2026, 11, 25, 4, 0, 0, 0, time.UTC)

func runAt(id int64, branch string, age time.Duration) groupRun {
	return groupRun{ID: id, Branch: branch, SHA: shaOf(branch), Created: pruneNow.Add(-age)}
}

type fakeRuns struct {
	runs      []groupRun
	refs      []groupRef
	entries   int
	runsErr   error
	refsErr   error
	cancelErr map[int64]error

	cancelled []int64
	calls     []string
	// afterFirst runs once the first listing has been taken, to change the
	// world between the two listings.
	afterFirst func()
}

func (f *fakeRuns) ActiveGroupRuns(context.Context) ([]groupRun, error) {
	defer f.listed("runs")
	return slices.Clone(f.runs), f.runsErr
}

func (f *fakeRuns) GroupRefs(context.Context) ([]groupRef, error) {
	defer f.listed("refs")
	return slices.Clone(f.refs), f.refsErr
}

func (f *fakeRuns) listed(kind string) {
	f.calls = append(f.calls, kind)
	if len(f.calls) == 1 && f.afterFirst != nil {
		f.afterFirst()
	}
}

func (f *fakeRuns) CancelRun(_ context.Context, id int64) error {
	f.cancelled = append(f.cancelled, id)
	return f.cancelErr[id]
}

func TestPruneRuns(t *testing.T) {
	tests := []struct {
		name string
		runs []groupRun
		refs []groupRef
		want []int64
	}{
		{
			name: "live groups keep their runs, queued or running",
			runs: []groupRun{runAt(1, groupA, time.Hour), runAt(2, groupB, 10*time.Minute)},
			refs: refs(groupA, groupB),
		},
		{
			name: "a rebuilt group's earlier run is cancelled, its live run is not",
			runs: []groupRun{runAt(1, groupBOld, time.Hour), runAt(2, groupB, 5*time.Minute), runAt(3, groupCOld, time.Hour)},
			refs: refs(groupA, groupB),
			want: []int64{1, 3},
		},
		{
			name: "an empty queue leaves no group to keep",
			runs: []groupRun{runAt(1, groupA, time.Hour)},
			want: []int64{1},
		},
		{
			name: "a run too new to trust the ref listing is kept",
			runs: []groupRun{runAt(1, groupBOld, 30*time.Second)},
			refs: refs(groupA),
		},
		{
			name: "runs outside the merge queue are not touched",
			runs: []groupRun{runAt(1, otherRef, time.Hour)},
			refs: refs(groupA),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeRuns{runs: tt.runs, refs: tt.refs}

			if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}

			got := slices.Clone(src.cancelled)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("cancelled %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPruneRunsReportsWithoutApply(t *testing.T) {
	src := &fakeRuns{runs: []groupRun{runAt(4242, groupBOld, time.Hour)}, refs: refs(groupA)}
	var out bytes.Buffer

	if err := pruneRuns(context.Background(), src, pruneNow, false, &out); err != nil {
		t.Fatal(err)
	}

	if len(src.cancelled) != 0 {
		t.Errorf("cancelled %v without apply", src.cancelled)
	}
	if !strings.Contains(out.String(), "4242") || !strings.Contains(out.String(), groupBOld) {
		t.Errorf("report does not name the run and its group: %q", out.String())
	}
}

// An unreadable queue is not an empty one: every live group would look
// superseded.
func TestPruneRunsCancelsNothingWhenAListingFails(t *testing.T) {
	stale := []groupRun{runAt(1, groupBOld, time.Hour)}
	tests := map[string]*fakeRuns{
		"refs": {runs: stale, refsErr: errors.New("boom")},
		"runs": {runs: stale, refs: refs(groupA), runsErr: errors.New("boom")},
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err == nil {
				t.Fatal("want an error")
			}

			if len(src.cancelled) != 0 {
				t.Errorf("cancelled %v", src.cancelled)
			}
		})
	}
}

// A group created between the two listings has a run the first listing does
// not contain, so it must not be judged against a ref listing that predates
// its ref. The run is old here so that only the listing order protects it.
func TestPruneRunsKeepsAGroupCreatedBetweenListings(t *testing.T) {
	src := &fakeRuns{runs: []groupRun{runAt(1, groupBOld, time.Hour)}, refs: refs(groupA)}
	src.afterFirst = func() {
		src.refs = append(src.refs, groupRef{Name: groupB, SHA: shaOf(groupB)})
		src.runs = append(src.runs, runAt(2, groupB, time.Hour))
	}

	if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(src.cancelled, []int64{1}) {
		t.Errorf("cancelled %v, want only the superseded run 1", src.cancelled)
	}
}

func TestPruneRunsContinuesAfterCancelFails(t *testing.T) {
	src := &fakeRuns{
		runs:      []groupRun{runAt(1, groupBOld, time.Hour), runAt(2, groupCOld, time.Hour)},
		refs:      refs(groupA),
		cancelErr: map[int64]error{1: errors.New("boom")},
	}

	err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{})

	if err == nil {
		t.Error("want the cancel failure reported")
	}
	if !slices.Equal(src.cancelled, []int64{1, 2}) {
		t.Errorf("attempted %v, want both", src.cancelled)
	}
}

func TestDecodeGroupRuns(t *testing.T) {
	raw := []byte(`{"total_count":1,"workflow_runs":[{"id":45532966201,"head_branch":"gh-readonly-queue/main/pr-592-74068d81","created_at":"2026-11-25T03:00:00Z","status":"in_progress"}]}`)

	got, err := decodeGroupRuns(raw)
	if err != nil {
		t.Fatal(err)
	}

	want := []groupRun{{ID: 45532966201, Branch: "gh-readonly-queue/main/pr-592-74068d81", Created: time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func shaOf(branch string) string { return "sha-" + branch }

func refs(names ...string) []groupRef {
	var out []groupRef
	for _, n := range names {
		out = append(out, groupRef{Name: n, SHA: shaOf(n)})
	}
	return out
}

func (f *fakeRuns) QueueEntries(context.Context) (int, error) { return f.entries, nil }

func TestDecodeGroupRefsMatchesRunBranch(t *testing.T) {
	raw := []byte(`[{"ref":"refs/heads/gh-readonly-queue/main/pr-556-0a1b2c","node_id":"x","url":"u","object":{"sha":"d34db33f","type":"commit","url":"u"}}]
[{"ref":"refs/heads/gh-readonly-queue/main/pr-592-74068d81","object":{"sha":"cafe1234","type":"commit"}}]`)
	runs, err := decodeGroupRuns([]byte(`{"workflow_runs":[{"id":1,"head_branch":"gh-readonly-queue/main/pr-556-0a1b2c","head_sha":"d34db33f","created_at":"2026-11-25T01:00:00Z"},{"id":2,"head_branch":"gh-readonly-queue/main/pr-592-74068d81","head_sha":"cafe1234","created_at":"2026-11-25T01:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err := decodeGroupRefs(raw)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("decoded %d refs from two pages, want 2", len(got))
	}
	if stale := supersededRuns(runs, got, pruneNow); len(stale) != 0 {
		t.Errorf("live runs judged superseded: %v", stale)
	}
}

func TestPruneRunsCancelsRunWhoseRefWasRecreated(t *testing.T) {
	run := runAt(1, groupA, time.Hour)
	src := &fakeRuns{runs: []groupRun{run}, refs: []groupRef{{Name: groupA, SHA: "recreated"}}}

	if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(src.cancelled, []int64{1}) {
		t.Errorf("cancelled %v, want the run of the old commit", src.cancelled)
	}
}

func TestPruneRunsCancelsNothingWhenRefsEmptyButQueueIsNot(t *testing.T) {
	src := &fakeRuns{runs: []groupRun{runAt(1, groupA, time.Hour)}, entries: 2}

	err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{})

	if err == nil {
		t.Error("want the inconsistency reported")
	}
	if len(src.cancelled) != 0 {
		t.Errorf("cancelled %v", src.cancelled)
	}
}

func TestPruneRunsAlreadyCompletedIsSuccess(t *testing.T) {
	src := &fakeRuns{
		runs:      []groupRun{runAt(1, groupBOld, time.Hour)},
		refs:      refs(groupA),
		cancelErr: map[int64]error{1: errRunCompleted},
	}

	if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err != nil {
		t.Errorf("a run that finished first is not a failure: %v", err)
	}
}

func TestPruneRunsGraceBoundary(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want []int64
	}{
		{pruneGrace - time.Second, nil},
		{pruneGrace, []int64{1}},
	}
	for _, tt := range tests {
		src := &fakeRuns{runs: []groupRun{runAt(1, groupBOld, tt.age)}, refs: refs(groupA)}

		if err := pruneRuns(context.Background(), src, pruneNow, true, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(src.cancelled, tt.want) {
			t.Errorf("age %v: cancelled %v, want %v", tt.age, src.cancelled, tt.want)
		}
	}
}
