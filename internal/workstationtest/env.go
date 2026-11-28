// Package workstationtest isolates workstation configuration in Go tests.
package workstationtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/katl-dev/katl/internal/checkoutcheck"
)

// Run gives a test package an empty workstation environment. Packages that
// call the workstation resolver in process pass its path-check hook.
func Run(m *testing.M, pathCheck *func(string) error) (status int) {
	tempDir, err := os.MkdirTemp("", "katl-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			fmt.Fprintf(os.Stderr, "remove workstation test directory: %v\n", err)
			status = 1
		}
	}()

	dir, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := pinGoCaches(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var escaped atomic.Bool
	if pathCheck != nil {
		root, err := os.OpenRoot(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer root.Close()
		*pathCheck = func(path string) error {
			err := checkPath(root, path)
			if err != nil {
				// Fail the package even if the caller discards the error.
				escaped.Store(true)
				fmt.Fprintln(os.Stderr, err)
			}
			return err
		}
		// Keep t.TempDir fixtures inside the same boundary as the default.
		if err := os.Setenv("TMPDIR", dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "KATLCTL_") || strings.HasPrefix(name, "GIT_") {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
	}
	for name, value := range map[string]string{
		"KATLCTL_CONFIG":      filepath.Join(dir, "katlctl.yaml"),
		"HOME":                dir,
		"XDG_CONFIG_HOME":     dir,
		"GIT_CONFIG_GLOBAL":   filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"KUBECONFIG":          filepath.Join(dir, "kubeconfig"),
		// Tools run by tests must not report on the developer's checkout.
		checkoutcheck.TestSuppress: "1",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	// A test that starts scripts/vmtest-run must not take leases or locks in
	// the host's coordination directory, but a VM run's tests must keep using
	// the one their runner bound.
	if os.Getenv("KATL_VMTEST_HOST_STATE_DIR") == "" {
		hostState := filepath.Join(dir, "vmtest-host")
		if err := os.Mkdir(hostState, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Setenv("KATL_VMTEST_HOST_STATE_DIR", hostState); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if err := disableGoTelemetry(nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	status = m.Run()
	if escaped.Load() && status == 0 {
		return 1
	}
	return status
}

// Point the command at the isolated config directory before calling this.
// Even local telemetry can start a sidecar that outlives the go command.
func disableGoTelemetry(env []string) error {
	cmd := exec.Command("go", "telemetry", "off")
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil && !errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("disable Go telemetry: %w\n%s", err, output)
	}
	return nil
}

func pinGoCaches() error {
	var paths struct {
		GOPATH     string
		GOMODCACHE string
		GOCACHE    string
	}

	// Resolve defaults and GOENV settings before isolating HOME and XDG_CONFIG_HOME.
	output, err := exec.Command("go", "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE").Output()
	switch {
	case err == nil:
		if err := json.Unmarshal(output, &paths); err != nil {
			return fmt.Errorf("decode Go cache paths: %w", err)
		}
	case errors.Is(err, exec.ErrNotFound):
		paths.GOPATH = os.Getenv("GOPATH")
		if paths.GOPATH == "" {
			if home, err := os.UserHomeDir(); err == nil {
				paths.GOPATH = filepath.Join(home, "go")
			}
		}
		paths.GOMODCACHE = os.Getenv("GOMODCACHE")
		if first, _, _ := strings.Cut(paths.GOPATH, string(os.PathListSeparator)); paths.GOMODCACHE == "" && first != "" {
			paths.GOMODCACHE = filepath.Join(first, "pkg", "mod")
		}
		paths.GOCACHE = os.Getenv("GOCACHE")
		if paths.GOCACHE == "" {
			if cache, err := os.UserCacheDir(); err == nil {
				paths.GOCACHE = filepath.Join(cache, "go-build")
			}
		}
	default:
		return fmt.Errorf("resolve Go cache paths: %w", err)
	}

	for name, value := range map[string]string{
		"GOPATH": paths.GOPATH, "GOMODCACHE": paths.GOMODCACHE, "GOCACHE": paths.GOCACHE,
	} {
		if value != "" {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPath(root *os.Root, path string) error {
	relative, ok := strings.CutPrefix(path, root.Name()+string(filepath.Separator))
	if !ok {
		return fmt.Errorf("katlctl test config path %q is outside test directory %q", path, root.Name())
	}
	// Root checks symlink and parent traversal even if the config itself
	// has yet to be created.
	if _, err := root.Stat(relative); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("katlctl test config path %q is inaccessible inside test directory %q: %w", path, root.Name(), err)
	}
	return nil
}
