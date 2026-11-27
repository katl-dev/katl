package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
	"github.com/katl-dev/katl/internal/retry"
	"github.com/katl-dev/katl/internal/runstatus"
)

// apiError is a GitHub API response with an HTTP status.
type apiError struct {
	status int
	text   string
}

func (e *apiError) Error() string { return e.text }

func notFound(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.status == 404
}

// transient reports whether a failed read may succeed when repeated: a
// network failure, a server error or a rate limit, but not a refusal.
func transient(err error) bool {
	var api *apiError
	if !errors.As(err, &api) || api.status == 0 {
		return true
	}
	return api.status >= 500 || api.status == 429 ||
		(api.status == 403 && strings.Contains(strings.ToLower(api.text), "rate limit"))
}

var httpStatus = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)

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
	text := fmt.Sprintf("gh api %s: %v: %s", endpoint, err, strings.TrimSpace(stderr.String()))
	status := 0
	if match := httpStatus.FindStringSubmatch(stderr.String()); match != nil {
		status, _ = strconv.Atoi(match[1])
	}
	return nil, &apiError{status: status, text: text}
}

// reader gathers the state machine's inputs from GitHub. Every read is
// retried with backoff while it fails transiently; a read that still fails
// fails the decision, because a missing answer is never "absent".
type reader struct {
	server, repository string
	read               func(ctx context.Context, endpoint string, paginate bool, accept string) ([]byte, error)
	// parse reads a status in the release record format.
	parse func(context, state, description, target string) (*releasestate.Record, error)
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
	head, err := r.refs(ctx, "heads/"+releasestate.Branch(version))
	if err != nil {
		return in, err
	}
	for _, branch := range head {
		if branch.Ref == "refs/heads/"+releasestate.Branch(version) {
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

type restStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	Creator     struct {
		Login string `json:"login"`
		ID    int    `json:"id"`
		Type  string `json:"type"`
	} `json:"creator"`
}

// records reads every status of the tagged commits, the run attempts that
// release records name, and the attestations of qualified digests.
func (r *reader) records(ctx context.Context, version releaseversion.Version, in *releasestate.Inputs) error {
	var commits []string
	for _, tag := range in.Tags {
		if !slices.Contains(commits, tag.Commit) {
			commits = append(commits, tag.Commit)
		}
	}
	prefix := "katl/release/" + version.Tag()
	for _, commit := range commits {
		var listed []restStatus
		if err := pages(ctx, r, r.repo("commits/"+commit+"/statuses?per_page=100"), func(page []restStatus) { listed = append(listed, page...) }); err != nil {
			return err
		}
		for _, raw := range listed {
			if raw.Context != prefix && !strings.HasPrefix(raw.Context, prefix+"-rc.") {
				continue
			}
			status := releasestate.Status{Context: raw.Context, TargetURL: raw.TargetURL, Creator: runstatus.Creator{
				Login: raw.Creator.Login, Type: raw.Creator.Type, DatabaseID: raw.Creator.ID,
			}}
			// REST names bots with a [bot] suffix that GraphQL and the trust rule omit.
			if status.Creator.Type == "Bot" {
				status.Creator.Login = strings.TrimSuffix(status.Creator.Login, "[bot]")
			}
			record, err := r.parse(raw.Context, raw.State, raw.Description, raw.TargetURL)
			if errors.Is(err, errRecordsUnreadable) {
				return err
			}
			if err != nil {
				status.Problem = err.Error()
			} else {
				status.Record = record
			}
			in.Statuses[commit] = append(in.Statuses[commit], status)
			if record == nil || !status.Creator.IsActionsBot() {
				continue
			}
			if err := r.attempt(ctx, record.Attempt, in); err != nil {
				return err
			}
			if record.Phase == releasestate.Qualified {
				if err := r.attestations(ctx, record.Digest, in); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// attempt reads a run attempt with its jobs and artifacts. An attempt that
// does not exist leaves the status unverified rather than failing the read,
// so a status naming a made-up run cannot block the release.
func (r *reader) attempt(ctx context.Context, attempt runstatus.Attempt, in *releasestate.Inputs) error {
	if _, done := in.Attempts[attempt]; done {
		return nil
	}
	run, err := runstatus.Read(ctx, func(ctx context.Context, endpoint string) ([]byte, error) {
		return r.get(ctx, endpoint, false, "")
	}, r.repository, attempt)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
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
			Name        string `json:"name"`
			WorkflowRun struct {
				ID int64 `json:"id"`
			} `json:"workflow_run"`
		} `json:"artifacts"`
	}
	if err := pages(ctx, r, r.repo(fmt.Sprintf("actions/runs/%d/artifacts?per_page=100", attempt.Run)), func(page artifactsPage) {
		for _, artifact := range page.Artifacts {
			if artifact.WorkflowRun.ID == attempt.Run {
				read.Artifacts = append(read.Artifacts, artifact.Name)
			}
		}
	}); err != nil {
		return err
	}
	in.Attempts[attempt] = read
	return nil
}

// Fulcio certificate extensions that identify a GitHub Actions signer.
var (
	oidSignerWorkflow = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	oidSourceDigest   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}
	oidSourceRef      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}
	oidConfigWorkflow = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 18}
	oidInvocation     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 21}
)

