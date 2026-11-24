package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
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
