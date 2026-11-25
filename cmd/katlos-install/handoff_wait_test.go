package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/installmedia"
)

type fakeHandoffWorld struct {
	t       time.Time
	addrAt  time.Duration // elapsed time at which an address appears; <0 never
	links   []handoffLink
	start   time.Time
	out     bytes.Buffer
	maxWait time.Duration
}

func (f *fakeHandoffWorld) waiter() handoffWaiter {
	f.start = time.Unix(1000, 0)
	f.t = f.start
	return handoffWaiter{
		detectHost: func() (string, error) {
			if f.addrAt >= 0 && f.t.Sub(f.start) >= f.addrAt {
				return "192.0.2.44", nil
			}
			return "", errors.New("no non-loopback interface address found")
		},
		links: func() ([]handoffLink, error) { return f.links, nil },
		now:   func() time.Time { return f.t },
		sleep: func(ctx context.Context, d time.Duration) error {
			f.t = f.t.Add(d)
			if f.t.Sub(f.start) > f.maxWait {
				return context.Canceled
			}
			return nil
		},
		out: &f.out,
	}
}

var wildcard = &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8080}

func TestHandoffWaitSlowAddressShowsProgress(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: 90 * time.Second, links: []handoffLink{{"lo0", false}, {"enp1s0", true}}, maxWait: time.Hour}

	got, err := f.waiter().wait(context.Background(), wildcard)
	if err != nil {
		t.Fatalf("wait() error = %v", err)
	}
	if got != "http://192.0.2.44:8080" {
		t.Fatalf("base URL = %q", got)
	}

	out := f.out.String()
	if !strings.Contains(out, "waiting for a DHCP address on enp1s0") {
		t.Fatalf("output lacks DHCP wait on interface:\n%s", out)
	}
	if strings.Contains(out, "lo0") {
		t.Fatalf("output names a link without carrier:\n%s", out)
	}
	if !strings.Contains(out, "elapsed)") || !strings.Contains(out, "katlos-install progress: network address acquired") {
		t.Fatalf("output lacks elapsed progress or acquisition line:\n%s", out)
	}
	// One line at start plus roughly one per 30s, not one per poll.
	if lines := strings.Count(out, "waiting for a DHCP address"); lines > 4 {
		t.Fatalf("%d progress lines, want at most 4:\n%s", lines, out)
	}
}

func TestHandoffWaitKeepsWaitingWhileCarrier(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: -1, links: []handoffLink{{"enp1s0", true}}, maxWait: time.Hour}

	_, err := f.waiter().wait(context.Background(), wildcard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait() error = %v, want the context error after an hour of waiting", err)
	}
}

func TestHandoffWaitNoCarrierRecoversWhenLinkReturns(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: -1, links: []handoffLink{{"enp1s0", false}}, maxWait: time.Hour}
	w := f.waiter()

	_, err := w.wait(context.Background(), wildcard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait() error = %v, want it to keep waiting without carrier", err)
	}
	if !strings.Contains(f.out.String(), "no interface has carrier") {
		t.Fatalf("output lacks no-carrier progress:\n%s", f.out.String())
	}

	f.out.Reset()
	f.links = []handoffLink{{"enp1s0", true}}
	f.addrAt = f.t.Sub(f.start) + 10*time.Second
	f.maxWait = 2 * time.Hour
	if _, err := w.wait(context.Background(), wildcard); err != nil {
		t.Fatalf("wait() after carrier returned error = %v", err)
	}
	if !strings.Contains(f.out.String(), "waiting for a DHCP address on enp1s0") {
		t.Fatalf("output lacks DHCP wait after carrier:\n%s", f.out.String())
	}
}

func TestHandoffWaitReportsLinkListFailure(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: -1, maxWait: time.Minute}
	w := f.waiter()
	w.links = func() ([]handoffLink, error) { return nil, errors.New("list network interfaces: boom") }

	_, _ = w.wait(context.Background(), wildcard)
	out := f.out.String()
	if !strings.Contains(out, "boom") || strings.Contains(out, "no interface has carrier") {
		t.Fatalf("output should report the link listing failure, not missing carrier:\n%s", out)
	}
}

