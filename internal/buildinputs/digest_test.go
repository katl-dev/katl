package buildinputs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

var testSpec = Spec{
	Domain:  "test-domain",
	Version: "v1",
	Include: []string{"src/**"},
	Exclude: []string{".md"},
	Salts:   map[string]string{"only": ""},
}

func TestDigestsReadRevision(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "src/input", "first\n")
	first := commit(t, repo)
	atFirst := digest(t, repo, "HEAD", testSpec)

	writeFile(t, repo, "src/input", "second\n")
	commit(t, repo)
	// An uncommitted change must not reach any revision's digest.
	writeFile(t, repo, "src/input", "uncommitted\n")

	if got := digest(t, repo, first, testSpec); got != atFirst {
		t.Errorf("digest of %s = %s, want %s from when it was HEAD", first, got, atFirst)
	}
	if got := digest(t, repo, "HEAD", testSpec); got == atFirst {
		t.Errorf("digest of HEAD = %s, the same as its parent despite a changed input", got)
	}
}

func TestDigestsExclude(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "src/notes.md", "notes\n")
	writeFile(t, repo, "src/input", "input\n")
	commit(t, repo)
	before := digest(t, repo, "HEAD", testSpec)

	writeFile(t, repo, "src/notes.md", "changed notes\n")
	commit(t, repo)
	if got := digest(t, repo, "HEAD", testSpec); got != before {
		t.Errorf("changing an excluded file changed the digest from %s to %s", before, got)
	}

	writeFile(t, repo, "src/input", "changed input\n")
	commit(t, repo)
	if got := digest(t, repo, "HEAD", testSpec); got == before {
		t.Errorf("changing an included file beside an excluded one left the digest at %s", got)
	}
}

func TestDigestsDomain(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "src/input", "input\n")
	commit(t, repo)

	other := testSpec
	other.Domain = "other-domain"
	first, second := digest(t, repo, "HEAD", testSpec), digest(t, repo, "HEAD", other)
	if first == second {
		t.Errorf("domains %q and %q share digest %s", testSpec.Domain, other.Domain, first)
	}
	if !strings.HasPrefix(first, testSpec.Version+":") {
		t.Errorf("digest %s lacks version prefix %q", first, testSpec.Version)
	}
}

func digest(t *testing.T, repo, revision string, spec Spec) string {
	t.Helper()
	digests, err := Digests(repo, revision, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 1 || digests["only"] == "" {
		t.Fatalf("digests = %v, want one for salt only", digests)
	}
	return digests["only"]
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	// Digests runs git with the process environment, so confine it too.
	for _, entry := range gittest.Env(nil, repo) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	return repo
}

func writeFile(t *testing.T, repo, name, data string) {
	t.Helper()
	name = filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo string) string {
	t.Helper()
	runGit(t, repo, "add", "--all")
	runGit(t, repo, "commit", "-qm", "fixture")
	return runGit(t, repo, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
