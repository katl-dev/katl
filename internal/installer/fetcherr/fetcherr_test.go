package fetcherr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/nettest"
)

const (
	userinfo = "opuser:hunter2"
	query    = "token=sekret-query"
)

// withSecrets adds credentials and a query secret to a request URL. None of
// them may reach an operator-visible message.
func withSecrets(t *testing.T, rawURL string) string {
	t.Helper()
	scheme, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		t.Fatalf("URL %q has no scheme", rawURL)
	}
	return scheme + "://" + userinfo + "@" + rest + "/bundle.katlcfg?" + query
}

func closedPortURL(t *testing.T) string {
	t.Helper()
	return "http://" + nettest.ReservePort(t).Endpoint
}

// nxdomainResolverTransport resolves every name through a local DNS server
// that answers NXDOMAIN.
func nxdomainResolverTransport(t *testing.T) *http.Transport {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen DNS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			reply := append([]byte(nil), buf[:n]...)
			if len(reply) < 12 {
				continue
			}
			reply[2] |= 0x80 // response
			reply[3] = 0x83  // recursion available, NXDOMAIN
			_, _ = conn.WriteTo(reply, peer)
		}
	}()
	dnsAddress := conn.LocalAddr().String()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", dnsAddress)
		},
	}
	return &http.Transport{DialContext: (&net.Dialer{Resolver: resolver}).DialContext}
}

func TestCause(t *testing.T) {
	blocked := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-blocked }))
	t.Cleanup(func() {
		close(blocked)
		hang.Close()
	})
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(untrusted.Close)

	tests := []struct {
		name      string
		client    *http.Client
		url       string
		wantCause string
		wantIs    error
	}{
		{
			name:      "connection refused",
			client:    &http.Client{},
			url:       closedPortURL(t),
			wantCause: "connection refused",
			wantIs:    syscall.ECONNREFUSED,
		},
		{
			name:      "timeout",
			client:    &http.Client{Timeout: 100 * time.Millisecond},
			url:       hang.URL,
			wantCause: "timed out",
		},
		{
			name:      "no such host",
			client:    &http.Client{Transport: nxdomainResolverTransport(t)},
			url:       "http://katl-missing.invalid.",
			wantCause: "no such host",
		},
		{
			name:      "untrusted certificate",
			client:    &http.Client{},
			url:       untrusted.URL,
			wantCause: "certificate signed by unknown authority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawURL := withSecrets(t, tt.url)
			req, err := http.NewRequest(http.MethodGet, rawURL, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := tt.client.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatal("request succeeded, want failure")
			}
			if !strings.Contains(err.Error(), "sekret-query") {
				t.Fatalf("precondition: raw client error %q should embed the URL", err)
			}

			got := Cause(err)

			if !strings.Contains(got.Error(), tt.wantCause) {
				t.Errorf("Cause() = %q, want it to name %q", got, tt.wantCause)
			}
			for _, secret := range []string{"hunter2", "opuser", "sekret-query", "token=", "bundle.katlcfg", rawURL} {
				if strings.Contains(got.Error(), secret) {
					t.Errorf("Cause() = %q leaks %q", got, secret)
				}
			}
			if tt.wantIs != nil && !errors.Is(got, tt.wantIs) {
				t.Errorf("Cause() does not unwrap to %v", tt.wantIs)
			}
		})
	}
}

func TestCauseKeepsNonURLErrors(t *testing.T) {
	want := errors.New("custom transport failure")

	if got := Cause(want); got != want {
		t.Fatalf("Cause() = %v, want the original error", got)
	}
}
