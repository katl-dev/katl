package queueadmission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

const testHead = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

// cleanPull is an ordinary pull request that nothing stands in the way of.
func cleanPull(number int) Pull {
	return Pull{
		Number: number, Head: testHead, BaseRef: "main", HeadRef: "feature",
		Author:         Of(Author{Login: "dev"}),
		Draft:          Of(false),
		Body:           Of("Why.\n"),
		Labels:         Of([]string{"enhancement", "area: dev-tooling"}),
		Closes:         Of([]int{}),
		BlockedBy:      Of(Blockers{}),
		Paths:          Of(ChangedPaths{Files: []string{"cmd/katl-queue/plan.go"}, Complete: true}),
		Review:         Of(Review{Match: reviewrecord.Missing}),
		ReviewDecision: Of(""),
		ReleaseNotes:   Of(ReleaseNotes{Count: 1}),
		Conflicts:      Of(Conflicts{}),
		Removal:        Of[*Removal](nil),
		AutoMerge:      Of(true),
		Mergeable:      Of("MERGEABLE"),
		Threads:        Of(0),
		Checks:         Of([]Check{{Name: "go", State: "passed", Required: true}}),
	}
}

func factsOf(queue []Entry, refs map[int]Ref, pulls ...Pull) Facts {
	f := Facts{Target: Target{Branch: "main", Queue: Of(queue)}, Pulls: map[int]Pull{}, Refs: Of(refs)}
	for _, p := range pulls {
		f.Pulls[p.Number] = p
	}
	return f
}

// summary lists each reason as "code gates actor".
func summary(v Verdict) []string {
	var out []string
	for _, r := range v.Reasons {
		out = append(out, string(r.Code)+" "+string(r.Gates)+" "+string(r.Actor))
	}
	return out
}

func reasonFor(v Verdict, code string) (Reason, bool) {
	for _, r := range v.Reasons {
		if string(r.Code) == code {
			return r, true
		}
	}
	return Reason{}, false
}

func TestEvaluateClean(t *testing.T) {
	v := Evaluate(factsOf(nil, nil, cleanPull(10)), 10)
	if len(v.Reasons) != 0 || !v.Admitted() || !v.Enqueueable() || v.Actor() != ActorNone || v.Head != testHead {
		t.Fatalf("clean verdict = %+v, admitted %v, enqueueable %v, actor %v", v, v.Admitted(), v.Enqueueable(), v.Actor())
	}
}

