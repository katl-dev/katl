// Package checkoutcheck reports how the local Katl checkout compares with the
// last fetched origin/main.
package checkoutcheck

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const mainRef = "refs/remotes/origin/main"

// TestSuppress names the environment variable that test environments set to 1
// so that tools run by tests do not report on the developer's checkout. The
// report depends on the developer's remote-tracking refs, which a test must
// neither read nor depend on.
const TestSuppress = "KATL_TEST_SUPPRESS_CHECKOUT_CHECK"

const worktreeAdvice = "Refresh the checkout or use a fresh detached worktree at origin/main under ${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev/worktrees."

type Tool string

const (
	Queue    Tool = "katl-queue"
	Release  Tool = "katl-release"
	Evidence Tool = "katl-dev-evidence"
)

// Root finds the Katl worktree containing the current directory. The module
// identifies a checkout even if origin is a fork, mirror, or absent.
func Root(ctx context.Context) string {
	root, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	line, _, _ := strings.Cut(string(data), "\n")
	if err != nil || strings.TrimSpace(line) != "module github.com/katl-dev/katl" {
		return ""
	}
	return root
}

// Warn writes a diagnostic to out if the checkout is behind. With no tool
// specified, it checks all three tools for the session-start report.
func Warn(ctx context.Context, out io.Writer, tools ...Tool) {
	if os.Getenv(TestSuppress) == "1" {
		return
	}
	root := Root(ctx)
	if root == "" {
		if len(tools) == 0 {
			fmt.Fprintln(out, "warning: run the session checkout check from a Katl Git worktree.")
		}
		return
	}

	main, err := git(ctx, root, "rev-parse", "--verify", "--quiet", mainRef+"^{commit}")
	if err != nil {
		fmt.Fprintln(out, "warning: cannot check Katl checkout freshness because local origin/main is unavailable. Run git fetch origin and retry.")
		return
	}
	head, err := git(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		fmt.Fprintf(out, "warning: cannot read Katl checkout HEAD: %v\n", err)
		return
	}
	if head == main {
		return
	}
	behind, err := git(ctx, root, "rev-list", "--count", head+".."+main)
	if err != nil {
		fmt.Fprintf(out, "warning: cannot count commits missing from the Katl checkout: %v\n", err)
		return
	}
	if behind == "0" {
		return
	}

	if len(tools) == 0 {
		tools = []Tool{Queue, Release, Evidence}
	}
	fmt.Fprintf(out, "warning: Katl checkout is behind local origin/main by %s commit(s). %s\n", behind, worktreeAdvice)
	describe(ctx, out, root, head, main, "AGENTS.md", "AGENTS.md")
	describe(ctx, out, root, head, main, "dev/agents", "dev/agents")
	for _, tool := range tools {
		name := string(tool)
		describe(ctx, out, root, head, main, name+" sources", "cmd/"+name, "scripts/"+name,
			"internal/checkoutcheck", "flake.nix", "contrib/home-manager/katl-dev.nix")
	}
}

func describe(ctx context.Context, out io.Writer, root, head, main, label string, paths ...string) {
	names, err := git(ctx, root, append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", head, main, "--"}, paths...)...)
	switch {
	case err != nil:
		fmt.Fprintf(out, "  %s could not be compared: %v\n", label, err)
	case names != "":
		fmt.Fprintf(out, "  Differences from origin/main in %s.\n", label)
	default:
		fmt.Fprintf(out, "  Local origin/main matches %s.\n", label)
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Partial clones may otherwise fetch missing objects during read-only Git commands.
	cmd.Env = append(cmd.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=")
	raw, err := cmd.Output()
	return strings.TrimSpace(string(raw)), err
}
