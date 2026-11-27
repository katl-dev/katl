package releasestate

import (
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/runstatus"
)

// TestTrust spoils one fact of rc.1's only record. A status that fails the
// rule is not a record, so rc.1 reads as a tag the action did not create,
// and status shows why it was ignored.
func TestTrust(t *testing.T) {
	final, _ := releaseversion.Parse("2026.12.1")
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
		{"other version's bundle", Qualified, func(w *world) { w.attempt().Artifacts = []string{BundleName(final, commit(1))} }, "has no bundle katl-2026.12.1-rc.1-x86_64-" + h1},
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

// TestUnpublishedNeedsNoAttestation covers the qualified records that next
// acts on before anything is published: the acting job verifies the bytes, so
// a missing attestation does not change the decision.
func TestUnpublishedNeedsNoAttestation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		world func() *world
		row   int
	}{
		{"candidate incomplete", func() *world { return newWorld(1).candidate(1, 1, Qualified) }, 21},
		{"final unapproved", func() *world { w := green(1); w.build("v2026.12.1", 1, Qualified); return w }, 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.world()
			delete(w.in.Attestations, w.status().Record.Digest)
			got, err := Decide(w.inputs())
			if err != nil || got.Row != tc.row || len(got.Warnings) != 0 {
				t.Fatalf("row %d %v, %v, want row %d without warnings", got.Row, got.Warnings, err, tc.row)
			}
		})
	}
}

// TestReleaseMatchesRecord spoils one fact of a published release whose
// record qualified. A release that differs from its record is refused, never
// taken as published.
func TestReleaseMatchesRecord(t *testing.T) {
	const rc = "v2026.12.1-rc.1"
	asset := func(w *world, tag, name string) *Asset {
		assets := w.in.Releases[tag].Assets
		return &assets[slices.IndexFunc(assets, func(a Asset) bool { return a.Name == name })]
	}
	release := func(w *world, tag string, change func(*Release)) {
		r := w.in.Releases[tag]
		change(&r)
		w.in.Releases[tag] = r
	}
	branchRef := "refs/heads/release/2026.12.1"
	// A release whose bytes match the record but whose attestation does not
	// is refused for its attestation, not reported as a digest mismatch.
	const digest, attestation = false, true
	for _, tc := range []struct {
		name       string
		final      bool
		unattested bool
		spoil      func(w *world, tag string)
	}{
		{"extra asset", false, digest, func(w *world, tag string) {
			release(w, tag, func(r *Release) {
				r.Assets = append(r.Assets, Asset{Name: "extra", Uploaded: true, Digest: "sha256:" + digestOf("extra")})
			})
		}},
		{"listed file missing", false, digest, func(w *world, tag string) {
			release(w, tag, func(r *Release) { r.Sums["katl-installer.efi"] = digestOf("efi") })
		}},
		{"asset digest differs from SHA256SUMS", false, digest, func(w *world, tag string) { asset(w, tag, "katlctl").Digest = "sha256:" + digestOf("other") }},
		{"asset not uploaded", false, digest, func(w *world, tag string) { asset(w, tag, "katlctl").Uploaded = false }},
		{"SHA256SUMS unread", false, digest, func(w *world, tag string) { release(w, tag, func(r *Release) { r.Sums = nil }) }},
		// A listing that names SHA256SUMS with the recorded digest would match
		// if SHA256SUMS were taken as just another listed file.
		{"SHA256SUMS lists itself", false, digest, func(w *world, tag string) {
			release(w, tag, func(r *Release) { r.Sums["SHA256SUMS"] = w.digests[tag] })
		}},
		{"SHA256SUMS differs from the record", false, digest, func(w *world, tag string) { asset(w, tag, "SHA256SUMS").Digest = "sha256:" + digestOf("other") }},
		{"unattested", false, attestation, func(w *world, tag string) { w.in.Attestations = nil }},
		{"attested by another attempt", false, attestation, func(w *world, tag string) {
			w.attestation().Invocation = testServer + "/" + testRepo + "/actions/runs/101/attempts/2"
		}},
		{"signed from a tag", false, attestation, func(w *world, tag string) {
			w.attestation().SignerWorkflow = testServer + "/" + testRepo + "/" + artifactsWorkflow + "@refs/tags/" + tag
		}},
		{"started outside release.yml", false, attestation, func(w *world, tag string) {
			w.attestation().ConfigWorkflow = testServer + "/" + testRepo + "/.github/workflows/ci.yml@" + branchRef
		}},
		{"attested at another commit", false, attestation, func(w *world, tag string) { w.attestation().SourceDigest = commit(2) }},
		{"attested from another ref", false, attestation, func(w *world, tag string) { w.attestation().SourceRef = "refs/tags/" + tag }},
		{"attested for other subjects", false, attestation, func(w *world, tag string) { w.attestation().Subjects = []string{digestOf("other")} }},
		{"final unattested", true, attestation, func(w *world, tag string) { w.in.Attestations[w.digests[tag]] = nil }},
		{"final attested from another ref", true, attestation, func(w *world, tag string) { w.attestation().SourceRef = "refs/heads/main" }},
		{"final asset not uploaded", true, digest, func(w *world, tag string) { asset(w, tag, "katlctl").Uploaded = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, tag, matching, row := green(1), rc, 23, 18
			if tc.final {
				w, tag, matching, row = released(1, 1), "v2026.12.1", 14, 10
			}
			if got, err := Decide(w.inputs()); err != nil || got.Row != matching {
				t.Fatalf("before spoiling: row %d (%s) %v, %v, want row %d", got.Row, got.State, got.Warnings, err, matching)
			}

			tc.spoil(w, tag)
			got, err := Decide(w.inputs())
			if err != nil {
				t.Fatal(err)
			}
			want := "it does not match the recorded sha256:" + w.digests[tag]
			if tc.unattested {
				want = "the SHA256SUMS is not attested by the recorded run attempt " + w.status().Record.Attempt.URL(testServer, testRepo)
			}
			if got.Row != row || !strings.Contains(got.Message, want) {
				t.Fatalf("row %d (%s): %s, want row %d with %q", got.Row, got.State, got.Message, row, want)
			}
		})
	}
}

// TestRecordSelection covers which of several records of one context holds.
// GitHub lists statuses newest first, so each case holds in both orders.
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
		// Records order by start, not by run number: a rerun of run 100 that
		// starts after run 101 holds the record, so the pre-release published
		// from run 101 shows as a mismatch.
		{"rerun of an earlier run holds", func() *world {
			w := newWorld(1)
			w.run = 100
			w.candidate(1, 1, Qualified).publish(rc, true)
			w.build(rc, 1, Qualified)
			w.renumber(runstatus.Attempt{Run: 100, Number: 2})
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
		for _, order := range []string{"oldest first", "newest first"} {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				in := tc.world().inputs()
				if order == "newest first" {
					for commit, statuses := range in.Statuses {
						in.Statuses[commit] = slices.Clone(statuses)
						slices.Reverse(in.Statuses[commit])
					}
				}
				got, err := Decide(in)
				if err != nil {
					t.Fatal(err)
				}
				if got.Row != tc.row {
					t.Fatalf("row %d (%s): %s %v, want row %d", got.Row, got.State, got.Message, got.Warnings, tc.row)
				}
			})
		}
	}
}