// Each case changes one fact of a clean pull request, so a rule keyed on the
// wrong fact, or with the wrong gate or actor, gives a different list.
func TestEvaluateCodes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(f *Facts, p *Pull)
		extra []Pull
		want  []string
	}{
		{name: "incomplete", edit: func(f *Facts, p *Pull) { p.Draft = Known[bool]{} },
			want: []string{"incomplete admission none"}},
		{name: "draft", edit: func(f *Facts, p *Pull) { p.Draft = Of(true) },
			want: []string{"draft admission owner"}},
		{name: "hold", edit: func(f *Facts, p *Pull) { p.Body = Of("Why.\n\nHold: until the cutover\n") },
			want: []string{"hold admission maintainer"}},
		{name: "dependency open", edit: func(f *Facts, p *Pull) { p.Body = Of("Depends on #20\n") },
			extra: []Pull{cleanPull(20)},
			want:  []string{"dependency-open admission none"}},
		{name: "dependency broken", edit: func(f *Facts, p *Pull) {
			p.Body = Of("Depends on #30\n")
			f.Refs = Of(map[int]Ref{30: {Pull: true, State: "CLOSED"}})
		}, want: []string{"dependency-broken admission owner"}},
		{name: "review missing", edit: func(f *Facts, p *Pull) {
			p.Paths = Of(ChangedPaths{Files: []string{"internal/efiboot/entry.go"}, Complete: true})
		}, want: []string{"review-missing admission reviewer"}},
		{name: "review stale", edit: func(f *Facts, p *Pull) {
			p.Paths = Of(ChangedPaths{Files: []string{"internal/efiboot/entry.go"}, Complete: true})
			p.Review = Of(Review{Match: reviewrecord.Stale, Verdict: reviewrecord.Approved})
		}, want: []string{"review-stale admission reviewer"}},
		{name: "changes requested", edit: func(f *Facts, p *Pull) { p.ReviewDecision = Of("CHANGES_REQUESTED") },
			want: []string{"changes-requested admission owner"}},
		{name: "conflict main", edit: func(f *Facts, p *Pull) { p.Conflicts = Of(Conflicts{Main: []string{"go.mod"}}) },
			want: []string{"conflict-main admission owner"}},
		{name: "conflict queued", edit: func(f *Facts, p *Pull) {
			p.Conflicts = Of(Conflicts{Queued: &Conflict{PR: 40, Paths: []string{"go.mod"}}})
			f.Target.Queue = Of([]Entry{{Number: 40}})
		}, extra: []Pull{cleanPull(40)}, want: []string{"conflict-queued admission none"}},
		{name: "conflict pending", edit: func(f *Facts, p *Pull) {
			p.Conflicts = Of(Conflicts{Pending: []Conflict{{PR: 41, Paths: []string{"go.mod"}}}})
		}, want: []string{"conflict-pending none none"}},
		{name: "merge alone", edit: func(f *Facts, p *Pull) {
			p.Paths = Of(ChangedPaths{Files: []string{"scripts/vmtest-run"}, Complete: true})
			f.Target.Queue = Of([]Entry{{Number: 50}})
		}, extra: []Pull{cleanPull(50)}, want: []string{"merge-alone admission none"}},
		{name: "metadata", edit: func(f *Facts, p *Pull) { p.Labels = Of([]string{}) },
			want: []string{"metadata admission owner"}},
		{name: "release note", edit: func(f *Facts, p *Pull) { p.ReleaseNotes = Of(ReleaseNotes{Count: 2}) },
			want: []string{"release-note admission owner"}},
		{name: "ejected", edit: func(f *Facts, p *Pull) {
			p.Removal = Of(&Removal{Reason: "failed_checks", FailedChecks: []string{"vmtest"}, HeadAtRemoval: testHead})
		}, want: []string{"ejected admission owner"}},
		{name: "ci failed", edit: func(f *Facts, p *Pull) { p.Checks = Of([]Check{{Name: "go", State: "failed", Required: true}}) },
			want: []string{"ci-failed github owner"}},
		{name: "threads unresolved", edit: func(f *Facts, p *Pull) { p.Threads = Of(2) },
			want: []string{"threads-unresolved github owner"}},
		{name: "unmergeable", edit: func(f *Facts, p *Pull) { p.Mergeable = Of("CONFLICTING") },
			want: []string{"unmergeable github owner"}},
		{name: "auto-merge off", edit: func(f *Facts, p *Pull) { p.AutoMerge = Of(false) },
			want: []string{"auto-merge-off github owner"}},
		{name: "ci optional failed", edit: func(f *Facts, p *Pull) { p.Checks = Of([]Check{{Name: "lint", State: "failed"}}) },
			want: []string{"ci-optional-failed none none"}},
		{name: "ci running", edit: func(f *Facts, p *Pull) { p.Checks = Of([]Check{{Name: "go", State: "running", Required: true}}) },
			want: []string{"ci-running none none"}},
		{name: "queued", edit: func(f *Facts, p *Pull) { f.Target.Queue = Of([]Entry{{Number: 10}}) },
			want: []string{"queued none none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			f := factsOf(nil, nil, tc.extra...)
			tc.edit(&f, &pull)
			f.Pulls[10] = pull
			if got := summary(Evaluate(f, 10)); !slices.Equal(got, tc.want) {
				t.Errorf("reasons = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero value read as empty would admit instead of reporting the fact.
func TestEvaluateAbsentFact(t *testing.T) {
	for _, tc := range []struct {
		fact string
		edit func(f *Facts, p *Pull)
	}{
		{"queue", func(f *Facts, p *Pull) { f.Target.Queue = Known[[]Entry]{} }},
		{"refs", func(f *Facts, p *Pull) { f.Refs = Known[map[int]Ref]{} }},
		{"author", func(f *Facts, p *Pull) { p.Author = Known[Author]{} }},
		{"draft", func(f *Facts, p *Pull) { p.Draft = Known[bool]{} }},
		{"body", func(f *Facts, p *Pull) { p.Body = Known[string]{} }},
		{"labels", func(f *Facts, p *Pull) { p.Labels = Known[[]string]{} }},
		{"closes", func(f *Facts, p *Pull) { p.Closes = Known[[]int]{} }},
		{"blockedBy", func(f *Facts, p *Pull) { p.BlockedBy = Known[Blockers]{} }},
		{"paths", func(f *Facts, p *Pull) { p.Paths = Known[ChangedPaths]{} }},
		{"review", func(f *Facts, p *Pull) { p.Review = Known[Review]{} }},
		{"reviewDecision", func(f *Facts, p *Pull) { p.ReviewDecision = Known[string]{} }},
		{"releaseNotes", func(f *Facts, p *Pull) { p.ReleaseNotes = Known[ReleaseNotes]{} }},
		{"conflicts", func(f *Facts, p *Pull) { p.Conflicts = Known[Conflicts]{} }},
		{"removal", func(f *Facts, p *Pull) { p.Removal = Known[*Removal]{} }},
		{"autoMerge", func(f *Facts, p *Pull) { p.AutoMerge = Known[bool]{} }},
		{"mergeable", func(f *Facts, p *Pull) { p.Mergeable = Known[string]{} }},
		{"threads", func(f *Facts, p *Pull) { p.Threads = Known[int]{} }},
		{"checks", func(f *Facts, p *Pull) { p.Checks = Known[[]Check]{} }},
	} {
		t.Run(tc.fact, func(t *testing.T) {
			pull := cleanPull(10)
			f := factsOf(nil, nil)
			tc.edit(&f, &pull)
			f.Pulls[10] = pull
			v := Evaluate(f, 10)
			if got := summary(v); !slices.Equal(got, []string{"incomplete admission none"}) || !slices.Equal(v.Reasons[0].Facts, []string{tc.fact}) {
				t.Fatalf("verdict = %+v, want incomplete naming %s", v.Reasons, tc.fact)
			}
		})
	}
}

// Unknown dependencies must neither be reported as missing nor treated as
// satisfied.
func TestEvaluateUnknownDependency(t *testing.T) {
	t.Run("not read", func(t *testing.T) {
		pull := cleanPull(10)
		pull.Body = Of("Depends on #60\n")
		v := Evaluate(factsOf(nil, nil, pull), 10)
		if got := summary(v); !slices.Equal(got, []string{"incomplete admission none"}) || !slices.Equal(v.Reasons[0].Facts, []string{"refs"}) {
			t.Fatalf("verdict = %+v, want incomplete naming refs", v.Reasons)
		}
	})
	t.Run("not found", func(t *testing.T) {
		pull := cleanPull(10)
		pull.Body = Of("Depends on #60\n")
		v := Evaluate(factsOf(nil, map[int]Ref{60: {State: "NOT_FOUND"}}, pull), 10)
		if got := summary(v); !slices.Equal(got, []string{"dependency-broken admission owner"}) {
			t.Fatalf("reasons = %q, want dependency-broken", got)
		}
	})
	t.Run("traversed pull without body", func(t *testing.T) {
		pull := cleanPull(10)
		pull.Body = Of("Depends on #20\n")
		other := cleanPull(20)
		other.Body = Known[string]{}
		v := Evaluate(factsOf(nil, nil, pull, other), 10)
		r, ok := reasonFor(v, "incomplete")
		if !ok || !slices.Equal(r.Facts, []string{"pulls[20].body"}) {
			t.Fatalf("reasons = %+v, want incomplete naming pulls[20].body", v.Reasons)
		}
	})
	t.Run("other target", func(t *testing.T) {
		pull := cleanPull(10)
		pull.BaseRef = "release/2026.11"
		pull.Draft = Of(true)
		v := Evaluate(factsOf(nil, nil, pull), 10)
		if got := summary(v); !slices.Equal(got, []string{"incomplete admission none"}) || !slices.Equal(v.Reasons[0].Facts, []string{"target"}) {
			t.Fatalf("verdict = %+v, want incomplete naming target alone", v.Reasons)
		}
	})
	t.Run("not open", func(t *testing.T) {
		v := Evaluate(factsOf(nil, nil), 10)
		if got := summary(v); !slices.Equal(got, []string{"incomplete admission none"}) || !slices.Equal(v.Reasons[0].Facts, []string{"pull"}) {
			t.Fatalf("verdict = %+v, want incomplete naming pull", v.Reasons)
		}
	})
}

func TestEvaluateHold(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{name: "inside a fence", body: "Example:\n\n```\nHold: after #12\n```\n"},
		{name: "inside a tilde fence", body: "~~~\nHold: after #12\n~~~\n"},
		{name: "outside a fence", body: "Why.\n\nHold: until the cutover lands\n", want: "Hold: until the cutover lands"},
		{name: "names nothing", body: "Why.\n\nHold:\n", want: "Hold: line names nothing that clears it; say what must happen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Body = Of(tc.body)
			r, held := reasonFor(Evaluate(factsOf(nil, nil, pull), 10), "hold")
			if held != (tc.want != "") || r.Detail != tc.want {
				t.Fatalf("hold = %v %q, want %q", held, r.Detail, tc.want)
			}
		})
	}
}

func TestEvaluateDependencies(t *testing.T) {
	closer := cleanPull(21)
	closer.Closes = Of([]int{80})
	queued := cleanPull(22)
	cycle := cleanPull(23)
	cycle.Body = Of("Depends on #10\n")
	refs := map[int]Ref{
		70: {Pull: true, State: "MERGED"},
		71: {Pull: true, State: "CLOSED"},
		72: {State: "CLOSED"},
	}
	for _, tc := range []struct {
		name      string
		body      string
		closes    []int
		blockedBy Blockers
		want      []string
		prs       []int
	}{
		{name: "merged", body: "Depends on #70\n"},
		{name: "closed issue", body: "Depends on #72\n"},
		{name: "open pull request", body: "Depends on #20\n", want: []string{"dependency-open admission none"}, prs: []int{20}},
		{name: "issue with an open closer", body: "Depends on #80\n", want: []string{"dependency-open admission none"}, prs: []int{21}},
		{name: "queued pull request", blockedBy: Blockers{Local: []int{22}}, want: []string{"dependency-open admission none"}, prs: []int{22}},
		{name: "closed without merging", body: "Depends on #71\n", want: []string{"dependency-broken admission owner"}, prs: []int{71}},
		{name: "other repository", blockedBy: Blockers{External: []string{"other/repo#5"}}, want: []string{"dependency-broken admission owner"}},
		{name: "cycle", body: "Depends on #23\n", want: []string{"dependency-broken admission owner"}, prs: []int{23}},
		{name: "self and own closing issue", body: "Depends on #10, #90\n", closes: []int{90}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Body = Of(tc.body)
			pull.Closes = Of(tc.closes)
			pull.BlockedBy = Of(tc.blockedBy)
			f := factsOf([]Entry{{Number: 22}}, refs, pull, cleanPull(20), closer, queued, cycle)
			v := Evaluate(f, 10)
			var got []string
			var prs []int
			for _, r := range v.Reasons {
				if strings.HasPrefix(string(r.Code), "dependency-") || r.Code == "incomplete" {
					got = append(got, string(r.Code)+" "+string(r.Gates)+" "+string(r.Actor))
					prs = append(prs, r.PRs...)
				}
			}
			if !slices.Equal(got, tc.want) || !slices.Equal(prs, tc.prs) {
				t.Fatalf("dependency reasons = %q %v, want %q %v (%+v)", got, prs, tc.want, tc.prs, v.Reasons)
			}
		})
	}
}

func TestEvaluateReview(t *testing.T) {
	risky := ChangedPaths{Files: []string{"internal/systemdisk/layout.go", "README.md"}, Complete: true}
	plain := ChangedPaths{Files: []string{"README.md"}, Complete: true}
	for _, tc := range []struct {
		name   string
		paths  ChangedPaths
		review Review
		want   []string
	}{
		{name: "risky without a record", paths: risky, review: Review{Match: reviewrecord.Missing},
			want: []string{"review-missing admission reviewer"}},
		{name: "risky with a stale approval", paths: risky, review: Review{Match: reviewrecord.Stale, Verdict: reviewrecord.Approved},
			want: []string{"review-stale admission reviewer"}},
		{name: "risky with a current approval", paths: risky, review: Review{Match: reviewrecord.Direct, Verdict: reviewrecord.Approved}},
		{name: "risky with an equivalent approval", paths: risky, review: Review{Match: reviewrecord.EquivalentPatch, Verdict: reviewrecord.Approved}},
		{name: "changes requested on a plain change", paths: plain, review: Review{Match: reviewrecord.Direct, Verdict: reviewrecord.ChangesRequested},
			want: []string{"changes-requested admission owner"}},
		{name: "stale changes requested on a plain change", paths: plain, review: Review{Match: reviewrecord.Stale, Verdict: reviewrecord.ChangesRequested}},
		{name: "incomplete file list", paths: ChangedPaths{Files: []string{"README.md"}}, review: Review{Match: reviewrecord.Missing},
			want: []string{"review-missing admission reviewer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Paths = Of(tc.paths)
			pull.Review = Of(tc.review)
			if got := summary(Evaluate(factsOf(nil, nil, pull), 10)); !slices.Equal(got, tc.want) {
				t.Fatalf("reasons = %q, want %q", got, tc.want)
			}
		})
	}

	pull := cleanPull(10)
	pull.Paths = Of(risky)
	r, _ := reasonFor(Evaluate(factsOf(nil, nil, pull), 10), "review-missing")
	if !slices.Equal(r.Paths, []string{"internal/systemdisk/layout.go"}) || !strings.Contains(r.Detail, "disk") {
		t.Fatalf("review-missing = %+v, want the disk path and its reason", r)
	}
}

// A pending conflict that gated would hold both members of a pair for good.
func TestEvaluateConflictingPair(t *testing.T) {
	first, second := cleanPull(1), cleanPull(2)
	first.Conflicts = Of(Conflicts{Pending: []Conflict{{PR: 2, Paths: []string{"go.mod"}}}})
	second.Conflicts = Of(Conflicts{Pending: []Conflict{{PR: 1, Paths: []string{"go.mod"}}}})
	f := factsOf(nil, nil, first, second)
	for _, n := range []int{1, 2} {
		v := Evaluate(f, n)
		if got := summary(v); !v.Admitted() || !slices.Equal(got, []string{"conflict-pending none none"}) {
			t.Fatalf("#%d reasons = %q, want an admitted conflict-pending", n, got)
		}
	}

	second.Conflicts = Of(Conflicts{Queued: &Conflict{PR: 1, Paths: []string{"go.mod"}}})
	f = factsOf([]Entry{{Number: 1}}, nil, first, second)
	v := Evaluate(f, 2)
	if got := summary(v); v.Admitted() || v.Actor() != ActorNone || !slices.Equal(got, []string{"conflict-queued admission none"}) {
		t.Fatalf("#2 reasons = %q, actor %v; want conflict-queued waiting", got, v.Actor())
	}
}

func TestEvaluateMergeAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue []Entry
		want  bool
	}{
		{name: "another entry queued", queue: []Entry{{Number: 50}}, want: true},
		{name: "empty queue"},
		{name: "itself queued", queue: []Entry{{Number: 10}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Paths = Of(ChangedPaths{Files: []string{"Containerfile.mkosi"}, Complete: true})
			v := Evaluate(factsOf(tc.queue, nil, pull, cleanPull(50)), 10)
			if _, got := reasonFor(v, "merge-alone"); got != tc.want || v.Admitted() == tc.want {
				t.Fatalf("merge-alone = %v, admitted %v; want %v", got, v.Admitted(), tc.want)
			}
		})
	}
}

