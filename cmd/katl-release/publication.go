package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
)

type publication struct {
	version     releaseversion.Version
	commit, dir string
	sums        []byte
	digest      string
	expected    map[string]string
	*reader
	call func(context.Context, string, ...string) ([]byte, error)
}

const (
	publicationAttempts = 3
	publicationTimeout  = 2 * time.Minute
)

func runPublication(ctx context.Context, args []string, stderr io.Writer, r *reader, call func(context.Context, string, ...string) ([]byte, error)) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: scripts/katl-release %s VERSION COMMIT DIRECTORY", args[0])
	}
	version, err := publicationVersion(args[1])
	if err != nil {
		return err
	}
	commit, dir := args[2], args[3]
	if !fullSHA.MatchString(commit) {
		return errors.New("COMMIT must be a full commit SHA")
	}
	p := publication{version: version, commit: commit, dir: dir, reader: r, call: call}
	if args[0] == "draft" {
		verify := func(name string, args ...string) ([]byte, error) {
			return call(ctx, name, args...)
		}
		if err := verifyBundle(dir, releasestate.Ref(version.Base()), commit, stderr, verify); err != nil {
			return err
		}
	}

	p.sums, err = os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	p.expected, err = decodeChecksums(p.sums)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(p.sums)
	p.digest = hex.EncodeToString(sum[:])
	if args[0] == "draft" {
		// Large bundles can take longer to upload than the publication request.
		ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		defer cancel()
		backoff := p.backoff()
		// A draft that changed under a stale listing is retried so the next
		// attempt lists the release it became.
		return backoff.DoAttempts(ctx, publicationAttempts, func(ctx context.Context) error {
			return p.draft(ctx, stderr)
		}, func(err error) bool { return transient(err) || errors.Is(err, errNotDraft) })
	}
	return p.publish(ctx, stderr)
}

func publicationVersion(value string) (releaseversion.Version, error) {
	v, err := validateVersion(value)
	if err != nil {
		return v, err
	}
	if !v.IsFinal() && (v.Stage != releaseversion.Candidate || v.Number == 0) {
		return v, fmt.Errorf("%s is neither a release candidate nor a final version", value)
	}
	return v, nil
}

func (r *reader) releasesForTag(ctx context.Context, tag string) (published *restRelease, drafts []restRelease, err error) {
	list, err := r.releaseList(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, release := range list {
		if release.TagName != tag {
			continue
		}
		if release.ID <= 0 {
			return nil, nil, fmt.Errorf("GitHub returned an invalid release ID for %s", release.TagName)
		}
		if release.Draft {
			drafts = append(drafts, release)
		} else {
			if published != nil {
				return nil, nil, fmt.Errorf("GitHub lists multiple published releases for %s", release.TagName)
			}
			published = &release
		}
	}
	return published, drafts, nil
}

func (p publication) listForPublish(ctx context.Context) (*restRelease, []restRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationTimeout)
	defer cancel()
	return p.releasesForTag(ctx, p.version.Tag())
}

func (p publication) draft(ctx context.Context, stderr io.Writer) error {
	published, drafts, err := p.releasesForTag(ctx, p.version.Tag())
	if err != nil {
		return err
	}
	if published != nil {
		if !slices.ContainsFunc(published.Assets, func(asset restReleaseAsset) bool {
			return asset.Name == "SHA256SUMS" && asset.State == "uploaded" && asset.Digest == "sha256:"+p.digest
		}) {
			return p.differentContents()
		}
		fmt.Fprintln(stderr, p.version.Tag(), "is already published.")
		return nil
	}

	notes := filepath.Join(p.dir, renderedNotes)
	if _, ok := p.expected[renderedNotes]; !ok {
		return fmt.Errorf("the release bundle is missing %s", renderedNotes)
	}
	for _, draft := range drafts {
		// A fresh draft keeps an interrupted upload from supplying any assets.
		if err := p.deleteDraft(ctx, draft.ID); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, "Drafting", p.version.Tag())
	created, err := p.write(ctx, "POST", p.repo("releases"),
		"-f", "tag_name="+p.version.Tag(), "-f", "target_commitish="+p.commit,
		"-f", "name=KatlOS "+p.version.String(), "-F", "body=@"+notes,
		"-F", "draft=true", "-F", "prerelease="+strconv.FormatBool(!p.version.IsFinal()))
	if err != nil {
		return err
	}
	var draft restRelease
	if err := json.Unmarshal(created, &draft); err != nil {
		return fmt.Errorf("decode created draft: %w", err)
	}
	if draft.ID <= 0 || !draft.Draft || draft.TagName != p.version.Tag() {
		return fmt.Errorf("GitHub did not create a draft for %s", p.version.Tag())
	}
	names := []string{"SHA256SUMS"}
	for name := range p.expected {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintln(stderr, "Uploading", name)
		// Upload by release ID so a tag lookup cannot select a different release.
		endpoint := "https://uploads.github.com/" + p.releaseEndpoint(draft.ID) + "/assets?name=" + url.QueryEscape(name)
		if _, err := p.write(ctx, "POST", endpoint, "--input", filepath.Join(p.dir, name), "-H", "Content-Type: application/octet-stream"); err != nil {
			return err
		}
	}

	// GitHub can answer 404 for the draft it has just created and list its
	// assets before every upload shows.
	_, err = awaitReadback(ctx, p.reader, func(ctx context.Context) (restRelease, bool, error) {
		release, err := p.readRelease(ctx, draft.ID)
		switch {
		case err != nil:
			return release, false, err
		case release.Draft && p.matches(release):
			return release, true, nil
		case release.Draft && p.assetsPending(release):
			return release, false, nil
		}
		return release, false, fmt.Errorf("draft of %s does not match the bundle's release flag and asset digests", p.version.Tag())
	}, notFound)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("draft of %s still lacks uploaded assets of the bundle after %s", p.version.Tag(), readbackWait)
	}
	return err
}

