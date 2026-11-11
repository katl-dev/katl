package scenarios

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
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
