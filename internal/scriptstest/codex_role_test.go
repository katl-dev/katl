package scriptstest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCodexRoleExec(t *testing.T) {
	repo := repoRoot(t)
	work := t.TempDir()
	bin := t.TempDir()
	argsPath := filepath.Join(work, "args")
	task := "Review \"this change\".\nKeep $HOME literal."
	writeArtifact(t, work, "task with spaces.md", task+"\n")
	writeFakeExecutable(t, bin, "codex", `if IFS= read -r unexpected; then
    printf 'unexpected stdin: %s\n' "$unexpected" >&2
    exit 3
fi
[[ /dev/stdin -ef /dev/null ]]
printf '%s\0' "$@" > "$KATL_FAKE_CODEX_ARGS"
printf 'progress\n' >&2
printf 'final message\n'
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KATL_FAKE_CODEX_ARGS", argsPath)

	for _, tc := range []struct {
		role, heading, sandbox, model, effort string
		taskArgs                              []string
	}{
		{"delivery", "Delivery", "workspace-write", "gpt-6.2-sol", "medium", []string{task}},
		{"reviewer", "Reviewer", "read-only", "gpt-6.2-astra", "high", []string{task}},
		{"debugger", "Debugger", "read-only", "gpt-6.2-astra", "high", []string{task}},
		{"debugger", "Debugger", "read-only", "gpt-6.2-astra", "low", []string{"--effort", "low", "@task with spaces.md"}},
	} {
		t.Run(tc.role+"/"+tc.effort, func(t *testing.T) {
			// A background caller can leave its input pipe open indefinitely.
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, filepath.Join(repo, "scripts", "codex-role"), append([]string{tc.role}, tc.taskArgs...)...)
			cmd.Dir = work
			cmd.Stdin = reader
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("launcher failed (context: %v): %v\n%s", ctx.Err(), err, &stderr)
			}
			if string(output) != "final message\n" {
				t.Fatalf("stdout = %q", output)
			}

			args := strings.Split(strings.TrimSuffix(string(mustReadFile(t, argsPath)), "\x00"), "\x00")
			want := []string{"exec", "-C", repo, "-s", tc.sandbox, "-m", tc.model, "-c", "model_reasoning_effort=" + tc.effort}
			if len(args) != len(want)+1 || !slices.Equal(args[:len(want)], want) {
				t.Fatalf("codex args = %q, want %q followed by one prompt", args, want)
			}

			prompt := args[len(want)]
			heading := "## " + tc.heading + "\n"
			if !strings.HasPrefix(prompt, heading) || !strings.Contains(prompt, "**Report.**") ||
				strings.Contains(strings.TrimPrefix(prompt, heading), "\n## ") ||
				!strings.HasSuffix(prompt, "\n\n"+task) {
				t.Fatalf("want only the %s section followed by the task, got %q", tc.heading, prompt)
			}
		})
	}
}

func TestCodexRoleRejectsInvalidTask(t *testing.T) {
	repo := repoRoot(t)
	work := t.TempDir()
	bin := t.TempDir()
	writeArtifact(t, work, "blank.md", "\n\t ")
	writeFakeExecutable(t, bin, "codex", "exit 99\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing role", nil, "usage:"},
		{"unknown role", []string{"journey", "task"}, "usage:"},
		{"missing task", []string{"delivery"}, "usage:"},
		{"extra argument", []string{"delivery", "task", "extra"}, "usage:"},
		{"missing effort", []string{"delivery", "--effort"}, "usage:"},
		{"empty effort", []string{"delivery", "--effort", "", "task"}, "usage:"},
		{"empty task", []string{"reviewer", ""}, "task must contain text"},
		{"blank task", []string{"reviewer", " \t\n"}, "task must contain text"},
		{"blank file", []string{"debugger", "@blank.md"}, "task must contain text"},
		{"missing file", []string{"debugger", "@missing.md"}, "cannot read task file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(repo, "scripts", "codex-role"), tc.args...)
			cmd.Dir = work
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(string(output), tc.want) {
				t.Fatalf("want exit 2 and %q, got %v\n%s", tc.want, err, output)
			}
		})
	}
}
