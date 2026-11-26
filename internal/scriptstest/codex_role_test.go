package scriptstest

import (
	"bytes"
	"context"
	"encoding/json"
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
	configHome := filepath.Join(t.TempDir(), "xdg config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	ghConfig := filepath.Join(home, "maintainer gh config")
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	if err := os.MkdirAll(ghConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	const fakeToken = "fixture_only_token:\\\"\nother.host:\n  oauth_token: injected"
	tokenFile := filepath.Join(configHome, "katl-dev", "codex-gh-token")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeExecutable(t, bin, "gh", `touch gh-called
printf 'maintainer_token\n'
`)
	writeFakeExecutable(t, bin, "systemd-creds", `[[ "$*" == "encrypt --user --name=hosts.yml - -" ]]
cat > credential-input
printf 'ZW5jcnlw\ndGVk\n'
`)
	buildCache, moduleCache := filepath.Join(home, ".cache", "go-build"), filepath.Join(home, "go", "pkg", "mod")
	for _, dir := range []string{buildCache, moduleCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOCACHE", buildCache)
	t.Setenv("GOMODCACHE", moduleCache)
	t.Setenv("KATLCTL_CONFIG", "")
	t.Setenv("TERM", "xterm")
	t.Setenv("CODEX_UNLISTED", "caller state")
	t.Setenv("GH_TOKEN", "caller_token")

	task := "Review \"this change\".\nKeep $HOME literal."
	writeFakeExecutable(t, bin, "codex", `if IFS= read -r unexpected; then
    printf 'unexpected stdin: %s\n' "$unexpected" >&2
    exit 3
fi
[[ /dev/stdin -ef /dev/null ]]
printf '%s\0' "$@" > codex-args
printf '%s\0' "$PATH" "$HOME" "$CODEX_HOME" "${XDG_CONFIG_HOME-unset}" "${KATL_MANAGER_PROBE-unset}" "${GH_CONFIG_DIR-unset}" "${GH_TOKEN-unset}" "${GITHUB_TOKEN-unset}" "${GH_ENTERPRISE_TOKEN-unset}" "${GITHUB_ENTERPRISE_TOKEN-unset}" > codex-env
printf 'progress\n' >&2
printf 'final message'
exit "$CODEX_TEST_STATUS"
`)

	for _, tc := range []struct {
		role, sandbox, model, effort string
		input                        string
		options                      []string
		logLocation                  string
		status                       int
		withToken                    bool
	}{
		{"delivery", "workspace-write", "gpt-6.2-sol", "medium", task, nil, "home", 0, true},
		{"reviewer", "read-only", "gpt-6.2-astra", "medium", task, nil, "xdg", 0, false},
		{"debugger", "read-only", "gpt-6.2-astra", "high", task, nil, "xdg", 23, false},
		{"debugger", "read-only", "gpt-6.2-astra", "low", "@task with spaces.md", []string{"--effort", "low"}, "explicit", 0, false},
		{"rebase", "workspace-write", "gpt-6.1-terra", "medium", task, nil, "xdg", 0, false},
	} {
		t.Run(tc.role+"/"+tc.effort, func(t *testing.T) {
			work := t.TempDir()
			writeArtifact(t, work, "task with spaces.md", task+"\n")
			evidenceDir := t.TempDir()
			const evidenceName = "evidence with spaces.txt"
			writeArtifact(t, work, evidenceName, "evidence\n")
			evidenceFile := filepath.Join(work, evidenceName)
			evidenceAlias := filepath.Join(work, "evidence-alias")
			if err := os.Symlink(evidenceFile, evidenceAlias); err != nil {
				t.Fatal(err)
			}
			if tc.withToken {
				if err := os.WriteFile(tokenFile, []byte(fakeToken+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(tokenFile); err != nil {
						t.Error(err)
					}
				})
			}

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
			logName := "katl-codex-" + tc.role + "-" + taskName + ".log"
			launcherArgs := append([]string{tc.role, "--env", "CODEX_TEST_STATUS"}, tc.options...)
			launcherArgs = append(launcherArgs, "--ro-bind", evidenceDir, "--ro-bind", evidenceAlias)
			stateHome := filepath.Join(work, "xdg state")
			t.Setenv("XDG_STATE_HOME", stateHome)
			logPath := filepath.Join(stateHome, "katl-dev", "logs", logName)
			switch tc.logLocation {
			case "home":
				t.Setenv("XDG_STATE_HOME", "")
				logPath = filepath.Join(home, ".local", "state", "katl-dev", "logs", logName)
			case "explicit":
				const customLog = "codex report.log"
				launcherArgs = append(launcherArgs, "--log", customLog)
				logPath = filepath.Join(work, customLog)
			}
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
			if _, err := os.Stat(filepath.Join(work, "gh-called")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("launcher consulted maintainer gh auth: stat error = %v", err)
			}
			const authNotice = "launching without GitHub auth"
			if tc.withToken {
				var credential map[string]map[string]string
				payload := mustReadFile(t, filepath.Join(work, "credential-input"))
				if err := json.Unmarshal(payload, &credential); err != nil || len(credential) != 1 ||
					len(credential["github.com"]) != 1 || credential["github.com"]["oauth_token"] != fakeToken {
					t.Fatal("encryptor did not receive a config containing only the dedicated token")
				}
				if strings.Contains(stderr.String(), authNotice) {
					t.Fatalf("authenticated launch reported missing auth: %s", &stderr)
				}
			} else if strings.Count(stderr.String(), authNotice) != 1 || !strings.Contains(stderr.String(), tokenFile) {
				t.Fatalf("anonymous launch did not report how to provision auth: %s", &stderr)
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
			for _, flag := range []string{"--user", "--collect", "--wait", "--working-directory=" + repo, "--expand-environment=no"} {
				if !slices.Contains(runArgs, flag) {
					t.Errorf("systemd-run missing %s", flag)
				}
			}
			properties := []string{
				"ProtectHome=tmpfs", "PrivateUsers=yes", "PrivateTmp=yes", `TemporaryFileSystem="` + home + `":ro`,
				`BindPaths="` + repo + `"`, `BindPaths="` + filepath.Dir(logPath) + `"`, `BindPaths="` + codexHome + `"`,
				`BindPaths="` + buildCache + `"`, `BindPaths="` + moduleCache + `"`,
				`BindReadOnlyPaths="` + evidenceDir + `"`, `BindReadOnlyPaths="` + evidenceFile + `"`,
				"SetCredential=config.yml:version: 1",
			}
			if tc.withToken {
				properties = append(properties, "SetCredentialEncrypted=hosts.yml:ZW5jcnlwdGVk", `InaccessiblePaths="-`+tokenFile+`"`)
			}
			for _, property := range properties {
				index := slices.Index(runArgs, property)
				if index <= 0 || runArgs[index-1] != "-p" {
					t.Errorf("systemd-run missing property %s", property)
				}
			}
			for _, assignment := range []string{
				"PATH=" + path, "HOME=" + home, "TERM=xterm", "CODEX_HOME=" + codexHome,
				"GOCACHE=" + buildCache, "GOMODCACHE=" + moduleCache, "GOTELEMETRY=off",
				"KATLCTL_CONFIG=" + filepath.Join(repo, "_build/katlctl/config.yaml"), "CODEX_TEST_STATUS=" + status,
				"NOSYSBASHRC=1",
			} {
				if !slices.Contains(runArgs, "--setenv="+assignment) {
					t.Errorf("systemd-run missing explicit assignment %s", assignment)
				}
			}
			for _, arg := range runArgs[:slices.Index(runArgs, "--")] {
				bindsHome := strings.HasPrefix(arg, "Bind") && strings.Contains(arg, `="`+home+`"`)
				if bindsHome || strings.Contains(arg, ghConfig) || strings.Contains(arg, "fixture_only_token") ||
					strings.Contains(arg, "maintainer_token") || strings.Contains(arg, "caller_token") {
					t.Fatal("unit received the plaintext token, maintainer gh config, or an unmasked home bind")
				}
				if strings.HasPrefix(arg, "Bind") && (strings.Contains(arg, tokenFile) || strings.Contains(arg, evidenceAlias)) {
					t.Fatalf("unit bound the original token file or the evidence alias: %s", arg)
				}
				if !tc.withToken && strings.Contains(arg, "hosts.yml") {
					t.Fatal("anonymous unit received a GitHub hosts credential")
				}
				if assignment, ok := strings.CutPrefix(arg, "--setenv="); ok {
					name, _, explicit := strings.Cut(assignment, "=")
					if !explicit || name == "XDG_CONFIG_HOME" || name == "CODEX_UNLISTED" || name == "GH_CONFIG_DIR" {
						t.Errorf("systemd-run unexpected environment variable %s", name)
					}
				}
			}
			if env := string(mustReadFile(t, filepath.Join(work, "codex-env"))); env != strings.Join([]string{
				path, home, codexHome, "unset", "unset", "/run/credentials/probe", "unset", "unset", "unset", "unset", "",
			}, "\x00") {
				t.Fatalf("unexpected Codex environment %q", env)
			}

			args := strings.Split(strings.TrimSuffix(string(mustReadFile(t, filepath.Join(work, "codex-args"))), "\x00"), "\x00")
			want := []string{"exec", "-C", repo, "-s", tc.sandbox, "-m", tc.model, "-c", "model_reasoning_effort=" + tc.effort, "-c", "allow_login_shell=false"}
			if tc.sandbox == "workspace-write" {
				want = append(want, "-c", "sandbox_workspace_write.writable_roots=[]", "--add-dir", buildCache, "--add-dir", moduleCache)
			}
			if len(args) != len(want)+1 || !slices.Equal(args[:len(want)], want) {
				t.Fatalf("codex args = %q, want %q followed by one prompt", args, want)
			}

			prompt := args[len(want)]
			roleFile := filepath.Join(repo, "dev", "agents", "roles", tc.role+".md")
			roleText := strings.TrimRight(string(mustReadFile(t, roleFile)), "\n")
			roleTextInPrompt, rest, found := strings.Cut(prompt, "\n\nRead dev/agents/README.md")
			if !found || roleTextInPrompt != roleText || !strings.HasSuffix(rest, "\n\n"+task) {
				t.Fatalf("want the full %s role, the shared workflow reference, and the task, got %q", tc.role, prompt)
			}
		})
	}
}

func TestCodexRoleClaudeRefusal(t *testing.T) {
	repo := repoRoot(t)
	var settings struct {
		Permissions struct{ Deny []string }
	}
	if err := json.Unmarshal(mustReadFile(t, filepath.Join(repo, ".claude", "settings.json")), &settings); err != nil {
		t.Fatal(err)
	}
	const rule = "Bash(codex exec:*)"
	if !slices.Contains(settings.Permissions.Deny, rule) {
		t.Fatal("Claude settings must deny a direct Codex launch")
	}
	if guide := mustReadFile(t, filepath.Join(repo, "dev", "agents", "README.md")); !bytes.Contains(guide, []byte("Use `scripts/codex-role`")) {
		t.Fatal("agent workflow must name the supported launcher")
	}
}

func TestCodexRoleRejectsLaunch(t *testing.T) {
	repo := repoRoot(t)
	bin := t.TempDir()
	fakeCodexSystemd(t, bin)
	writeFakeExecutable(t, bin, "codex", "exit 99\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	privateDir := filepath.Join(home, "private")
	tokenFile := filepath.Join(home, ".config", "katl-dev", "codex-gh-token")
	for _, dir := range []string{privateDir, filepath.Dir(tokenFile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tokenFile, []byte("mode_fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	alias := filepath.Join(outside, "home-alias")
	if err := os.Symlink(privateDir, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("GOCACHE", t.TempDir())
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", outside)
	t.Setenv("KATLCTL_CONFIG", "")

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
		{"GH_TOKEN", []string{"reviewer", "--env", "GH_TOKEN", "job", "task"}, 2, "--env GH_TOKEN is reserved"},
		{"GITHUB_TOKEN", []string{"reviewer", "--env", "GITHUB_TOKEN", "job", "task"}, 2, "--env GITHUB_TOKEN is reserved"},
		{"GH_ENTERPRISE_TOKEN", []string{"reviewer", "--env", "GH_ENTERPRISE_TOKEN", "job", "task"}, 2, "--env GH_ENTERPRISE_TOKEN is reserved"},
		{"GITHUB_ENTERPRISE_TOKEN", []string{"reviewer", "--env", "GITHUB_ENTERPRISE_TOKEN", "job", "task"}, 2, "--env GITHUB_ENTERPRISE_TOKEN is reserved"},
		{"bind home", []string{"reviewer", "--bind", home, "job", "task"}, 1, "bind would expose all of HOME"},
		{"bind ancestor", []string{"reviewer", "--bind", filepath.Dir(home), "job", "task"}, 1, "bind would expose all of HOME"},
		{"bind private directory", []string{"reviewer", "--bind", privateDir, "job", "task"}, 1, "bind would expose content under HOME"},
		{"ro-bind home symlink", []string{"reviewer", "--ro-bind", alias, "job", "task"}, 1, "bind would expose content under HOME"},
		{"ro-bind tmp", []string{"reviewer", "--ro-bind", "/tmp/.", "job", "task"}, 1, "bind would expose all of /tmp"},
		{"insecure token file", []string{"reviewer", "job", "task"}, 1, "regular file with mode 0600"},
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
        --setenv=*) environment+=("${1#--setenv=}") ;;
        SetCredential=config.yml:*) environment+=(CREDENTIALS_DIRECTORY=/run/credentials/probe) ;;
    esac
    shift
done
shift
# Model the ambient environment of the user manager plus the unit assignments.
exec env -i KATL_MANAGER_PROBE=manager GH_TOKEN=manager GITHUB_TOKEN=manager \
    GH_ENTERPRISE_TOKEN=manager GITHUB_ENTERPRISE_TOKEN=manager "${environment[@]}" "$@"
`)
}
