// Package releasestate owns the release state machine of
// docs/internal/release-state-machine.md: from what a reader gathered about
// one version's branch, tags, records, releases and runs, it chooses exactly
// one action or refusal.
package releasestate

import (
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/releaserecord"
	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/runstatus"
)

// Workflow paths the trust rule names.
const (
	ReleaseWorkflow   = ".github/workflows/release.yml"
	artifactsWorkflow = ".github/workflows/release-artifacts.yml"
	// ReleaseEnvironment is where the final waits for approval.
	ReleaseEnvironment = "release"
)

const branchPrefix = "refs/heads/release/"

// Phase is what one build attempt reached, as its record states.
type Phase = releaserecord.Phase

const (
	Building  = releaserecord.Building
	Failed    = releaserecord.Failed
	Qualified = releaserecord.Qualified
)

// Record is a status in the release record format, as releaserecord parses
// it: the tag its context names, its phase, the SHA256SUMS digest of a
// qualified record, and the run attempt its target names.
type Record struct {
	Tag     string
	Phase   Phase
	Digest  string
	Attempt runstatus.Attempt
}

// Status is one commit status as GitHub lists it. Record is nil when the
// status is not in the record format, and Problem then says why.
type Status struct {
	Context   string
	Creator   runstatus.Creator
	TargetURL string
	Record    *Record
	Problem   string
}

// Job is one job of a run attempt, in GitHub's listing order.
type Job struct {
	Name, Conclusion string
}

// Attempt is a run attempt that a status names.
type Attempt struct {
	Run  runstatus.Run
	Jobs []Job
	// Artifacts are the names of the artifacts this attempt uploaded, not
	// those of the run's other attempts.
	Artifacts []string
}

// Attestation is the verified identity of one build-provenance attestation
// of a published SHA256SUMS, with the digests of its subjects.
type Attestation struct {
	Subjects []string
	// SignerWorkflow is the workflow that signed, ConfigWorkflow the one the
	// run started from, each as <server>/<repo>/<path>@<ref>.
	SignerWorkflow, ConfigWorkflow string
	// Invocation is the run attempt URL.
	Invocation   string
	SourceDigest string
	SourceRef    string
}

// Asset is one asset of a published release.
type Asset struct {
	Name     string
	Uploaded bool
	// Digest is GitHub's asset digest, sha256:<hex>.
	Digest string
}

// Release is a published, non-draft GitHub release.
type Release struct {
	URL        string
	Prerelease bool
	Assets     []Asset
	// Sums is the content of its SHA256SUMS asset, name to hex digest, or
	// nil when it has none.
	Sums map[string]string
}

// ActiveRun is the oldest other Release run on the branch that is queued,
// in progress, waiting or requested.
type ActiveRun struct {
	URL             string
	ApprovalPending bool
}

// Tag is a tag of the version and the commit it peels to.
type Tag struct {
	Name, Commit string
}

// Inputs is everything a decision reads. A reader fills it; Decide reads
// nothing else.
type Inputs struct {
	Server, Repository string
	// Ref is the dispatched ref, or refs/heads/release/<argument> for status.
	Ref string
	// Head is the branch head, empty when the branch does not exist.
	Head string
	// Dispatch is the commit the run was dispatched at; empty outside the workflow.
	Dispatch string
	Retry    string
	// Tags are every tag v<V> and v<V>-rc.<suffix>.
	Tags []Tag
	// Ancestry holds the pairs AncestryQueries names: whether the first commit
	// is the second or one of its ancestors.
	Ancestry map[[2]string]bool
	Statuses map[string][]Status
	Attempts map[runstatus.Attempt]Attempt
	// Attestations are by SHA256SUMS digest, verified only for a published
	// SHA256SUMS whose digest a qualified record of its tag names.
	Attestations map[string][]Attestation
	// Releases are the published releases of the version's tags, by tag.
	Releases            map[string]Release
	Active              *ActiveRun
	ExtensionReferences []string
}

// ParseRef returns the final version that a release branch names.
func ParseRef(ref string) (releaseversion.Version, bool) {
	name, ok := strings.CutPrefix(ref, branchPrefix)
	if !ok {
		return releaseversion.Version{}, false
	}
	version, err := releaseversion.Parse(name)
	if err != nil || !version.IsFinal() {
		return releaseversion.Version{}, false
	}
	return version, true
}

// Branch is the release branch of a version, without refs/heads/.
func Branch(version releaseversion.Version) string { return "release/" + version.String() }

// Ref is the full ref of the version's release branch.
func Ref(version releaseversion.Version) string { return RefForVersion(version.String()) }

// RefForVersion preserves the requested version for the decision's refusal
// when the version does not name a release branch.
func RefForVersion(version string) string { return branchPrefix + version }

// IsVersionTag reports whether a tag is an input of the version: its final
// tag or one named v<V>-rc.<suffix>.
func IsVersionTag(version releaseversion.Version, name string) bool {
	return name == version.Tag() || strings.HasPrefix(name, version.Tag()+"-rc.")
}

type candidate struct {
	number uint64
	tag    Tag
}

// candidates returns the valid candidate tags in number order, or the first
// invalid one in name order.
func candidates(version releaseversion.Version, tags []Tag) ([]candidate, *Tag) {
	sorted := slices.Clone(tags)
	slices.SortFunc(sorted, func(a, b Tag) int { return strings.Compare(a.Name, b.Name) })
	var valid []candidate
	for _, tag := range sorted {
		if tag.Name == version.Tag() {
			continue
		}
		parsed, err := releaseversion.ParseTag(tag.Name)
		if err != nil || parsed.Stage != releaseversion.Candidate || parsed.Number == 0 || parsed.Base() != version {
			return nil, &tag
		}
		valid = append(valid, candidate{number: parsed.Number, tag: tag})
	}
	slices.SortFunc(valid, func(a, b candidate) int {
		switch {
		case a.number < b.number:
			return -1
		case a.number > b.number:
			return 1
		}
		return 0
	})
	return valid, nil
}

// AncestryQueries lists the commit pairs Decide reads from Inputs.Ancestry:
// each tag's commit against the head, and each candidate against the next.
func AncestryQueries(version releaseversion.Version, head string, tags []Tag) [][2]string {
	var pairs [][2]string
	add := func(pair [2]string) {
		if !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	for _, tag := range tags {
		add([2]string{tag.Commit, head})
	}
	valid, _ := candidates(version, tags)
	for i := 1; i < len(valid); i++ {
		add([2]string{valid[i-1].tag.Commit, valid[i].tag.Commit})
	}
	return pairs
}

// later orders attempts by start; a tie falls to the later run and attempt.
func later(a, b Attempt) bool {
	if !a.Run.StartedAt.Equal(b.Run.StartedAt) {
		return a.Run.StartedAt.After(b.Run.StartedAt)
	}
	if a.Run.ID != b.Run.ID {
		return a.Run.ID > b.Run.ID
	}
	return a.Run.Attempt > b.Run.Attempt
}
