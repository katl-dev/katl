package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/releaserecord"
	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/retry"
	"github.com/katl-dev/katl/internal/runstatus"
)

// apiError is a failed gh api call with the HTTP status gh reported, or
// status 0 when it reported none.
type apiError struct {
	status int
	// network is set for a failure without a status that never reached
	// GitHub, such as a refused connection or a timeout.
	network bool
	text    string
}

func (e *apiError) Error() string { return e.text }

func notFound(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.status == 404
}

// transient reports whether a failed read may succeed when repeated: a
// network failure, a server error or a rate limit, but not a refusal, a
// missing gh or missing credentials.
func transient(err error) bool {
	var api *apiError
	if !errors.As(err, &api) {
		return false
	}
	switch {
	case api.status == 0:
		return api.network
	case api.status >= 500, api.status == 429:
		return true
	}
	return api.status == 403 && strings.Contains(strings.ToLower(api.text), "rate limit")
}

var (
	httpStatus = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)
	// networkFailure matches what gh and Go's HTTP client print when a request
	// gets no response.
	networkFailure = regexp.MustCompile(`(?i)error connecting to|dial tcp|i/o timeout|connection reset|connection refused|no such host|TLS handshake timeout|timeout awaiting response|unexpected EOF`)
)

// ghAuthRequired is gh's exit status when it has no credentials.
const ghAuthRequired = 4

// ghRead runs gh api for one GET and classifies its failure from the status
// gh reports. With paginate, every page is read and returned as one array.
func ghRead(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
	args := []string{"api", endpoint}
	if paginate {
		args = append(args, "--paginate", "--slurp")
	}
	if accept != "" {
		args = append(args, "-H", "Accept: "+accept)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("gh api %s: install the GitHub CLI: %w", endpoint, err)
	}
	message := strings.TrimSpace(stderr.String())
	text := fmt.Sprintf("gh api %s: %v: %s", endpoint, err, message)
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == ghAuthRequired {
		return nil, &apiError{text: text + " (run gh auth login or set GH_TOKEN)"}
	}
	status := 0
	if match := httpStatus.FindStringSubmatch(message); match != nil {
		status, _ = strconv.Atoi(match[1])
	}
	return nil, &apiError{status: status, network: status == 0 && networkFailure.MatchString(message), text: text}
}

// reader gathers the state machine's inputs from GitHub. Every read is
// retried with backoff while it fails transiently; a read that still fails
// fails the decision, because a missing answer is never "absent".
type reader struct {
	server, repository string
	read               func(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error)
	// provenance verifies the attestation of a published SHA256SUMS.
	provenance *provenance
	// self is the run making the decision, excluded from the active runs.
	self int64
	// delay is the first retry delay; zero means the default.
	delay time.Duration
}

const readAttempts = 6

func (r *reader) get(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error) {
	var body []byte
	delay := r.delay
	if delay == 0 {
		delay = 2 * time.Second
	}
	backoff := retry.NewBackoff(delay, 15*delay)
	err := backoff.DoAttempts(ctx, readAttempts, func(ctx context.Context) error {
		var err error
		body, err = r.read(ctx, endpoint, paginate, accept)
		return err
	}, transient)
	return body, err
}

func (r *reader) repo(path string) string { return "repos/" + r.repository + "/" + path }

// decode reads one endpoint into value.
func (r *reader) decode(ctx context.Context, endpoint string, value any) error {
	body, err := r.get(ctx, endpoint, false, "")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nil
}

// pages reads every page of a list endpoint; each page decodes into page,
// and collect receives it.
func pages[T any](ctx context.Context, r *reader, endpoint string, collect func(T)) error {
	body, err := r.get(ctx, endpoint, true, "")
	if err != nil {
		return err
	}
	var all []T
	if err := json.Unmarshal(body, &all); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	for _, page := range all {
		collect(page)
	}
	return nil
}

type gitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// Read gathers the inputs for the version a ref names. A ref that names no
// release needs nothing more for its refusal.
func (r *reader) Read(ctx context.Context, ref, dispatch, retryReason string) (releasestate.Inputs, error) {
	in := releasestate.Inputs{
		Server: r.server, Repository: r.repository, Ref: ref, Dispatch: dispatch, Retry: retryReason,
		Statuses: map[string][]releasestate.Status{}, Attempts: map[runstatus.Attempt]releasestate.Attempt{},
		Attestations: map[string][]releasestate.Attestation{}, Releases: map[string]releasestate.Release{},
		Ancestry: map[[2]string]bool{},
	}
	version, ok := releasestate.ParseRef(ref)
	if !ok {
		return in, nil
	}
	defer r.provenance.close()
	head, err := r.refs(ctx, "heads/"+releasestate.Branch(version))
	if err != nil {
		return in, err
	}
	for _, branch := range head {
		if branch.Ref == releasestate.Ref(version) {
			in.Head = branch.Object.SHA
		}
	}
	if in.Head == "" {
		return in, nil
	}

	tags, err := r.refs(ctx, "tags/"+version.Tag())
	if err != nil {
		return in, err
	}
	for _, tag := range tags {
		name := strings.TrimPrefix(tag.Ref, "refs/tags/")
		if !releasestate.IsVersionTag(version, name) {
			continue
		}
		commit, err := r.peel(ctx, tag)
		if err != nil {
			return in, err
		}
		in.Tags = append(in.Tags, releasestate.Tag{Name: name, Commit: commit})
	}
	for _, pair := range releasestate.AncestryQueries(version, in.Head, in.Tags) {
		// GitHub compares base...head: "ahead" means head descends from base.
		var comparison struct{ Status string }
		if err := r.decode(ctx, r.repo("compare/"+pair[0]+"..."+pair[1]+"?per_page=1"), &comparison); err != nil {
			return in, err
		}
		in.Ancestry[pair] = comparison.Status == "identical" || comparison.Status == "ahead"
	}

	if err := r.records(ctx, version, &in); err != nil {
		return in, err
	}
	if err := r.releases(ctx, version, &in); err != nil {
		return in, err
	}
	if err := r.active(ctx, version, &in); err != nil {
		return in, err
	}
	in.ExtensionReferences, err = r.extensions(ctx, version)
	return in, err
}

// refs lists the refs that start with the prefix; GitHub matches by prefix,
// so callers keep only exact names.
func (r *reader) refs(ctx context.Context, prefix string) ([]gitRef, error) {
	var refs []gitRef
	err := pages(ctx, r, r.repo("git/matching-refs/"+prefix+"?per_page=100"), func(page []gitRef) { refs = append(refs, page...) })
	return refs, err
}

