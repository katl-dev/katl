package scriptstest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodexRoleExec(t *testing.T) {
	repo := repoRoot(t)
	bin := t.TempDir()
	fakeCodexSystemd(t, bin)
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, "codex config")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	xdgConfig := filepath.Join(home, "xdg config")
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("GH_CONFIG_DIR", "")
	ghConfig := filepath.Join(xdgConfig, "gh")
	if err := os.MkdirAll(ghConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	buildCache, moduleCache := filepath.Join(home, ".cache", "go-build"), filepath.Join(home, "go", "pkg", "mod")
	for _, dir := range []string{buildCache, moduleCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOCACHE", buildCache)
	t.Setenv("GOMODCACHE", moduleCache)

	task := "Review \"this change\".\nKeep $HOME literal."
	writeFakeExecutable(t, bin, "codex", `if IFS= read -r unexpected; then
    printf 'unexpected stdin: %s\n' "$unexpected" >&2
    exit 3
fi
[[ /dev/stdin -ef /dev/null ]]
printf '%s\0' "$@" > codex-args
printf '%s\0' "$PATH" "$HOME" "$CODEX_HOME" "$XDG_CONFIG_HOME" > codex-env
printf 'progress\n' >&2
printf 'final message'
exit "$CODEX_TEST_STATUS"
`)

	for _, tc := range []struct {
		role, heading, sandbox, model, effort string
		input                                 string
		options                               []string
		status                                int
	}{
		{"delivery", "Delivery", "workspace-write", "gpt-6.2-sol", "medium", task, nil, 0},
		{"reviewer", "Reviewer", "read-only", "gpt-6.2-astra", "medium", task, nil, 0},
		{"debugger", "Debugger", "read-only", "gpt-6.2-astra", "high", task, nil, 23},
		{"debugger", "Debugger", "read-only", "gpt-6.2-astra", "low", "@task with spaces.md", []string{"--effort", "low"}, 0},
	} {
		t.Run(tc.role+"/"+tc.effort, func(t *testing.T) {
			work := t.TempDir()
			writeArtifact(t, work, "task with spaces.md", task+"\n")

			// A background caller can leave its input pipe open indefinitely.
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			const taskName = "issue:413-v2.review_service.service"
			const logName = "codex report.log"
			logPath := filepath.Join(work, logName)
			launcherArgs := append([]string{tc.role, "--log", logName}, tc.options...)
			launcherArgs = append(launcherArgs, taskName, tc.input)
			cmd := exec.CommandContext(ctx, filepath.Join(repo, "scripts", "codex-role"), launcherArgs...)
			cmd.Dir = work
			cmd.Stdin = reader
			status := strconv.Itoa(tc.status)
			t.Setenv("CODEX_TEST_STATUS", status)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if tc.status == 0 && err != nil {
				t.Fatalf("want exit 0, got %v (context: %v)\n%s", err, ctx.Err(), &stderr)
			}
			if tc.status != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.status {
					t.Fatalf("want exit %s, got %v (context: %v)\n%s", status, err, ctx.Err(), &stderr)
				}
			}
			if !strings.Contains(string(output), logPath) {
				t.Fatalf("stdout = %q", output)
			}
			if log := string(mustReadFile(t, logPath)); log != "progress\nfinal message\nexit="+status+"\n" {
				t.Fatalf("log = %q, want progress, final message and exit=%s", log, status)
			}

			unit := "katl-codex-" + tc.role + "-" + taskName + ".service"
			if checked := string(mustReadFile(t, filepath.Join(work, "checked-unit"))); checked != unit+"\n" {
				t.Fatalf("checked unit = %q, want %q", checked, unit)
			}
			runArgs := strings.Split(string(mustReadFile(t, filepath.Join(work, "systemd-run-args"))), "\x00")
			unitIndex := slices.Index(runArgs, "--unit")
			if unitIndex < 0 || runArgs[unitIndex+1] != unit {
				t.Fatalf("systemd-run did not launch %q", unit)
			}
			for _, flag := range []string{"--user", "--collect", "--wait", "--same-dir", "--expand-environment=no"} {
				if !slices.Contains(runArgs, flag) {
					t.Errorf("systemd-run missing %s", flag)
				}
			}
			for _, property := range []string{
				"ProtectHome=tmpfs", "PrivateUsers=yes", `TemporaryFileSystem="` + home + `":ro`,
				`BindPaths="` + repo + `"`, `BindPaths="` + work + `"`, `BindPaths="` + codexHome + `"`,
				`BindPaths="` + buildCache + `"`, `BindPaths="` + moduleCache + `"`, `BindReadOnlyPaths="` + ghConfig + `"`,
			} {
				index := slices.Index(runArgs, property)
				if index <= 0 || runArgs[index-1] != "-p" {
					t.Errorf("systemd-run missing property %s", property)
				}
			}
			if env := string(mustReadFile(t, filepath.Join(work, "codex-env"))); env != strings.Join([]string{path, home, codexHome, xdgConfig, ""}, "\x00") {
				t.Fatalf("unexpected Codex environment %q", env)
			}

			args := strings.Split(strings.TrimSuffix(string(mustReadFile(t, filepath.Join(work, "codex-args"))), "\x00"), "\x00")
			want := []string{"exec", "-C", repo, "-s", tc.sandbox, "-m", tc.model, "-c", "model_reasoning_effort=" + tc.effort}
			if tc.role == "delivery" {
				want = append(want, "-c", "sandbox_workspace_write.writable_roots=[]", "--add-dir", buildCache, "--add-dir", moduleCache)
			}
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

func TestCodexRoleRejectsLaunch(t *testing.T) {
	repo := repoRoot(t)
	bin := t.TempDir()
	fakeCodexSystemd(t, bin)
	writeFakeExecutable(t, bin, "codex", "exit 99\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tc := range []struct {
		name   string
		args   []string
		status int
		want   string
	}{
		{"missing role", nil, 2, "usage:"},
		{"unknown role", []string{"journey", "job", "task"}, 2, "usage:"},
		{"missing name", []string{"delivery", "task"}, 2, "usage:"},
		{"empty name", []string{"delivery", "", "task"}, 2, "invalid task name"},
		{"invalid name", []string{"delivery", "a/b", "task"}, 2, "invalid task name"},
		{"long name", []string{"delivery", strings.Repeat("a", 233), "task"}, 2, "invalid task name"},
		{"extra argument", []string{"delivery", "job", "task", "extra"}, 2, "usage:"},
		{"missing effort", []string{"delivery", "--effort"}, 2, "usage:"},
		{"empty effort", []string{"delivery", "--effort", "", "job", "task"}, 2, "usage:"},
		{"empty task", []string{"reviewer", "job", ""}, 2, "task must contain text"},
		{"blank task", []string{"reviewer", "job", " \t\n"}, 2, "task must contain text"},
		{"blank file", []string{"debugger", "job", "@blank.md"}, 2, "task must contain text"},
		{"missing file", []string{"debugger", "job", "@missing.md"}, 2, "cannot read task file"},
		{"active name", []string{"reviewer", "taken", "task"}, 1, "katl-codex-reviewer-taken.service is already active. Choose another task name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			writeArtifact(t, work, "blank.md", "\n\t ")

			cmd := exec.Command(filepath.Join(repo, "scripts", "codex-role"), tc.args...)
			cmd.Dir = work
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.status || !strings.Contains(string(output), tc.want) {
				t.Fatalf("want exit %d and %q, got %v\n%s", tc.status, tc.want, err, output)
			}
			if _, err := os.Stat(filepath.Join(work, "systemd-run-args")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected job attempted to launch: stat error = %v", err)
			}
		})
	}
}

func fakeCodexSystemd(t *testing.T, bin string) {
	t.Helper()
	writeFakeExecutable(t, bin, "systemctl", `[[ $# == 4 && "$1 $2 $3" == "--user is-active --quiet" ]]
printf '%s\n' "$4" > checked-unit
if [[ "$4" == katl-codex-reviewer-taken.service ]]; then
    exit 0
fi
exit 3
`)
	writeFakeExecutable(t, bin, "systemd-run", `printf '%s\0' "$@" > systemd-run-args
environment=()
while [[ "$1" != -- ]]; do
    case "$1" in
        --unit) shift ;;
        --setenv=*) name="${1#--setenv=}"; environment+=("$name=${!name}") ;;
    esac
    shift
done
shift
# A service does not inherit the launcher's environment.
exec env -i "${environment[@]}" "$@"
`)
}
