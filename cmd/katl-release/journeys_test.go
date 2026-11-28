package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestJourneys(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gittest.Env([]string{"PATH=" + os.Getenv("PATH")}, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(subject string) string {
		git("commit", "--allow-empty", "-qm", subject)
		return git("rev-parse", "HEAD")
	}

	git("init", "-qb", "main")
	shipped := commit("Already shipped (#10)")
	git("tag", "-am", "Previous final release", "v2026.11.7")
	cut := commit("Branch cut")
	afterCut := commit("Main after the branch cut (#11)")
	git("switch", "-qc", "release/2026.11.8", cut)
	admitted := commit("Admitted to the release branch (#12)")
	head := commit("Journey passed (#13)")

	const (
		prList  = repoPrefix + "issues?labels=user-facing&per_page=100&state=closed"
		gapList = repoPrefix + "issues?labels=release-blocker%2Cjourney%3A+gap&per_page=100&state=open"
	)
	pr := func(number int, title string, merged any, labels ...string) map[string]any {
		var namedLabels []map[string]string
		for _, label := range labels {
			namedLabels = append(namedLabels, map[string]string{"name": label})
		}
		return map[string]any{
			"number": number, "title": title, "html_url": fmt.Sprintf("https://github.com/katl-dev/katl/pull/%d", number),
			"pull_request": map[string]any{"merged_at": merged}, "labels": namedLabels,
		}
	}
	ref := func(name, kind, sha string) map[string]any {
		return map[string]any{"ref": name, "object": map[string]string{"type": kind, "sha": sha}}
	}
	mergedAt := "2026-11-28T12:00:00Z"
	openPR := pr(21, "Open PR with both labels", nil, "release-blocker", "journey: gap")
	fix := map[string]any{"number": 22, "title": "Fix the failed journey", "html_url": "https://github.com/katl-dev/katl/issues/22"}
	tag := git("rev-parse", "refs/tags/v2026.11.7")
	f := &fakeGitHub{t: t, responses: map[string]any{
		repoPrefix + "git/tags/" + tag: map[string]any{"object": map[string]string{"type": "commit", "sha": shipped}},
		repoPrefix + "pulls/10":        map[string]string{"merge_commit_sha": shipped},
		repoPrefix + "pulls/11":        map[string]string{"merge_commit_sha": afterCut},
		repoPrefix + "pulls/12":        map[string]string{"merge_commit_sha": admitted},
	}, pages: map[string][]any{
		repoPrefix + "git/matching-refs/heads/release/2026.11.8?per_page=100": {
			[]any{ref("refs/heads/release/2026.11.8", "commit", head)},
		},
		repoPrefix + "git/matching-refs/tags/v2026.11.7?per_page=100": {
			[]any{ref("refs/tags/v2026.11.7", "tag", tag)},
		},
		prList: {
			[]any{
				pr(10, "Already shipped", mergedAt, "user-facing"),
				pr(11, "Merged to main after the cut", mergedAt, "user-facing"),
				pr(13, "Journey passed", mergedAt, "user-facing", "journey: passed"),
				pr(14, "Closed unmerged", nil, "user-facing"),
				map[string]any{"number": 15, "title": "Closed issue"},
			},
			[]any{pr(12, "Admitted to the release branch", mergedAt, "user-facing")},
		},
		gapList: {[]any{openPR}, []any{fix}},
	}}
	// pin is scripts/production-upgrade-baseline.txt at the branch head; nil
	// means the file is missing there.
	pin := []byte("v2026.11.7\n")
	r := newTestReader(f)
	r.read = func(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
		if file, ok := strings.CutPrefix(endpoint, repoPrefix+"contents/"); ok {
			if file != "scripts/production-upgrade-baseline.txt?ref="+head || accept != "application/vnd.github.raw" {
				t.Errorf("unexpected contents read %q as %q", endpoint, accept)
				return nil, &apiError{status: 404, text: "not found"}
			}
			if pin == nil {
				return nil, &apiError{status: 404, text: "gh: Not Found (HTTP 404)"}
			}
			return pin, nil
		}
		path, ok := strings.CutPrefix(endpoint, repoPrefix+"compare/")
		if !ok {
			return f.read(ctx, endpoint, paginate, accept)
		}
		rangeSpec, ok := strings.CutSuffix(path, "?per_page=1")
		if !ok {
			t.Fatalf("unexpected compare endpoint %q", endpoint)
		}
		left, right, _ := strings.Cut(git("rev-list", "--left-right", "--count", rangeSpec), "\t")
		var status string
		switch {
		case left == "0" && right == "0":
			status = "identical"
		case left == "0":
			status = "ahead"
		case right == "0":
			status = "behind"
		default:
			status = "diverged"
		}
		return json.Marshal(map[string]string{"status": status})
	}
	args := []string{"2026.11.8"}
	var out bytes.Buffer
	const header = "Outstanding journeys for release/2026.11.8 since v2026.11.7\n"
	const gap = "  Gap fix #21 Open PR with both labels (https://github.com/katl-dev/katl/pull/21)\n" +
		"  Gap fix #22 Fix the failed journey (https://github.com/katl-dev/katl/issues/22)\n"
	const outstanding = "  PR #12 Admitted to the release branch (https://github.com/katl-dev/katl/pull/12)\n"
	if err := journeys(t.Context(), args, &out, r); err == nil || out.String() != header+outstanding+gap {
		t.Fatalf("journeys = %q, %v", out.String(), err)
	}

	f.pages[prList] = f.pages[prList][:1]
	out.Reset()
	if err := journeys(t.Context(), args, &out, r); err == nil || out.String() != header+gap {
		t.Fatalf("journeys with only a gap fix = %q, %v", out.String(), err)
	}

	f.pages[gapList] = []any{[]any{}}
	out.Reset()
	const clear = "Journey gate clear for release/2026.11.8 since v2026.11.7.\n"
	if err := journeys(t.Context(), args, &out, r); err != nil || out.String() != clear {
		t.Fatalf("journeys after fixing the gap = %q, %v", out.String(), err)
	}

	readErr := errors.New("GitHub read failed")
	f.failures = map[string][]error{gapList: {readErr}}
	out.Reset()
	if err := journeys(t.Context(), args, &out, r); !errors.Is(err, readErr) || out.Len() != 0 {
		t.Fatalf("journeys after a read failure = %q, %v", out.String(), err)
	}

	f.failures = nil

	// GitHub has no v2026.11.6 tag.
	f.pages[repoPrefix+"git/matching-refs/tags/v2026.11.6?per_page=100"] = []any{[]any{}}

	// Each refusal names the pin, the branch it was read on and the offending
	// value, and says to correct the pin on that branch.
	refused := []struct {
		name      string
		pin       []byte
		offending string
	}{
		{"missing", nil, ""},
		{"candidate", []byte("v2026.11.7-rc.1\n"), "v2026.11.7-rc.1"},
		{"not a tag", []byte("2026.11.7\n"), `"2026.11.7"`},
		{"same release", []byte("v2026.11.8\n"), "v2026.11.8"},
		{"later release", []byte("v2026.12.0\n"), "v2026.12.0"},
		{"untagged earlier release", []byte("v2026.11.6\n"), "v2026.11.6"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			pin = tc.pin
			out.Reset()
			err := journeys(t.Context(), args, &out, r)
			if err == nil || out.Len() != 0 {
				t.Fatalf("journeys = %q, %v, want a refusal", out.String(), err)
			}
			for _, part := range []string{"scripts/production-upgrade-baseline.txt", "release/2026.11.8", tc.offending, "Update the pin on release/2026.11.8 through a pull request"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("journeys error %q does not contain %q", err, part)
				}
			}
		})
	}

	t.Run("previous tag argument", func(t *testing.T) {
		pin = []byte("v2026.11.7\n")
		out.Reset()
		err := journeys(t.Context(), []string{"2026.11.8", "v2026.11.7"}, &out, r)
		if err == nil || err.Error() != "usage: scripts/katl-release journeys VERSION" || out.Len() != 0 {
			t.Fatalf("journeys with a previous tag = %q, %v", out.String(), err)
		}
	})
}
