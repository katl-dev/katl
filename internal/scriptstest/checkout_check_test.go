package scriptstest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/checkoutcheck"
	"github.com/katl-dev/katl/internal/gittest"
)

// A tool run from a checkout behind its local origin/main warns on stderr
// only, so the warning never changes output that scripts parse. The test
// environment suppresses the warning altogether.
func TestCheckoutWarning(t *testing.T) {
	tool := buildCommand(t, t.TempDir(), "katl-release")

	fixtures := t.TempDir()
	dir := filepath.Join(fixtures, "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(gittest.Env(os.Environ(), dir), "HOME="+fixtures)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/katl-dev/katl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "go.mod")
	git("commit", "--quiet", "-m", "base")
	git("commit", "--quiet", "--allow-empty", "-m", "upstream change")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("reset", "--quiet", "--hard", "HEAD~1")

	for _, tc := range []struct {
		name     string
		suppress string
		warning  string
	}{
		{name: "stale checkout", warning: "warning: Katl checkout is behind local origin/main by 1 commit(s)."},
		{name: "test environment", suppress: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(tool, "stable-tags", "v2026.11.1", "v2026.11.2-rc.1")
			cmd.Dir = dir
			cmd.Env = append(env, checkoutcheck.TestSuppress+"="+tc.suppress)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("katl-release stable-tags: %v\n%s", err, stderr.String())
			}

			if got := stdout.String(); got != "v2026.11.1\n" {
				t.Errorf("stdout = %q, want only the stable tag", got)
			}
			switch {
			case tc.warning == "" && stderr.Len() != 0:
				t.Errorf("stderr = %q, want no warning", stderr.String())
			case !strings.Contains(stderr.String(), tc.warning):
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.warning)
			}
		})
	}
}
