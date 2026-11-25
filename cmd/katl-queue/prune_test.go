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
	return groupRun{ID: id, Branch: branch, Created: pruneNow.Add(-age)}
}

type fakeRuns struct {
	runs      []groupRun
	refs      []string
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

func (f *fakeRuns) GroupRefs(context.Context) ([]string, error) {
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
		refs []string
		want []int64
	}{
		{
			name: "live groups keep their runs, queued or running",
			runs: []groupRun{runAt(1, groupA, time.Hour), runAt(2, groupB, 10*time.Minute)},
			refs: []string{groupA, groupB},
		},
		{
			name: "a rebuilt group's earlier run is cancelled, its live run is not",
			runs: []groupRun{runAt(1, groupBOld, time.Hour), runAt(2, groupB, 5*time.Minute), runAt(3, groupCOld, time.Hour)},
			refs: []string{groupA, groupB},
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
			refs: []string{groupA},
		},
		{
			name: "runs outside the merge queue are not touched",
			runs: []groupRun{runAt(1, otherRef, time.Hour)},
			refs: []string{groupA},
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
	src := &fakeRuns{runs: []groupRun{runAt(7, groupBOld, time.Hour)}, refs: []string{groupA}}
	var out bytes.Buffer

	if err := pruneRuns(context.Background(), src, pruneNow, false, &out); err != nil {
		t.Fatal(err)
	}

	if len(src.cancelled) != 0 {
		t.Errorf("cancelled %v without apply", src.cancelled)
	}
	if !strings.Contains(out.String(), "7") || !strings.Contains(out.String(), groupBOld) {
		t.Errorf("report does not name the run and its group: %q", out.String())
	}
}

// An unreadable queue is not an empty one: every live group would look
// superseded.
func TestPruneRunsCancelsNothingWhenAListingFails(t *testing.T) {
	stale := []groupRun{runAt(1, groupBOld, time.Hour)}
	tests := map[string]*fakeRuns{
		"refs": {runs: stale, refsErr: errors.New("boom")},
		"runs": {runs: stale, refs: []string{groupA}, runsErr: errors.New("boom")},
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
	src := &fakeRuns{runs: []groupRun{runAt(1, groupBOld, time.Hour)}, refs: []string{groupA}}
	src.afterFirst = func() {
		src.refs = append(src.refs, groupB)
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
		refs:      []string{groupA},
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
