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

	t.Run("refuses a long title without committing", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		title := "area: " + strings.Repeat("long ", 15)
		output, err := run(dir, "Body.\n", title, "--allow-empty")
		if err == nil || !strings.Contains(output, "shorten") {
			t.Fatalf("expected a shorten hint, got err=%v\n%s", err, output)
		}
		if out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD").CombinedOutput(); err == nil {
			t.Fatalf("a commit was created: %s", out)
		}
	})

	t.Run("keeps a long trailer on one line", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		note := "Release-Note: " + strings.Repeat("reader-facing ", 10) + "summary"
		if output, err := run(dir, "Body.\n\n"+note+"\n", "area: summary", "--allow-empty"); err != nil {
			t.Fatalf("commit failed: %v\n%s", err, output)
		}

		want := "area: summary\n\nBody.\n\n" + note + "\n"
		if got := runGit(t, dir, "log", "-1", "--format=%B"); strings.TrimRight(got, "\n")+"\n" != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
		// Git's own trailer parser must see the whole value.
		parsed := runGit(t, dir, "log", "-1", "--format=%(trailers:key=Release-Note,valueonly,unfold=false)")
		if got, want := strings.TrimSpace(parsed), strings.TrimPrefix(note, "Release-Note: "); got != want {
			t.Fatalf("parsed trailer = %q, want %q", got, want)
		}
	})

	t.Run("wraps a long body paragraph above several trailers", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		body := strings.Repeat("alpha ", 20) + "omega\n\nSecond paragraph.\n\n" +
			"Release-Note: " + strings.Repeat("long ", 20) + "end\n" +
			"Fixes: #503\n"
		if output, err := run(dir, body, "area: summary", "--allow-empty"); err != nil {
			t.Fatalf("commit failed: %v\n%s", err, output)
		}

		want := "area: summary\n\n" +
			"alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha\n" +
			"alpha alpha alpha alpha alpha alpha alpha alpha alpha omega\n\n" +
			"Second paragraph.\n\n" +
			"Release-Note: " + strings.Repeat("long ", 20) + "end\n" +
			"Fixes: #503\n"
		if got := runGit(t, dir, "log", "-1", "--format=%B"); strings.TrimRight(got, "\n")+"\n" != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	})

	t.Run("wraps a final paragraph that is not all trailers", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")

		long := "Note: " + strings.Repeat("word ", 30)
		if output, err := run(dir, "Body.\n\n"+long+"\nplain line\n", "area: summary", "--allow-empty"); err != nil {
			t.Fatalf("commit failed: %v\n%s", err, output)
		}
		for _, line := range strings.Split(runGit(t, dir, "log", "-1", "--format=%B"), "\n") {
			if len(line) > 72 {
				t.Fatalf("line exceeds 72 columns: %q", line)
			}
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
