package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyInput(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	writeTestFile(t, filepath.Join(preseed, "etc/katl/install-manifest.json"), `{"kind":"InstallManifest"}`)

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		EtcDir:      etcDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	assertFile(t, filepath.Join(etcDir, "install-manifest.json"), `{"kind":"InstallManifest"}`)
	if got := stdout.String(); !strings.Contains(got, "copied") {
		t.Fatalf("stdout = %q, want copied log", got)
	}
}

func TestApplyInputMountsSeedDevice(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "seed-device")
	preseed := filepath.Join(root, "mounted-seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, device, "")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)

	commands := &NoopCommandRunner{}
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{filepath.Join(root, "missing-seed-device"), device},
		SeedMount:   preseed,
		Commands:    commands,
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	if len(commands.Calls) != 1 || commands.Calls[0].Name != "mount" {
		t.Fatalf("commands = %#v, want mount", commands.Calls)
	}
	if got := strings.Join(commands.Calls[0].Args, " "); !strings.Contains(got, device) || !strings.Contains(got, preseed) {
		t.Fatalf("mount args = %#v", commands.Calls[0].Args)
	}
	if got := stdout.String(); !strings.Contains(got, "mounted seed device") || !strings.Contains(got, "copied") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestApplyInputSkipsMissingSeedDevice(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"waitForConfig":true}`)

	commands := &NoopCommandRunner{}
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{filepath.Join(root, "missing-seed-device")},
		SeedMount:   filepath.Join(root, "missing-mount"),
		Commands:    commands,
		RunDir:      runDir,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"waitForConfig":true}`)
	if len(commands.Calls) != 0 {
		t.Fatalf("commands = %#v, want no seed mount", commands.Calls)
	}
}

func TestApplyInputWaitsForSeedDevice(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "seed-device")
	preseed := filepath.Join(root, "mounted-seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	go func() {
		time.Sleep(50 * time.Millisecond)
		writeTestFile(t, device, "")
	}()

	commands := &NoopCommandRunner{}
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{device},
		SeedMount:   preseed,
		SeedWait:    time.Second,
		Commands:    commands,
		RunDir:      runDir,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	if len(commands.Calls) != 1 || commands.Calls[0].Name != "mount" {
		t.Fatalf("commands = %#v, want mount", commands.Calls)
	}
}

func TestApplyInputNone(t *testing.T) {
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{filepath.Join(t.TempDir(), "missing")},
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	if got, want := stdout.String(), "katl input: no preseed files found\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestApplyInputJSON(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{`)

	err := ApplyInput(InputApplyRequest{PreseedDirs: []string{preseed}, RunDir: filepath.Join(root, "run")})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("ApplyInput() error = %v, want JSON error", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
