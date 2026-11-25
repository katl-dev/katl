package main

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func removalBy(id, reason, actor string) removalEvent {
	r := removalEvent{ID: id, CreatedAt: epoch.Add(time.Hour), Reason: reason}
	if actor != "" {
		r.Actor = &struct {
			Login string `json:"login"`
		}{actor}
	}
	return r
}

func TestSettleRemoval(t *testing.T) {
	failed := removalBy("r2", "failed_checks", "github-merge-queue")

	tests := []struct {
		name        string
		removal     removalEvent
		head        string
		marks       []marker
		wantEjected bool
	}{
		{name: "no push", removal: failed, head: "A", marks: []marker{removalMarker("r2", "A")}, wantEjected: true},
		// The author pushed B, then katl-queue rebased it to C. Whatever
		// produced the head, it is not the one the removal was observed at.
		{name: "author push then rebase", removal: failed, head: "C", marks: []marker{removalMarker("r2", "A")}},
		{name: "author push", removal: failed, head: "B", marks: []marker{removalMarker("r2", "A")}},
		// Without a record, whether the author already pushed is unknowable,
		// so the removal holds unless a push after it is recorded.
		{name: "push before the record existed", removal: failed, head: "B", wantEjected: true},
		{
			name: "latest of several removals recorded", removal: failed, head: "B",
			marks: []marker{removalMarker("r1", "A"), removalMarker("r2", "B")}, wantEjected: true,
		},
		{
			name: "several removals, latest settled", removal: failed, head: "C",
			marks: []marker{removalMarker("r1", "C"), removalMarker("r2", "B")},
		},
		{
			// An earlier removal recorded at the current head says nothing
			// about the latest one.
			name: "several removals, latest not yet recorded", removal: failed, head: "C",
			marks: []marker{removalMarker("r1", "C")}, wantEjected: true,
		},
		{name: "timed out", removal: removalBy("r2", "checks_timed_out", "github-merge-queue"), head: "A", wantEjected: true},
		{name: "merge conflict removal", removal: removalBy("r2", "merge_conflict", "github-merge-queue"), head: "A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := openPR{candidate: candidate{Head: tt.head}, Removal: &tt.removal}

			pr.settle(tt.marks)

			if pr.Ejected != tt.wantEjected || pr.Dequeued {
				t.Errorf("Ejected, Dequeued = %v, %v; want %v, false", pr.Ejected, pr.Dequeued, tt.wantEjected)
			}
		})
	}

	t.Run("hand dequeue", func(t *testing.T) {
		manual := removalBy("r3", "manual", "someone")
		held := openPR{candidate: candidate{Head: "A"}, Removal: &manual}
		settled := openPR{candidate: candidate{Head: "B"}, Removal: &manual}

		held.settle([]marker{removalMarker("r3", "A")})
		settled.settle([]marker{removalMarker("r3", "A")})

		if !held.Dequeued || held.Ejected {
			t.Errorf("unchanged head: Dequeued, Ejected = %v, %v; want true, false", held.Dequeued, held.Ejected)
		}
		if settled.Dequeued {
			t.Error("a new head must settle the hand dequeue")
		}
	})

	// katl-queue runs as the operator's own login, so a hand dequeue is a
	// manual removal by the viewer too.
	t.Run("hand dequeue by the viewer", func(t *testing.T) {
		manual := removalBy("r3", "manual", viewer)
		pr := openPR{candidate: candidate{Head: "A"}, Removal: &manual}

		pr.settle(nil)

		if !pr.Dequeued || pr.SelfDequeued || !pr.unrecorded() {
			t.Errorf("Dequeued, SelfDequeued, unrecorded = %v, %v, %v; want true, false, true", pr.Dequeued, pr.SelfDequeued, pr.unrecorded())
		}
	})

	t.Run("own dequeue", func(t *testing.T) {
		manual := removalBy("r3", "manual", viewer)
		held := openPR{candidate: candidate{Head: "A"}, Removal: &manual}
		settled := openPR{candidate: candidate{Head: "B"}, Removal: &manual}

		held.settle([]marker{dequeueMarker("A")})
		settled.settle([]marker{dequeueMarker("A"), removalMarker("r3", "A")})

		if !held.SelfDequeued || held.Dequeued || held.Ejected || held.unrecorded() {
			t.Errorf("unchanged head: %+v; want held, and neither recorded nor explained", held)
		}
		if settled.SelfDequeued || settled.Dequeued {
			t.Error("a new head must settle katl-queue's own dequeue")
		}
	})

	t.Run("dequeue marker of another head", func(t *testing.T) {
		manual := removalBy("r3", "manual", viewer)
		pr := openPR{candidate: candidate{Head: "B"}, Removal: &manual}

		pr.settle([]marker{dequeueMarker("A")})

		if pr.SelfDequeued || !pr.Dequeued {
			t.Errorf("SelfDequeued, Dequeued = %v, %v; want false, true: a marker covers one head", pr.SelfDequeued, pr.Dequeued)
		}
	})

	t.Run("optional report", func(t *testing.T) {
		pr := openPR{candidate: candidate{Head: "A"}}

		pr.settle([]marker{optionalMarker("A"), optionalMarker("old")})
		other := openPR{candidate: candidate{Head: "B"}}
		other.settle([]marker{optionalMarker("A")})

		if !pr.OptionalReported || other.OptionalReported {
			t.Errorf("OptionalReported = %v, %v; want true, false: a report covers one head", pr.OptionalReported, other.OptionalReported)
		}
	})
}

