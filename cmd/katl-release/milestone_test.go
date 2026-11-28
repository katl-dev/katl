package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestMergeMilestone(t *testing.T) {
	f := newMilestoneFixture(t)

	out, err := f.run("2026.12.9", "2026.12.10")
	wantPreview := "Would move issue #45 to v2026.12.10.\n" +
		"Would move PR #46 to v2026.12.10.\n" +
		"Would comment on plan issue #44.\nWould close v2026.12.9.\n"
	if err != nil || out != wantPreview || f.writes != 0 {
		t.Fatalf("preview = %q, %v, %d writes", out, err, f.writes)
	}

	out, err = f.run("--apply", "2026.12.9", "2026.12.10")
	wantApply := "Moved issue #45 to v2026.12.10.\n" +
		"Moved PR #46 to v2026.12.10.\n" +
		"Commented on plan issue #44.\nClosed v2026.12.9.\n"
	if err != nil || out != wantApply {
		t.Fatalf("apply = %q, %v", out, err)
	}
	for number, want := range map[int]int{44: 17, 45: 21, 46: 21, 47: 17, 48: 17} {
		item := f.items[number]
		if item.milestone != want || !slices.Equal(item.labels, []string{"keep", "area: release"}) {
			t.Errorf("#%d = %+v, want milestone %d and original labels", number, item, want)
		}
	}
	for _, number := range []int{45, 46} {
		if got := f.moves[number]; len(got) != 1 || !slices.Equal(got[0], []string{"-F", "milestone=21"}) {
			t.Errorf("#%d moves = %q, want one PATCH of only the milestone", number, got)
		}
	}
	if f.sourceState != "closed" || len(f.comments) != 1 || !strings.Contains(f.comments[0], "(https://github.com/example/katl/milestone/21)") {
		t.Fatalf("source = %s, comments = %v", f.sourceState, f.comments)
	}

	out, err = f.run("--apply", "2026.12.9", "2026.12.10")
	if err != nil || out != "v2026.12.9 is already merged into v2026.12.10 (plan issue #44).\n" || f.writes != 4 {
		t.Fatalf("repeat = %q, %v, %d total writes", out, err, f.writes)
	}
}

func TestMergeMilestoneRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*milestoneFixture)
	}{
		{"missing following milestone", "expected one following milestone named v2026.12.10", func(f *milestoneFixture) { f.targetState = "" }},
		{"closed following milestone", "following milestone v2026.12.10 must be open", func(f *milestoneFixture) { f.targetState = "closed" }},
		{"published", "v2026.12.9 is already published", func(f *milestoneFixture) { f.published = true }},
		{"missing plan link", "link the plan issue", func(f *milestoneFixture) { f.description = "" }},
		{"open plan issue", `issue #44 "plan: v2026.12.9 scope and priorities"; close each as not planned, then rerun`, func(f *milestoneFixture) { f.items[44].state = "open" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMilestoneFixture(t)
			tc.change(f)

			_, err := f.run("--apply", "2026.12.9", "2026.12.10")
			if err == nil || !strings.Contains(err.Error(), tc.want) || f.writes != 0 {
				t.Fatalf("apply = %v, %d writes, want %q and no writes", err, f.writes, tc.want)
			}
		})
	}
}

func TestMergeMilestoneResumesAfterFailedMove(t *testing.T) {
	f := newMilestoneFixture(t)
	f.moveFailures = map[int][]error{46: {ghExit("gh: Validation Failed (HTTP 422)")}}

	_, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err == nil || !strings.Contains(err.Error(), "move PR #46") || !strings.Contains(err.Error(), "merge-milestone --apply 2026.12.9 2026.12.10 to resume") {
		t.Fatalf("apply = %v, want the failed move and resume instructions", err)
	}
	if f.sourceState != "open" || len(f.comments) != 0 || f.items[45].milestone != 21 || f.items[46].milestone != 17 {
		t.Fatalf("source = %s, comments = %d, milestones #45 = %d, #46 = %d", f.sourceState, len(f.comments), f.items[45].milestone, f.items[46].milestone)
	}

	out, err := f.run("--apply", "2026.12.9", "2026.12.10")
	want := "Moved PR #46 to v2026.12.10.\nCommented on plan issue #44.\nClosed v2026.12.9.\n"
	if err != nil || out != want || len(f.moves[45]) != 1 || len(f.comments) != 1 || f.sourceState != "closed" {
		t.Fatalf("resume = %q, %v, #45 moves = %d, comments = %d, source = %s", out, err, len(f.moves[45]), len(f.comments), f.sourceState)
	}
}

func TestMergeMilestoneRetriesTransientMove(t *testing.T) {
	f := newMilestoneFixture(t)
	f.moveFailures = map[int][]error{45: {ghExit("gh: Bad Gateway (HTTP 502)")}}

	_, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err != nil || len(f.moves[45]) != 2 || f.items[45].milestone != 21 || f.sourceState != "closed" {
		t.Fatalf("apply = %v, #45 moves = %d, milestone = %d, source = %s", err, len(f.moves[45]), f.items[45].milestone, f.sourceState)
	}
}

