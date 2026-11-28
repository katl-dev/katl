package releasestate

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/runstatus"
)

const (
	h1 = "111111111111"
	h2 = "222222222222"
	h3 = "333333333333"
)

// TestDecide has one case per row of the state table, and one per
// enumerated reason of the rows that have several. Each refusal is the
// design's message with this world's values.
func TestDecide(t *testing.T) {
	runURL := testServer + "/" + testRepo + "/actions/runs/"
	returned := map[Action]bool{}
	for _, tc := range []struct {
		name   string
		world  func() *world
		row    int
		action Action
		want   string // the refusal, or the action's version and commit
	}{
		{"1 series branch", func() *world { w := newWorld(1); w.in.Ref = "refs/heads/release/2026.12.x"; return w },
			1, Refuse, "refs/heads/release/2026.12.x does not name one release. The release action runs only on the branch release/<version>, where <version> is a complete final version such as 2026.12.1. Dispatch it on the branch of the version to release."},
		{"1 prerelease branch", func() *world { w := newWorld(1); w.in.Ref = "refs/heads/release/2026.12.1-rc.1"; return w },
			1, Refuse, "refs/heads/release/2026.12.1-rc.1 does not name one release."},
		{"1 tag named like a branch", func() *world { w := newWorld(1); w.in.Ref = "refs/tags/release/2026.12.1"; return w },
			1, Refuse, "refs/tags/release/2026.12.1 does not name one release."},
		{"2 branch missing", func() *world { w := newWorld(1); w.in.Head = ""; return w },
			2, Refuse, "release/2026.12.1 does not exist. Cut it from the selected main commit, then dispatch the release action on it."},
		{"3 branch moved", func() *world { w := newWorld(2); w.in.Dispatch = commit(1); return w },
			3, Refuse, "release/2026.12.1 moved from " + h1 + " to " + h2 + " after this run was dispatched. Dispatch the release action again."},
		{"4 rc.0", func() *world { return newWorld(2).tag("v2026.12.1-rc.0", 1).candidate(1, 2, Qualified) },
			4, Refuse, "v2026.12.1-rc.0 is not a candidate tag: candidates are v2026.12.1-rc.1, v2026.12.1-rc.2 and so on. Nothing was changed. Have a repository administrator remove v2026.12.1-rc.0 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"4 suffix outside the grammar", func() *world { return newWorld(2).candidate(1, 1, Qualified).tag("v2026.12.1-rc.1x", 2) },
			4, Refuse, "v2026.12.1-rc.1x is not a candidate tag:"},
		{"5 gap", func() *world { return newWorld(3).candidate(1, 1, Qualified).candidate(3, 3, Qualified) },
			5, Refuse, "v2026.12.1-rc.2 is missing below v2026.12.1-rc.3: candidates are numbered from rc.1 without a gap. Nothing was changed. Have a repository administrator remove v2026.12.1-rc.3 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"6 candidate off the branch", func() *world { return newWorld(2).candidate(1, 7, Qualified) },
			6, Refuse, "v2026.12.1-rc.1 is at 777777777777, which is not on release/2026.12.1 (head " + h2 + "). Nothing was changed. Have a repository administrator remove v2026.12.1-rc.1 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"6 branch behind the final", func() *world { return released(2, 1) },
			6, Refuse, "v2026.12.1-rc.1 is at " + h2 + ", which is not on release/2026.12.1 (head " + h1 + ")."},
		{"7 two candidates on one commit", func() *world { return newWorld(2).candidate(1, 2, Failed).candidate(2, 2, Qualified) },
			7, Refuse, "v2026.12.1-rc.1 at " + h2 + " and v2026.12.1-rc.2 at " + h2 + " are out of order: each candidate is on a later commit than the one before it, and no commit carries two. Nothing was changed. Have a repository administrator remove v2026.12.1-rc.2 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"7 reversed", func() *world { return newWorld(2).candidate(1, 2, Failed).candidate(2, 1, Qualified) },
			7, Refuse, "v2026.12.1-rc.1 at " + h2 + " and v2026.12.1-rc.2 at " + h1 + " are out of order"},
		{"8 foreign candidate", func() *world { return newWorld(2).candidate(1, 1, Failed).tag("v2026.12.1-rc.2", 2) },
			8, Refuse, "v2026.12.1-rc.2 has no status katl/release/v2026.12.1-rc.2 from a Release run on release/2026.12.1 at " + h2 + ", so the release action did not create it. Nothing was changed. Have a repository administrator remove v2026.12.1-rc.2 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"9 final without a candidate", func() *world { return newWorld(1).tag("v2026.12.1", 1) },
			9, Refuse, "v2026.12.1 at " + h1 + " was not created by the release action: the final goes on the commit of the latest candidate, and that is not " + h1 + ". Nothing was changed. Have a repository administrator remove v2026.12.1 if it was created by mistake, or abandon 2026.12.1 and cut the next version."},
		{"9 final off the latest candidate", func() *world { return released(2, 2).candidate(2, 3, Qualified).head(3) },
			9, Refuse, "v2026.12.1 at " + h2 + " was not created by the release action: the final goes on the commit of the latest candidate, and that is not " + h2 + "."},
		{"9 candidate never qualified", func() *world { return newWorld(1).candidate(1, 1, Failed).tag("v2026.12.1", 1) },
			9, Refuse, "v2026.12.1 at " + h1 + " was not created by the release action: v2026.12.1-rc.1 at that commit never qualified."},
		{"9 final never qualified", func() *world { return green(1).tag("v2026.12.1", 1) },
			9, Refuse, "v2026.12.1 at " + h1 + " was not created by the release action: no Release run on release/2026.12.1 qualified v2026.12.1 at that commit."},
		{"10 final release missing", func() *world { w := released(1, 1); delete(w.in.Releases, "v2026.12.1"); return w },
			10, Refuse, "The GitHub release v2026.12.1 disagrees with what the release action recorded: the tag exists and no release is published for it. Published bytes are never replaced. Nothing was changed. If the release was edited or deleted by mistake, undo that and dispatch again; otherwise abandon 2026.12.1 and cut the next version."},
		{"10 final release differs", func() *world { w := released(1, 1); w.publish("v2026.12.1", true); return w },
			10, Refuse, "The GitHub release v2026.12.1 disagrees with what the release action recorded: it does not match the recorded sha256:" + digestOf("v2026.12.1@102") + "."},
		{"11 approval superseded", func() *world { return green(1).head(2).active(true) },
			11, Refuse, "v2026.12.1 is waiting for approval at " + h1 + ", and release/2026.12.1 has moved to " + h2 + ": " + runURL + "900. Reject the approval; the next dispatch then tags v2026.12.1-rc.2 at the head. Approving it publishes nothing, because the publish job refuses a branch that moved."},
		{"12 approval pending", func() *world { return green(1).active(true) },
			12, Refuse, "v2026.12.1 is waiting for approval: " + runURL + "900. Approve or reject it there."},
		// With the final tagged, a new commit cannot supersede the approval:
		// row 11 needs an untagged final.
		{"12 approval pending after the final", func() *world { return released(1, 2).active(true) },
			12, Refuse, "v2026.12.1 is waiting for approval: " + runURL + "900. Approve or reject it there."},
		{"13 run active", func() *world { return newWorld(1).active(false) },
			13, Refuse, "A Release run for 2026.12.1 is in progress: " + runURL + "900. Wait for it to finish, then run scripts/katl-release status 2026.12.1."},
		{"14 released", func() *world { return released(1, 1) },
			14, Refuse, "v2026.12.1 is released: " + testServer + "/" + testRepo + "/releases/tag/v2026.12.1. There is nothing left to do for 2026.12.1."},
		{"15 released, branch ahead", func() *world { return released(1, 2) },
			15, Refuse, "v2026.12.1 is released at " + h1 + ", and release/2026.12.1 has moved on to " + h2 + ". A released version never changes; cut the next version for the later commits."},
		{"16 first candidate", func() *world { return newWorld(1) }, 16, Candidate, "2026.12.1-rc.1 " + commit(1)},
		{"17 superseded green", func() *world { return green(1).head(2) }, 17, Candidate, "2026.12.1-rc.2 " + commit(2)},
		{"17 superseded contradiction", func() *world { return newWorld(2).candidate(1, 1, Failed).publish("v2026.12.1-rc.1", false) }, 17, Candidate, "2026.12.1-rc.2 " + commit(2)},
		{"18 published without qualifying", func() *world { return newWorld(1).candidate(1, 1, Failed).publish("v2026.12.1-rc.1", true) },
			18, Refuse, "The GitHub pre-release v2026.12.1-rc.1 disagrees with what the release action recorded: it is published, and no Release run qualified v2026.12.1-rc.1. Published bytes are never replaced. Nothing was changed. Restore the pre-release flag if it was edited; otherwise push a commit to release/2026.12.1, and the next dispatch tags v2026.12.1-rc.2."},
		{"18 pre-release flag cleared", func() *world { return newWorld(1).candidate(1, 1, Qualified).publish("v2026.12.1-rc.1", false) },
			18, Refuse, "The GitHub pre-release v2026.12.1-rc.1 disagrees with what the release action recorded: it does not match the recorded sha256:" + digestOf("v2026.12.1-rc.1@101") + "."},
		{"19 candidate failed", func() *world {
			return newWorld(2).candidate(1, 1, Qualified).publish("v2026.12.1-rc.1", true).candidate(2, 2, Failed)
		},
			19, Refuse, "v2026.12.1-rc.2 did not qualify at " + h2 + ": Build / Production VM (lts), Build / Production VM (standard) failed (" + runURL + "102/attempts/1). Push a fix to release/2026.12.1; the next dispatch then tags v2026.12.1-rc.3. If the source is not at fault, dispatch again with a retry reason to rebuild v2026.12.1-rc.2."},
		{"19 candidate cancelled", func() *world { return newWorld(1).candidate(1, 1, Building) },
			19, Refuse, "v2026.12.1-rc.1 did not qualify at " + h1 + ": the run was cancelled (" + runURL + "101/attempts/1)."},
		{"20 candidate retry", func() *world { return newWorld(1).candidate(1, 1, Failed).retry("  runner lost  ") }, 20, RetryCandidate, "2026.12.1-rc.1 " + commit(1)},
		{"21 candidate incomplete", func() *world { return newWorld(1).candidate(1, 1, Qualified) }, 21, ResumeCandidate, "2026.12.1-rc.1 " + commit(1)},
		{"22 extensions published", func() *world {
			w := green(1)
			w.in.ExtensionReferences = []string{"ghcr.io/katl-dev/katl/extensions/drbd9:v2026.12.1-lts-x86_64-drbd9-9.3.4"}
			return w.retry("x")
		},
			22, Refuse, "Extension bundles for 2026.12.1 are already published with different contents. Published bytes are never replaced. Have a repository administrator delete those package versions if no release uses them and dispatch again; otherwise abandon 2026.12.1 and cut the next version."},
		{"23 final start", func() *world { return green(1) }, 23, Final, "2026.12.1 " + commit(1)},
		{"24 final failed", func() *world { w := green(1); w.build("v2026.12.1", 1, Failed); return w },
			24, Refuse, "v2026.12.1 did not qualify at " + h1 + ": Build / Production VM (lts), Build / Production VM (standard) failed (" + runURL + "102/attempts/1). v2026.12.1-rc.1 is still green. If the source is at fault, push a fix to release/2026.12.1; the next dispatch then tags v2026.12.1-rc.2. Otherwise dispatch again with a retry reason to rebuild v2026.12.1."},
		{"25 final retry", func() *world { w := green(1); w.build("v2026.12.1", 1, Building); return w.retry("flake") }, 25, RetryFinal, "2026.12.1 " + commit(1)},
		{"26 final unapproved", func() *world { w := green(1); w.build("v2026.12.1", 1, Qualified); return w }, 26, Final, "2026.12.1 " + commit(1)},
		{"27 retry for the first candidate", func() *world { return newWorld(1).retry("x") },
			27, Refuse, "There is nothing to retry for 2026.12.1: a dispatch without a retry reason would tag v2026.12.1-rc.1 at " + h1 + ". Dispatch again without one."},
		{"27 retry for a superseded candidate", func() *world { return newWorld(2).candidate(1, 1, Failed).retry("x") },
			27, Refuse, "There is nothing to retry for 2026.12.1: a dispatch without a retry reason would tag v2026.12.1-rc.2 at " + h2 + ". Dispatch again without one."},
		{"27 retry for an incomplete candidate", func() *world { return newWorld(1).candidate(1, 1, Qualified).retry("x") },
			27, Refuse, "There is nothing to retry for 2026.12.1: a dispatch without a retry reason would finish publishing v2026.12.1-rc.1. Dispatch again without one."},
		{"27 retry for a final start", func() *world { return green(1).retry("x") },
			27, Refuse, "There is nothing to retry for 2026.12.1: a dispatch without a retry reason would build v2026.12.1 at " + h1 + " for approval. Dispatch again without one."},
		{"27 retry for an unapproved final", func() *world { w := green(1); w.build("v2026.12.1", 1, Qualified); return w.retry("x") },
			27, Refuse, "There is nothing to retry for 2026.12.1: a dispatch without a retry reason would build v2026.12.1 at " + h1 + " for approval."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.world().inputs())
			if err != nil {
				t.Fatal(err)
			}
			returned[got.Action] = true
			if got.Row != tc.row || got.Action != tc.action {
				t.Fatalf("row %d (%s) %s, want row %d %s: %s %v", got.Row, got.State, got.Action, tc.row, tc.action, got.Message, got.Warnings)
			}
			if tc.action == Refuse {
				if !strings.HasPrefix(got.Message, tc.want) {
					t.Fatalf("message\n got %s\nwant %s", got.Message, tc.want)
				}
				return
			}
			if got.Message != "" || got.Version+" "+got.Commit != tc.want {
				t.Fatalf("action %s %s %s, want %s (message %q)", got.Action, got.Version, got.Commit, tc.want, got.Message)
			}
		})
	}

	// The release workflow is traced once per listed action, so the list
	// must name each action the table reaches exactly once.
	listed := slices.Sorted(slices.Values(Actions()))
	distinct := slices.Compact(slices.Clone(listed))
	if len(distinct) != len(listed) {
		t.Errorf("Actions() = %v has duplicates", Actions())
	}
	if reached := slices.Sorted(maps.Keys(returned)); !slices.Equal(reached, distinct) {
		t.Errorf("Decide returns %v over the state table, but Actions() = %v", reached, Actions())
	}
}

