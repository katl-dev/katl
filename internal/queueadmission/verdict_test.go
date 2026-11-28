package queueadmission

import (
	"encoding/json"
	"fmt"
	"testing"
)

func renderedVerdict() Verdict {
	paths := []string{".github/workflows/system-extensions.yml"}
	return Verdict{PR: 931, Head: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", Reasons: []Reason{
		{Code: "conflict-main", Gates: GateAdmission, Actor: ActorOwner, Detail: "conflicts with main",
			ClearsBy: "rebase onto the base branch", Paths: paths},
		{Code: "conflict-pending", Gates: GateNone, Actor: ActorNone,
			Detail:   "conflicts with #920, which has auto-merge enabled and has not joined the queue",
			ClearsBy: "advisory: the other PR is not queued yet", PRs: []int{920}, Paths: paths},
		{Code: "ci-running", Gates: GateNone, Actor: ActorNone, Detail: "3 checks running", ClearsBy: "checks finish"},
	}}
}

func TestVerdictText(t *testing.T) {
	want := `#931 (a1b2c3d) into main: gated, owner must act
  conflict-main  [gates, owner]  conflicts with main
      clears by: rebase onto the base branch
      .github/workflows/system-extensions.yml
  conflict-pending  [advisory]  conflicts with #920, which has auto-merge enabled and has not joined the queue
      clears by: advisory: the other PR is not queued yet
      .github/workflows/system-extensions.yml
  ci-running  [progress]  3 checks running
`
	if got := renderedVerdict().Text("main"); got != want {
		t.Fatalf("text:\n%s\nwant:\n%s", got, want)
	}
}

func TestVerdictTextStates(t *testing.T) {
	var many []string
	for i := range 12 {
		many = append(many, fmt.Sprintf("p%02d", i))
	}
	for _, tc := range []struct {
		name    string
		reasons []Reason
		want    string
	}{
		{name: "admitted", want: "#5 (abc) into main: admitted\n"},
		{name: "github only", reasons: []Reason{{Code: "threads-unresolved", Gates: GateGitHub, Actor: ActorOwner, Detail: "1 unresolved review thread", ClearsBy: "resolve the review threads"}},
			want: "#5 (abc) into main: admitted, owner must act\n  threads-unresolved  [github, owner]  1 unresolved review thread\n      clears by: resolve the review threads\n"},
		{name: "waiting", reasons: []Reason{{Code: "merge-alone", Gates: GateAdmission, Actor: ActorNone, Detail: "d", ClearsBy: "the queue drains", Paths: many}},
			want: "#5 (abc) into main: gated, waiting\n  merge-alone  [gates, none]  d\n      clears by: the queue drains\n" +
				"      p00\n      p01\n      p02\n      p03\n      p04\n      p05\n      p06\n      p07\n      p08\n      p09\n      ... and 2 more\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Verdict{PR: 5, Head: "abc", Reasons: tc.reasons}).Text("main"); got != tc.want {
				t.Fatalf("text:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestVerdictJSON(t *testing.T) {
	got, err := json.Marshal(renderedVerdict())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"pr":931,"head":"a1b2c3d4e5f60718293a4b5c6d7e8f9012345678","admitted":false,"enqueueable":false,"actor":"owner","reasons":[` +
		`{"code":"conflict-main","gates":"admission","actor":"owner","detail":"conflicts with main","clearsBy":"rebase onto the base branch","paths":[".github/workflows/system-extensions.yml"]},` +
		`{"code":"conflict-pending","gates":"none","actor":"none","detail":"conflicts with #920, which has auto-merge enabled and has not joined the queue","clearsBy":"advisory: the other PR is not queued yet","prs":[920],"paths":[".github/workflows/system-extensions.yml"]},` +
		`{"code":"ci-running","gates":"none","actor":"none","detail":"3 checks running","clearsBy":"checks finish"}]}`
	if string(got) != want {
		t.Fatalf("json:\n%s\nwant:\n%s", got, want)
	}

	clean, err := json.Marshal(Verdict{PR: 1, Head: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"pr":1,"head":"abc","admitted":true,"enqueueable":true,"actor":"none","reasons":[]}`; string(clean) != want {
		t.Fatalf("json = %s, want %s", clean, want)
	}
}
