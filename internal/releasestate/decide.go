package releasestate

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/releaserecord"
	"github.com/katl-dev/katl/internal/runstatus"
)

// Action is what the workflow does next.
type Action string

const (
	Candidate       Action = "candidate"
	RetryCandidate  Action = "retry-candidate"
	ResumeCandidate Action = "resume-candidate"
	Final           Action = "final"
	RetryFinal      Action = "retry-final"
	Refuse          Action = "refuse"
)

const (
	StateReleased                   = "released"
	StateSuperseded                 = "superseded"
	StateCandidateIncomplete        = "candidate-incomplete"
	StateExtensionsAlreadyPublished = "extensions-already-published"
	StateFinalStart                 = "final-start"
	StateFinalUnapproved            = "final-unapproved"
)

// RecordSummary is a trusted record as status reports it.
type RecordSummary struct {
	Tag    string
	Commit string
	Phase  Phase
	Digest string
	URL    string
}

// Decision is the state of a version and the one step it allows.
type Decision struct {
	// Row is the row of the design's state table, State its name.
	Row   int
	State string

	Action  Action
	Message string

	// Version is the candidate version to build or publish, or the final.
	Version string
	Commit  string
	// Candidate is the candidate a final is built from.
	Candidate string
	Reason    string
	// Build and Digest name the recorded build that resume-candidate publishes.
	Build  runstatus.Attempt
	Digest string

	CandidateRecord *RecordSummary
	FinalRecord     *RecordSummary
	Approval        string
	Warnings        []string
}

const removeOrAbandon = "Nothing was changed. Have a repository administrator remove %s if it was created by mistake, or abandon %s and cut the next version."

