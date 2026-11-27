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

// requireUserNamespaces skips the test on hosts where Bubblewrap cannot
// create a user namespace.
func requireUserNamespaces(t *testing.T) {
	t.Helper()
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
}

func TestHermeticHidesHome(t *testing.T) {
	requireUserNamespaces(t)

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
	t.Setenv("KATLCTL_CONFIG", "")
	t.Setenv("KATL_HERMETIC_PROBE", "1")
	t.Setenv("KATL_HERMETIC_OPT_IN", "value with spaces")
	strayDir, err := os.MkdirTemp("/tmp", "katl-stray-git-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(strayDir) })
	strayGit := filepath.Join(strayDir, ".git")
	if err := os.Mkdir(strayGit, 0o700); err != nil {
		t.Fatal(err)
	}
	strayFile, err := os.CreateTemp("/tmp", "katl-stray-file-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(strayFile.Name()) })
	if err := strayFile.Close(); err != nil {
		t.Fatal(err)
	}
	boundFile, err := os.CreateTemp("/tmp", "katl-bound-file-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(boundFile.Name()) })
	if err := boundFile.Close(); err != nil {
		t.Fatal(err)
	}

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

	cmd := exec.Command(script, "--bind", scratch, "--env", "KATL_HERMETIC_OPT_IN", "--bind", run, "--bind", boundFile.Name(), "--", "sh", "-ec", `
		test "$HOME" = "$1"
		test ! -e "$1/.config/katl/katlctl.yaml"
		test ! -e "/proc/$4/root$1/.config/katl/katlctl.yaml"
		test ! -e "$1/main/unrelated"
		test -z "${GOENV+x}"
		test ! -e "$5"
		test ! -e "$6"
		test -z "${KATL_HERMETIC_PROBE+x}"
		test "$KATL_HERMETIC_OPT_IN" = 'value with spaces'
		test "$GOTELEMETRY" = off
		test "$KATLCTL_CONFIG" = "$PWD/_build/katlctl/config.yaml"
		test "$(git log -1 --format=%s)" = fixture
		: > written
		: > "$GOCACHE/written"
		: > "$GOMODCACHE/written"
		: > "$2/written"
		: > "$3/written"
		printf written > "$7"
	`, "hermetic-test", home, scratch, run, strconv.Itoa(os.Getpid()), strayGit, strayFile.Name(), boundFile.Name())
	cmd.Dir = worktree
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hermetic test: %v\n%s", err, output)
	}
	for _, dir := range []string{worktree, buildCache, moduleCache, scratch, run} {
		if _, err := os.Stat(filepath.Join(dir, "written")); err != nil {
			t.Errorf("write in %s did not reach the host: %v", dir, err)
		}
	}
	if data, err := os.ReadFile(boundFile.Name()); err != nil || string(data) != "written" {
		t.Errorf("write to a bound file did not reach the host: %q, %v", data, err)
	}

	cmd = exec.Command(script, "--", "printenv", "KATLCTL_CONFIG")
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), "KATLCTL_CONFIG=custom.yaml")
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != filepath.Join(worktree, "custom.yaml")+"\n" {
		t.Fatalf("worktree config: got %v\n%s", err, output)
	}
}

// Root-owned host files appear unowned inside the user namespace, so an SSH
// client that read the host's ssh_config would refuse the files it includes.
func TestHermeticHidesHostSSHConfig(t *testing.T) {
	requireUserNamespaces(t)
	if _, err := os.Stat("/etc/ssh/ssh_config"); err != nil {
		t.Skipf("host has no system SSH client configuration to hide: %v", err)
	}
	t.Setenv("KATLCTL_CONFIG", "")

	repo := repoRoot(t)
	cmd := exec.Command(filepath.Join(repo, "scripts/hermetic"), "--", "sh", "-ec", `test -z "$(ls -A /etc/ssh)"`)
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host /etc/ssh is visible under hermetic: %v\n%s", err, output)
	}
}

func TestHermeticKatlctlConfig(t *testing.T) {
	repo := repoRoot(t)
	outside := t.TempDir()
	for _, config := range []string{filepath.Join(outside, "config.yaml"), filepath.Join(repo, "..", "config.yaml")} {
		cmd := exec.Command(filepath.Join(repo, "scripts/hermetic"), "--env", "KATLCTL_CONFIG", "--", "true")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "KATLCTL_CONFIG="+config)
		if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "KATLCTL_CONFIG must point to a file inside the worktree") {
			t.Errorf("expected refusal for %s, got %v\n%s", config, err, output)
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
