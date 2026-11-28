package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
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

func TestMergeMilestoneResumesAfterLostCommentResponse(t *testing.T) {
	f := newMilestoneFixture(t)
	f.loseCommentResponse = true
	// An open plan issue moves with the rest of the open work. Its link in
	// the source milestone still identifies it on the retry.
	f.items[44].state = "open"

	_, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err == nil || !strings.Contains(err.Error(), "merge-milestone --apply 2026.12.9 2026.12.10 to resume") {
		t.Fatalf("apply = %v, want resume instructions", err)
	}
	if f.sourceState != "open" || f.items[44].milestone != 21 || len(f.comments) != 1 {
		t.Fatalf("source = %s, plan milestone = %d, comments = %d", f.sourceState, f.items[44].milestone, len(f.comments))
	}

	out, err := f.run("--apply", "2026.12.9", "2026.12.10")
	if err != nil || out != "Closed v2026.12.9.\n" || len(f.comments) != 1 || f.writes != 5 {
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
		t: t, items: items, sourceState: "open", targetState: "open",
		description: "Scope (#947). [Plan](https://github.com/example/katl/issues/44)",
	}
}

func (f *milestoneFixture) run(args ...string) (string, error) {
	r := &reader{server: "https://github.com", repository: "example/katl", read: f.read}
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
		for _, number := range []int{44, 45, 46, 47, 48} {
			item := f.items[number]
			if item.milestone == 17 && item.state == "open" {
				pages = append(pages, []map[string]any{item.response()})
			}
		}
		return json.Marshal(pages)
	default:
		f.t.Fatalf("unexpected read %s", endpoint)
		return nil, nil
	}
}

func (f *milestoneFixture) write(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "gh" || len(args) != 6 || !slices.Equal(args[:2], []string{"api", "--method"}) {
		f.t.Fatalf("unexpected command %s %v", name, args)
	}
	f.writes++
	method, endpoint, kind, field := args[2], args[3], args[4], args[5]
	switch {
	case method == "PATCH" && kind == "-F" && field == "milestone=21":
		for _, item := range f.items {
			if endpoint == fmt.Sprintf("repos/example/katl/issues/%d", item.number) && item.state == "open" && item.milestone == 17 {
				item.milestone = 21
				return nil, nil
			}
		}
	case method == "POST" && endpoint == "repos/example/katl/issues/44/comments" && kind == "-f":
		body, ok := strings.CutPrefix(field, "body=")
		if !ok || body == "" {
			f.t.Fatalf("missing comment body: %q", field)
		}
		f.comments = append(f.comments, body)
		if f.loseCommentResponse {
			f.loseCommentResponse = false
			return nil, errors.New("response lost after posting")
		}
		return nil, nil
	case method == "PATCH" && endpoint == "repos/example/katl/milestones/17" && kind == "-f" && field == "state=closed":
		if len(f.comments) != 1 {
			f.t.Fatalf("closed before the single plan comment: %v", f.comments)
		}
		f.sourceState = "closed"
		return nil, nil
	}
	f.t.Fatalf("unexpected GitHub write %v", args)
	return nil, nil
}