func (p publication) publish(ctx context.Context, stderr io.Writer) error {
	published, drafts, err := p.listForPublish(ctx)
	if err != nil {
		return err
	}
	if published != nil {
		if !p.matches(*published) {
			return p.differentContents()
		}
		fmt.Fprintln(stderr, p.version.Tag(), "is already published.")
		return p.finishPublish(ctx, published.ID, drafts)
	}
	if len(drafts) == 0 {
		return fmt.Errorf("no draft of %s exists. Run katl-release draft first", p.version.Tag())
	}
	if len(drafts) != 1 || !p.matches(drafts[0]) {
		return fmt.Errorf("no unique draft of %s matches the bundle. Run katl-release draft first", p.version.Tag())
	}

	draft := drafts[0]
	makeLatest := "false"
	if p.version.IsFinal() {
		makeLatest = "legacy"
	}
	fields := []string{"-F", "draft=false", "-f", "tag_name=" + p.version.Tag(),
		"-f", "target_commitish=" + p.commit, "-f", "make_latest=" + makeLatest}
	fmt.Fprintln(stderr, "Publishing", p.version.Tag())

	publishedID := draft.ID
	var orphaned []restRelease
	backoff := p.backoff()
	sameDraft := false
	err = backoff.DoAttempts(ctx, publicationAttempts, func(ctx context.Context) error {
		sameDraft = false
		patchCtx, cancel := context.WithTimeout(ctx, publicationTimeout)
		_, patchErr := p.write(patchCtx, "PATCH", p.releaseEndpoint(draft.ID), fields...)
		patchContextErr := patchCtx.Err()
		cancel()
		if patchErr == nil {
			return nil
		}
		if patchContextErr != nil {
			patchErr = errors.Join(patchErr, patchContextErr)
		}

		// A failed response may follow a successful publish. Retry only while
		// the same verified draft is still the unique release to publish.
		published, currentDrafts, err := p.listForPublish(ctx)
		if err != nil {
			return errors.Join(patchErr, fmt.Errorf("check publication after PATCH: %w", err))
		}
		if published != nil {
			if !p.matches(*published) {
				return p.differentContents()
			}
			publishedID = published.ID
			orphaned = currentDrafts
			// A different release won. Check the original by ID even if
			// it was omitted from the reconciliation listing.
			if publishedID != draft.ID && !slices.ContainsFunc(orphaned, func(r restRelease) bool { return r.ID == draft.ID }) {
				orphaned = append(orphaned, draft)
			}
			return nil
		}
		sameDraft = len(currentDrafts) == 1 && currentDrafts[0].ID == draft.ID && p.matches(currentDrafts[0])
		return patchErr
	}, func(err error) bool { return sameDraft && (transient(err) || errors.Is(err, context.DeadlineExceeded)) })
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w; rerun `scripts/katl-release publish %s %s %s` to reconcile", err, p.version, p.commit, p.dir)
	}
	if err != nil {
		return err
	}
	return p.finishPublish(ctx, publishedID, orphaned)
}

func (p publication) finishPublish(ctx context.Context, publishedID int64, drafts []restRelease) error {
	ctx, cancel := context.WithTimeout(ctx, publicationTimeout)
	defer cancel()
	if err := p.confirmPublished(ctx, publishedID); err != nil {
		return err
	}
	for _, draft := range drafts {
		if draft.ID == publishedID {
			continue
		}
		backoff := p.backoff()
		err := backoff.DoAttempts(ctx, publicationAttempts, func(ctx context.Context) error {
			return p.deleteDraft(ctx, draft.ID)
		}, transient)
		if errors.Is(err, errNotDraft) {
			continue
		}
		if err != nil {
			return fmt.Errorf("confirmed %s, but could not remove orphaned draft %d: %w", p.version.Tag(), draft.ID, err)
		}
	}
	return nil
}

