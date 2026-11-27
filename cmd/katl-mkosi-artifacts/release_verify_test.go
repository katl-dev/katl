package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

// verifierSudo models sudo's secure_path: the command runs with PATH replaced
// by a directory that holds only env. It records that it ran.
func verifierSudo(t *testing.T, dir string) (sudoDir, ran string) {
	t.Helper()
	env := workstationtest.Tool(t, "env")
	securePath := filepath.Join(dir, "secure")
	sudoDir = filepath.Join(dir, "sudo")
	for _, path := range []string{securePath, sudoDir} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(env, filepath.Join(securePath, "env")); err != nil {
		t.Fatal(err)
	}
	ran = filepath.Join(dir, "sudo-ran")
	sudo := "#!/bin/sh\n: > " + ran + "\n[ \"$1\" = -- ] && shift\nPATH=" + securePath + " exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(sudoDir, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	return sudoDir, ran
}

func writeTools(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The verifier runs as root through sudo and runs more than the tools it
// checks for, such as util-linux's mountpoint when it unmounts an image. Every
// command it runs must resolve as it does for the caller, so the child gets the
// caller's whole PATH.
func TestVerifierKeepsCallerPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root runs the verifier in process, without sudo")
	}
	dir := t.TempDir()
	sudoDir, _ := verifierSudo(t, dir)
	tools := filepath.Join(dir, "tools")
	writeTools(t, tools, verifierTools...)
	utilLinux := filepath.Join(dir, "util-linux")
	writeTools(t, utilLinux, "mountpoint")
	t.Setenv("PATH", strings.Join([]string{sudoDir, tools, utilLinux}, string(os.PathListSeparator)))

	report := filepath.Join(dir, "resolved")
	if err := verifyReleaseImages(report, "mountpoint", dir, io.Discard, io.Discard); err != nil {
		t.Fatalf("verifyReleaseImages: %v", err)
	}

	resolved, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(utilLinux, "mountpoint"); string(resolved) != want {
		t.Fatalf("verifier under sudo resolved mountpoint as %q, want the caller's %s", resolved, want)
	}
}

// A caller outside the development shell gets told to enter it, before
// anything is elevated.
func TestVerifierRequiresShellTools(t *testing.T) {
	dir := t.TempDir()
	sudoDir, ran := verifierSudo(t, dir)
	tools := filepath.Join(dir, "tools")
	writeTools(t, tools, "systemd-dissect", "depmod", "modinfo")
	t.Setenv("PATH", sudoDir+string(os.PathListSeparator)+tools)

	err := verifyReleaseImages(filepath.Join(dir, "resolved"), "runtime", dir, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mksquashfs") || !strings.Contains(err.Error(), "development shell") {
		t.Fatalf("verifyReleaseImages error = %v, want one naming mksquashfs and the development shell", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("verifier elevated without its tools")
	}
}
