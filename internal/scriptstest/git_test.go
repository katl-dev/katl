package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	gitTestName  = "Katl Test"
	gitTestEmail = "test@katl.dev"
)

// hermeticGitEnv returns env with every GIT_* variable removed and git confined
// to dir: no system or global config, no repository discovery above dir's
// parent, and a fixed commit identity supplied through the environment so that
// no config file ever receives it.
func hermeticGitEnv(env []string, dir string) []string {
	out := make([]string, 0, len(env)+7)
	for _, value := range env {
		if !strings.HasPrefix(value, "GIT_") {
			out = append(out, value)
		}
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
		"GIT_AUTHOR_NAME="+gitTestName,
		"GIT_AUTHOR_EMAIL="+gitTestEmail,
		"GIT_COMMITTER_NAME="+gitTestName,
		"GIT_COMMITTER_EMAIL="+gitTestEmail,
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv(os.Environ(), dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