func TestDecideRemovals(t *testing.T) {
	f := newDecideFixture(t)
	failed := removalBy("r-failed", "failed_checks", "github-merge-queue")
	timedOut := removalBy("r-timeout", "checks_timed_out", "github-merge-queue")
	manual := removalBy("r-manual", "manual", "someone")
	own := removalBy("r-own", "manual", viewer)
	handByViewer := removalBy("r-hand", "manual", viewer)
	conflict := removalBy("r-conflict", "merge_conflict", "github-merge-queue")

	tests := []struct {
		name    string
		removal removalEvent
		rec     recording
		want    []string
	}{
		{"failed checks, first seen", failed, unrecorded, []string{"explain #1"}},
		{"failed checks, unchanged head", failed, recordedAtHead, nil},
		{"failed checks, new head", failed, recordedBefore, []string{"enqueue #1"}},
		{"timed out, first seen", timedOut, unrecorded, []string{"explain #1"}},
		// A hand dequeue is operator intent: katl-queue neither re-enqueues
		// it nor explains it, only records the head it was left at.
		{"hand dequeue, first seen", manual, unrecorded, []string{"record #1"}},
		{"hand dequeue, unchanged head", manual, recordedAtHead, nil},
		{"hand dequeue, new head", manual, recordedBefore, []string{"enqueue #1"}},
		{"hand dequeue by the viewer", handByViewer, unrecorded, []string{"record #1"}},
		{"hand dequeue by the viewer, recorded", handByViewer, recordedAtHead, nil},
		{"own dequeue", own, dequeuedBySelf, nil},
		{"other removal reason", conflict, unrecorded, []string{"enqueue #1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := world{PRs: []openPR{f.clean(1, "b", removedWith(tt.removal, tt.rec))}}

			got := f.decide(w)

			if !slices.Equal(actionSummary(got), tt.want) {
				t.Errorf("actions = %v, want %v", actionSummary(got), tt.want)
			}
		})
	}
}

