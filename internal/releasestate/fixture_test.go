package releasestate

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/runstatus"
)

const (
	testServer = "https://github.com"
	testRepo   = "katl-dev/katl"
	testRef    = "refs/heads/release/2026.12.1"
)

// commit n of the branch history; its first 12 characters repeat the digit.
func commit(n int) string { return strings.Repeat(fmt.Sprint(n), 40) }

var bot = runstatus.Creator{Login: "github-actions", Type: "Bot", DatabaseID: 41898282}

// world is a release branch whose history is commit(1), commit(2), ... in
// order; any other commit is off the branch.
type world struct {
	in       Inputs
	history  []string
	run      int64
	attempts map[runstatus.Attempt]*Attempt
	// digests is the SHA256SUMS digest of each tag's latest qualified build.
	digests map[string]string
	// last locates the most recently written status.
	last struct {
		commit string
		index  int
	}
}

func newWorld(head int) *world {
	w := &world{in: Inputs{
		Server: testServer, Repository: testRepo, Ref: testRef, Head: commit(head),
		Statuses: map[string][]Status{}, Attempts: map[runstatus.Attempt]Attempt{},
		Attestations: map[string][]Attestation{}, Releases: map[string]Release{},
	}, run: 100, attempts: map[runstatus.Attempt]*Attempt{}, digests: map[string]string{}}
	for i := 1; i <= head; i++ {
		w.history = append(w.history, commit(i))
	}
	return w
}

func (w *world) tag(name string, at int) *world {
	w.in.Tags = append(w.in.Tags, Tag{Name: name, Commit: commit(at)})
	return w
}

func digestOf(tag string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(tag))) }

// build records one attempt of the tag's build at commit, which starts
// after every earlier one, and returns it so a case can spoil it.
func (w *world) build(tag string, at int, phase Phase) runstatus.Attempt {
	w.run++
	attempt := runstatus.Attempt{Run: w.run, Number: 1}
	sha := commit(at)
	run := runstatus.Run{
		ID: attempt.Run, Attempt: 1, Branch: "release/2026.12.1", SHA: sha, Path: ReleaseWorkflow,
		Event: "workflow_dispatch", Status: "completed", Conclusion: "success",
		StartedAt: time.Date(2026, 12, 1, 0, 0, int(w.run), 0, time.UTC),
	}
	run.Repository.FullName, run.HeadRepository.FullName = testRepo, testRepo
	jobs := []Job{{Name: "Decide", Conclusion: "success"}, {Name: "Build / Release validation", Conclusion: "success"}}
	record := &Record{Tag: tag, Phase: phase, Attempt: attempt}
	switch phase {
	case Building:
		// A cancelled job cannot write failed, so its record stays building.
		run.Conclusion = "cancelled"
		jobs[1].Conclusion = "cancelled"
	case Failed:
		run.Conclusion = "failure"
		jobs = append(jobs, Job{Name: "Build / Production VM (lts)", Conclusion: "failure"}, Job{Name: "Build / Production VM (standard)", Conclusion: "timed_out"})
		jobs[1].Conclusion = "skipped"
	case Qualified:
		record.Digest = digestOf(fmt.Sprintf("%s@%d", tag, w.run))
		w.digests[tag] = record.Digest
		ref := "refs/heads/release/2026.12.1"
		w.in.Attestations[record.Digest] = append(w.in.Attestations[record.Digest], Attestation{
			Subjects:       []string{record.Digest},
			SignerWorkflow: testServer + "/" + testRepo + "/" + artifactsWorkflow + "@" + ref,
			ConfigWorkflow: testServer + "/" + testRepo + "/" + ReleaseWorkflow + "@" + ref,
			Invocation:     attempt.URL(testServer, testRepo), SourceDigest: sha, SourceRef: ref,
		})
	}
	built, _ := releaseversion.ParseTag(tag)
	w.attempts[attempt] = &Attempt{Run: run, Jobs: jobs, Artifacts: []string{BundleName(built, sha)}}
	w.last.commit, w.last.index = sha, len(w.in.Statuses[sha])
	w.in.Statuses[sha] = append(w.in.Statuses[sha], Status{
		Context: "katl/release/" + tag, Creator: bot, TargetURL: attempt.URL(testServer, testRepo), Record: record,
	})
	return attempt
}

// candidate tags the candidate at commit and records one build of it.
func (w *world) candidate(n, at int, phase Phase) *world {
	tag := fmt.Sprintf("v2026.12.1-rc.%d", n)
	w.tag(tag, at)
	w.build(tag, at, phase)
	return w
}

// publish publishes the tag's release with the bytes of its qualified record.
func (w *world) publish(tag string, prerelease bool) *world {
	sums := map[string]string{"katl-installer.iso": digestOf(tag + "/iso"), "katlctl": digestOf(tag + "/katlctl")}
	assets := []Asset{{Name: "SHA256SUMS", Uploaded: true, Digest: "sha256:" + w.digests[tag]}}
	for name, digest := range sums {
		assets = append(assets, Asset{Name: name, Uploaded: true, Digest: "sha256:" + digest})
	}
	w.in.Releases[tag] = Release{URL: testServer + "/" + testRepo + "/releases/tag/" + tag, Prerelease: prerelease, Assets: assets, Sums: sums}
	return w
}

// green is rc.1 published at the head commit(at).
func green(at int) *world {
	return newWorld(at).candidate(1, at, Qualified).publish("v2026.12.1-rc.1", true)
}

// released is v2026.12.1 published from a green rc.1 at commit(at).
func released(at, head int) *world {
	w := newWorld(head).candidate(1, at, Qualified).publish("v2026.12.1-rc.1", true)
	w.build("v2026.12.1", at, Qualified)
	return w.tag("v2026.12.1", at).publish("v2026.12.1", false)
}

func (w *world) active(approval bool) *world {
	w.in.Active = &ActiveRun{URL: testServer + "/" + testRepo + "/actions/runs/900", ApprovalPending: approval}
	return w
}

func (w *world) retry(reason string) *world { w.in.Retry = reason; return w }

func (w *world) inputs() Inputs {
	version, _ := ParseRef(w.in.Ref)
	w.in.Attempts = map[runstatus.Attempt]Attempt{}
	for key, attempt := range w.attempts {
		w.in.Attempts[key] = *attempt
	}
	w.in.Ancestry = map[[2]string]bool{}
	for _, pair := range AncestryQueries(version, w.in.Head, w.in.Tags) {
		a, b := slices.Index(w.history, pair[0]), slices.Index(w.history, pair[1])
		w.in.Ancestry[pair] = a >= 0 && b >= 0 && a <= b
	}
	return w.in
}

// status is the most recently written status.
func (w *world) status() *Status { return &w.in.Statuses[w.last.commit][w.last.index] }

// attempt is the run attempt of the most recent build.
func (w *world) attempt() *Attempt { return w.attempts[runstatus.Attempt{Run: w.run, Number: 1}] }

// attestation is the attestation of the most recent qualified build.
func (w *world) attestation() *Attestation {
	digest := w.status().Record.Digest
	return &w.in.Attestations[digest][len(w.in.Attestations[digest])-1]
}
