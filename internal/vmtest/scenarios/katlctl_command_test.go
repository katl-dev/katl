package scenarios

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runKatlctlCommand(t *testing.T, ctx context.Context, repo string, args []string, stdout, stderr io.Writer) error {
	t.Helper()
	katlctl := buildKatlctlCommand(t, ctx, repo)
	cmd := exec.CommandContext(ctx, katlctl, args...)
	cmd.Dir = repo
	cmd.Env = os.Environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func buildKatlctlCommand(t *testing.T, ctx context.Context, repo string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katlctl")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", path, "./cmd/katlctl")
	cmd.Dir = repo
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build katlctl: %v\n%s", err, output)
	}
	return path
}

type transcriptEntry struct {
	Method          string   `json:"method"`
	Argv            []string `json:"argv,omitempty"`
	Redaction       string   `json:"redaction,omitempty"`
	StdoutBytes     uint32   `json:"stdoutBytes,omitempty"`
	WriteBytes      uint32   `json:"writeBytes,omitempty"`
	SensitiveOutput bool     `json:"sensitiveOutput,omitempty"`
}
