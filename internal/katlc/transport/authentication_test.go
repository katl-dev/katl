package transport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/nodeidentity"
)

func TestServerAuthenticationFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "", "unknown", "mtls", "trusted-network"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if mode != "missing" {
				path := filepath.Join(root, nodeidentity.ManagementAuthenticationPath)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(mode), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			config, err := ServerTLSConfig(root)
			if mode == "trusted-network" {
				if err != nil || config != nil {
					t.Fatalf("trusted mode: %v, %v", config, err)
				}
			} else if err == nil {
				t.Fatal("missing TLS credentials accepted")
			}
		})
	}
}
