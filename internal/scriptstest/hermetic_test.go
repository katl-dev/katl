package scriptstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestHermeticHidesHome(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("find bwrap (enter nix develop): %v", err)
	}
	if output, err := exec.Command(bwrap, "--unshare-user", "--dev-bind", "/", "/", "--", "true").CombinedOutput(); err != nil {
		msg := string(output)
		if strings.Contains(msg, "user namespace") || strings.Contains(msg, "Creating new namespace failed: Operation not permitted") {
			t.Skipf("user namespaces unavailable on this host: %s", msg)
		}
		t.Fatalf("probe Bubblewrap namespaces: %v\n%s", err, msg)
	}

	script := filepath.Join(repoRoot(t), "scripts/hermetic")
	home := t.TempDir()
	sentinelDir := filepath.Join(home, ".config/katl")
	main := filepath.Join(home, "main")
	worktree := filepath.Join(home, "linked worktree")
	buildCache := filepath.Join(home, "build cache")
	moduleCache := filepath.Join(home, "modules")
	scratch := filepath.Join(home, "scratch")
	run := filepath.Join(home, "run")
	for _, dir := range []string{sentinelDir, main, buildCache, moduleCache, scratch, run} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeArtifact(t, sentinelDir, "katlctl.yaml", "operator context\n")
	goEnv := writeArtifact(t, home, "go-env", "GOCACHE="+buildCache+"\nGOMODCACHE="+moduleCache+"\n")
	t.Setenv("HOME", home)
	t.Setenv("GOENV", goEnv)
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "fixture"},
		{"worktree", "add", "--quiet", "--detach", worktree},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = main
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	writeArtifact(t, main, "unrelated", "main checkout\n")

	cmd := exec.Command(script, "--bind", scratch, "--bind", run, "--", "sh", "-ec", `
		test "$HOME" = "$1"
		test ! -e "$1/.config/katl/katlctl.yaml"
		test ! -e "/proc/$4/root$1/.config/katl/katlctl.yaml"
		test ! -e "$1/main/unrelated"
		test ! -e "$GOENV"
		test "$(git log -1 --format=%s)" = fixture
		: > written
		: > "$GOCACHE/written"
		: > "$GOMODCACHE/written"
		: > "$2/written"
		: > "$3/written"
	`, "hermetic-test", home, scratch, run, strconv.Itoa(os.Getpid()))
	cmd.Dir = worktree
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hermetic test: %v\n%s", err, output)
	}
	for _, dir := range []string{worktree, buildCache, moduleCache, scratch, run} {
		if _, err := os.Stat(filepath.Join(dir, "written")); err != nil {
			t.Errorf("write in %s did not reach the host: %v", dir, err)
		}
	}
}

func TestHermeticRequiresBubblewrap(t *testing.T) {
	repo := repoRoot(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	cmd := exec.Command(filepath.Join(repo, "scripts/hermetic"), "--", bash, "-c", `: > "$1"`, "hermetic-test", marker)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+bin)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(string(output), "bwrap is required") {
		t.Fatalf("expected missing bwrap error, got %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran without Bubblewrap: stat error = %v", err)
	}
}