func TestEvaluateEjection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		removal Removal
		want    bool
	}{
		{name: "failed checks, head unchanged", removal: Removal{Reason: "failed_checks", FailedChecks: []string{"vmtest"}, HeadAtRemoval: testHead}, want: true},
		{name: "timed out, unrecorded and no push", removal: Removal{Reason: "checks_timed_out"}, want: true},
		{name: "new head", removal: Removal{Reason: "failed_checks", FailedChecks: []string{"vmtest"}, HeadAtRemoval: "0000000000000000000000000000000000000000"}},
		{name: "push after an unrecorded removal", removal: Removal{Reason: "failed_checks", FailedChecks: []string{"vmtest"}, PushedAfter: true}},
		{name: "linked after", removal: Removal{Reason: "failed_checks", FailedChecks: []string{"vmtest"}, HeadAtRemoval: testHead, LinkedAfter: true}},
		{name: "only the admission check", removal: Removal{Reason: "failed_checks", FailedChecks: []string{"queue-admission"}, HeadAtRemoval: testHead}},
		{name: "manual", removal: Removal{Reason: "manual", HeadAtRemoval: testHead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Removal = Of(&tc.removal)
			if _, got := reasonFor(Evaluate(factsOf(nil, nil, pull), 10), "ejected"); got != tc.want {
				t.Fatalf("ejected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluateCI(t *testing.T) {
	for _, tc := range []struct {
		name        string
		checks      []Check
		want        []string
		detail      string
		enqueueable bool
	}{
		{name: "failed required", checks: []Check{{Name: "go", State: "failed", Required: true}},
			want: []string{"ci-failed github owner"}, detail: "go"},
		{name: "cancelled required", checks: []Check{{Name: "vmtest", State: "cancelled", Required: true}},
			want: []string{"ci-failed github owner"}, detail: "vmtest cancelled; re-run it"},
		{name: "failed optional", checks: []Check{{Name: "lint", State: "failed"}, {Name: "go", State: "passed", Required: true}},
			want: []string{"ci-optional-failed none none"}, enqueueable: true},
		{name: "cancelled optional", checks: []Check{{Name: "lint", State: "cancelled"}}, enqueueable: true},
		{name: "only the admission check failed", checks: []Check{{Name: "queue-admission", State: "failed", Required: true}}, enqueueable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Checks = Of(tc.checks)
			v := Evaluate(factsOf(nil, nil, pull), 10)
			if got := summary(v); !slices.Equal(got, tc.want) || v.Enqueueable() != tc.enqueueable {
				t.Fatalf("reasons = %q, enqueueable %v; want %q, %v", got, v.Enqueueable(), tc.want, tc.enqueueable)
			}
			if tc.detail != "" && !strings.Contains(v.Reasons[0].Detail, tc.detail) {
				t.Fatalf("detail = %q, want it to contain %q", v.Reasons[0].Detail, tc.detail)
			}
		})
	}
}

// Reporting auto-merge-off beside another gate would redirect the actor to
// the owner.
func TestEvaluateAutoMergeOff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		edit        func(p *Pull)
		want        []string
		actor       Actor
		enqueueable bool
	}{
		{name: "review missing", edit: func(p *Pull) {
			p.Paths = Of(ChangedPaths{Files: []string{".github/workflows/ci.yml"}, Complete: true})
		}, want: []string{"review-missing admission reviewer"}, actor: ActorReviewer},
		{name: "hold", edit: func(p *Pull) { p.Body = Of("Hold: until the cutover\n") },
			want: []string{"hold admission maintainer"}, actor: ActorMaintainer},
		{name: "clean", edit: func(p *Pull) {}, want: []string{"auto-merge-off github owner"}, actor: ActorOwner, enqueueable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.AutoMerge = Of(false)
			tc.edit(&pull)
			v := Evaluate(factsOf(nil, nil, pull), 10)
			if got := summary(v); !slices.Equal(got, tc.want) || v.Actor() != tc.actor || v.Enqueueable() != tc.enqueueable {
				t.Fatalf("reasons = %q, actor %v, enqueueable %v; want %q, %v, %v", got, v.Actor(), v.Enqueueable(), tc.want, tc.actor, tc.enqueueable)
			}
		})
	}
}

