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
	call command
}

func runPublication(ctx context.Context, args []string, stderr io.Writer, r *reader, call command) error {
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
		if err := verifyBundle(dir, releasestate.Ref(version.Base()), commit, stderr, call); err != nil {
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
		return p.draft(ctx, stderr)
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

func (p publication) listed(ctx context.Context) (published *restRelease, drafts []restRelease, err error) {
	list, err := p.releaseList(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, release := range list {
		if release.TagName != p.version.Tag() {
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

func (p publication) draft(ctx context.Context, stderr io.Writer) error {
	published, drafts, err := p.listed(ctx)
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
		if _, err := p.write("DELETE", p.releaseEndpoint(draft.ID)); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, "Drafting", p.version.Tag())
	created, err := p.write("POST", p.repo("releases"),
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
		if _, err := p.write("POST", endpoint, "--input", filepath.Join(p.dir, name), "-H", "Content-Type: application/octet-stream"); err != nil {
			return err
		}
	}

	draft, err = p.readRelease(ctx, draft.ID)
	if err != nil {
		return err
	}
	if !draft.Draft || !p.matches(draft) {
		return fmt.Errorf("draft of %s does not match the bundle's release flag and asset digests", p.version.Tag())
	}
	return nil
}

func (p publication) publish(ctx context.Context, stderr io.Writer) error {
	published, drafts, err := p.listed(ctx)
	if err != nil {
		return err
	}
	if published != nil {
		if !p.matches(*published) {
			return p.differentContents()
		}
		fmt.Fprintln(stderr, p.version.Tag(), "is already published.")
		return p.confirmPublished(ctx, published.ID)
	}
	if len(drafts) == 0 {
		return fmt.Errorf("no draft of %s exists. Run katl-release draft first", p.version.Tag())
	}
	if len(drafts) != 1 || !p.matches(drafts[0]) {
		return fmt.Errorf("no unique draft of %s matches the bundle. Run katl-release draft first", p.version.Tag())
	}

	draft := drafts[0]
	fields := []string{"-F", "draft=false", "-f", "tag_name=" + p.version.Tag(), "-f", "target_commitish=" + p.commit}
	if !p.version.IsFinal() {
		fields = append(fields, "-f", "make_latest=false")
	}
	// Publishing the draft also creates its tag at the bound commit.
	// Do not retry the write. A later invocation can read back a completed publish.
	fmt.Fprintln(stderr, "Publishing", p.version.Tag())
	if _, err := p.write("PATCH", p.releaseEndpoint(draft.ID), fields...); err != nil {
		return err
	}
	return p.confirmPublished(ctx, draft.ID)
}

func (p publication) confirmPublished(ctx context.Context, id int64) error {
	tag := p.version.Tag()
	var ref gitRef
	if err := p.decode(ctx, p.repo("git/ref/tags/"+tag), &ref); err != nil {
		return err
	}
	commit, err := p.peel(ctx, ref)
	if err != nil {
		return err
	}
	if commit != p.commit {
		return fmt.Errorf("published tag %s points to %s, expected %s", tag, commit, p.commit)
	}

	release, err := p.readRelease(ctx, id)
	if err != nil {
		return err
	}
	if release.Draft || !p.matches(release) {
		return fmt.Errorf("published release %s does not match the bundle's release flag and asset digests", tag)
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

func (p publication) readRelease(ctx context.Context, id int64) (restRelease, error) {
	var release restRelease
	err := p.decode(ctx, p.releaseEndpoint(id), &release)
	return release, err
}

func (p publication) matches(release restRelease) bool {
	if release.TagName != p.version.Tag() || release.Prerelease == p.version.IsFinal() {
		return false
	}
	content := releasestate.Release{Sums: p.expected}
	for _, asset := range release.Assets {
		content.Assets = append(content.Assets, asset.asset())
	}
	return content.MatchesAssets(p.digest)
}

func (p publication) write(method, endpoint string, args ...string) ([]byte, error) {
	data, err := p.call("gh", append([]string{"api", "--method", method, endpoint}, args...)...)
	if err != nil {
		return nil, ghFailure("gh api "+method+" "+endpoint, err, stderrOf(err))
	}
	return data, nil
}
