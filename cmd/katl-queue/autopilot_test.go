package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	f := newPlanFixture(t)
	oldBase := f.base
	f.base = f.repo.commit(oldBase, map[string]string{"a": "main moved\n"})

	open := func(number int, file string, edit func(*openPR)) openPR {
		c := f.pr(number, file, number)
		c.Head = f.repo.commit(f.base, map[string]string{file: fmt.Sprintf("pr %d\n", number)})
		pr := openPR{candidate: c, HeadRef: fmt.Sprintf("branch-%d", number), BaseRef: "main"}
		if edit != nil {
			edit(&pr)
		}
		return pr
	}
	stale := func(number int, edit func(*openPR)) openPR {
		pr := open(number, "a", edit)
		pr.Head = f.repo.commit(oldBase, map[string]string{"a": fmt.Sprintf("stale %d\n", number)})
		return pr
	}
	removed := removalMark{At: epoch.Add(time.Hour), Reason: "CHECKS_FAILED"}

	w := world{
		Branch: "main",
		Base:   f.base,
		Entries: []queueEntryState{
			{Number: 20, State: "UNMERGEABLE"},
			{Number: 21, State: "AWAITING_CHECKS"},
		},
		Queue: []queuedPR{
			{Number: 20, Head: f.pr(20, "c", 0).Head},
			{Number: 21, Head: f.pr(21, "c", 0).Head},
		},
		PRs: []openPR{
			open(1, "b", nil),
			stale(2, nil),
			stale(3, func(p *openPR) { p.NeedsLabel = true }),
			open(4, "b2", func(p *openPR) { p.NeedsLabel = true }),
			open(5, "b3", func(p *openPR) { p.Ejected, p.Removal = true, &removed }),
			open(6, "b4", func(p *openPR) { p.Ejected, p.Removal, p.Reported = true, &removed, []string{"2026-01-01T01:00:00Z"} }),
			open(7, "b5", func(p *openPR) { p.BaseRef = "release/1.0" }),
			open(8, "b6", func(p *openPR) { p.Draft = true }),
			open(9, "b7", func(p *openPR) { p.HeadRef = "release/hotfix" }),
			open(10, "b8", func(p *openPR) { p.Fork = true }),
			open(11, "b9", func(p *openPR) { p.ChecksFailed = true }),
			open(12, "b10", func(p *openPR) { p.AutoMerge = true }),
			// Conflicts with the queued #21, whose head edits c.
			open(13, "c", nil),
		},
	}

	got, err := decide(context.Background(), f.objects, w)
	if err != nil {
		t.Fatal(err)
	}

	var actions []string
	for _, a := range got.Actions {
		actions = append(actions, fmt.Sprintf("%s #%d", a.Kind, a.Number))
	}
	want := []string{
		"dequeue #20", // the queue marked it unmergeable
		"explain #5",  // removal not yet reported
		"unlabel #4",  // needs-rebase, now clean
		"rebase #2",   // conflicts with main
		"enqueue #1",
		"enqueue #4",
	}
	if !slices.Equal(actions, want) {
		t.Errorf("actions = %v, want %v", actions, want)
	}

	held := map[int]heldPR{}
	for _, h := range got.Plan.Held {
		held[h.Number] = h
	}
	for _, n := range []int{5, 6, 11} {
		if _, ok := held[n]; !ok {
			t.Errorf("#%d is not held; checks and ejections are never retried", n)
		}
	}
	if held[13].Blocker != 21 {
		t.Errorf("#13 held behind #%d, want #21", held[13].Blocker)
	}
	for _, n := range []int{3, 7, 8, 9, 10} {
		if _, ok := held[n]; ok || slices.Contains(got.Plan.Enqueue, n) {
			t.Errorf("#%d is planned or held, but autopilot must leave it alone", n)
		}
	}
}

type recorder struct {
	calls         []string
	rebase        map[string][]string
	failEnqueueOf int
}

func (r *recorder) record(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recorder) Enqueue(_ context.Context, n int) error {
	r.record("enqueue %d", n)
	if n == r.failEnqueueOf {
		return errors.New("boom")
	}
	return nil
}
func (r *recorder) Dequeue(_ context.Context, n int) error { r.record("dequeue %d", n); return nil }
func (r *recorder) AddLabel(_ context.Context, n int, l string) error {
	r.record("label %d %s", n, l)
	return nil
}
func (r *recorder) RemoveLabel(_ context.Context, n int, l string) error {
	r.record("unlabel %d %s", n, l)
	return nil
}
func (r *recorder) Comment(_ context.Context, n int, body string) error {
	r.record("comment %d %s", n, strings.ReplaceAll(strings.TrimSpace(body), "\n", " | "))
	return nil
}
func (r *recorder) Why(_ context.Context, n int) (string, error) {
	return fmt.Sprintf("#%d removed", n), nil
}
func (r *recorder) Rebase(_ context.Context, branch, head string) ([]string, error) {
	r.record("rebase %s", branch)
	return r.rebase[branch], nil
}