// automationFixture reads a pull request the automation opened, as gh pr view
// prints it. gh prefixes App logins with app/; GitHub's GraphQL login does
// not carry it.
func automationFixture(t *testing.T, name string) Pull {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var pr struct {
		Author struct {
			IsBot bool   `json:"is_bot"`
			Login string `json:"login"`
		} `json:"author"`
		HeadRefName string                  `json:"headRefName"`
		Labels      []struct{ Name string } `json:"labels"`
		Body        string                  `json:"body"`
		Files       []struct{ Path string } `json:"files"`
	}
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	pull := cleanPull(10)
	pull.HeadRef = pr.HeadRefName
	pull.Author = Of(Author{Login: strings.TrimPrefix(pr.Author.Login, "app/"), Bot: pr.Author.IsBot})
	pull.Body = Of(pr.Body)
	labels := []string{}
	for _, l := range pr.Labels {
		labels = append(labels, l.Name)
	}
	pull.Labels = Of(labels)
	var files []string
	for _, f := range pr.Files {
		files = append(files, f.Path)
	}
	pull.Paths = Of(ChangedPaths{Files: files, Complete: true})
	pull.ReleaseNotes = Of(ReleaseNotes{})
	return pull
}

// A branch-name-only exemption would admit an edited or impersonated
// automation pull request without review.
func TestEvaluateAutomation(t *testing.T) {
	queue := []Entry{{Number: 50}}
	for _, fixture := range []string{"pr-976-builder-pin.json", "pr-958-kernel-lock.json"} {
		t.Run(fixture, func(t *testing.T) {
			pull := automationFixture(t, fixture)
			if v := Evaluate(factsOf(queue, nil, pull, cleanPull(50)), 10); len(v.Reasons) != 0 {
				t.Fatalf("automation reasons = %q, want none", summary(v))
			}

			failed := pull
			failed.Checks = Of([]Check{{Name: "go", State: "failed", Required: true}})
			v := Evaluate(factsOf(queue, nil, failed, cleanPull(50)), 10)
			if got := summary(v); !slices.Equal(got, []string{"ci-failed github none"}) || v.Actor() != ActorNone {
				t.Fatalf("failed automation reasons = %q, actor %v; want ci-failed with no actor", got, v.Actor())
			}

			for name, edit := range map[string]func(p *Pull){
				"extra file": func(p *Pull) {
					paths, _ := p.Paths.get()
					p.Paths = Of(ChangedPaths{Files: append(slices.Clone(paths.Files), "docs/README.md"), Complete: true})
				},
				"not a bot":     func(p *Pull) { p.Author = Of(Author{Login: automationLogin}) },
				"another login": func(p *Pull) { p.Author = Of(Author{Login: "other-bot", Bot: true}) },
			} {
				changed := pull
				edit(&changed)
				r, ok := reasonFor(Evaluate(factsOf(queue, nil, changed, cleanPull(50)), 10), "review-missing")
				if !ok || r.Actor != ActorReviewer {
					t.Errorf("%s: review-missing = %v %+v, want it with actor reviewer", name, ok, r)
				}
			}
		})
	}
}

