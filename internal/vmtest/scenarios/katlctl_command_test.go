package scenarios

import (
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/katl-dev/katl/internal/vmtest"
)

// katlctlRunner is a katlctl binary together with the workstation config that
// every command it runs uses.
type katlctlRunner struct {
	bin    vmtest.Katlctl
	config string
}

func buildKatlctlCommand(t *testing.T, ctx context.Context, repo, config string) katlctlRunner {
	t.Helper()
	return katlctlRunner{bin: vmtest.BuildKatlctl(t, ctx, repo), config: config}
}

func (k katlctlRunner) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	return k.bin.Command(ctx, k.config, args...)
}

func runKatlctlCommand(t *testing.T, ctx context.Context, katlctl katlctlRunner, repo string, args []string, stdout, stderr io.Writer) error {
	t.Helper()
	cmd, err := katlctl.command(ctx, args...)
	if err != nil {
		return err
	}
	cmd.Dir = repo
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type transcriptEntry struct {
	Method          string   `json:"method"`
	Argv            []string `json:"argv,omitempty"`
	Redaction       string   `json:"redaction,omitempty"`
	StdoutBytes     uint32   `json:"stdoutBytes,omitempty"`
	WriteBytes      uint32   `json:"writeBytes,omitempty"`
	SensitiveOutput bool     `json:"sensitiveOutput,omitempty"`
}
