package scriptstest

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// buildCommand builds ./cmd/<name> from the checkout into dir and returns its
// path. The binary carries no VCS stamp: tests do not read one, and in a
// linked worktree Go would take it from whatever .git directory lies above
// the checkout.
func buildCommand(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/"+name)
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return path
}