// peel follows annotated tags to the commit.
func (r *reader) peel(ctx context.Context, ref gitRef) (string, error) {
	object := ref.Object
	for range 8 {
		if object.Type == "commit" {
			return object.SHA, nil
		}
		if object.Type != "tag" {
			return "", fmt.Errorf("%s points to a %s, not a commit", ref.Ref, object.Type)
		}
		var tag struct {
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if err := r.decode(ctx, r.repo("git/tags/"+object.SHA), &tag); err != nil {
			return "", err
		}
		object.SHA, object.Type = tag.Object.SHA, tag.Object.Type
	}
	return "", fmt.Errorf("%s does not peel to a commit", ref.Ref)
}

// restStatus is a commit status as REST lists it, with its creator.
type restStatus struct {
	releaserecord.Status
	Creator struct {
		Login string `json:"login"`
		ID    int    `json:"id"`
		Type  string `json:"type"`
	} `json:"creator"`
}

// records reads every status of the tagged commits and the run attempts that
// release records name.
func (r *reader) records(ctx context.Context, version releaseversion.Version, in *releasestate.Inputs) error {
	var commits []string
	for _, tag := range in.Tags {
		if !slices.Contains(commits, tag.Commit) {
			commits = append(commits, tag.Commit)
		}
	}
	prefix := releaserecord.Context(version.String())
	for _, commit := range commits {
		var listed []restStatus
		if err := pages(ctx, r, r.repo("commits/"+commit+"/statuses?per_page=100"), func(page []restStatus) { listed = append(listed, page...) }); err != nil {
			return err
		}
		for _, raw := range listed {
			if raw.Context != prefix && !strings.HasPrefix(raw.Context, prefix+"-rc.") {
				continue
			}
			status := releasestate.Status{Context: raw.Context, TargetURL: raw.TargetURL,
				Creator: runstatus.RESTCreator(raw.Creator.Login, raw.Creator.Type, raw.Creator.ID)}
			parsed, err := releaserecord.Parse(raw.Status)
			if err != nil {
				status.Problem = err.Error()
			} else {
				status.Record = &releasestate.Record{Tag: "v" + parsed.Version, Phase: releasestate.Phase(parsed.Phase), Digest: parsed.Digest,
					Attempt: runstatus.Attempt{Run: parsed.Run.ID, Number: parsed.Run.Attempt}}
			}
			in.Statuses[commit] = append(in.Statuses[commit], status)
			// Only a record of a run in this repository names an attempt to read.
			if status.Record == nil || !status.Creator.IsActionsBot() || parsed.Run.Server != r.server || parsed.Run.Repository != r.repository {
				continue
			}
			if err := r.attempt(ctx, status.Record.Attempt, in); err != nil {
				return err
			}
		}
	}
	return nil
}

// attempt reads a run attempt with its jobs and the artifacts it uploaded.
// An attempt that does not exist leaves the status unverified rather than
// failing the read, so a status naming a made-up run cannot block the
// release.
func (r *reader) attempt(ctx context.Context, attempt runstatus.Attempt, in *releasestate.Inputs) error {
	if _, done := in.Attempts[attempt]; done {
		return nil
	}
	get := func(ctx context.Context, endpoint string) ([]byte, error) { return r.get(ctx, endpoint, false, "") }
	run, err := runstatus.Read(ctx, get, r.repository, attempt)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// GitHub lists a run's artifacts without the attempt that uploaded them.
	// Attempts of one run follow each other, so an attempt's artifacts are
	// those created from its start until the next attempt starts.
	var until time.Time
	following, err := runstatus.Read(ctx, get, r.repository, runstatus.Attempt{Run: attempt.Run, Number: attempt.Number + 1})
	switch {
	case notFound(err):
	case err != nil:
		return err
	default:
		until = following.StartedAt
	}

	read := releasestate.Attempt{Run: run}
	type jobsPage struct {
		Jobs []struct{ Name, Conclusion string } `json:"jobs"`
	}
	if err := pages(ctx, r, runstatus.AttemptEndpoint(r.repository, attempt)+"/jobs?per_page=100", func(page jobsPage) {
		for _, job := range page.Jobs {
			read.Jobs = append(read.Jobs, releasestate.Job{Name: job.Name, Conclusion: job.Conclusion})
		}
	}); err != nil {
		return err
	}
	type artifactsPage struct {
		Artifacts []struct {
			Name        string    `json:"name"`
			CreatedAt   time.Time `json:"created_at"`
			WorkflowRun struct {
				ID int64 `json:"id"`
			} `json:"workflow_run"`
		} `json:"artifacts"`
	}
	if err := pages(ctx, r, r.repo(fmt.Sprintf("actions/runs/%d/artifacts?per_page=100", attempt.Run)), func(page artifactsPage) {
		for _, artifact := range page.Artifacts {
			if artifact.WorkflowRun.ID == attempt.Run && !artifact.CreatedAt.Before(run.StartedAt) &&
				(until.IsZero() || artifact.CreatedAt.Before(until)) {
				read.Artifacts = append(read.Artifacts, artifact.Name)
			}
		}
	}); err != nil {
		return err
	}
	in.Attempts[attempt] = read
	return nil
}

type restRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
	Assets     []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// releases reads the published releases of the version's tags, with the
// content of each one's SHA256SUMS, and verifies the attestation of a
// SHA256SUMS that a qualified record names.
func (r *reader) releases(ctx context.Context, version releaseversion.Version, in *releasestate.Inputs) error {
	var listed []restRelease
	if err := pages(ctx, r, r.repo("releases?per_page=100"), func(page []restRelease) { listed = append(listed, page...) }); err != nil {
		return err
	}
	for _, release := range listed {
		tag := slices.IndexFunc(in.Tags, func(tag releasestate.Tag) bool { return tag.Name == release.TagName })
		if release.Draft || tag < 0 {
			continue
		}
		read := releasestate.Release{URL: release.HTMLURL, Prerelease: release.Prerelease}
		for _, asset := range release.Assets {
			read.Assets = append(read.Assets, releasestate.Asset{Name: asset.Name, Uploaded: asset.State == "uploaded", Digest: asset.Digest})
			if asset.Name != "SHA256SUMS" || asset.State != "uploaded" {
				continue
			}
			body, err := r.get(ctx, r.repo(fmt.Sprintf("releases/assets/%d", asset.ID)), false, "application/octet-stream")
			if err != nil {
				return err
			}
			// Unreadable sums cannot match a record; the release then reads as
			// contradicting it.
			if sums, err := decodeChecksums(body); err == nil {
				read.Sums = sums
			}
			if err := r.attest(version, in, in.Tags[tag], asset.Digest, body); err != nil {
				return err
			}
		}
		in.Releases[release.TagName] = read
	}
	return nil
}

// attest verifies the attestation of a published SHA256SUMS when a qualified
// record of its tag, on the tag's commit, names its digest. Only such a
// release can match a record, so no other attestation is read.
func (r *reader) attest(version releaseversion.Version, in *releasestate.Inputs, tag releasestate.Tag, assetDigest string, sums []byte) error {
	digest, ok := strings.CutPrefix(assetDigest, "sha256:")
	if !ok {
		return nil
	}
	if _, done := in.Attestations[digest]; done {
		return nil
	}
	named := slices.ContainsFunc(in.Statuses[tag.Commit], func(status releasestate.Status) bool {
		record := status.Record
		return record != nil && status.Creator.IsActionsBot() && record.Tag == tag.Name &&
			record.Phase == releasestate.Qualified && record.Digest == digest
	})
	if !named {
		return nil
	}

	file, err := os.CreateTemp("", "katl-release-SHA256SUMS-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(sums)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	out, err := r.provenance.verify(file.Name(), releasestate.Ref(version), tag.Commit)
	if err != nil {
		return fmt.Errorf("SHA256SUMS of %s: %w", tag.Name, err)
	}
	var verified []struct {
		VerificationResult struct {
			Signature struct {
				Certificate struct {
					BuildSignerURI         string `json:"buildSignerURI"`
					BuildConfigURI         string `json:"buildConfigURI"`
					RunInvocationURI       string `json:"runInvocationURI"`
					SourceRepositoryDigest string `json:"sourceRepositoryDigest"`
					SourceRepositoryRef    string `json:"sourceRepositoryRef"`
				} `json:"certificate"`
			} `json:"signature"`
			Statement struct {
				Subject []struct {
					Digest struct {
						SHA256 string `json:"sha256"`
					} `json:"digest"`
				} `json:"subject"`
			} `json:"statement"`
		} `json:"verificationResult"`
	}
	if err := json.Unmarshal(out, &verified); err != nil {
		return fmt.Errorf("decode gh attestation verify output for SHA256SUMS of %s: %w", tag.Name, err)
	}
	in.Attestations[digest] = []releasestate.Attestation{}
	for _, result := range verified {
		certificate := result.VerificationResult.Signature.Certificate
		attestation := releasestate.Attestation{
			SignerWorkflow: certificate.BuildSignerURI, ConfigWorkflow: certificate.BuildConfigURI,
			Invocation: certificate.RunInvocationURI, SourceDigest: certificate.SourceRepositoryDigest,
			SourceRef: certificate.SourceRepositoryRef,
		}
		for _, subject := range result.VerificationResult.Statement.Subject {
			attestation.Subjects = append(attestation.Subjects, subject.Digest.SHA256)
		}
		in.Attestations[digest] = append(in.Attestations[digest], attestation)
	}
	return nil
}

type restRun struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// active finds the oldest other Release run on the branch that is working,
// and whether it waits for approval in the release environment.
func (r *reader) active(ctx context.Context, version releaseversion.Version, in *releasestate.Inputs) error {
	var oldest *restRun
	for _, status := range []string{"queued", "in_progress", "waiting", "requested"} {
		endpoint := r.repo(fmt.Sprintf("actions/workflows/%s/runs?branch=%s&status=%s&per_page=100",
			url.PathEscape(path.Base(releasestate.ReleaseWorkflow)), url.QueryEscape(releasestate.Branch(version)), status))
		type runsPage struct {
			Runs []restRun `json:"workflow_runs"`
		}
		if err := pages(ctx, r, endpoint, func(page runsPage) {
			for _, run := range page.Runs {
				if run.ID != r.self && (oldest == nil || run.CreatedAt.Before(oldest.CreatedAt)) {
					oldest = &run
				}
			}
		}); err != nil {
			return err
		}
	}
	if oldest == nil {
		return nil
	}
	in.Active = &releasestate.ActiveRun{URL: oldest.HTMLURL}
	var pending []struct {
		Environment struct {
			Name string `json:"name"`
		} `json:"environment"`
	}
	if err := r.decode(ctx, r.repo(fmt.Sprintf("actions/runs/%d/pending_deployments", oldest.ID)), &pending); err != nil {
		return err
	}
	for _, deployment := range pending {
		if deployment.Environment.Name == releasestate.ReleaseEnvironment {
			in.Active.ApprovalPending = true
		}
	}
	return nil
}

// extensions lists the registry tags of the repository's extension packages
// that belong to the final version.
func (r *reader) extensions(ctx context.Context, version releaseversion.Version) ([]string, error) {
	owner, name, _ := strings.Cut(r.repository, "/")
	var packages []string
	type pkg struct {
		Name string `json:"name"`
	}
	if err := pages(ctx, r, "orgs/"+owner+"/packages?package_type=container&per_page=100", func(page []pkg) {
		for _, p := range page {
			if strings.HasPrefix(p.Name, name+"/extensions/") {
				packages = append(packages, p.Name)
			}
		}
	}); err != nil {
		return nil, err
	}
	var references []string
	for _, p := range packages {
		type packageVersion struct {
			Name     string `json:"name"`
			Metadata struct {
				Container struct {
					Tags []string `json:"tags"`
				} `json:"container"`
			} `json:"metadata"`
		}
		if err := pages(ctx, r, "orgs/"+owner+"/packages/container/"+url.PathEscape(p)+"/versions?per_page=100", func(page []packageVersion) {
			for _, v := range page {
				for _, tag := range v.Metadata.Container.Tags {
					if extensionrelease.IsReleaseTagOf(tag, version.String()) {
						references = append(references, p+":"+tag+"@"+v.Name)
					}
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	return references, nil
}
