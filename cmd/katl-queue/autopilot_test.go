package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/gittest"
)

// decideFixture is a main that moved past oldBase, so that a pull request
// built on oldBase and editing a conflicts with it.
type decideFixture struct {
	planFixture
	oldBase string
}

func newDecideFixture(t *testing.T) decideFixture {
	f := newPlanFixture(t)
	oldBase := f.base
	f.base = f.repo.commit(oldBase, map[string]string{"a": "main moved\n"})
	return decideFixture{planFixture: f, oldBase: oldBase}
}

// clean is a pull request that merges over main.
func (f decideFixture) clean(number int, file string, edit func(*openPR)) openPR {
	c := f.pr(number, file, number)
	c.Head = f.repo.commit(f.base, map[string]string{file: fmt.Sprintf("pr %d\n", number)})
	pr := openPR{candidate: c, HeadRef: fmt.Sprintf("branch-%d", number), BaseRef: "main"}
	if edit != nil {
		edit(&pr)
	}
	return pr
}

// stale is a pull request that conflicts with main.
func (f decideFixture) stale(number int, edit func(*openPR)) openPR {
	pr := f.clean(number, "a", edit)
	pr.Head = f.repo.commit(f.oldBase, map[string]string{"a": fmt.Sprintf("stale %d\n", number)})
	return pr
}

