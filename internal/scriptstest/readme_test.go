package scriptstest

import (
	"strings"
	"testing"
)

func TestReadmeReleaseBadgeUsesPublicationOrder(t *testing.T) {
	readme := string(mustReadFile(t, repoRoot(t)+"/README.md"))
	const badge = "https://img.shields.io/github/v/release/katl-dev/katl?include_prereleases&sort=date"
	if !strings.Contains(readme, badge) {
		t.Fatalf("README release badge must select the latest published prerelease with %q", badge)
	}
	if strings.Contains(readme, "include_prereleases&sort=semver") {
		t.Fatal("README release badge must not rank dev and alpha releases by SemVer identifiers")
	}
}