// head moves the branch to commit(n), extending the history.
func (w *world) head(n int) *world {
	for i := len(w.history) + 1; i <= n; i++ {
		w.history = append(w.history, commit(i))
	}
	w.in.Head = commit(n)
	return w
}

func TestDecideActionParameters(t *testing.T) {
	retry, err := Decide(newWorld(1).candidate(1, 1, Failed).retry("  runner lost \n").inputs())
	if err != nil || retry.Reason != "runner lost" {
		t.Fatalf("retry reason = %q, %v, want it trimmed", retry.Reason, err)
	}

	resume, err := Decide(newWorld(1).candidate(1, 1, Qualified).inputs())
	if err != nil || resume.Build != (runstatus.Attempt{Run: 101, Number: 1}) || resume.Digest != digestOf("v2026.12.1-rc.1@101") {
		t.Fatalf("resume = %+v, %v, want the recorded attempt and digest", resume, err)
	}

	final, err := Decide(green(1).inputs())
	if err != nil || final.Candidate != "2026.12.1-rc.1" || final.Reason != "" {
		t.Fatalf("final = %+v, %v, want candidate 2026.12.1-rc.1", final, err)
	}
}

func TestDecideRequiresAncestry(t *testing.T) {
	in := newWorld(2).candidate(1, 1, Qualified).inputs()
	in.Ancestry = nil
	if _, err := Decide(in); err == nil {
		t.Fatal("a missing ancestry was read as an answer")
	}
}
