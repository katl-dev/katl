package kubernetesrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGit confines git, including git run by the code under test, to dir:
// inherited GIT_* variables are dropped, no system or global config is read,
// and repository discovery stops at dir's parent.
func isolateGit(t *testing.T, dir string) {
	t.Helper()
	for _, value := range os.Environ() {
		if name, _, _ := strings.Cut(value, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
}
