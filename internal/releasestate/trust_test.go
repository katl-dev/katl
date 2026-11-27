package releasestate

import (
	"strings"
	"testing"
)

// TestTrust spoils one fact of rc.1's only record. A status that fails the
// rule is not a record, so rc.1 reads as a tag the action did not create,
// and status shows why it was ignored.
func TestTrust(t *testing.T) {
	const tag = "v2026.12.1-rc.1"
	for _, tc := range []struct {
		name    string
		phase   Phase
		spoil   func(w *world)
		warning string
	}{
		{"other writer", Qualified, func(w *world) { w.status().Creator.Login = "someone" }, "created by someone"},
		{"REST login not normalised", Qualified, func(w *world) { w.status().Creator.Login = "github-actions[bot]" }, "not the GitHub Actions bot"},
		{"user with the bot's name", Qualified, func(w *world) { w.status().Creator.Type = "User" }, "not the GitHub Actions bot"},
		{"not a record", Qualified, func(w *world) { s := w.status(); s.Record, s.Problem = nil, "its description is not a phase" }, "its description is not a phase"},
		{"other repository's run", Qualified, func(w *world) {
			w.status().TargetURL = strings.Replace(w.status().TargetURL, testRepo, "fork/katl", 1)
		}, "not a run attempt of katl-dev/katl"},
		{"target disagrees with the record", Qualified, func(w *world) { w.status().Record.Attempt.Number = 2 }, "not a run attempt"},
		{"unread attempt", Qualified, func(w *world) { delete(w.attempts, w.status().Record.Attempt) }, "is not a dispatched Release run"},
		{"other branch", Qualified, func(w *world) { w.attempt().Run.Branch = "main" }, "is not a dispatched Release run"},
		{"push event", Qualified, func(w *world) { w.attempt().Run.Event = "push" }, "is not a dispatched Release run"},
		{"other workflow", Qualified, func(w *world) { w.attempt().Run.Path = artifactsWorkflow }, "is not a dispatched Release run"},
		{"other commit", Qualified, func(w *world) { w.attempt().Run.SHA = commit(2) }, "is not a dispatched Release run"},
		{"fork head", Qualified, func(w *world) { w.attempt().Run.HeadRepository.FullName = "fork/katl" }, "is not a dispatched Release run"},
		{"validation failed", Qualified, func(w *world) { w.attempt().Jobs[1].Conclusion = "failure" }, "Release validation did not succeed"},
		{"validation missing", Qualified, func(w *world) { w.attempt().Jobs = w.attempt().Jobs[:1] }, "Release validation did not succeed"},
		{"other version's bundle", Qualified, func(w *world) { w.attempt().Artifacts = []string{bundleName("v2026.12.1", commit(1))} }, "has no bundle katl-2026.12.1-rc.1-x86_64-" + h1},
		{"unattested digest", Qualified, func(w *world) { w.in.Attestations = nil }, "has no attestation"},
		{"attested by another attempt", Qualified, func(w *world) {
			w.attestation().Invocation = testServer + "/" + testRepo + "/actions/runs/101/attempts/2"
		}, "has no attestation"},
		{"attested from a tag", Qualified, func(w *world) {
			w.attestation().SignerWorkflow = testServer + "/" + testRepo + "/" + artifactsWorkflow + "@refs/tags/" + tag
		}, "has no attestation"},
		{"attested outside release.yml", Qualified, func(w *world) {
			w.attestation().ConfigWorkflow = testServer + "/" + testRepo + "/.github/workflows/ci.yml@refs/heads/release/2026.12.1"
		}, "has no attestation"},
		{"attested at another commit", Qualified, func(w *world) { w.attestation().SourceDigest = commit(2) }, "has no attestation"},
		{"failed while validation succeeded", Failed, func(w *world) { w.attempt().Jobs[1].Conclusion = "success" }, "did not end without qualifying"},
		{"failed while still running", Failed, func(w *world) { w.attempt().Run.Status = "in_progress" }, "did not end without qualifying"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(1).candidate(1, 1, tc.phase)
			tc.spoil(w)
			got, err := Decide(w.inputs())
			if err != nil {
				t.Fatal(err)
			}
			if got.Row != 8 || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], tc.warning) {
				t.Fatalf("row %d %v, want row 8 with a warning containing %q", got.Row, got.Warnings, tc.warning)
			}
		})
	}
}

// TestRecordSelection covers which of several records of one context holds.
func TestRecordSelection(t *testing.T) {
	const rc = "v2026.12.1-rc.1"
	for _, tc := range []struct {
		name  string
		world func() *world
		row   int
	}{
		// A later failed attempt cannot unqualify a candidate: its bytes are
		// published from the qualified attempt.
		{"qualified candidate stands", func() *world { w := newWorld(1).candidate(1, 1, Qualified); w.build(rc, 1, Failed); return w }, 21},
		// The pre-release carries rc.1's first bytes, so a later qualified
		// attempt holding the record shows as a mismatch.
		{"later qualified candidate holds", func() *world {
			w := newWorld(1).candidate(1, 1, Qualified).publish(rc, true)
			w.build(rc, 1, Qualified)
			return w
		}, 18},
		// An untagged final's qualified record gives way to a later attempt.
		{"later final attempt replaces an unapproved final", func() *world {
			w := green(1)
			w.build("v2026.12.1", 1, Qualified)
			w.build("v2026.12.1", 1, Failed)
			return w
		}, 24},
		{"building then failed in one attempt", func() *world {
			w := newWorld(1).candidate(1, 1, Failed)
			building := *w.status()
			building.Record = &Record{Tag: rc, Phase: Building, Attempt: building.Record.Attempt}
			w.in.Statuses[commit(1)] = append(w.in.Statuses[commit(1)], building)
			return w
		}, 19},
		{"ignored writer does not shadow a record", func() *world {
			w := newWorld(1).candidate(1, 1, Qualified)
			w.build(rc, 1, Failed)
			w.status().Creator.Login = "someone"
			return w
		}, 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.world().inputs())
			if err != nil {
				t.Fatal(err)
			}
			if got.Row != tc.row {
				t.Fatalf("row %d (%s): %s %v, want row %d", got.Row, got.State, got.Message, got.Warnings, tc.row)
			}
		})
	}
}
