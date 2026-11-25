package main

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/gittest"
)

// planFixture is a repository with a base commit over three files and a
// scratch object store for simulating over it.
type planFixture struct {
	t       *testing.T
	repo    fixtureRepo
	base    string
	objects *scratchObjects
}

func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	repo := fixtureRepo{t: t, dir: t.TempDir()}
	repo.git("init", "--quiet", "--initial-branch=main")
	repo.git("commit", "--quiet", "--allow-empty", "-m", "root")
	base := repo.commit(repo.git("rev-parse", "HEAD"), map[string]string{"a": "a\n", "b": "b\n", "c": "c\n"})
	objects, err := newScratchObjects(repo.dir, gittest.Env(os.Environ(), repo.dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	return planFixture{t: t, repo: repo, base: base, objects: objects}
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// pr makes a candidate that rewrites file, created age hours after the epoch.
func (f planFixture) pr(number int, file string, age int, labels ...string) candidate {
	return candidate{
		Number:  number,
		Head:    f.repo.commit(f.base, map[string]string{file: "change " + string(rune('A'+number)) + "\n"}),
		Created: epoch.Add(time.Duration(age) * time.Hour),
		Labels:  labels,
	}
}

func (f planFixture) plan(in planInput) plan {
	f.t.Helper()
	in.Base = f.base
	p, err := planQueue(context.Background(), f.objects, in)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

type heldWant struct {
	Number, Blocker int
	Files           []string
}

func heldSummary(p plan) []heldWant {
	var out []heldWant
	for _, h := range p.Held {
		out = append(out, heldWant{h.Number, h.Blocker, h.Files})
	}
	return out
}

func (f planFixture) expect(p plan, enqueue []int, held ...heldWant) {
	f.t.Helper()
	if !slices.Equal(p.Enqueue, enqueue) {
		f.t.Errorf("enqueue = %v, want %v", p.Enqueue, enqueue)
	}
	if !reflect.DeepEqual(heldSummary(p), held) && !(len(held) == 0 && len(p.Held) == 0) {
		f.t.Errorf("held = %+v, want %+v (reasons: %+v)", heldSummary(p), held, p.Held)
	}
}

func TestPlanConflictHoldsBehindBlocker(t *testing.T) {
	f := newPlanFixture(t)
	a := f.pr(1, "a", 0)
	b := f.pr(2, "a", 1)
	c := f.pr(3, "b", 2)

	// Listed out of order: the plan, not the input order, decides.
	p := f.plan(planInput{Candidates: []candidate{c, b, a}})

	f.expect(p, []int{1, 3}, heldWant{2, 1, []string{"a"}})
}

func TestPlanPriorityThenAge(t *testing.T) {
	f := newPlanFixture(t)
	plainOld := f.pr(10, "a", 0)
	blocker := f.pr(11, "a", 5, "release-blocker") // conflicts with plainOld, outranks it
	devExperience := f.pr(12, "b", 4, "dev-experience")
	plainYoung := f.pr(14, "c", 3)
	plainOlder := f.pr(13, "c", 1) // conflicts with plainYoung, older

	p := f.plan(planInput{Candidates: []candidate{plainOld, plainYoung, devExperience, blocker, plainOlder}})

	f.expect(p, []int{11, 12, 13}, heldWant{10, 11, []string{"a"}}, heldWant{14, 13, []string{"c"}})
}

func TestPlanHoldsBehindQueuedEntry(t *testing.T) {
	f := newPlanFixture(t)
	queued := f.pr(1, "a", 0)
	clash := f.pr(2, "a", 1)
	free := f.pr(3, "b", 2)

	p := f.plan(planInput{Queue: []queuedPR{{Number: 1, Head: queued.Head}}, Candidates: []candidate{clash, free}})

	f.expect(p, []int{3}, heldWant{2, 1, []string{"a"}})
}

func TestPlanHoldsConflictWithBase(t *testing.T) {
	f := newPlanFixture(t)
	stale := f.pr(1, "a", 0)
	f.base = f.repo.commit(f.base, map[string]string{"a": "main moved\n"})

	p := f.plan(planInput{Candidates: []candidate{stale}})

	f.expect(p, nil, heldWant{1, 0, []string{"a"}})
}

func TestPlanDependencies(t *testing.T) {
	f := newPlanFixture(t)
	dep := func(c candidate, deps ...int) candidate {
		c.Deps = deps
		return c
	}

	t.Run("diamond", func(t *testing.T) {
		// D is the highest priority and oldest, yet follows B and C, which
		// follow A.
		d := dep(f.pr(4, "c", 0, "release-blocker"), 2, 3)
		a := f.pr(1, "a", 3)
		b := dep(f.pr(2, "b", 2), 1)
		c := dep(f.pr(3, "b2", 1), 1)

		p := f.plan(planInput{Candidates: []candidate{d, c, b, a}})

		f.expect(p, []int{1}, heldWant{3, 1, nil}, heldWant{2, 1, nil}, heldWant{4, 2, nil})
	})

	t.Run("declared dependency without a textual conflict", func(t *testing.T) {
		first := f.pr(1, "a", 5)
		second := dep(f.pr(2, "b", 0, "release-blocker"), 1)

		p := f.plan(planInput{Candidates: []candidate{second, first}})

		f.expect(p, []int{1}, heldWant{2, 1, nil})
	})

	t.Run("dependency declared on a closed issue", func(t *testing.T) {
		// #20 is the issue #1 closes; #2's issue is blocked by it.
		first := f.pr(1, "a", 5)
		first.Closes = []int{20}
		second := dep(f.pr(2, "b", 0), 20)

		p := f.plan(planInput{Candidates: []candidate{second, first}})

		f.expect(p, []int{1}, heldWant{2, 1, nil})
	})

	t.Run("cycle", func(t *testing.T) {
		x := dep(f.pr(1, "a", 0), 2)
		y := dep(f.pr(2, "b", 1), 1)
		after := dep(f.pr(3, "c", 2), 1)
		free := f.pr(4, "c2", 3)

		p := f.plan(planInput{Candidates: []candidate{x, y, after, free}})

		f.expect(p, []int{4}, heldWant{1, 0, nil}, heldWant{2, 0, nil}, heldWant{3, 0, nil})
		if p.Held[0].Reason != "in a dependency cycle: #1, #2" || p.Held[2].Reason == p.Held[0].Reason {
			t.Errorf("cycle reasons = %q, %q", p.Held[0].Reason, p.Held[2].Reason)
		}
	})

	t.Run("dependency that cannot merge", func(t *testing.T) {
		refs := map[int]externalRef{
			90: {Pull: true, State: "CLOSED"},
			91: {Pull: true, State: "OPEN"},
			92: {Pull: true, State: "MERGED"},
			93: {State: "CLOSED"},
			94: {State: "OPEN"},
		}
		closedPull := dep(f.pr(1, "a", 0), 90)
		openPull := dep(f.pr(2, "b", 1), 91)
		mergedPull := dep(f.pr(3, "c", 2), 92)
		closedIssue := dep(f.pr(4, "a2", 3), 93)
		openIssue := dep(f.pr(5, "b2", 4), 94)
		unknown := dep(f.pr(6, "c2", 5), 95)

		p := f.plan(planInput{Refs: refs, Candidates: []candidate{closedPull, openPull, mergedPull, closedIssue, openIssue, unknown}})

		f.expect(p, []int{3, 4}, heldWant{1, 90, nil}, heldWant{2, 91, nil}, heldWant{5, 94, nil}, heldWant{6, 95, nil})
	})

	t.Run("dependency with auto-merge on that is not queued yet", func(t *testing.T) {
		pending := f.pr(1, "a", 0)
		pending.AutoMerge = true
		waiting := dep(f.pr(2, "b", 1), 1)

		p := f.plan(planInput{Candidates: []candidate{pending, waiting}})

		f.expect(p, []int{1}, heldWant{2, 1, nil})
	})

	t.Run("dependency in the queue", func(t *testing.T) {
		queued := f.pr(1, "a", 0)
		waiting := dep(f.pr(2, "b", 1), 1)
		viaIssue := dep(f.pr(3, "c", 2), 30)

		p := f.plan(planInput{
			Queue:      []queuedPR{{Number: 1, Head: queued.Head, Closes: []int{30}}},
			Candidates: []candidate{waiting, viaIssue},
		})

		f.expect(p, nil, heldWant{2, 1, nil}, heldWant{3, 1, nil})
	})

	t.Run("dependency that has merged", func(t *testing.T) {
		waiting := dep(f.pr(1, "a", 0), 90, 91)
		refs := map[int]externalRef{90: {Pull: true, State: "MERGED"}, 91: {State: "CLOSED"}}

		p := f.plan(planInput{Refs: refs, Candidates: []candidate{waiting}})

		f.expect(p, []int{1})
	})

	t.Run("dependency that is held", func(t *testing.T) {
		broken := f.pr(1, "a", 0)
		broken.ChecksFailed = true
		waiting := dep(f.pr(2, "b", 1), 1)

		p := f.plan(planInput{Candidates: []candidate{broken, waiting}})

		f.expect(p, nil, heldWant{1, 0, nil}, heldWant{2, 1, nil})
	})
}

func TestPlanNeverRetriesChecks(t *testing.T) {
	f := newPlanFixture(t)
	failed := f.pr(1, "a", 0)
	failed.ChecksFailed = true
	ejected := f.pr(2, "b", 1)
	ejected.Ejected = true
	fine := f.pr(3, "c", 2)

	p := f.plan(planInput{Candidates: []candidate{failed, ejected, fine}})

	f.expect(p, []int{3}, heldWant{1, 0, nil}, heldWant{2, 0, nil})
}

func TestPlanChecksFailedReason(t *testing.T) {
	f := newPlanFixture(t)
	failed := f.pr(1, "a", 0)
	failed.ChecksFailed = true

	p := f.plan(planInput{Candidates: []candidate{failed}})

	// The rollup covers every check, not only the required ones.
	if len(p.Held) != 1 || strings.Contains(p.Held[0].Reason, "required") {
		t.Errorf("held = %+v; the reason must not claim the failed checks are required", p.Held)
	}
}

func TestPlanHoldsExternalBlocker(t *testing.T) {
	f := newPlanFixture(t)
	blocked := f.pr(1, "a", 0)
	blocked.External = []string{"other/repo#1"}
	// A blocker in this repository that happens to share the number.
	local := f.pr(2, "b", 1)
	local.Deps = []int{1}
	refs := map[int]externalRef{1: {Pull: true, State: "MERGED"}}

	p := f.plan(planInput{Refs: refs, Candidates: []candidate{blocked, local}})

	f.expect(p, nil, heldWant{1, 0, nil}, heldWant{2, 1, nil})
	if !strings.Contains(p.Held[0].Reason, "other/repo#1") {
		t.Errorf("reason = %q, want it to name the other repository's blocker", p.Held[0].Reason)
	}
}

func TestPlanMarksOnlyBaseConflictsForRebase(t *testing.T) {
	f := newPlanFixture(t)
	stale := f.pr(1, "a", 0)
	stale2 := f.pr(2, "a", 1)
	queued := f.pr(3, "b", 2)
	behindQueue := f.pr(4, "b", 3)
	f.base = f.repo.commit(f.base, map[string]string{"a": "main moved\n"})

	p := f.plan(planInput{
		Queue:      []queuedPR{{Number: 3, Head: queued.Head}},
		Candidates: []candidate{stale, stale2, behindQueue},
	})

	rebase := map[int]bool{}
	for _, h := range p.Held {
		rebase[h.Number] = h.Rebase
	}
	if want := (map[int]bool{1: true, 2: true, 4: false}); !reflect.DeepEqual(rebase, want) {
		t.Errorf("rebase = %v, want %v: a rebase onto the base cannot resolve a conflict with a queued entry", rebase, want)
	}
}

func TestDeclaredDeps(t *testing.T) {
	body := "Closes #5\n\nDepends on #7, #8\n  depends on #9\nSee #10 and mention depends on #11 inline\n"
	want := []int{7, 8, 9}
	if got := declaredDeps(body); !slices.Equal(got, want) {
		t.Errorf("declaredDeps() = %v, want %v", got, want)
	}
}

func TestStartable(t *testing.T) {
	issues := []issueInfo{
		{Number: 1, Created: epoch.Add(3 * time.Hour)},
		{Number: 2, Created: epoch, Labels: []string{"dev-experience"}},
		{Number: 3, Created: epoch, Assigned: true},
		{Number: 4, Created: epoch, Blockers: []int{1}},
		{Number: 5, Created: epoch.Add(time.Hour), Blockers: []int{80}},
		{Number: 6, Created: epoch.Add(2 * time.Hour), Labels: []string{"release-blocker"}, Blockers: []int{80, 81}},
		{Number: 7, Created: epoch, Blockers: []int{82}},
		{Number: 8, Created: epoch, External: []string{"other/repo#3"}},
	}
	refs := map[int]externalRef{80: {State: "CLOSED"}, 81: {Pull: true, State: "MERGED"}, 1: {State: "OPEN"}}

	var got []int
	for _, issue := range startable(issues, refs) {
		got = append(got, issue.Number)
	}

	// #4 waits on open #1, #3 is assigned, #7's blocker is unknown, #8 is
	// blocked in another repository.
	if want := []int{6, 2, 5, 1}; !slices.Equal(got, want) {
		t.Errorf("startable = %v, want %v", got, want)
	}
}

func TestDeclaredHold(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"none", "Closes #5\nholding pattern\n", ""},
		{"plain", "Closes #5\n\nHold: awaiting the VM gate\n", "awaiting the VM gate"},
		{"case and indentation", "   hOLD:   needs a journey  \n", "needs a journey"},
		{"empty reason", "Hold:\nHold:   \n", ""},
		{"inline mention", "see Hold: later\n", ""},
		{"fenced", "```\nHold: example\n```\n~~~sh\nHold: another\n~~~\n", ""},
		{"after a fence", "```\nHold: example\n```\nHold: real\n", "real"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := declaredHold(tt.body); got != tt.want {
				t.Errorf("declaredHold() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanHoldsDeclaredHold(t *testing.T) {
	f := newPlanFixture(t)
	held := f.pr(1, "a", 0)
	held.Hold = "awaiting the VM gate"
	fine := f.pr(2, "b", 1)

	p := f.plan(planInput{Candidates: []candidate{held, fine}})

	f.expect(p, []int{2}, heldWant{1, 0, nil})
	if !strings.Contains(p.Held[0].Reason, "awaiting the VM gate") {
		t.Errorf("reason = %q, want it to give the declared reason", p.Held[0].Reason)
	}
}

func TestDecideDisablesAutoMergeForHold(t *testing.T) {
	f := newDecideFixture(t)
	w := world{PRs: []openPR{f.clean(1, "b", func(p *openPR) { p.AutoMerge, p.Hold = true, "journey pending" })}}

	got := f.decide(w)

	if want := []string{"disable-auto-merge #1"}; !slices.Equal(actionSummary(got), want) {
		t.Errorf("actions = %v, want %v", actionSummary(got), want)
	}
}
