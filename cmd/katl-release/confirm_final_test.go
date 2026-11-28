package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const finalFixtureDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// Confirming approval binds the final's qualified record to this attempt and
// digest, and re-runs the rules for branch movement and existing extensions.
func TestConfirmFinal(t *testing.T) {
	const extensionsRefusal = "Extension bundles for 2026.12.1 are already published with different contents. Published bytes are never replaced. Have a repository administrator delete those package versions if no release uses them and dispatch again; otherwise abandon 2026.12.1 and cut the next version."
	for _, tc := range []struct {
		name, attempt, digest, want string
	}{
		{name: "this attempt", attempt: "1", digest: finalFixtureDigest},
		{name: "other attempt", attempt: "2", digest: finalFixtureDigest, want: "this attempt has no qualified record"},
		{name: "other digest", attempt: "1", digest: strings.Repeat("1", 64), want: "this attempt has no qualified record"},
		{name: "head moved", attempt: "1", digest: finalFixtureDigest,
			want: "release/2026.12.1 moved from 111111111111 to 999999999999 after this run was dispatched. Dispatch the release action again."},
		{name: "extension exists", attempt: "1", digest: finalFixtureDigest, want: extensionsRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, attestation := finalWorld(t)
			switch tc.name {
			case "head moved":
				moved := strings.Repeat("9", 40)
				f.pages[repoPrefix+"git/matching-refs/heads/release/2026.12.1?per_page=100"] = []any{[]any{
					map[string]any{"ref": "refs/heads/release/2026.12.1", "object": map[string]string{"sha": moved, "type": "commit"}},
				}}
				f.responses[repoPrefix+"compare/"+readerHead+"..."+moved+"?per_page=1"] = map[string]string{"status": "ahead"}
			case "extension exists":
				f.registry.tags[registryPath(drbd9Repository)] = map[string]string{finalDRBD9Tag: finalDRBD9Digest}
			}
			t.Setenv("GITHUB_SERVER_URL", "https://github.com")
			t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
			t.Setenv("GITHUB_REF", "refs/heads/release/2026.12.1")
			t.Setenv("GITHUB_SHA", readerHead)
			t.Setenv("GITHUB_RUN_ID", "8")
			t.Setenv("GITHUB_RUN_ATTEMPT", tc.attempt)
			r := newTestReader(f)
			r.self = 8
			r.provenance = &provenance{call: attestation.call, repository: repository}

			err := confirmFinal(context.Background(), []string{"2026.12.1", readerHead, tc.digest}, r)
			requireReleaseError(t, err, tc.want)
			if (tc.name == "head moved" || tc.name == "extension exists") && err.Error() != tc.want {
				t.Errorf("refusal = %q, want the state-machine message %q", err, tc.want)
			}
		})
	}
}

// finalWorld has a published, attested candidate followed by a qualified
// final that is still inside its own Release run.
func finalWorld(t *testing.T) (*fakeGitHub, *fakeProvenance) {
	t.Helper()
	f := readerWorld(t)
	payloadDigest := strings.Repeat("c", 64)
	sums := []byte(payloadDigest + "  katlctl\n")
	candidateDigest := fmt.Sprintf("%x", sha256.Sum256(sums))
	const origin = "https://github.com/katl-dev/katl"
	const candidateURL = origin + "/actions/runs/7/attempts/1"
	const finalURL = origin + "/actions/runs/8/attempts/1"

	f.pages[repoPrefix+"commits/"+readerHead+"/statuses?per_page=100"] = []any{[]any{
		map[string]any{"context": "katl/release/v2026.12.1-rc.1", "state": "success", "description": "qualified sha256:" + candidateDigest,
			"target_url": candidateURL, "creator": botCreator},
		map[string]any{"context": "katl/release/v2026.12.1", "state": "success", "description": "qualified sha256:" + finalFixtureDigest,
			"target_url": finalURL, "creator": botCreator},
	}}
	for _, tc := range []struct {
		id                           int64
		status, started, built, name string
	}{
		{7, "completed", "2026-12-01T01:00:00Z", "2026-12-01T01:30:00Z", "katl-2026.12.1-rc.1-x86_64-111111111111"},
		{8, "in_progress", "2026-12-01T02:00:00Z", "2026-12-01T02:30:00Z", "katl-2026.12.1-x86_64-111111111111"},
	} {
		run := runAttempt(tc.id, 1, readerHead, tc.started)
		run["status"] = tc.status
		f.responses[fmt.Sprintf("%sactions/runs/%d/attempts/1", repoPrefix, tc.id)] = run
		f.responses[fmt.Sprintf("%sactions/runs/%d/attempts/2", repoPrefix, tc.id)] = &apiError{status: 404}
		f.pages[fmt.Sprintf("%sactions/runs/%d/attempts/1/jobs?per_page=100", repoPrefix, tc.id)] = []any{map[string]any{
			"jobs": []any{map[string]any{"name": "Build / Release validation", "conclusion": "success"}},
		}}
		f.pages[fmt.Sprintf("%sactions/runs/%d/artifacts?per_page=100", repoPrefix, tc.id)] = []any{map[string]any{
			"artifacts": []any{map[string]any{"name": tc.name, "created_at": tc.built, "workflow_run": map[string]any{"id": tc.id}}},
		}}
	}
	f.pages[runsQuery("in_progress")] = []any{map[string]any{"workflow_runs": []any{map[string]any{"id": 8}}}}
	f.registry.tags[registryPath(drbd9Repository)] = map[string]string{}
	f.pages[repoPrefix+"releases?per_page=100"] = []any{[]any{
		map[string]any{"tag_name": "v2026.12.1-rc.1", "prerelease": true, "assets": []any{
			map[string]any{"id": 11, "name": "SHA256SUMS", "state": "uploaded", "digest": "sha256:" + candidateDigest},
			map[string]any{"id": 12, "name": "katlctl", "state": "uploaded", "digest": "sha256:" + payloadDigest},
		}},
	}}
	f.raw[repoPrefix+"releases/assets/11"] = sums
	verified := fmt.Sprintf(`[{"verificationResult":{
		"signature":{"certificate":{
			"buildSignerURI":"%s/.github/workflows/release-artifacts.yml@refs/heads/release/2026.12.1",
			"buildConfigURI":"%s/.github/workflows/release.yml@refs/heads/release/2026.12.1",
			"runInvocationURI":"%s","sourceRepositoryDigest":"%s","sourceRepositoryRef":"refs/heads/release/2026.12.1"}},
		"statement":{"subject":[{"name":"SHA256SUMS","digest":{"sha256":"%s"}}]}}}]`, origin, origin, candidateURL, readerHead, candidateDigest)
	return f, &fakeProvenance{t: t, sums: sums, output: verified}
}
