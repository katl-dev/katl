package vmtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Katlctl is a katlctl binary built for a VM test. Its path is unexported so
// that tests can run it only through Command, which confines every
// workstation lookup to the test's own directory. A test must never reach the
// operator's katlctl config: its current context may name a real cluster.
type Katlctl struct {
	path string
}

// NewKatlctl wraps an existing katlctl binary, such as a release artifact.
func NewKatlctl(path string) Katlctl {
	return Katlctl{path: path}
}

// BuildKatlctl builds ./cmd/katlctl from repo into a test temporary directory.
func BuildKatlctl(t testing.TB, ctx context.Context, repo string) Katlctl {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katlctl")
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", path, "./cmd/katlctl")
	cmd.Dir = repo
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build katlctl: %v\n%s", err, output)
	}
	return Katlctl{path: path}
}

// Command returns a katlctl command whose workstation config is configPath,
// an absolute path inside the test's run directory. It refuses an unbuilt
// Katlctl or a relative configPath rather than fall back to a default lookup.
func (k Katlctl) Command(ctx context.Context, configPath string, args ...string) (*exec.Cmd, error) {
	if k.path == "" {
		return nil, fmt.Errorf("katlctl was not built with vmtest.BuildKatlctl")
	}
	env, err := katlctlEnv(os.Environ(), configPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, k.path, args...)
	cmd.Env = env
	return cmd, nil
}

// katlctlEnv returns environ with every inherited katlctl setting removed and
// the workstation config set to configPath. HOME and XDG_CONFIG_HOME also
// point beside configPath, so no other per-user default lookup reaches the
// operator's home directory.
func katlctlEnv(environ []string, configPath string) ([]string, error) {
	if !filepath.IsAbs(configPath) {
		return nil, fmt.Errorf("katlctl config %q must be an absolute path inside the test run directory", configPath)
	}
	configPath = filepath.Clean(configPath)
	if userConfig, err := os.UserConfigDir(); err == nil {
		operatorDir := filepath.Join(userConfig, "katl")
		if rel, err := filepath.Rel(operatorDir, configPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("katlctl config %q is in the operator's katlctl config directory; use a path inside the test run directory", configPath)
		}
	}
	home := filepath.Join(filepath.Dir(configPath), "home")

	env := make([]string, 0, len(environ)+4)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case strings.HasPrefix(name, "KATLCTL_"), name == "HOME", name == "XDG_CONFIG_HOME":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"KATLCTL_CONFIG="+configPath,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	), nil
}