// Decide evaluates the rows in order and returns the first that holds. It
// fails only when the inputs lack an ancestry that AncestryQueries names.
func Decide(in Inputs) (Decision, error) {
	version, ok := ParseRef(in.Ref)
	if !ok {
		return refuse(1, "not-release-branch", "%s does not name one release. The release action runs only on the branch release/<version>, where <version> is a complete final version such as 2026.12.1. Dispatch it on the branch of the version to release.", in.Ref), nil
	}
	V := version.String()
	if in.Head == "" {
		return refuse(2, "branch-missing", "release/%s does not exist. Cut it from the selected main commit, then dispatch the release action on it.", V), nil
	}
	H := in.Head
	if in.Dispatch != "" && in.Dispatch != H {
		return refuse(3, "branch-moved", "release/%s moved from %s to %s after this run was dispatched. Dispatch the release action again.", V, short(in.Dispatch), short(H)), nil
	}

	valid, invalid := candidates(version, in.Tags)
	if invalid != nil {
		return refuse(4, "candidate-tag-invalid", "%s is not a candidate tag: candidates are v%s-rc.1, v%s-rc.2 and so on. "+removeOrAbandon, invalid.Name, V, V, invalid.Name, V), nil
	}
	for i, c := range valid {
		if c.number != uint64(i+1) {
			return refuse(5, "candidate-gap", "v%s-rc.%d is missing below %s: candidates are numbered from rc.1 without a gap. "+removeOrAbandon, V, i+1, c.tag.Name, c.tag.Name, V), nil
		}
	}
	var final *Tag
	for i := range in.Tags {
		if in.Tags[i].Name == version.Tag() {
			final = &in.Tags[i]
		}
	}

	ancestor := func(a, b string) (bool, error) {
		value, ok := in.Ancestry[[2]string{a, b}]
		if !ok {
			return false, fmt.Errorf("the ancestry of %s and %s was not read", short(a), short(b))
		}
		return value, nil
	}
	ordered := make([]Tag, 0, len(valid)+1)
	for _, c := range valid {
		ordered = append(ordered, c.tag)
	}
	if final != nil {
		ordered = append(ordered, *final)
	}
	for _, tag := range ordered {
		on, err := ancestor(tag.Commit, H)
		if err != nil {
			return Decision{}, err
		}
		if !on {
			return refuse(6, "tag-off-branch", "%s is at %s, which is not on release/%s (head %s). "+removeOrAbandon, tag.Name, short(tag.Commit), V, short(H), tag.Name, V), nil
		}
	}
	for i := 1; i < len(valid); i++ {
		a, b := valid[i-1].tag, valid[i].tag
		before, err := ancestor(a.Commit, b.Commit)
		if err != nil {
			return Decision{}, err
		}
		if !before || a.Commit == b.Commit {
			return refuse(7, "candidates-misordered", "%s at %s and %s at %s are out of order: each candidate is on a later commit than the one before it, and no commit carries two. "+removeOrAbandon, a.Name, short(a.Commit), b.Name, short(b.Commit), b.Name, V), nil
		}
	}

	var warnings []string
	var latest *trusted
	for i, c := range valid {
		record := in.record(version, c.tag.Name, c.tag.Commit, false, &warnings)
		if record == nil {
			d := refuse(8, "candidate-foreign", "%s has no status %s from a Release run on release/%s at %s, so the release action did not create it. "+removeOrAbandon, c.tag.Name, releaserecord.Context(strings.TrimPrefix(c.tag.Name, "v")), V, short(c.tag.Commit), c.tag.Name, V)
			d.Warnings = warnings
			return d, nil
		}
		if i == len(valid)-1 {
			latest = record
		}
	}

	d := Decision{Warnings: warnings}
	var N uint64
	var C, rcTag string
	if latest != nil {
		N, C, rcTag = uint64(len(valid)), latest.Commit, latest.Tag
		d.CandidateRecord = summary(in, latest)
	}
	K := N + 1
	var finalRecord *trusted
	if latest != nil {
		finalRecord = in.record(version, version.Tag(), C, final == nil, &d.Warnings)
		d.FinalRecord = summary(in, finalRecord)
	}
	if in.Active != nil && in.Active.ApprovalPending {
		d.Approval = in.Active.URL
	}
	set := func(row int, state string, action Action, format string, args ...any) Decision {
		d.Row, d.State, d.Action = row, state, action
		if action == Refuse {
			d.Message = fmt.Sprintf(format, args...)
		}
		return d
	}

	if final != nil {
		X := final.Commit
		var reason string
		switch {
		case latest == nil || X != C:
			reason = fmt.Sprintf("the final goes on the commit of the latest candidate, and that is not %s", short(X))
		case latest.Phase != Qualified:
			reason = fmt.Sprintf("v%s-rc.%d at that commit never qualified", V, N)
		case finalRecord == nil || finalRecord.Phase != Qualified:
			reason = fmt.Sprintf("no Release run on release/%s qualified v%s at that commit", V, V)
		}
		if reason != "" {
			return set(9, "final-foreign", Refuse, "v%s at %s was not created by the release action: %s. "+removeOrAbandon, V, short(X), reason, version.Tag(), V), nil
		}
		if release, published := in.Releases[version.Tag()]; !published {
			reason = "the tag exists and no release is published for it"
		} else if ok, mismatch := in.matches(version, release, finalRecord, false); !ok {
			reason = mismatch
		}
		if reason != "" {
			return set(10, "final-contradicts-record", Refuse, "The GitHub release v%s disagrees with what the release action recorded: %s. Published bytes are never replaced. Nothing was changed. If the release was edited or deleted by mistake, undo that and dispatch again; otherwise abandon %s and cut the next version.", V, reason, V), nil
		}
	}

	if in.Active != nil {
		url := in.Active.URL
		switch {
		case in.Active.ApprovalPending && final == nil && latest != nil && H != C:
			return set(11, "approval-superseded", Refuse, "v%s is waiting for approval at %s, and release/%s has moved to %s: %s. Reject the approval; the next dispatch then tags v%s-rc.%d at the head. Approving it publishes nothing, because the publish job refuses a branch that moved.", V, short(C), V, short(H), url, V, K), nil
		case in.Active.ApprovalPending:
			return set(12, "approval-pending", Refuse, "v%s is waiting for approval: %s. Approve or reject it there.", V, url), nil
		default:
			return set(13, "run-active", Refuse, "A Release run for %s is in progress: %s. Wait for it to finish, then run scripts/katl-release status %s.", V, url, V), nil
		}
	}

	if final != nil {
		if H == final.Commit {
			return set(14, StateReleased, Refuse, "v%s is released: %s. There is nothing left to do for %s.", V, in.Releases[version.Tag()].URL, V), nil
		}
		return set(15, "released-branch-ahead", Refuse, "v%s is released at %s, and release/%s has moved on to %s. A released version never changes; cut the next version for the later commits.", V, short(final.Commit), V, short(H)), nil
	}

	retry := strings.TrimSpace(in.Retry)
	if latest == nil {
		if retry == "" {
			d.Version, d.Commit = version.Candidate(1).String(), H
			return set(16, "first-candidate", Candidate, ""), nil
		}
		return set(27, "retry-not-applicable", Refuse, "There is nothing to retry for %s: a dispatch without a retry reason would tag v%s-rc.1 at %s. Dispatch again without one.", V, V, short(H)), nil
	}
	if H != C {
		if retry == "" {
			d.Version, d.Commit = version.Candidate(K).String(), H
			return set(17, StateSuperseded, Candidate, ""), nil
		}
		return set(27, "retry-not-applicable", Refuse, "There is nothing to retry for %s: a dispatch without a retry reason would tag v%s-rc.%d at %s. Dispatch again without one.", V, V, K, short(H)), nil
	}

	pre, prePublished := in.Releases[rcTag]
	if prePublished && latest.Phase != Qualified {
		return set(18, "prerelease-contradicts-record", Refuse, preContradicts, V, N, fmt.Sprintf("it is published, and no Release run qualified v%s-rc.%d", V, N), V, V, K), nil
	}
	if prePublished {
		if ok, mismatch := in.matches(version, pre, latest, true); !ok {
			return set(18, "prerelease-contradicts-record", Refuse, preContradicts, V, N, mismatch, V, V, K), nil
		}
	}
	candidateVersion := version.Candidate(N).String()
	if latest.Phase != Qualified {
		if retry == "" {
			return set(19, "candidate-failed", Refuse, "v%s-rc.%d did not qualify at %s: %s (%s). Push a fix to release/%s; the next dispatch then tags v%s-rc.%d. If the source is not at fault, dispatch again with a retry reason to rebuild v%s-rc.%d.", V, N, short(C), outcome(latest.attempt), latest.Attempt.URL(in.Server, in.Repository), V, V, K, V, N), nil
		}
		d.Version, d.Commit, d.Reason = candidateVersion, C, retry
		return set(20, "candidate-retry", RetryCandidate, ""), nil
	}
	if !prePublished {
		if retry == "" {
			d.Version, d.Commit, d.Build, d.Digest = candidateVersion, C, latest.Attempt, latest.Digest
			return set(21, StateCandidateIncomplete, ResumeCandidate, ""), nil
		}
		return set(27, "retry-not-applicable", Refuse, "There is nothing to retry for %s: a dispatch without a retry reason would finish publishing v%s-rc.%d. Dispatch again without one.", V, V, N), nil
	}

	// rc.N is published at the head.
	if len(in.ExtensionReferences) > 0 {
		return set(22, StateExtensionsAlreadyPublished, Refuse, "Extension bundles for %s are already published with different contents. Published bytes are never replaced. Have a repository administrator delete those package versions if no release uses them and dispatch again; otherwise abandon %s and cut the next version.", V, V), nil
	}
	buildFinal := func(reason string) { d.Version, d.Commit, d.Candidate, d.Reason = V, C, candidateVersion, reason }
	switch {
	case finalRecord == nil && retry == "":
		buildFinal("")
		return set(23, StateFinalStart, Final, ""), nil
	case finalRecord != nil && finalRecord.Phase != Qualified && retry == "":
		return set(24, "final-failed", Refuse, "v%s did not qualify at %s: %s (%s). v%s-rc.%d is still green. If the source is at fault, push a fix to release/%s; the next dispatch then tags v%s-rc.%d. Otherwise dispatch again with a retry reason to rebuild v%s.", V, short(C), outcome(finalRecord.attempt), finalRecord.Attempt.URL(in.Server, in.Repository), V, N, V, V, K, V), nil
	case finalRecord != nil && finalRecord.Phase != Qualified:
		buildFinal(retry)
		return set(25, "final-retry", RetryFinal, ""), nil
	case finalRecord != nil && retry == "":
		buildFinal("")
		return set(26, StateFinalUnapproved, Final, ""), nil
	}
	return set(27, "retry-not-applicable", Refuse, "There is nothing to retry for %s: a dispatch without a retry reason would build v%s at %s for approval. Dispatch again without one.", V, V, short(C)), nil
}

const preContradicts = "The GitHub pre-release v%s-rc.%d disagrees with what the release action recorded: %s. Published bytes are never replaced. Nothing was changed. Restore the pre-release flag if it was edited; otherwise push a commit to release/%s, and the next dispatch tags v%s-rc.%d."

func refuse(row int, state, format string, args ...any) Decision {
	return Decision{Row: row, State: state, Action: Refuse, Message: fmt.Sprintf(format, args...)}
}

func summary(in Inputs, record *trusted) *RecordSummary {
	if record == nil {
		return nil
	}
	return &RecordSummary{Tag: record.Tag, Commit: record.Commit, Phase: record.Phase, Digest: record.Digest, URL: record.Attempt.URL(in.Server, in.Repository)}
}
