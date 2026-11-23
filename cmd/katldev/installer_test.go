package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Git worktree nested inside a jj workspace, and a jj workspace nested inside
// a Git checkout, each own their own _build state.
func TestNearestRoot(t *testing.T) {
	for _, roots := range [][]string{
		{"/src/katl", "/src/katl/.claude/worktrees/agent"},
		{"/src/katl/.claude/worktrees/agent", "/src/katl"},
	} {
		got, err := nearestRoot(roots)
		if err != nil {
			t.Fatal(err)
		}
		if got != "/src/katl/.claude/worktrees/agent" {
			t.Errorf("nearestRoot(%q) = %q, want the nested checkout", roots, got)
		}
	}
}

// A longer path is not nearer when the checkouts are unrelated, and a
// symlinked spelling of the same checkout is not a different one.
func TestNearestRootRequiresAncestry(t *testing.T) {
	if _, err := nearestRoot([]string{"/src/katl", "/srv/other-checkout-with-a-longer-name"}); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("nearestRoot(unrelated) error = %v, want unrelated checkouts", err)
	}

	dir := t.TempDir()
	real := filepath.Join(dir, "katl")
	if err := os.MkdirAll(filepath.Join(real, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "a-much-longer-symlink-name")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := nearestRoot([]string{link, filepath.Join(real, "nested")})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(real, "nested"))
	if got != want {
		t.Fatalf("nearestRoot() = %q, want %q", got, want)
	}
}

// A published release ISO boots as given; the checkout is neither built nor
// consulted.
func TestPrepareInstallerUsesGivenISO(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "katl-installer.iso")
	if err := os.WriteFile(iso, []byte("release iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := installerManager{repoRoot: "/nonexistent-checkout", stdout: io.Discard, stderr: io.Discard}

	got, digest, err := manager.prepareInstaller(context.Background(), installerOptions{ISO: iso})

	if err != nil {
		t.Fatalf("prepareInstaller() error = %v", err)
	}
	if got != iso || len(digest) != 64 {
		t.Fatalf("prepareInstaller() = %q, %q, want the given ISO and its digest", got, digest)
	}
}
