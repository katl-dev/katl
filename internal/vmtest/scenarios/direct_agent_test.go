package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestDirectAgentRetryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dialFails bool
		opError   error
	}{
		{name: "connection reset before dispatch", dialFails: true},
		{name: "response lost after effect", opError: syscall.ECONNRESET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := dialNodeAgent
			t.Cleanup(func() { dialNodeAgent = original })
			dials := 0
			dialNodeAgent = func(context.Context, uint32, uint32, string) (*vmtest.AgentClient, error) {
				dials++
				if tc.dialFails && dials == 1 {
					return nil, syscall.ECONNRESET
				}
				client, server := net.Pipe()
				t.Cleanup(func() { _ = server.Close() })
				return vmtest.NewAgentClient(client, ""), nil
			}
			effects := filepath.Join(t.TempDir(), "effects")

			_, err := withDirectAgent(context.Background(), vmtest.RunningInstalledRuntimeNode{}, time.Second, func(context.Context, *vmtest.AgentClient) (bool, error) {
				file, err := os.OpenFile(effects, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("effect\n"); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				return true, tc.opError
			})

			if !errors.Is(err, tc.opError) {
				t.Fatalf("operation error = %v, want %v", err, tc.opError)
			}
			got, err := os.ReadFile(effects)
			if err != nil || string(got) != "effect\n" {
				t.Fatalf("effects = %q, %v; operation must execute once", got, err)
			}
		})
	}
}

type deadlineRecorder struct {
	argv   []string
	budget time.Duration
}

func (r *deadlineRecorder) Run(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	r.argv = req.Argv
	if deadline, ok := ctx.Deadline(); ok {
		r.budget = time.Until(deadline)
	}
	return &vmtestpb.CommandResult{}, nil
}

// An image import's work grows with its archive, so the agent must give it
// at least minImageImportRate on top of the probe deadline: a fixed deadline
// cut off the ~440 MB Kubernetes image fixture on a slow CI runner.
func TestImageArchiveImportDeadline(t *testing.T) {
	original := dialNodeAgent
	t.Cleanup(func() { dialNodeAgent = original })
	dir := t.TempDir()
	runner := &deadlineRecorder{}
	agent := vmtest.AgentServer{AllowedWritePaths: []string{dir + string(os.PathSeparator)}, CommandRunner: runner}
	dialNodeAgent = func(context.Context, uint32, uint32, string) (*vmtest.AgentClient, error) {
		client, server := net.Pipe()
		go func() { _ = agent.Serve(context.Background(), server) }()
		return vmtest.NewAgentClient(client, ""), nil
	}

	budgets := map[int]time.Duration{}
	for _, size := range []int{1 << 20, 16 << 20} {
		path := filepath.Join(dir, fmt.Sprintf("images-%d.tar", size))
		archive := bytes.Repeat([]byte{'k'}, size)
		if err := importNodeImageArchive(context.Background(), vmtest.RunningInstalledRuntimeNode{}, path, archive, 0o644); err != nil {
			t.Fatal(err)
		}

		if want := []string{"ctr", "-n", "k8s.io", "images", "import", path}; !slices.Equal(runner.argv, want) {
			t.Fatalf("import ran %q, want %q", runner.argv, want)
		}
		if written, err := os.ReadFile(path); err != nil || !bytes.Equal(written, archive) {
			t.Fatalf("archive on the node differs from the staged archive: %v", err)
		}
		budgets[size] = runner.budget
	}

	if small := budgets[1<<20]; small < 29*time.Second {
		t.Errorf("1 MiB import budget %v is shorter than the 30s probe deadline", small)
	}
	growth := budgets[16<<20] - budgets[1<<20]
	if want := time.Duration(15<<20) * time.Second / minImageImportRate; growth < want-time.Second {
		t.Errorf("15 MiB more archive added %v to the import budget, want at least %v", growth, want)
	}
}
