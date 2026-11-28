package main

import (
	"bytes"
	"context"
	"testing"
)

// A retry of the same run can upload artifacts with identical names. A
// resume must never silently take one of those instead of the recorded bytes.
func TestCandidateArtifacts(t *testing.T) {
	const bundle = "katl-2026.12.1-rc.1-x86_64-111111111111"
	const expired = "The bundle of v2026.12.1-rc.1 from https://github.com/katl-dev/katl/actions/runs/5/attempts/1 has expired, so its publication cannot be finished. Push a commit to release/2026.12.1; the next dispatch then tags v2026.12.1-rc.2."
	for _, tc := range []struct {
		name, wantErr string
		expired       bool
	}{
		{name: "recorded attempt"},
		{name: "expired despite newer bundle", expired: true, wantErr: expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			f.responses[repoPrefix+"actions/runs/5/attempts/2"] = runAttempt(5, 2, readerHead, "2026-12-01T01:00:00Z")
			item := func(id int, name, created string, expired bool) map[string]any {
				return map[string]any{"id": id, "name": name, "created_at": created, "expired": expired, "workflow_run": map[string]int{"id": 5}}
			}
			f.pages[repoPrefix+"actions/runs/5/artifacts?per_page=100"] = []any{map[string]any{"artifacts": []any{
				item(44, bundle, "2026-12-01T01:30:00Z", false),
				item(31, bundle, "2026-12-01T00:30:00Z", tc.expired),
				item(32, "katl-extension-bundles-standard-"+readerHead, "2026-12-01T00:25:00Z", false),
				item(33, "katl-extension-bundles-lts-"+readerHead, "2026-12-01T00:25:00Z", false),
			}}}
			var out bytes.Buffer
			err := candidateArtifacts(context.Background(), []string{"2026.12.1-rc.1", readerHead, "5", "1"}, &out, newTestReader(f))
			requireReleaseError(t, err, tc.wantErr)
			if want := "{\"bundle\":31,\"lts\":33,\"standard\":32}\n"; !tc.expired && out.String() != want {
				t.Errorf("artifacts = %q, want %q", &out, want)
			}
		})
	}
}