// attestations reads the attestations GitHub stores for a digest and the
// identity their certificates name. This reads the identity only: verifying
// the signatures needs the attested bytes, which the acting jobs verify with
// gh attestation verify before anything is published.
func (r *reader) attestations(ctx context.Context, digest string, in *releasestate.Inputs) error {
	if _, done := in.Attestations[digest]; done {
		return nil
	}
	in.Attestations[digest] = nil
	type attestationsPage struct {
		Attestations []struct {
			Bundle struct {
				VerificationMaterial struct {
					Certificate struct {
						RawBytes string `json:"rawBytes"`
					} `json:"certificate"`
					Chain struct {
						Certificates []struct {
							RawBytes string `json:"rawBytes"`
						} `json:"certificates"`
					} `json:"x509CertificateChain"`
				} `json:"verificationMaterial"`
				Envelope struct {
					Payload string `json:"payload"`
				} `json:"dsseEnvelope"`
			} `json:"bundle"`
		} `json:"attestations"`
	}
	var listed attestationsPage
	err := r.decode(ctx, r.repo("attestations/sha256:"+digest+"?per_page=100"), &listed)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, item := range listed.Attestations {
		raw := item.Bundle.VerificationMaterial.Certificate.RawBytes
		if raw == "" && len(item.Bundle.VerificationMaterial.Chain.Certificates) > 0 {
			raw = item.Bundle.VerificationMaterial.Chain.Certificates[0].RawBytes
		}
		attestation, ok := decodeAttestation(raw, item.Bundle.Envelope.Payload)
		if ok {
			in.Attestations[digest] = append(in.Attestations[digest], attestation)
		}
	}
	return nil
}

func decodeAttestation(certificate, payload string) (releasestate.Attestation, bool) {
	der, err := base64.StdEncoding.DecodeString(certificate)
	if err != nil {
		return releasestate.Attestation{}, false
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return releasestate.Attestation{}, false
	}
	var attestation releasestate.Attestation
	fields := map[string]*string{
		oidSignerWorkflow.String(): &attestation.SignerWorkflow, oidConfigWorkflow.String(): &attestation.ConfigWorkflow,
		oidInvocation.String(): &attestation.Invocation, oidSourceDigest.String(): &attestation.SourceDigest,
		oidSourceRef.String(): &attestation.SourceRef,
	}
	for _, extension := range cert.Extensions {
		if field, ok := fields[extension.Id.String()]; ok {
			if _, err := asn1.UnmarshalWithParams(extension.Value, field, "utf8"); err != nil {
				return releasestate.Attestation{}, false
			}
		}
	}
	statement, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return releasestate.Attestation{}, false
	}
	var parsed struct {
		Subject []struct {
			Digest struct {
				SHA256 string `json:"sha256"`
			} `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(statement, &parsed); err != nil {
		return releasestate.Attestation{}, false
	}
	for _, subject := range parsed.Subject {
		attestation.Subjects = append(attestation.Subjects, subject.Digest.SHA256)
	}
	return attestation, true
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
// content of each one's SHA256SUMS.
func (r *reader) releases(ctx context.Context, version releaseversion.Version, in *releasestate.Inputs) error {
	var listed []restRelease
	if err := pages(ctx, r, r.repo("releases?per_page=100"), func(page []restRelease) { listed = append(listed, page...) }); err != nil {
		return err
	}
	for _, release := range listed {
		if release.Draft || !slices.ContainsFunc(in.Tags, func(tag releasestate.Tag) bool { return tag.Name == release.TagName }) {
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
		}
		in.Releases[release.TagName] = read
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
