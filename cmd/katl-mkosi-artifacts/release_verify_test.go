package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

// The verifier runs as root through sudo, whose secure_path would replace the
// development or ci shell's inspection tools with the host's.
func TestVerifyReleaseImagesKeepsCallerPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root runs the verifier in process, without sudo")
	}
	env := workstationtest.Tool(t, "env")
	dir := t.TempDir()
	securePath := filepath.Join(dir, "secure")
	sudoDir := filepath.Join(dir, "sudo")
	shellDir := filepath.Join(dir, "shell")
	for _, path := range []string{securePath, sudoDir, shellDir} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(env, filepath.Join(securePath, "env")); err != nil {
		t.Fatal(err)
	}
	// Model sudo's secure_path: the command runs with PATH replaced.
	sudo := "#!/bin/sh\n[ \"$1\" = -- ] && shift\nPATH=" + securePath + " exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(sudoDir, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(shellDir, "katl-inspection-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sudoDir+string(os.PathListSeparator)+shellDir)

	report := filepath.Join(dir, "resolved")
	if err := verifyReleaseImages(report, filepath.Base(tool), dir, io.Discard, io.Discard); err != nil {
		t.Fatalf("verifyReleaseImages: %v", err)
	}

	resolved, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved) != tool {
		t.Fatalf("verifier under sudo resolved %q, want the caller's %s", resolved, tool)
	}
}