func TestMergeMilestoneResumesAfterLostCommentResponse(t *testing.T) {
	f := newMilestoneFixture(t)
	// A transient failure shows that the comment is not retried in-process.
	f.loseCommentResponse = true

	_, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err == nil || !strings.Contains(err.Error(), "merge-milestone --apply 2026.12.9 2026.12.10 to resume") {
		t.Fatalf("apply = %v, want resume instructions", err)
	}
	if f.sourceState != "open" || len(f.comments) != 1 {
		t.Fatalf("source = %s, comments = %d", f.sourceState, len(f.comments))
	}

	out, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err != nil || out != "Closed v2026.12.9.\n" || len(f.comments) != 1 || f.writes != 4 {
		t.Fatalf("resume = %q, %v, %d comments, %d total writes", out, err, len(f.comments), f.writes)
	}
}

type milestoneFixtureItem struct {
	number, milestone int
	state, title      string
	pr                bool
	labels            []string
}

func (i *milestoneFixtureItem) response() map[string]any {
	item := map[string]any{"number": i.number, "state": i.state, "title": i.title, "labels": i.labels}
	if i.pr {
		item["pull_request"] = map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/example/katl/pulls/%d", i.number)}
	}
	return item
}

type milestoneFixture struct {
	t                                     *testing.T
	items                                 map[int]*milestoneFixtureItem
	description, sourceState, targetState string
	published, loseCommentResponse        bool
	writes                                int
	comments                              []string
	// moves records the fields of every issue PATCH attempt.
	moves map[int][][]string
	// moveFailures holds the errors that the next moves of an issue return.
	moveFailures map[int][]error
	// staleReadbacks is how many listings after the first move still show
	// the moved items in the source milestone.
	staleReadbacks int
	readbacks      int
	// addDuringMerge opens issue #49 in the source milestone once the moves
	// are done.
	addDuringMerge bool
}

func newMilestoneFixture(t *testing.T) *milestoneFixture {
	items := map[int]*milestoneFixtureItem{
		44: {number: 44, state: "closed", title: "plan: v2026.12.9 scope and priorities"},
		45: {number: 45, state: "open"},
		46: {number: 46, state: "open", pr: true},
		47: {number: 47, state: "closed"},
		48: {number: 48, state: "closed", pr: true},
	}
	for _, item := range items {
		item.milestone = 17
		item.labels = []string{"keep", "area: release"}
	}
	return &milestoneFixture{
		t: t, items: items, sourceState: "open", targetState: "open", moves: map[int][][]string{},
		description: "Scope (#947). [Plan](https://github.com/example/katl/issues/44)",
	}
}

func (f *milestoneFixture) run(args ...string) (string, error) {
	r := &reader{server: "https://github.com", repository: "example/katl", read: f.read, delay: time.Millisecond}
	var stdout, stderr bytes.Buffer
	err := mergeMilestone(context.Background(), args, &stdout, &stderr, r, f.write)
	return stdout.String(), err
}

func (f *milestoneFixture) read(_ context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
	path := strings.TrimPrefix(endpoint, "repos/example/katl/")
	if accept != "" || paginate != (path != "issues/44") {
		f.t.Fatalf("unexpected read options for %s, paginate=%v accept=%q", endpoint, paginate, accept)
	}
	switch path {
	case "releases?per_page=100":
		body := `[[{"id":10,"tag_name":"v2026.12.9-rc.1","prerelease":true}],[{"id":11,"tag_name":"v2026.12.9","draft":true}]]`
		if f.published {
			body = `[[],[{"id":11,"tag_name":"v2026.12.9","html_url":"https://github.com/example/katl/releases/tag/v2026.12.9"}]]`
		}
		return []byte(body), nil
	case "milestones?state=all&per_page=100":
		source := map[string]any{"number": 17, "title": "v2026.12.9", "description": f.description, "state": f.sourceState}
		pages := [][]map[string]any{{source}}
		if f.targetState != "" {
			target := map[string]any{"number": 21, "title": "v2026.12.10", "state": f.targetState, "html_url": "https://github.com/example/katl/milestone/21"}
			pages = append(pages, []map[string]any{target})
		}
		return json.Marshal(pages)
	case "issues/44":
		return json.Marshal(f.items[44].response())
	case "issues/44/comments?per_page=100":
		comments := [][]map[string]string{{{"body": "Release scope discussion"}}}
		for _, comment := range f.comments {
			comments = append(comments, []map[string]string{{"body": comment}})
		}
		return json.Marshal(comments)
	case "issues?milestone=17&state=open&per_page=100":
		pages := [][]map[string]any{}
		stale := false
		if len(f.moves) > 0 {
			f.readbacks++
			stale = f.readbacks <= f.staleReadbacks
			if f.addDuringMerge && f.items[49] == nil {
				f.items[49] = &milestoneFixtureItem{number: 49, milestone: 17, state: "open"}
			}
		}
		for _, number := range slices.Sorted(maps.Keys(f.items)) {
			item := f.items[number]
			if (item.milestone == 17 || stale && f.moves[number] != nil) && item.state == "open" {
				pages = append(pages, []map[string]any{item.response()})
			}
		}
		return json.Marshal(pages)
	default:
		f.t.Fatalf("unexpected read %s", endpoint)
		return nil, nil
	}
}

