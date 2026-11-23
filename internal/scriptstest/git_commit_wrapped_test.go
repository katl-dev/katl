package scriptstest

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCommitWrapped(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "git-commit-wrapped")
	run := func(dir, body string, args ...string) (string, error) {
		cmd := exec.Command(script, args...)
		cmd.Dir = dir
		cmd.Env = hermeticGitEnv(environmentWithout(), dir)
		cmd.Stdin = strings.NewReader(body)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	t.Run("wraps the body and keeps trailers on their own lines", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		long := strings.Repeat("word ", 30)
		if output, err := run(dir, long+"\n\nRelease-Note: skip\nSigned-off-by: A <a@example.com>\n", "area: summary", "--allow-empty"); err != nil {
			t.Fatalf("commit failed: %v\n%s", err, output)
		}

		lines := strings.Split(strings.TrimRight(runGit(t, dir, "log", "-1", "--format=%B"), "\n"), "\n")
		if lines[0] != "area: summary" || lines[1] != "" {
			t.Fatalf("message must start with the title and a blank line: %q", lines)
		}
		for _, line := range lines {
			if len(line) > 72 {
				t.Fatalf("line exceeds 72 columns: %q", line)
			}
		}
		tail := lines[len(lines)-2:]
		if tail[0] != "Release-Note: skip" || tail[1] != "Signed-off-by: A <a@example.com>" {
			t.Fatalf("trailers were not preserved: %q", tail)
		}
	})

	t.Run("rejects an empty body without committing", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		if output, err := run(dir, " \n", "area: summary", "--allow-empty"); err == nil {
			t.Fatalf("expected failure for empty body:\n%s", output)
		}
		if out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD").CombinedOutput(); err == nil {
			t.Fatalf("a commit was created: %s", out)
		}
	})

	t.Run("help succeeds", func(t *testing.T) {
		output, err := run(t.TempDir(), "", "--help")
		if err != nil || !strings.Contains(output, "usage: git commit-wrapped") {
			t.Fatalf("help failed: %v\n%s", err, output)
		}
	})
}