// katl-queue's dequeue of an unmergeable entry is a manual removal that an
// unchanged head would otherwise re-enqueue, only to be marked unmergeable
// again.
func TestDecideDoesNotRepeatOwnDequeue(t *testing.T) {
	f := newDecideFixture(t)
	unmergeable := f.clean(1, "b", func(p *openPR) { p.InQueue = true })
	w := world{
		Entries: []queueEntryState{{Number: 1, ID: "id-1", State: "UNMERGEABLE"}},
		Queue:   []queuedPR{{Number: 1, Head: unmergeable.Head}},
		PRs:     []openPR{unmergeable},
	}

	first := f.decide(w)
	dequeue := first.Actions[0]
	fx := &recorder{}
	if err := apply(context.Background(), fx, first.Actions, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The next pass sees the pull request out of the queue, removed by this
	// login, with the marker the first pass left.
	afterwards := unmergeable
	afterwards.InQueue = false
	removedWith(removalBy("r-self", "manual", viewer), dequeuedBySelf)(&afterwards)
	second := f.decide(world{PRs: []openPR{afterwards}})

	if dequeue.Kind != actionDequeue || dequeue.Head != unmergeable.Head {
		t.Fatalf("first pass actions = %v, want a dequeue of the head it saw", actionSummary(first))
	}
	if len(fx.calls) != 2 || !strings.Contains(fx.calls[0], dequeueMarker(unmergeable.Head).String()) || fx.calls[1] != "dequeue id-1" {
		t.Errorf("calls = %v, want the marker comment before the dequeue", fx.calls)
	}
	if got := actionSummary(second); len(got) != 0 {
		t.Errorf("second pass actions = %v, want none for an unchanged head", got)
	}
}

func TestDecideOptionalChecks(t *testing.T) {
	f := newDecideFixture(t)
	optional := func(p *openPR) { p.OptionalFailed = []string{"lint"} }

	t.Run("reported once, not held", func(t *testing.T) {
		w := world{PRs: []openPR{
			f.clean(1, "b", optional),
			f.clean(2, "b2", func(p *openPR) { optional(p); p.OptionalReported = true }),
		}}

		got := f.decide(w)

		if want := []string{"report #1", "enqueue #1", "enqueue #2"}; !slices.Equal(actionSummary(got), want) {
			t.Errorf("actions = %v, want %v", actionSummary(got), want)
		}
		if len(got.Plan.Held) != 0 {
			t.Errorf("held = %+v; a failing optional check does not hold a pull request", got.Plan.Held)
		}
	})

	t.Run("kept auto-merge", func(t *testing.T) {
		w := world{PRs: []openPR{
			f.clean(1, "b", func(p *openPR) { optional(p); p.AutoMerge, p.OptionalReported = true, true }),
			f.clean(2, "b2", func(p *openPR) { p.Deps = []int{1} }),
		}}

		got := f.decide(w)

		// #1 is held only because #2 depends on it; GitHub would still queue
		// it, so auto-merge stays on.
		if len(got.Actions) != 0 {
			t.Errorf("actions = %v, want none: auto-merge is never disabled for an optional failure", actionSummary(got))
		}
		if len(got.Plan.Held) != 2 {
			t.Errorf("held = %+v, want #1 for its dependent and #2 behind it", got.Plan.Held)
		}
	})
}

func TestPlanOptionalChecksHoldOnlyDependedOn(t *testing.T) {
	f := newPlanFixture(t)
	failing := f.pr(1, "a", 0)
	failing.OptionalFailed = []string{"lint"}
	dependent := f.pr(2, "b", 1)
	dependent.Deps = []int{1}
	alone := f.pr(3, "c", 2)
	alone.OptionalFailed = []string{"lint"}

	p := f.plan(planInput{Candidates: []candidate{failing, dependent, alone}})

	f.expect(p, []int{3}, heldWant{1, 0, nil}, heldWant{2, 1, nil})
	if !p.Held[0].Advisory {
		t.Error("a hold for an optional failure must be advisory")
	}
}

func TestPlanHoldsDequeued(t *testing.T) {
	f := newPlanFixture(t)
	dequeued := f.pr(1, "a", 0)
	dequeued.Dequeued = true
	fine := f.pr(2, "b", 1)

	p := f.plan(planInput{Candidates: []candidate{dequeued, fine}})

	f.expect(p, []int{2}, heldWant{1, 0, nil})
}

func TestDecideKeepsAutoMergeForRebaseHold(t *testing.T) {
	f := newDecideFixture(t)
	w := world{PRs: []openPR{
		f.stale(1, func(p *openPR) { p.AutoMerge = true }),
		f.clean(2, "b2", func(p *openPR) { p.AutoMerge, p.ChecksFailed = true, true }),
		f.clean(3, "b3", func(p *openPR) { p.AutoMerge = true }),
		f.clean(4, "b4", func(p *openPR) {
			removedWith(removalBy("r", "manual", "someone"), recordedAtHead)(p)
			p.AutoMerge = true
		}),
	}}

	got := f.decide(w)

	// #1 waits only for a rebase and #3 is ready, so both keep auto-merge. A
	// required failure and a hand dequeue do not.
	if want := []string{"disable-auto-merge #2", "disable-auto-merge #4", "rebase #1"}; !slices.Equal(actionSummary(got), want) {
		t.Errorf("actions = %v, want %v", actionSummary(got), want)
	}
}

func TestCheckFailures(t *testing.T) {
	raw := []byte(`{"data": {"repository": {
	  "pr1": {"commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
	    {"__typename": "CheckRun", "name": "CI", "conclusion": "FAILURE", "isRequired": true},
	    {"__typename": "CheckRun", "name": "lint", "conclusion": "TIMED_OUT", "isRequired": false},
	    {"__typename": "CheckRun", "name": "docs", "conclusion": "SUCCESS", "isRequired": false},
	    {"__typename": "CheckRun", "name": "slow", "conclusion": null, "isRequired": true},
	    {"__typename": "CheckRun", "name": "cancelled", "conclusion": "CANCELLED", "isRequired": true},
	    {"__typename": "StatusContext", "context": "legacy", "state": "ERROR", "isRequired": false},
	    {"__typename": "StatusContext", "context": "ok", "state": "SUCCESS", "isRequired": true}
	  ]}}}}]}},
	  "pr2": {"commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}}
	}}}`)

	got, err := decodeCheckFailures(raw, []int{1, 2})

	if err != nil {
		t.Fatal(err)
	}
	if want := (checkFailures{Required: []string{"CI"}, Optional: []string{"lint", "legacy"}}); !slices.Equal(got[1].Required, want.Required) || !slices.Equal(got[1].Optional, want.Optional) {
		t.Errorf("pr1 = %+v, want %+v", got[1], want)
	}
	if f := got[2]; len(f.Required)+len(f.Optional) != 0 {
		t.Errorf("pr2 = %+v, want no failures without a rollup", f)
	}
}

func TestMarkersNeedViewer(t *testing.T) {
	g := &gh{owner: "o", name: "n"}

	if _, err := g.markers(context.Background(), 1); err == nil {
		t.Error("markers read comments without knowing which user is katl-queue")
	}
}

func TestWorldRemovalWithoutMarker(t *testing.T) {
	const removedAt = "2026-11-25T01:52:33Z"
	tests := []struct {
		name        string
		pushes      string
		wantEjected bool
	}{
		{name: "force-push after removal", pushes: `{"__typename": "HeadRefForcePushedEvent", "createdAt": "2026-11-25T03:00:00Z"}`},
		{name: "commit made after removal", pushes: `{"__typename": "PullRequestCommit", "commit": {"pushedDate": null, "committedDate": "2026-11-25T03:00:00Z"}}`},
		{name: "commit pushed after removal", pushes: `{"__typename": "PullRequestCommit", "commit": {"pushedDate": "2026-11-25T03:00:00Z", "committedDate": "2026-11-24T00:00:00Z"}}`},
		{name: "force-push before removal", pushes: `{"__typename": "HeadRefForcePushedEvent", "createdAt": "2026-11-25T01:00:00Z"}`, wantEjected: true},
		{name: "commit made before removal, push time unknown", pushes: `{"__typename": "PullRequestCommit", "commit": {"pushedDate": null, "committedDate": "2026-11-24T00:00:00Z"}}`, wantEjected: true},
		{name: "no pushes", wantEjected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pr prResponse
			raw := `{"number": 535, "headRefOid": "4287c936", "baseRefName": "main",
			  "timelineItems": {"nodes": [{"id": "r1", "createdAt": "` + removedAt + `", "reason": "failed_checks", "actor": {"login": "github-merge-queue"}}]},
			  "pushes": {"nodes": [` + tt.pushes + `]}}`
			if err := json.Unmarshal([]byte(raw), &pr); err != nil {
				t.Fatal(err)
			}

			w := worldFrom(queueSnapshot{Viewer: viewer, PRs: []prResponse{pr}}, "")
			got := w.PRs[0]
			got.settle(nil)

			if got.Ejected != tt.wantEjected {
				t.Errorf("Ejected = %v, want %v", got.Ejected, tt.wantEjected)
			}
			if got.unrecorded() != tt.wantEjected {
				t.Errorf("unrecorded = %v, want %v", got.unrecorded(), tt.wantEjected)
			}
		})
	}
}