func (f decideFixture) decide(w world) decision {
	f.t.Helper()
	w.Viewer, w.Branch, w.Base = viewer, "main", f.base
	d, err := decide(context.Background(), f.objects, w)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func actionSummary(d decision) []string {
	var out []string
	for _, a := range d.Actions {
		out = append(out, fmt.Sprintf("%s #%d", a.Kind, a.Number))
	}
	return out
}

const viewer = "katl-queue-bot"

var checksRemoval = removalEvent{ID: "r-checks", CreatedAt: epoch.Add(time.Hour), Reason: "failed_checks"}

// recording is what katl-queue has recorded about a removal.
type recording int

const (
	unrecorded recording = iota
	recordedAtHead
	recordedBefore
	// dequeuedBySelf is a pull request katl-queue itself dequeued, which
	// left only its dequeue marker.
	dequeuedBySelf
)

// removedWith gives a pull request the removal and settles it against the
// marker katl-queue would have left, as the world reader does.
func removedWith(r removalEvent, rec recording) func(*openPR) {
	return func(p *openPR) {
		p.Removal = &r
		var marks []marker
		switch rec {
		case recordedAtHead:
			marks = []marker{removalMarker(r.ID, p.Head)}
		case recordedBefore:
			marks = []marker{removalMarker(r.ID, "0ld")}
		case dequeuedBySelf:
			marks = []marker{dequeueMarker(p.Head)}
		}
		p.settle(marks)
	}
}

func TestDecide(t *testing.T) {
	f := newDecideFixture(t)
	queued := func(number int) openPR {
		pr := f.clean(number, "c", func(p *openPR) { p.InQueue = true })
		pr.Head = f.pr(number, "c", 0).Head
		return pr
	}

	w := world{
		Entries: []queueEntryState{
			{Number: 20, ID: "id-20", State: "UNMERGEABLE"},
			{Number: 21, ID: "id-21", State: "AWAITING_CHECKS"},
		},
		Queue: []queuedPR{
			{Number: 20, Head: queued(20).Head},
			{Number: 21, Head: queued(21).Head},
		},
		PRs: []openPR{
			queued(20),
			queued(21),
			f.clean(1, "b", nil),
			f.stale(2, nil),
			f.stale(3, func(p *openPR) { p.NeedsLabel = true }),
			f.clean(4, "b2", func(p *openPR) { p.NeedsLabel = true }),
			f.clean(5, "b3", removedWith(checksRemoval, unrecorded)),
			f.clean(6, "b4", removedWith(checksRemoval, recordedAtHead)),
			f.clean(7, "b5", func(p *openPR) { p.BaseRef = "release/1.0" }),
			f.clean(8, "b6", func(p *openPR) { p.Draft = true }),
			f.clean(9, "b7", func(p *openPR) { p.HeadRef = "release/hotfix" }),
			f.clean(10, "b8", func(p *openPR) { p.Fork = true }),
			f.clean(11, "b9", func(p *openPR) { p.ChecksFailed = true }),
			f.clean(12, "b10", func(p *openPR) { p.AutoMerge = true }),
			// Conflicts with the queued #21, whose head edits c.
			f.clean(13, "c", nil),
		},
	}

	got := f.decide(w)

	want := []string{
		"dequeue #20", // the queue marked it unmergeable
		"explain #5",  // removal not yet reported
		"unlabel #4",  // needs-rebase, now clean
		"rebase #2",   // conflicts with main
		"enqueue #1",
		"enqueue #4",
	}
	if actions := actionSummary(got); !slices.Equal(actions, want) {
		t.Errorf("actions = %v, want %v", actions, want)
	}
	if got.Actions[0].ID != "id-20" {
		t.Errorf("dequeue ID = %q, want the entry's pull request ID", got.Actions[0].ID)
	}

	held := map[int]heldPR{}
	for _, h := range got.Plan.Held {
		held[h.Number] = h
	}
	for _, n := range []int{3, 5, 6, 11} {
		if _, ok := held[n]; !ok {
			t.Errorf("#%d is not held; checks, ejections and conflicts needing a person are never retried", n)
		}
	}
	if held[13].Blocker != 21 {
		t.Errorf("#13 held behind #%d, want #21", held[13].Blocker)
	}
	for _, n := range []int{7, 8, 9, 10} {
		if _, ok := held[n]; ok || slices.Contains(got.Plan.Enqueue, n) {
			t.Errorf("#%d is planned or held, but autopilot must leave it alone", n)
		}
	}
}

func TestDecideRebasesOnlyWhatItWouldEnqueue(t *testing.T) {
	f := newDecideFixture(t)
	w := world{
		PRs: []openPR{
			f.stale(1, nil),
			f.stale(2, func(p *openPR) { p.ChecksFailed = true }),
			f.stale(3, removedWith(checksRemoval, unrecorded)),
			f.stale(4, func(p *openPR) { p.Deps = []int{50} }),
			f.stale(5, func(p *openPR) { p.Deps = []int{6} }),
			f.stale(6, func(p *openPR) { p.Deps = []int{5} }),
			f.stale(7, func(p *openPR) { p.Deps = []int{1} }),
			f.stale(8, func(p *openPR) { p.External = []string{"other/repo#1"} }),
		},
		Refs: map[int]externalRef{50: {State: "OPEN"}},
	}

	got := f.decide(w)

	// #1 would be enqueued once it merges over main. The others are held for
	// a reason a rebase does not change, and pushing to them would only
	// disturb their author.
	if want := []string{"explain #3", "rebase #1"}; !slices.Equal(actionSummary(got), want) {
		t.Errorf("actions = %v, want %v", actionSummary(got), want)
	}
}

func TestDecideDisablesAutoMergeOnHeldPRs(t *testing.T) {
	f := newDecideFixture(t)
	w := world{
		PRs: []openPR{
			f.clean(1, "b", func(p *openPR) { p.AutoMerge, p.ChecksFailed = true, true }),
			f.clean(2, "b2", func(p *openPR) { p.AutoMerge, p.Deps = true, []int{1} }),
			f.clean(3, "b3", func(p *openPR) { p.ChecksFailed = true }),
			f.clean(4, "b4", func(p *openPR) { p.AutoMerge = true }),
		},
	}

	got := f.decide(w)

	// Auto-merge would put #1 and #2 in the queue, where they would hold up
	// everything behind them. #4 is ready, so it keeps it.
	if want := []string{"disable-auto-merge #1", "disable-auto-merge #2"}; !slices.Equal(actionSummary(got), want) {
		t.Errorf("actions = %v, want %v", actionSummary(got), want)
	}
}

func TestDecideLeavesUnmanagedEntriesAlone(t *testing.T) {
	f := newDecideFixture(t)
	w := world{
		Entries: []queueEntryState{{Number: 30, ID: "id-30", State: "UNMERGEABLE"}},
		Queue:   []queuedPR{{Number: 30, Head: f.pr(30, "c", 0).Head}},
	}

	got := f.decide(w)

	if len(got.Actions) != 0 {
		t.Errorf("actions = %v, want none for an entry outside the managed pull requests", actionSummary(got))
	}
}

func TestParseMarkers(t *testing.T) {
	text := "hello\n<!-- katl-queue:rebased abc123 -->\n  <!-- katl-queue:ejection 2026-01-01T01:00:00Z -->  \n" +
		"quoted <!-- katl-queue:rebased nope --> inline\n<!-- other:rebased x -->\n"

	got := parseMarkers(text)

	want := []marker{{"rebased", "abc123"}, {"ejection", "2026-01-01T01:00:00Z"}}
	if !slices.Equal(got, want) {
		t.Errorf("parseMarkers = %v, want %v", got, want)
	}
}

func TestWorldFrom(t *testing.T) {
	raw := `{"repository": {
	  "defaultBranchRef": {"name": "main", "target": {"oid": "base"}},
	  "mergeQueue": {"entries": {"nodes": [
	    {"state": "AWAITING_CHECKS", "pullRequest": {"id": "ID1", "number": 1, "headRefOid": "h1", "closingIssuesReferences": {"nodes": [{"number": 10}]}}}
	  ]}},
	  "pullRequests": {"nodes": [
	    {"id": "ID1", "number": 1, "headRefOid": "h1", "isInMergeQueue": true, "mergeQueueEntry": {"position": 1, "state": "AWAITING_CHECKS"},
	     "milestone": {"title": "M"}, "baseRefName": "main", "headRefName": "one"},
	    {"id": "ID2", "number": 2, "headRefOid": "h2", "milestone": {"title": "M"}, "baseRefName": "main", "headRefName": "two",
	     "autoMergeRequest": {"enabledAt": "2026-01-01T00:00:00Z"},
	     "timelineItems": {"nodes": [{"createdAt": "2026-01-01T00:00:00Z", "reason": "merged"}]}},
	    {"id": "ID3", "number": 3, "headRefOid": "h3", "milestone": {"title": "M"}, "baseRefName": "main", "headRefName": "three",
	     "timelineItems": {"nodes": [{"createdAt": "2026-01-01T00:00:00Z", "reason": "failed_checks"}]}},
	    {"id": "ID4", "number": 4, "headRefOid": "h4", "baseRefName": "main", "headRefName": "four"}
	  ]}}}`
	var resp struct {
		Repository struct {
			DefaultBranchRef struct {
				Name   string
				Target struct{ Oid string }
			}
			MergeQueue   struct{ Entries struct{ Nodes []queueEntry } }
			PullRequests struct{ Nodes []prResponse }
		}
	}
	if err := decodeGraphQL([]byte(`{"data": `+raw+`}`), &resp); err != nil {
		t.Fatal(err)
	}
	repo := resp.Repository
	snap := queueSnapshot{Branch: repo.DefaultBranchRef.Name, BaseOid: repo.DefaultBranchRef.Target.Oid, Entries: repo.MergeQueue.Entries.Nodes, PRs: repo.PullRequests.Nodes}

	w := worldFrom(snap, "M")

	if len(w.Queue) != 1 || !slices.Equal(w.Queue[0].Closes, []int{10}) || w.Entries[0].ID != "ID1" {
		t.Errorf("queue = %+v, entries = %+v", w.Queue, w.Entries)
	}
	var numbers []int
	for _, pr := range w.PRs {
		numbers = append(numbers, pr.Number)
	}
	if !slices.Equal(numbers, []int{1, 2, 3}) {
		t.Fatalf("pull requests = %v, want those in milestone M", numbers)
	}
	if !w.PRs[0].InQueue || w.PRs[1].InQueue {
		t.Error("InQueue does not follow the merge queue entry")
	}
	if !w.PRs[1].AutoMerge {
		t.Error("auto-merge request not read")
	}
	if w.PRs[1].Removal != nil {
		t.Errorf("the record GitHub writes when an entry merges is not a removal: %+v", w.PRs[1].Removal)
	}
	if w.PRs[2].Removal == nil || w.PRs[2].Removal.Reason != "failed_checks" {
		t.Errorf("removal = %+v", w.PRs[2].Removal)
	}
}

func TestApplyScope(t *testing.T) {
	tests := []struct {
		command, milestone string
		apply              bool
		wantErr            bool
	}{
		{"autopilot", "", true, true},
		{"autopilot", "v1", true, false},
		{"autopilot", "", false, false},
		{"plan", "", true, true},
		{"plan", "v1", true, false},
		{"plan", "", false, false},
	}
	for _, tt := range tests {
		err := applyScope(tt.command, tt.milestone, tt.apply)
		if (err != nil) != tt.wantErr {
			t.Errorf("applyScope(%q, %q, %v) = %v", tt.command, tt.milestone, tt.apply, err)
		}
	}
}

type recorder struct {
	calls         []string
	rebase        map[string]rebaseResult
	markers       []marker
	failEnqueueOf int
	failLabel     bool
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
func (r *recorder) Dequeue(_ context.Context, id string) error {
	r.record("dequeue %s", id)
	return nil
}
func (r *recorder) DisableAutoMerge(_ context.Context, n int) error {
	r.record("disable-auto %d", n)
	return nil
}
func (r *recorder) AddLabel(_ context.Context, n int, l string) error {
	r.record("label %d %s", n, l)
	if r.failLabel {
		return errors.New("label failed")
	}
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
func (r *recorder) Markers(_ context.Context, n int) ([]marker, error) { return r.markers, nil }
func (r *recorder) Why(_ context.Context, n int) (string, error) {
	return fmt.Sprintf("#%d removed", n), nil
}
func (r *recorder) Rebase(_ context.Context, branch, head string) (rebaseResult, error) {
	r.record("rebase %s", branch)
	return r.rebase[branch], nil
}

func TestApply(t *testing.T) {
	actions := []action{
		{Kind: actionDequeue, Number: 6, ID: "id-6", Head: "h6"},
		{Kind: actionDisable, Number: 7},
		{Kind: actionExplain, Number: 5, RemovalID: "r-5", Head: "h5"},
		{Kind: actionRecord, Number: 8, RemovalID: "r-8", Head: "h8"},
		{Kind: actionReport, Number: 9, Head: "h9", Checks: []string{"lint", "docs"}},
		{Kind: actionRebase, Number: 2, Branch: "clean", Head: "old"},
		{Kind: actionRebase, Number: 3, Branch: "conflicting", Head: "c0ffee"},
		{Kind: actionEnqueue, Number: 4},
		{Kind: actionEnqueue, Number: 1},
	}
	fx := &recorder{
		rebase:        map[string]rebaseResult{"clean": {Head: "new1"}, "conflicting": {Conflicts: []string{"a", "b"}}},
		failEnqueueOf: 4,
	}

	var out strings.Builder
	err := apply(context.Background(), fx, actions, &out)

	for i, call := range fx.calls {
		// The comment names every conflicted file.
		if strings.HasPrefix(call, "comment 3 ") && strings.Contains(call, "`a`") && strings.Contains(call, "`b`") {
			fx.calls[i] = strings.SplitN(call, " | ", 2)[0]
		}
		// The rebase comment explains itself in prose.
		if strings.HasPrefix(call, "comment 2 Rebased") {
			fx.calls[i] = "comment 2 Rebased"
		}
		// The report names the failing checks.
		if strings.HasPrefix(call, "comment 9 ") {
			if !strings.Contains(call, "- lint") || !strings.Contains(call, "- docs") {
				t.Errorf("report comment = %q, want it to list the failing checks", call)
			}
			fx.calls[i] = strings.SplitN(call, " | ", 2)[0]
		}
		if strings.HasPrefix(call, "comment 8 ") || strings.HasPrefix(call, "comment 6 ") {
			fx.calls[i] = strings.SplitN(call, " | ", 2)[0]
		}
	}
	want := []string{
		"comment 6 <!-- katl-queue:dequeue h6 -->",
		"dequeue id-6",
		"disable-auto 7",
		"comment 5 <!-- katl-queue:removal r-5@h5 --> | ``` | #5 removed | ```",
		"comment 8 <!-- katl-queue:removal r-8@h8 -->",
		"comment 9 <!-- katl-queue:optional h9 -->",
		"rebase clean",
		"comment 2 Rebased",
		"rebase conflicting",
		"label 3 needs-rebase",
		"comment 3 <!-- katl-queue:needs-rebase c0ffee -->",
		"enqueue 4",
		"enqueue 1",
	}
	if !slices.Equal(fx.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fx.calls, "\n"), strings.Join(want, "\n"))
	}
	if err == nil || !strings.Contains(err.Error(), "#4") {
		t.Errorf("apply error = %v, want the failed enqueue of #4 reported without stopping #1", err)
	}
}

func TestApplyConflictComment(t *testing.T) {
	conflicting := action{Kind: actionRebase, Number: 3, Branch: "conflicting", Head: "c0ffee"}
	result := map[string]rebaseResult{"conflicting": {Conflicts: []string{"a"}}}
	commentsOn := func(fx *recorder) int {
		n := 0
		for _, call := range fx.calls {
			if strings.HasPrefix(call, "comment ") {
				n++
			}
		}
		return n
	}

	t.Run("label failed", func(t *testing.T) {
		fx := &recorder{rebase: result, failLabel: true}

		err := apply(context.Background(), fx, []action{conflicting}, &strings.Builder{})

		if err == nil || commentsOn(fx) != 0 {
			t.Errorf("err = %v, calls = %v; a comment promising the label's removal needs the label", err, fx.calls)
		}
	})

	t.Run("already explained for this head", func(t *testing.T) {
		fx := &recorder{rebase: result, markers: []marker{{"needs-rebase", "c0ffee"}}}

		err := apply(context.Background(), fx, []action{conflicting}, &strings.Builder{})

		if err != nil || commentsOn(fx) != 0 {
			t.Errorf("err = %v, calls = %v; the conflict was already explained", err, fx.calls)
		}
	})

	t.Run("head changed since", func(t *testing.T) {
		fx := &recorder{rebase: result, markers: []marker{{"needs-rebase", "old"}}}

		err := apply(context.Background(), fx, []action{conflicting}, &strings.Builder{})

		if err != nil || commentsOn(fx) != 1 {
			t.Errorf("err = %v, calls = %v; a new head's conflict is new information", err, fx.calls)
		}
	})
}

func TestRebaseEnv(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/x", "GH_TOKEN=t", "LC_ALL=C", "XDG_CONFIG_HOME=/c",
		"GIT_DIR=/elsewhere", "GIT_WORK_TREE=/elsewhere", "GIT_INDEX_FILE=/i", "GIT_CONFIG_GLOBAL=/evil", "GIT_SSH_COMMAND=x",
		"AWS_SECRET_ACCESS_KEY=s", "EDITOR=vim",
	}

	got := rebaseEnv(environ, "Name", "name@example.com")

	has := func(entry string) bool { return slices.Contains(got, entry) }
	for _, entry := range []string{"PATH=/bin", "HOME=/home/x", "GH_TOKEN=t", "LC_ALL=C", "XDG_CONFIG_HOME=/c"} {
		if !has(entry) {
			t.Errorf("env lacks %s, which git or gh needs", entry)
		}
	}
	for _, entry := range got {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") && !slices.Contains([]string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"}, name) {
			t.Errorf("env passes %s", entry)
		}
		if name == "AWS_SECRET_ACCESS_KEY" || name == "EDITOR" {
			t.Errorf("env passes %s, which the rebase does not need", entry)
		}
	}
	// The last value of a name wins.
	last := map[string]string{}
	for _, entry := range got {
		name, value, _ := strings.Cut(entry, "=")
		last[name] = value
	}
	if last["GIT_CONFIG_GLOBAL"] != "/dev/null" || last["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Errorf("configuration = %q, %q; want global config ignored", last["GIT_CONFIG_GLOBAL"], last["GIT_CONFIG_NOSYSTEM"])
	}
	if last["GIT_CONFIG_COUNT"] != "2" || last["GIT_CONFIG_KEY_0"] != "gc.auto" || last["GIT_CONFIG_VALUE_0"] != "0" || last["GIT_CONFIG_KEY_1"] != "maintenance.auto" || last["GIT_CONFIG_VALUE_1"] != "false" {
		t.Errorf("env %q does not disable background maintenance, which could outlive the throwaway repository", got)
	}
	if last["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q; a credential failure must fail the rebase, not wait for input", last["GIT_TERMINAL_PROMPT"])
	}
	if last["GIT_COMMITTER_NAME"] != "Name" || last["GIT_COMMITTER_EMAIL"] != "name@example.com" {
		t.Errorf("identity = %q, %q", last["GIT_COMMITTER_NAME"], last["GIT_COMMITTER_EMAIL"])
	}
}

