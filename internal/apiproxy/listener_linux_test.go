package apiproxy

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListenBeforeAddressAssignment(t *testing.T) {
	for _, address := range []string{"192.0.2.123:0", "[2001:db8::123]:0"} {
		t.Run(address, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := Server{
				Config: Config{
					TLSName:       "api.example.test",
					CheckInterval: DefaultInterval, CheckTimeout: DefaultTimeout,
					CanonicalEndpoint: "api.example.test:6443", CAFile: DefaultCAFile,
					Listeners: []Listener{
						{Address: address, Exposure: ExposureWorkstation},
						{Address: "127.0.0.1:0", Exposure: ExposureNodeLocal},
					},
					Backends: []Backend{{Name: "cp-1", Address: "127.0.0.1:6443", Local: true}},
				},
				StatusPath: filepath.Join(t.TempDir(), "status.json"),
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			listenConfig := net.ListenConfig{Control: configureListener}
			nodeLocal := make(chan netip.AddrPort, 1)
			go func() {
				defer close(done)
				result <- server.run(ctx, func(ctx context.Context, network, address string) (net.Listener, error) {
					listener, err := listenConfig.Listen(ctx, network, address)
					if err == nil && address == "127.0.0.1:0" {
						nodeLocal <- listener.Addr().(*net.TCPAddr).AddrPort()
					}
					return listener, err
				})
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
					bound := <-nodeLocal
					if bound.Addr() != netip.MustParseAddr("127.0.0.1") {
						t.Fatalf("node-local listener bound to %s, want loopback", bound)
					}
					conn, err := net.DialTimeout("tcp", bound.String(), time.Second)
					if err != nil {
						t.Fatalf("node-local listener unavailable: %v", err)
					}
					conn.Close()

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
