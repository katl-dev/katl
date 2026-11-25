package scriptstest

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gittest.Env(os.Environ(), dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