func TestCheckFailuresLatestRunPerName(t *testing.T) {
	raw := []byte(`{"data": {"repository": {
	  "pr1": {"commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
	    {"__typename": "CheckRun", "name": "CI", "conclusion": "FAILURE", "startedAt": "2026-11-25T00:22:00Z", "completedAt": "2026-11-25T00:30:00Z", "isRequired": true},
	    {"__typename": "CheckRun", "name": "CI", "conclusion": "SUCCESS", "startedAt": "2026-11-25T00:59:00Z", "completedAt": "2026-11-25T00:59:57Z", "isRequired": true},
	    {"__typename": "CheckRun", "name": "lint", "conclusion": "SUCCESS", "startedAt": "2026-11-25T00:10:00Z", "completedAt": "2026-11-25T00:11:00Z", "isRequired": false},
	    {"__typename": "CheckRun", "name": "lint", "conclusion": "FAILURE", "startedAt": "2026-11-25T00:50:00Z", "completedAt": "2026-11-25T00:51:00Z", "isRequired": false}
	  ]}}}}]}}
	}}}`)

	got, err := decodeCheckFailures(raw, []int{1})

	if err != nil {
		t.Fatal(err)
	}
	if len(got[1].Required) != 0 {
		t.Errorf("required failures = %v, want none: the latest CI run succeeded", got[1].Required)
	}
	if !slices.Equal(got[1].Optional, []string{"lint"}) {
		t.Errorf("optional failures = %v, want [lint]: the latest lint run failed", got[1].Optional)
	}
}
