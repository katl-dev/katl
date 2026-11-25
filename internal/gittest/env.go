// Package gittest supplies the environment fixture repositories run git in.
package gittest

import (
	"path/filepath"
	"strings"
)

// Identity is the author and committer of fixture commits.
const (
	Name  = "Katl Test"
	Email = "test@katl.dev"
)

// Env returns environ with every GIT_* variable removed and git confined to
// dir: no system or global configuration, no repository discovery above dir's
// parent, and a fixed identity supplied through the environment so that no
// config file ever receives it.
//
// Background maintenance is disabled. Git starts a detached maintenance
// process after commands such as commit, fetch and push, and that process can
// still be repacking when the test returns and removes the repository.
func Env(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+12)
	for _, value := range environ {
		if !strings.HasPrefix(value, "GIT_") {
			out = append(out, value)
		}
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=gc.auto",
		"GIT_CONFIG_VALUE_0=0",
		"GIT_CONFIG_KEY_1=maintenance.auto",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
		"GIT_AUTHOR_NAME="+Name,
		"GIT_AUTHOR_EMAIL="+Email,
		"GIT_COMMITTER_NAME="+Name,
		"GIT_COMMITTER_EMAIL="+Email,
	)
}