func TestApply(t *testing.T) {
	removedAt := epoch.Add(time.Hour)
	actions := []action{
		{Kind: actionExplain, Number: 5, RemovedAt: removedAt},
		{Kind: actionRebase, Number: 2, Branch: "clean"},
		{Kind: actionRebase, Number: 3, Branch: "conflicting"},
		{Kind: actionEnqueue, Number: 4},
		{Kind: actionEnqueue, Number: 1},
	}
	fx := &recorder{rebase: map[string][]string{"conflicting": {"a", "b"}}, failEnqueueOf: 4}

	var out strings.Builder
	err := apply(context.Background(), fx, actions, &out)

	want := []string{
		"comment 5 <!-- katl-queue:ejection 2026-01-01T01:00:00Z --> | ``` | #5 removed | ```",
		"rebase clean",
		"rebase conflicting",
		"label 3 needs-rebase",
		"comment 3",
		"enqueue 4",
		"enqueue 1",
	}
	for i, call := range fx.calls {
		// The comment names every conflicted file.
		if strings.HasPrefix(call, "comment 3 ") && strings.Contains(call, "`a`") && strings.Contains(call, "`b`") {
			fx.calls[i] = "comment 3"
		}
	}
	if !slices.Equal(fx.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fx.calls, "\n"), strings.Join(want, "\n"))
	}
	if err == nil || !strings.Contains(err.Error(), "#4") {
		t.Errorf("apply error = %v, want the failed enqueue of #4 reported without stopping #1", err)
	}
}

func TestRebaser(t *testing.T) {
	f := newPlanFixture(t)
	remote := t.TempDir()
	f.repo.git("init", "--quiet", "--bare", remote)
	moved := f.repo.commit(f.base, map[string]string{"a": "main moved\n"})
	push := func(oid, branch string) { f.repo.git("push", "--quiet", "--force", remote, oid+":refs/heads/"+branch) }
	remoteHead := func(branch string) string {
		out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "refs/heads/"+branch).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	remoteFile := func(branch, file string) string {
		out, err := exec.Command("git", "--git-dir", remote, "show", "refs/heads/"+branch+":"+file).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	rebase := func(branch, head string) ([]string, error) {
		return rebaser{local: f.repo.dir, remote: remote, env: hermeticGitEnv(f.repo.dir)}.rebase(context.Background(), branch, head, moved)
	}

	t.Run("clean", func(t *testing.T) {
		head := f.repo.commit(f.base, map[string]string{"b": "pr\n"})
		push(head, "clean")

		conflicts, err := rebase("clean", head)
		if err != nil || conflicts != nil {
			t.Fatalf("rebase = %v, %v", conflicts, err)
		}

		if parent := strings.TrimSpace(f.repo.git("--git-dir", remote, "rev-parse", "refs/heads/clean^")); parent != moved {
			t.Errorf("pushed branch is on %s, want the moved base %s", parent, moved)
		}
		if got := remoteFile("clean", "a") + remoteFile("clean", "b"); got != "main moved\npr\n" {
			t.Errorf("pushed tree files = %q", got)
		}
	})

	t.Run("conflicting", func(t *testing.T) {
		head := f.repo.commit(f.base, map[string]string{"a": "pr\n"})
		push(head, "conflicting")

		conflicts, err := rebase("conflicting", head)

		if err != nil || !slices.Equal(conflicts, []string{"a"}) {
			t.Fatalf("rebase = %v, %v; want conflict in a", conflicts, err)
		}
		if got := remoteHead("conflicting"); got != head {
			t.Errorf("remote branch moved to %s, want it untouched at %s", got, head)
		}
	})

	t.Run("author pushed meanwhile", func(t *testing.T) {
		head := f.repo.commit(f.base, map[string]string{"b": "pr\n"})
		newer := f.repo.commit(head, map[string]string{"c": "author fix\n"})
		push(newer, "raced")

		_, err := rebase("raced", head)

		if err == nil {
			t.Error("rebase pushed over a branch that moved since it was read")
		}
		if got := remoteHead("raced"); got != newer {
			t.Errorf("remote branch = %s, want the author's %s", got, newer)
		}
	})
}
