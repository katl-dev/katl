package main

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrevision"
	"github.com/katl-dev/katl/internal/gittest"
)

// Wrong: deciding for every base, which holds a release branch to main's
// versions, or emitting nothing for main.
func TestPlanExtensionsBase(t *testing.T) {
	repo := t.TempDir()
	// planExtensions runs git with the process environment, so confine it too.
	for _, entry := range gittest.Env(nil, repo) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	put(t, repo, "extensions/bird/extension.env", "KATL_EXTENSION_NAME=bird\nKATL_EXTENSION_ARTIFACT_VERSION=v3.4.0-katl.7\n")
	put(t, repo, "extensions/drbd9/recipe.json", "{}\n")
	put(t, repo, "mkosi.conf", "[Build]\n")
	gitIn(t, repo, "add", "--all")
	gitIn(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	put(t, repo, "mkosi.conf", "[Build]\nchanged\n")
	gitIn(t, repo, "commit", "-qam", "head")

	actions, err := planExtensions(repo, "main", base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Name != "bird" || actions[0].Answer != extensionrevision.Unproven || !actions[0].rejects() {
		t.Errorf("actions into main = %+v, want one rejected unproven bird action", actions)
	}

	actions, err = planExtensions(repo, "release/2026.11", base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Errorf("actions into a release branch = %+v, want none", actions)
	}
}
