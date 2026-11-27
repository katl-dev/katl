package releasestate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/runstatus"
)

// releaseValidationJob is the job whose success qualifies a build. Release
// runs it through release-artifacts.yml, so it is listed under the calling
// job's name.
const releaseValidationJob = "Release validation"

// trusted is a record that passed the trust rule.
type trusted struct {
	Record
	Commit  string
	attempt Attempt
}

// recordContext is the status context of a tag's record.
func recordContext(tag string) string { return "katl/release/" + tag }

// releaseOrigin accepts dispatched Release runs on the version's branch.
func (in Inputs) releaseOrigin(version releaseversion.Version) runstatus.Origin {
	branch := Branch(version)
	return runstatus.Origin{Repository: in.Repository, Workflow: ReleaseWorkflow, Events: map[string]func(string) bool{
		"workflow_dispatch": func(b string) bool { return b == branch },
	}}
}

// bundleName is the artifact a build of the tag's version at commit uploads.
func bundleName(tag, commit string) string {
	return "katl-" + strings.TrimPrefix(tag, "v") + "-x86_64-" + commit[:min(12, len(commit))]
}

// validation returns the conclusion of the attempt's Release validation job,
// or "" when it has not exactly one.
func validation(attempt Attempt) string {
	var found []string
	for _, job := range attempt.Jobs {
		if job.Name == releaseValidationJob || strings.HasSuffix(job.Name, " / "+releaseValidationJob) {
			found = append(found, job.Conclusion)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// verify applies the trust rule to one status of the tag's context on
// commit. It returns the record, or why the status is not one.
func (in Inputs) verify(version releaseversion.Version, tag, commit string, status Status) (trusted, string) {
	if !status.Creator.IsActionsBot() {
		return trusted{}, fmt.Sprintf("it was created by %s, not the GitHub Actions bot", status.Creator.Login)
	}
	if status.Record == nil {
		return trusted{}, status.Problem
	}
	record := *status.Record
	if attempt, ok := runstatus.ParseAttemptURL(in.Server, in.Repository, status.TargetURL); !ok || attempt != record.Attempt || record.Tag != tag {
		return trusted{}, "its target is not a run attempt of " + in.Repository
	}
	attempt, ok := in.Attempts[record.Attempt]
	if !ok || !in.releaseOrigin(version).Produced(attempt.Run, record.Attempt, commit) {
		return trusted{}, fmt.Sprintf("run attempt %s is not a dispatched Release run on %s at %s", record.Attempt, Branch(version), short(commit))
	}
	switch record.Phase {
	case Building:
	case Failed:
		if attempt.Run.Status != "completed" || validation(attempt) == "success" {
			return trusted{}, fmt.Sprintf("run attempt %s did not end without qualifying", record.Attempt)
		}
	case Qualified:
		if validation(attempt) != "success" {
			return trusted{}, fmt.Sprintf("Release validation did not succeed in run attempt %s", record.Attempt)
		}
		if !slices.Contains(attempt.Artifacts, bundleName(tag, commit)) {
			return trusted{}, fmt.Sprintf("run attempt %s has no bundle %s", record.Attempt, bundleName(tag, commit))
		}
		if !in.attested(version, commit, record) {
			return trusted{}, fmt.Sprintf("SHA256SUMS sha256:%s has no attestation from run attempt %s", record.Digest, record.Attempt)
		}
	default:
		return trusted{}, fmt.Sprintf("its phase %q is not a record phase", record.Phase)
	}
	return trusted{Record: record, Commit: commit, attempt: attempt}, ""
}

// attested reports whether the record's SHA256SUMS has an attestation from
// release-artifacts.yml as the record's attempt called it, at commit on the
// version's branch.
func (in Inputs) attested(version releaseversion.Version, commit string, record Record) bool {
	ref := "refs/heads/" + Branch(version)
	workflow := func(path string) string { return in.Server + "/" + in.Repository + "/" + path + "@" + ref }
	for _, attestation := range in.Attestations[record.Digest] {
		if slices.Contains(attestation.Subjects, record.Digest) &&
			attestation.SignerWorkflow == workflow(artifactsWorkflow) &&
			attestation.ConfigWorkflow == workflow(ReleaseWorkflow) &&
			attestation.Invocation == record.Attempt.URL(in.Server, in.Repository) &&
			attestation.SourceDigest == commit && attestation.SourceRef == ref {
			return true
		}
	}
	return false
}

// record selects the record of a tag's context on commit. A qualified record
// stands against later attempts unless replaceable, which only an untagged
// final is. Statuses of the context that fail the rule become warnings.
func (in Inputs) record(version releaseversion.Version, tag, commit string, replaceable bool, warnings *[]string) *trusted {
	var records []trusted
	for _, status := range in.Statuses[commit] {
		if status.Context != recordContext(tag) {
			continue
		}
		record, problem := in.verify(version, tag, commit, status)
		if problem != "" {
			*warnings = append(*warnings, fmt.Sprintf("Ignored status %s on %s: %s.", status.Context, short(commit), problem))
			continue
		}
		records = append(records, record)
	}

	var best *trusted
	pick := func(only Phase) {
		for i := range records {
			record := &records[i]
			if only != "" && record.Phase != only {
				continue
			}
			if best == nil || later(record.attempt, best.attempt) ||
				(record.Attempt == best.Attempt && progress(record.Phase) > progress(best.Phase)) {
				best = record
			}
		}
	}
	if !replaceable {
		pick(Qualified)
	}
	if best == nil {
		pick("")
	}
	return best
}

func progress(phase Phase) int {
	if phase == Building {
		return 0
	}
	return 1
}

// matches reports whether a published release carries exactly the record's
// bytes: the right pre-release flag, its SHA256SUMS with the recorded digest,
// and only the files SHA256SUMS lists, each with the digest it gives.
func matches(release Release, record *trusted, prerelease bool) bool {
	if release.Prerelease != prerelease || release.Sums == nil || record == nil || record.Phase != Qualified {
		return false
	}
	want := map[string]string{"SHA256SUMS": record.Digest}
	for name, digest := range release.Sums {
		if name == "SHA256SUMS" {
			return false
		}
		want[name] = digest
	}
	if len(release.Assets) != len(want) {
		return false
	}
	for _, asset := range release.Assets {
		digest, ok := want[asset.Name]
		if !ok || !asset.Uploaded || asset.Digest != "sha256:"+digest {
			return false
		}
		delete(want, asset.Name)
	}
	return len(want) == 0
}

// outcome describes how a record's attempt ended without qualifying.
func outcome(attempt Attempt) string {
	var failed []string
	for _, job := range attempt.Jobs {
		if job.Conclusion == "failure" || job.Conclusion == "timed_out" {
			failed = append(failed, job.Name)
		}
	}
	switch {
	case len(failed) > 0:
		return strings.Join(failed, ", ") + " failed"
	case attempt.Run.Conclusion == "cancelled":
		return "the run was cancelled"
	}
	return "the run ended without qualifying"
}

func short(commit string) string { return commit[:min(12, len(commit))] }
