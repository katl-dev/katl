package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Without a current pin, scripts/mkosi builds the builder for the Fedora
// release that mkosi.conf selects.
func TestBuilderRelease(t *testing.T) {
	repo := repoRoot(t)
	fixture := builderFixture(t, "99", "FROM scratch\n")
	containers := newFakeContainers(t, "podman")

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "builder-version")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), containers.env("KATL_REPO_ROOT="+fixture, "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_IMAGE=", "GITHUB_STEP_SUMMARY=")...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("builder-version: %v\n%s", err, output)
	}

	builds := containers.callsStartingWith(t, "build")
	want := []string{"--build-arg FEDORA_RELEASE=99", "--tag localhost/katl-mkosi-builder:" + contentTag("99", "FROM scratch\n")}
	if len(builds) != 1 {
		t.Fatalf("builds = %q", builds)
	}
	for _, arg := range want {
		if !strings.Contains(builds[0], arg) {
			t.Fatalf("builder does not use selected release: %s", builds[0])
		}
	}
}
