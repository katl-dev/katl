package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestRunGitIgnoresInheritedGitEnvironment(t *testing.T) {
	sentinel := t.TempDir()
	runGit(t, sentinel, "init", "--quiet")
	sentinelConfig := filepath.Join(sentinel, ".git", "config")
	configBefore, err := os.ReadFile(sentinelConfig)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))
	t.Setenv("GIT_WORK_TREE", sentinel)

	fixture := t.TempDir()
	runGit(t, fixture, "init", "--quiet")
	runGit(t, fixture, "config", "fixture.key", "value")
	runGit(t, fixture, "commit", "--quiet", "--allow-empty", "-m", "fixture")

	if _, err := os.Stat(filepath.Join(fixture, ".git")); err != nil {
		t.Fatalf("fixture was not initialized: %v", err)
	}
	author := strings.TrimSpace(runGit(t, fixture, "log", "-1", "--format=%an <%ae>"))
	if author != gittest.Name+" <"+gittest.Email+">" {
		t.Fatalf("fixture author = %q", author)
	}
	configAfter, err := os.ReadFile(sentinelConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(configAfter) != string(configBefore) {
		t.Fatalf("sentinel config changed:\n%s", configAfter)
	}
	if heads, err := os.ReadDir(filepath.Join(sentinel, ".git", "refs", "heads")); err != nil || len(heads) != 0 {
		t.Fatalf("sentinel gained branches: %v, %v", heads, err)
	}
}

func TestHermeticGitStaysInsideFixture(t *testing.T) {
	sentinel := t.TempDir()
	runGit(t, sentinel, "init", "--quiet")
	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))

	// The fixture is not a repository, but its parent is.
	parent := t.TempDir()
	runGit(t, parent, "init", "--quiet")
	fixture := filepath.Join(parent, "fixture")
	if err := os.Mkdir(fixture, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = fixture
	cmd.Env = gittest.Env(os.Environ(), fixture)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git discovered a repository from a non-repository fixture: %s", output)
	}
}