func TestVerdictActor(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *Facts, p *Pull)
		want Actor
	}{
		{name: "hold and open dependency", edit: func(f *Facts, p *Pull) { p.Body = Of("Hold: until the cutover\nDepends on #20\n") }, want: ActorMaintainer},
		{name: "review missing and CI failed", edit: func(f *Facts, p *Pull) {
			p.Paths = Of(ChangedPaths{Files: []string{"internal/generation/apply.go"}, Complete: true})
			p.Checks = Of([]Check{{Name: "go", State: "failed", Required: true}})
		}, want: ActorOwner},
		{name: "waiting only", edit: func(f *Facts, p *Pull) {
			p.Body = Of("Depends on #20\n")
			p.Checks = Of([]Check{{Name: "go", State: "running", Required: true}})
		}, want: ActorNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			f := factsOf(nil, nil, cleanPull(20))
			tc.edit(&f, &pull)
			f.Pulls[10] = pull
			if got := Evaluate(f, 10).Actor(); got != tc.want {
				t.Fatalf("actor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluateMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		gated  bool
	}{
		{name: "kind and area, no milestone", labels: []string{"bug", "area: queue", "flaky-test"}},
		{name: "missing kind", labels: []string{"area: queue", "test-design"}, gated: true},
		{name: "two kinds", labels: []string{"bug", "enhancement", "area: queue"}, gated: true},
		{name: "missing area", labels: []string{"documentation"}, gated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := cleanPull(10)
			pull.Labels = Of(tc.labels)
			if _, got := reasonFor(Evaluate(factsOf(nil, nil, pull), 10), "metadata"); got != tc.gated {
				t.Fatalf("metadata = %v, want %v", got, tc.gated)
			}
		})
	}
}