func TestWaitForAnnouncementAddressReportsInitialFailureFirst(t *testing.T) {
	var out bytes.Buffer
	var atWait string
	_, _ = waitForAnnouncementAddress(&out, errors.New("download manifest: connection refused"), func() (string, error) {
		atWait = out.String()
		return "", context.Canceled
	})
	if !strings.Contains(atWait, "automatic install failed: download manifest: connection refused") {
		t.Fatalf("initial failure not printed before waiting:\n%q", atWait)
	}
}

func TestWaitForRemoteInputs(t *testing.T) {
	dir := t.TempDir()
	remote := dir + "/remote.yaml"
	local := dir + "/local.yaml"
	if err := os.WriteFile(remote, []byte("katlosImage:\n  url: https://example.test/katlos.raw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("katlosImage:\n  localRef: katlos.raw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		input     installer.BootInput
		wantWaits bool
	}{
		{"bundle URL", installer.BootInput{BundleURL: "https://example.test/c.katlcfg"}, true},
		{"manifest URL", installer.BootInput{ManifestURL: "https://example.test/m.yaml"}, true},
		{"local manifest with remote image", installer.BootInput{ManifestPath: remote}, true},
		{"local manifest with local image", installer.BootInput{ManifestPath: local}, false},
		{"unreadable local manifest", installer.BootInput{ManifestPath: dir + "/missing.yaml"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waits := 0
			err := waitForRemoteInputs(tt.input, installmedia.Media{}, func() (string, error) {
				waits++
				return "http://192.0.2.44:8080", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if (waits > 0) != tt.wantWaits {
				t.Fatalf("waited %d times, want wait=%v", waits, tt.wantWaits)
			}
		})
	}
}

// All-local inputs must not reach the real waiter even with no address and no
// carrier, since it has no deadline.
func TestWaitForRemoteInputsOfflineDoesNotBlock(t *testing.T) {
	path := t.TempDir() + "/m.yaml"
	if err := os.WriteFile(path, []byte("katlosImage:\n  localRef: katlos.raw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeHandoffWorld{addrAt: -1, links: []handoffLink{{"enp1s0", false}}, maxWait: time.Hour}
	w := f.waiter()

	err := waitForRemoteInputs(installer.BootInput{ManifestPath: path}, installmedia.Media{}, func() (string, error) {
		return w.wait(context.Background(), wildcard)
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.out.Len() != 0 {
		t.Fatalf("unexpected address-wait progress:\n%s", f.out.String())
	}
}

func TestWaitForRemoteInputsWaitsBeforeFetch(t *testing.T) {
	path := t.TempDir() + "/m.yaml"
	if err := os.WriteFile(path, []byte("katlosImage:\n  url: https://example.test/k.raw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeHandoffWorld{addrAt: 90 * time.Second, links: []handoffLink{{"enp1s0", true}}, maxWait: time.Hour}
	w := f.waiter()

	err := waitForRemoteInputs(installer.BootInput{ManifestPath: path}, installmedia.Media{}, func() (string, error) {
		return w.wait(context.Background(), wildcard)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "waiting for a DHCP address on enp1s0") || f.t.Sub(f.start) < 90*time.Second {
		t.Fatalf("did not wait for an address (elapsed %s):\n%s", f.t.Sub(f.start), f.out.String())
	}
}

func TestWaitForRemoteInputsAddressPresent(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: 0, maxWait: time.Hour}
	w := f.waiter()

	err := waitForRemoteInputs(installer.BootInput{BundleURL: "https://example.test/c.katlcfg"}, installmedia.Media{}, func() (string, error) {
		return w.wait(context.Background(), wildcard)
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.t != f.start || f.out.Len() != 0 {
		t.Fatalf("waited with an address present: elapsed %s, output %q", f.t.Sub(f.start), f.out.String())
	}
}
