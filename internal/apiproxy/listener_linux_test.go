package apiproxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListenBeforeAddressAssignment(t *testing.T) {
	for _, address := range []string{"192.0.2.123:7445", "[2001:db8::123]:7445"} {
		t.Run(address, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := Server{
				Config: Config{
					TLSName: "api.example.test",
					Listeners: []Listener{
						{Address: address, Exposure: ExposureWorkstation},
						{Address: "127.0.0.1:7445", Exposure: ExposureNodeLocal},
					},
					Backends: []Backend{{Name: "cp-1", Address: "127.0.0.1:6443", Local: true}},
				},
				StatusPath: filepath.Join(t.TempDir(), "status.json"),
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result <- server.Run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("proxy did not stop during cleanup")
				}
			})

			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case err := <-result:
					t.Fatalf("proxy exited before address assignment: %v", err)
				case <-deadline:
					t.Fatal("proxy did not start before address assignment")
				case <-ticker.C:
					if _, err := os.Stat(server.StatusPath); err != nil {
						continue
					}
					conn, err := net.DialTimeout("tcp", "127.0.0.1:7445", time.Second)
					if err != nil {
						t.Fatalf("node-local listener unavailable: %v", err)
					}
					conn.Close()

					unexpected, err := net.DialTimeout("tcp", "127.0.0.2:7445", time.Second)
					if err == nil {
						unexpected.Close()
						t.Fatal("proxy accepts connections on an unconfigured address")
					}

					cancel()
					if err := <-result; err != nil {
						t.Fatal(err)
					}
					return
				}
			}
		})
	}
}