// requireMoved fails the test if a write that must follow every move runs
// while open work remains in the source milestone.
func (f *milestoneFixture) requireMoved(write string) {
	for _, item := range f.items {
		if item.state == "open" && item.milestone == 17 {
			f.t.Fatalf("%s while #%d is still open in the source milestone", write, item.number)
		}
	}
}

func (f *milestoneFixture) write(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "gh" || len(args) < 4 || len(args)%2 != 0 || !slices.Equal(args[:2], []string{"api", "--method"}) {
		f.t.Fatalf("unexpected command %s %v", name, args)
	}
	f.writes++
	method, endpoint, fields := args[2], args[3], args[4:]
	if number, ok := strings.CutPrefix(endpoint, "repos/example/katl/issues/"); ok && method == "PATCH" {
		n, _ := strconv.Atoi(number)
		item := f.items[n]
		if item == nil || item.state != "open" {
			f.t.Fatalf("PATCH of an item that is not open: %s", endpoint)
		}
		f.moves[n] = append(f.moves[n], fields)
		if failures := f.moveFailures[n]; len(failures) > 0 {
			f.moveFailures[n] = failures[1:]
			return nil, failures[0]
		}
		// Apply the fields as GitHub does: labels sent with a PATCH replace
		// the item's labels.
		var labels []string
		for i := 0; i < len(fields); i += 2 {
			key, value, _ := strings.Cut(fields[i+1], "=")
			switch strings.TrimSuffix(key, "[]") {
			case "milestone":
				item.milestone, _ = strconv.Atoi(value)
			case "labels":
				labels = append(labels, value)
				item.labels = labels
			}
		}
		return nil, nil
	}
	if len(fields) != 2 {
		f.t.Fatalf("unexpected GitHub write %v", args)
	}
	kind, field := fields[0], fields[1]
	switch {
	case method == "POST" && endpoint == "repos/example/katl/issues/44/comments" && kind == "-f":
		f.requireMoved("comment")
		body, ok := strings.CutPrefix(field, "body=")
		if !ok || body == "" {
			f.t.Fatalf("missing comment body: %q", field)
		}
		f.comments = append(f.comments, body)
		if f.loseCommentResponse {
			f.loseCommentResponse = false
			return nil, ghExit("gh: Bad Gateway (HTTP 502)")
		}
		return nil, nil
	case method == "PATCH" && endpoint == "repos/example/katl/milestones/17" && kind == "-f" && field == "state=closed":
		f.requireMoved("close")
		if len(f.comments) != 1 {
			f.t.Fatalf("closed before the single plan comment: %v", f.comments)
		}
		f.sourceState = "closed"
		return nil, nil
	}
	f.t.Fatalf("unexpected GitHub write %v", args)
	return nil, nil
}

func TestMergeMilestoneWaitsForMoves(t *testing.T) {
	// GitHub can still list an item in the source milestone just after it
	// was moved. An item the merge did not move is refused at once.
	const never = 1 << 30
	for _, tc := range []struct {
		name      string
		stale     int
		add       bool
		want      string
		readbacks int
	}{
		{name: "moves show late", stale: 2, readbacks: 3},
		{name: "moves never show", stale: never, want: "v2026.12.9 still lists 2 moved items as open after " + readbackWait.String()},
		{name: "item added during merge", add: true, want: "v2026.12.9 still has 1 open items, including issue #49", readbacks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newMilestoneFixture(t)
				f.staleReadbacks, f.addDuringMerge = tc.stale, tc.add

				_, err := f.run("--apply", "2026.12.9", "2026.12.10")

				if tc.want == "" {
					if err != nil || f.sourceState != "closed" || f.readbacks != tc.readbacks {
						t.Fatalf("apply = %v, source = %s after %d listings", err, f.sourceState, f.readbacks)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) || f.sourceState != "open" || len(f.comments) != 0 {
					t.Fatalf("apply = %v, source = %s, %d comments; want %q", err, f.sourceState, len(f.comments), tc.want)
				}
				if tc.readbacks != 0 && f.readbacks != tc.readbacks {
					t.Fatalf("refused after %d listings; want %d", f.readbacks, tc.readbacks)
				}
			})
		})
	}
}
