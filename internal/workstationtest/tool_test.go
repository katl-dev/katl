package workstationtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolMissing(t *testing.T) {
	const child = "KATL_TEST_MISSING_TOOL"
	const name = "katl-test-absent-tool"
	if os.Getenv(child) == "1" {
		Tool(t, name)
		return
	}

	t.Run("required", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestToolMissing$", "-test.v")
		cmd.Env = []string{child + "=1", "PATH=" + t.TempDir(), "KATL_REQUIRE_TOOLS=1"}
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "--- FAIL: TestToolMissing") || !strings.Contains(string(output), name) {
			t.Fatalf("expected failure naming the missing tool, got %v\n%s", err, output)
		}
	})
	t.Run("optional", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestToolMissing$", "-test.v")
		cmd.Env = []string{child + "=1", "PATH=" + t.TempDir()}
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- SKIP: TestToolMissing") || !strings.Contains(string(output), name) {
			t.Fatalf("expected skip naming the missing tool, got %v\n%s", err, output)
		}
	})
}

func TestToolFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("KATL_REQUIRE_TOOLS", "1")
	path := filepath.Join(dir, "katl-test-tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Tool(t, "katl-test-tool"); got != path {
		t.Fatalf("Tool() = %q, want %q", got, path)
	}
}
