package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

const (
	testReviewHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testReviewBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

const testApprovedFile = `{
  "version": 1,
  "pr": 950,
  "head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "base": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "verdict": "approved",
  "summary": "Ready to merge.",
  "findings": []
}`

type fakeReviewGitHub struct {
	targets  []reviewTarget
	previous []reviewrecord.Comment
	posted   []string
	member   bool
	ancestor string
}

func (g *fakeReviewGitHub) reviewTarget(_ context.Context, number int) (reviewTarget, error) {
	if number != 950 {
		return reviewTarget{}, fmt.Errorf("unexpected PR #%d", number)
	}
	target := g.targets[0]
	if len(g.targets) > 1 {
		g.targets = g.targets[1:]
	}
	return target, nil
}

func (g *fakeReviewGitHub) comments(_ context.Context, _ int) ([]reviewrecord.Comment, error) {
	return g.previous, nil
}

func (g *fakeReviewGitHub) mergeBase(_ context.Context, base, head string) (string, error) {
	if base != testReviewBase || head != testReviewHead {
		return "", fmt.Errorf("unexpected comparison from %s to %s", base, head)
	}
	if g.ancestor != "" {
		return g.ancestor, nil
	}
	return testReviewBase, nil
}

func (g *fakeReviewGitHub) comment(_ context.Context, _ int, body string) error {
	g.posted = append(g.posted, body)
	g.previous = append(g.previous, reviewrecord.Comment{
		ID: int64(len(g.previous) + 1), Member: g.member, Author: g.targets[0].viewer, Body: body,
	})
	return nil
}

func TestPostReviewRefusals(t *testing.T) {
	const finding = `[{"severity":"blocker","title":"Lost state","body":"Preserve the original state."}]`
	change := func(old, new string) string { return strings.Replace(testApprovedFile, old, new, 1) }
	open := reviewTarget{state: "OPEN", head: testReviewHead}
	for _, test := range []struct {
		name        string
		file        string
		targets     []reviewTarget
		notAncestor bool
		message     string
	}{
		{name: "unknown field", file: change(`"version": 1`, `"version": 1, "extra": true`), message: `unknown field "extra"`},
		{name: "unknown finding field", file: change("[]", strings.Replace(finding, `"severity":`, `"extra":true,"severity":`, 1)), message: `unknown field "extra"`},
		{name: "wrong version", file: change(`"version": 1`, `"version": 2`), message: "set version to 1"},
		{name: "trailing object", file: testApprovedFile + "{}", message: "keep exactly one"},
		{name: "short head", file: change(testReviewHead, "aaaaaaaaaaaa"), message: "set head to the full"},
		{name: "short base", file: change(testReviewBase, "bbbbbbbbbbbb"), message: "set base to the full"},
		{name: "wrong PR", file: change("950", "951"), message: "use a verdict for PR #950"},
		{name: "stale head", targets: []reviewTarget{{state: "OPEN", head: testReviewBase}}, message: "review its current head"},
		{name: "unrelated base", notAncestor: true, message: "use the reviewed ancestor"},
		{name: "blocker on approval", file: change("[]", finding), message: "resolve finding 1"},
		{name: "changes without findings", file: change("approved", "changes-requested"), message: "add at least one finding"},
		{name: "closed PR", targets: []reviewTarget{{state: "CLOSED", head: testReviewHead}}, message: "use an open PR"},
		{name: "merged PR", targets: []reviewTarget{{state: "MERGED", head: testReviewHead}}, message: "use an open PR"},
		{name: "head changes before post", targets: []reviewTarget{open, {state: "OPEN", head: testReviewBase}}, message: "review its current head"},
		{name: "closed before post", targets: []reviewTarget{open, {state: "CLOSED", head: testReviewHead}}, message: "use an open PR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.file == "" {
				test.file = testApprovedFile
			}
			if len(test.targets) == 0 {
				test.targets = []reviewTarget{open}
			}
			g := fakeReviewGitHub{targets: test.targets}
			if test.notAncestor {
				g.ancestor = "cccccccccccccccccccccccccccccccccccccccc"
			}

			err := postReview(t.Context(), &g, 950, strings.NewReader(test.file), io.Discard)

			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Errorf("error = %v, want %q", err, test.message)
			}
			if len(g.posted) != 0 {
				t.Errorf("refusal posted %d comments", len(g.posted))
			}
		})
	}
}

func TestPostReviewRepeat(t *testing.T) {
	for _, member := range []bool{true, false} {
		t.Run(fmt.Sprintf("member=%t", member), func(t *testing.T) {
			g := fakeReviewGitHub{
				targets: []reviewTarget{{state: "OPEN", head: testReviewHead, viewer: "poster"}},
				member:  member,
			}
			// Even an exact copy by an unrelated nonmember must not suppress
			// a real post.
			file, err := reviewrecord.Decode(strings.NewReader(testApprovedFile))
			if err != nil {
				t.Fatal(err)
			}
			g.previous = []reviewrecord.Comment{{ID: 1, Author: "unrelated", Body: file.Comment()}}
			for _, want := range []string{"Posted approved review", "Review already recorded"} {
				var out strings.Builder
				err := postReview(t.Context(), &g, 950, strings.NewReader(testApprovedFile), &out)
				if err != nil || !strings.Contains(out.String(), want) {
					t.Fatalf("post = %q, %v; want %q", out.String(), err, want)
				}
			}
			if len(g.posted) != 1 {
				t.Errorf("posted %d comments, want one", len(g.posted))
			}
		})
	}
}

func TestPostReviewSupersedesVerdict(t *testing.T) {
	g := fakeReviewGitHub{
		targets: []reviewTarget{{state: "OPEN", head: testReviewHead, viewer: "poster"}},
		member:  true,
	}
	requested := strings.Replace(testApprovedFile, "approved", "changes-requested", 1)
	requested = strings.Replace(requested, "[]", `[{"severity":"major","title":"Lost state","body":"Preserve the original state."}]`, 1)

	for _, file := range []string{testApprovedFile, requested, testApprovedFile} {
		if err := postReview(t.Context(), &g, 950, strings.NewReader(file), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	got, match, err := reviewrecord.Select(g.previous, testReviewHead, testReviewBase, nil)
	if len(g.posted) != 3 || got.Verdict != reviewrecord.Approved || match != reviewrecord.Direct || err != nil {
		t.Errorf("posted %d comments, selected %s (%v, %v); want three comments and a direct approval", len(g.posted), got.Verdict, match, err)
	}
}