func TestRebaser(t *testing.T) {
	f := newPlanFixture(t)
	remote := t.TempDir()
	f.repo.git("init", "--quiet", "--bare", remote)
	moved := f.repo.commit(f.base, map[string]string{"a": "main moved\n"})

	// The operator's environment is hostile: it points git at another
	// repository and a global configuration that would sign every commit
	// with a program that fails.
	other := t.TempDir()
	f.repo.git("init", "--quiet", "--bare", other)
	hostile := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(hostile, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(gittest.Env(os.Environ(), f.repo.dir), "HOME="+t.TempDir(), "GIT_DIR="+other, "GIT_WORK_TREE="+other, "GIT_CONFIG_GLOBAL="+hostile)

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
	rebase := func(branch, head string) (rebaseResult, error) {
		return rebaser{local: f.repo.dir, remote: remote, env: env}.rebase(context.Background(), branch, head, moved)
	}

	t.Run("clean", func(t *testing.T) {
		head := f.repo.commit(f.base, map[string]string{"b": "pr\n"})
		push(head, "clean")

		res, err := rebase("clean", head)
		if err != nil || res.Conflicts != nil {
			t.Fatalf("rebase = %+v, %v", res, err)
		}

		if got := remoteHead("clean"); got != res.Head {
			t.Errorf("remote branch = %s, reported head %s", got, res.Head)
		}
		if parent := strings.TrimSpace(f.repo.git("--git-dir", remote, "rev-parse", "refs/heads/clean^")); parent != moved {
			t.Errorf("pushed branch is on %s, want the moved base %s", parent, moved)
		}
		if got := remoteFile("clean", "a") + remoteFile("clean", "b"); got != "main moved\npr\n" {
			t.Errorf("pushed tree files = %q", got)
		}
		if out, err := exec.Command("git", "--git-dir", other, "for-each-ref").Output(); err != nil || len(out) != 0 {
			t.Errorf("the repository GIT_DIR named was written to: %q, %v", out, err)
		}
	})

	t.Run("conflicting", func(t *testing.T) {
		head := f.repo.commit(f.base, map[string]string{"a": "pr\n"})
		push(head, "conflicting")

		res, err := rebase("conflicting", head)

		if err != nil || !slices.Equal(res.Conflicts, []string{"a"}) {
			t.Fatalf("rebase = %+v, %v; want conflict in a", res, err)
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

		// The lease names the head the decision was made from, so the
		// push is refused as stale; a failure of any other step would
		// leave the lease untested.
		if err == nil || !strings.Contains(err.Error(), "stale info") {
			t.Errorf("rebase error = %v, want the push refused as stale info", err)
		}
		if got := remoteHead("raced"); got != newer {
			t.Errorf("remote branch = %s, want the author's %s", got, newer)
		}
	})
}
