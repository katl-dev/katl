package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer"
)

func TestVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	version, commit, date = "dev", "abc123", "2026-06-01T00:00:00Z"
	t.Cleanup(func() {
		version, commit, date = oldVersion, oldCommit, oldDate
	})

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "katlos-install version=dev commit=abc123 date=2026-06-01T00:00:00Z\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestApplyInput(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(preseed, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(preseed, "install-input.json"), []byte(`{"waitForConfig":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--apply-input",
		"--preseed-dir", preseed,
		"--run-dir", runDir,
		"--etc-dir", etcDir,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "install-input.json")); err != nil {
		t.Fatalf("input file missing: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestBootInput(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	inputPath := filepath.Join(runDir, "install-input.json")
	inputJSON := `{"manifestPath":"/run/katl/install-manifest.json","installMode":"auto"}`
	if err := os.WriteFile(inputPath, []byte(inputJSON), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	input, err := bootInput(runDir, etcDir)
	if err != nil {
		t.Fatalf("bootInput() error = %v", err)
	}
	if input.Action != installer.InstallActionRun || !input.CanMutateDisks() {
		t.Fatalf("action = %s canMutate = %t, want run", input.Action, input.CanMutateDisks())
	}
}

func TestBootWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var stdout bytes.Buffer
	err := runBoot(ctx, filepath.Join(t.TempDir(), "run"), filepath.Join(t.TempDir(), "etc"), "127.0.0.1:0", &stdout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runBoot() error = %v, want deadline", err)
	}
	if got := stdout.String(); !strings.Contains(got, "waiting for config") {
		t.Fatalf("stdout = %q, want handoff announcement", got)
	}
}

func TestBootHold(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "install-input.json"), []byte(`{"holdForDebug":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var stdout bytes.Buffer
	err := runBoot(ctx, runDir, filepath.Join(root, "etc"), "127.0.0.1:0", &stdout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runBoot() error = %v, want deadline", err)
	}
	if got := stdout.String(); !strings.Contains(got, "debug hold active") {
		t.Fatalf("stdout = %q, want debug hold log", got)
	}
}
