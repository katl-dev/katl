package checkoutcheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestWarn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream [][]string
		noMain   bool
		detached bool
		tool     []Tool
		want     []string
		absent   []string
	}{
		{name: "up to date"},
		{
			name:     "behind with rules and tool changes",
			upstream: [][]string{{"AGENTS.md", "dev/agents/README.md"}, {"cmd/katl-queue/main.go"}},
			want: []string{
				"behind local origin/main by 2 commit(s)",
				"Differences from origin/main in AGENTS.md.",
				"Differences from origin/main in dev/agents.",
				"Differences from origin/main in katl-queue sources.",
				"Local origin/main matches katl-release sources.",
				"Local origin/main matches katl-dev-evidence sources.",
				"fresh detached worktree at origin/main under ${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev/worktrees",
			},
		},
		{
			name:     "behind with unrelated changes",
			upstream: [][]string{{"README.md"}},
			tool:     []Tool{Queue},
			want: []string{
				"behind local origin/main by 1 commit(s)",
				"Local origin/main matches AGENTS.md.",
				"Local origin/main matches dev/agents.",
				"Local origin/main matches katl-queue sources.",
			},
			absent: []string{"katl-release sources", "katl-dev-evidence sources"},
		},
		{
			name:   "no origin main ref",
			noMain: true,
			tool:   []Tool{Release},
			want:   []string{"local origin/main is unavailable", "git fetch origin"},
		},
		{
			name:     "detached worktree",
			upstream: [][]string{{"AGENTS.md"}},
			detached: true,
			tool:     []Tool{Evidence},
			want:     []string{"behind local origin/main by 1 commit(s)", "Differences from origin/main in AGENTS.md."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtures := t.TempDir()
			dir := filepath.Join(fixtures, "repo")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", fixtures)
			for _, variable := range gittest.Env(nil, dir) {
				name, value, _ := strings.Cut(variable, "=")
				t.Setenv(name, value)
			}
			t.Chdir(dir)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", args...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}

			git("init", "--quiet")
			if err := os.WriteFile("go.mod", []byte("module github.com/katl-dev/katl\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "go.mod")
			git("commit", "--quiet", "-m", "base")
			head := git("rev-parse", "HEAD")
			git("switch", "--quiet", "-c", "upstream")
			for _, paths := range tc.upstream {
				for _, path := range paths {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("upstream\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				git("add", "--all")
				git("commit", "--quiet", "-m", "upstream change")
			}
			if !tc.noMain {
				git("update-ref", "refs/remotes/origin/main", "HEAD")
			}
			if tc.detached {
				worktree := filepath.Join(fixtures, "detached")
				git("worktree", "add", "--quiet", "--detach", worktree, head)
				t.Chdir(worktree)
			} else {
				git("switch", "--quiet", "-c", "session", head)
			}

			var warning bytes.Buffer
			Warn(t.Context(), &warning, tc.tool...)
			got := warning.String()
			if len(tc.want) == 0 && got != "" {
				t.Fatalf("unexpected warning: %s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("warning missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("warning unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}
}
