package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrevision"
	"github.com/katl-dev/katl/internal/gittest"
)

// Each case changes one fixture repository and runs the subcommand as the
// workflow does. Wrong: an exit status that disagrees with the decision, such
// as passing Unproven or failing Revalidate, or deciding for a release branch,
// which holds it to main's versions.
func TestRunExtensions(t *testing.T) {
	const definition = "extensions/bird/extension.env"
	version := func(v string) func(*testing.T, string) {
		return func(t *testing.T, repo string) {
			put(t, repo, definition, "KATL_EXTENSION_NAME=bird\nKATL_EXTENSION_ARTIFACT_VERSION="+v+"\n")
		}
	}
	change := func(name string) func(*testing.T, string) {
		return func(t *testing.T, repo string) { put(t, repo, name, "changed\n") }
	}

	for _, tc := range []struct {
		name    string
		baseRef string
		change  func(*testing.T, string)
		want    []extensionAction
		wantErr bool
	}{
		{
			name:    "removed",
			baseRef: "main",
			change: func(t *testing.T, repo string) {
				if err := os.Remove(filepath.Join(repo, definition)); err != nil {
					t.Fatal(err)
				}
			},
			want: []extensionAction{{Name: "bird", Decision: extensionrevision.Decision{Answer: extensionrevision.None}}},
		},
		{
			name:    "new version",
			baseRef: "main",
			change:  version("v3.4.0-katl.8"),
			want:    []extensionAction{{Name: "bird", Version: "v3.4.0-katl.8", Decision: extensionrevision.Decision{Answer: extensionrevision.NewVersion}}},
		},
		{
			name:    "unchanged",
			baseRef: "main",
			change:  change("docs/extensions.md"),
			want:    []extensionAction{{Name: "bird", Version: "v3.4.0-katl.7", Decision: extensionrevision.Decision{Answer: extensionrevision.Unchanged}}},
		},
		{
			name:    "revalidate",
			baseRef: "main",
			change:  change("internal/sysextimage/sysextimage.go"),
			want:    []extensionAction{{Name: "bird", Version: "v3.4.0-katl.7", Decision: extensionrevision.Decision{Answer: extensionrevision.Revalidate}}},
		},
		{
			name:    "invalid",
			baseRef: "main",
			change:  version("v3.4.0-katl.6"),
			want:    []extensionAction{{Name: "bird", Version: "v3.4.0-katl.6", Decision: extensionrevision.Decision{Answer: extensionrevision.Invalid}}},
			wantErr: true,
		},
		{
			name:    "unproven",
			baseRef: "main",
			change:  change("mkosi.conf"),
			want:    []extensionAction{{Name: "bird", Version: "v3.4.0-katl.7", Decision: extensionrevision.Decision{Answer: extensionrevision.Unproven}}},
			wantErr: true,
		},
		{
			name:    "release branch",
			baseRef: "release/2026.11",
			change:  change("mkosi.conf"),
			want:    []extensionAction{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			// runExtensions runs git with the process environment, so confine it too.
			for _, entry := range gittest.Env(nil, repo) {
				key, value, _ := strings.Cut(entry, "=")
				t.Setenv(key, value)
			}
			gitIn(t, repo, "init", "-q", "-b", "main")
			version("v3.4.0-katl.7")(t, repo)
			put(t, repo, "extensions/drbd9/recipe.json", "{}\n")
			put(t, repo, "mkosi.conf", "[Build]\n")
			put(t, repo, "internal/sysextimage/sysextimage.go", "package sysextimage\n")
			put(t, repo, "docs/extensions.md", "notes\n")
			gitIn(t, repo, "add", "--all")
			gitIn(t, repo, "commit", "-qm", "base")
			tc.change(t, repo)
			gitIn(t, repo, "add", "--all")
			gitIn(t, repo, "commit", "-qm", "head")
			t.Chdir(repo)

			var stdout strings.Builder
			err := runExtensions([]string{"--base-ref", tc.baseRef, "--base", "HEAD^1"}, &stdout, io.Discard)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("runExtensions error = %v, want error %t", err, tc.wantErr)
			}

			var got struct {
				Extensions []extensionAction `json:"extensions"`
			}
			if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
				t.Fatalf("stdout %q: %v", stdout.String(), err)
			}
			if len(got.Extensions) != len(tc.want) {
				t.Fatalf("extensions = %+v, want %+v", got.Extensions, tc.want)
			}
			for i, want := range tc.want {
				action := got.Extensions[i]
				if action.Name != want.Name || action.Version != want.Version || action.Answer != want.Answer || action.Reason == "" {
					t.Errorf("extension %d = %+v, want %s %q %s with a reason", i, action, want.Name, want.Version, want.Answer)
				}
			}
		})
	}
}
