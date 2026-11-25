package gittest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEnvDisablesBackgroundMaintenance(t *testing.T) {
	dir := t.TempDir()
	// A hostile inherited configuration must not re-enable it.
	environ := append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=1")

	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		cmd := exec.Command("git", "config", "--get", key)
		cmd.Dir = dir
		cmd.Env = Env(environ, dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git config --get %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