func (p publication) confirmPublished(ctx context.Context, id int64) error {
	tag := p.version.Tag()
	var ref gitRef
	// The tag can become visible shortly after the published release.
	backoff := p.backoff()
	err := backoff.DoAttempts(ctx, publicationAttempts, func(ctx context.Context) error {
		return p.decode(ctx, p.repo("git/ref/tags/"+tag), &ref)
	}, notFound)
	if err != nil {
		return err
	}
	commit, err := p.peel(ctx, ref)
	if err != nil {
		return err
	}
	if commit != p.commit {
		return fmt.Errorf("published tag %s points to %s, expected %s", tag, commit, p.commit)
	}

	// GitHub can still report a release it has just published as a draft.
	release, err := awaitReadback(ctx, p.reader, func(ctx context.Context) (restRelease, bool, error) {
		release, err := p.readRelease(ctx, id)
		switch {
		case err != nil:
			return release, false, err
		case !p.matches(release):
			return release, false, fmt.Errorf("published release %s does not match the bundle's release flag and asset digests", tag)
		}
		return release, !release.Draft, nil
	}, nil)
	if errors.Is(err, context.DeadlineExceeded) && release.Draft {
		return fmt.Errorf("GitHub still reports %s as a draft after %s", tag, readbackWait)
	}
	if err != nil {
		return err
	}
	for _, asset := range release.Assets {
		if asset.Name != "SHA256SUMS" {
			continue
		}
		body, err := p.assetContent(ctx, asset.ID)
		if err != nil {
			return err
		}
		if !bytes.Equal(body, p.sums) {
			return fmt.Errorf("published SHA256SUMS of %s differs from the bundle", tag)
		}
		return nil
	}
	return fmt.Errorf("published release %s has no SHA256SUMS asset", tag)
}

func (p publication) differentContents() error {
	return fmt.Errorf("%s is already published with different contents. Published bytes are never replaced.", p.version.Tag())
}

func (p publication) releaseEndpoint(id int64) string {
	return p.repo("releases/" + strconv.FormatInt(id, 10))
}

// errNotDraft reports that a listed release is no longer a draft of this
// version's tag, so it was not deleted.
var errNotDraft = errors.New("release is no longer a draft of this version")

// deleteDraft deletes release id only while it is still a draft of this
// version's tag. A listing can be stale, so the bound ID is read again
// immediately before the DELETE. A release that is already gone counts as
// deleted.
func (p publication) deleteDraft(ctx context.Context, id int64) error {
	current, err := p.readRelease(ctx, id)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.ID != id {
		return fmt.Errorf("GitHub returned release ID %d when reading release %d", current.ID, id)
	}
	if !current.Draft || current.TagName != p.version.Tag() {
		return fmt.Errorf("release %d: %w", id, errNotDraft)
	}

	_, err = p.write(ctx, "DELETE", p.releaseEndpoint(id))
	if notFound(err) {
		return nil
	}
	return err
}

func (p publication) readRelease(ctx context.Context, id int64) (restRelease, error) {
	var release restRelease
	err := p.decode(ctx, p.releaseEndpoint(id), &release)
	return release, err
}

// assetsPending reports a release whose assets GitHub has not finished
// listing: no asset contradicts the bundle, but some are missing, not yet
// uploaded or without a digest.
func (p publication) assetsPending(release restRelease) bool {
	return p.classify(release) == releasestate.AssetsPending
}

func (p publication) matches(release restRelease) bool {
	return p.classify(release) == releasestate.AssetsMatch
}

// classify compares a release with the bundle. A wrong tag or release flag
// contradicts the bundle whatever the assets show.
func (p publication) classify(release restRelease) releasestate.AssetState {
	if release.TagName != p.version.Tag() || release.Prerelease == p.version.IsFinal() {
		return releasestate.AssetsContradict
	}
	content := releasestate.Release{Sums: p.expected}
	for _, asset := range release.Assets {
		content.Assets = append(content.Assets, asset.asset())
	}
	return content.ClassifyAssets(p.digest)
}

func (p publication) write(ctx context.Context, method, endpoint string, args ...string) ([]byte, error) {
	return writeGitHub(ctx, p.call, method, endpoint, args...)
}

func writeGitHub(ctx context.Context, call func(context.Context, string, ...string) ([]byte, error), method, endpoint string, args ...string) ([]byte, error) {
	data, err := call(ctx, "gh", append([]string{"api", "--method", method, endpoint}, args...)...)
	if err != nil {
		return nil, ghFailure("gh api "+method+" "+endpoint, err, stderrOf(err))
	}
	return data, nil
}
