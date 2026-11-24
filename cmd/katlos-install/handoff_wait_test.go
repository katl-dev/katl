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
	sleeps  int
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
			f.sleeps++
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
	if !strings.Contains(out, "(30s elapsed)") || !strings.Contains(out, "(1m0s elapsed)") {
		t.Fatalf("output lacks periodic elapsed time:\n%s", out)
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

func TestHandoffWaitNoCarrierFailsActionably(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: -1, links: []handoffLink{{"enp1s0", false}}, maxWait: time.Hour}

	_, err := f.waiter().wait(context.Background(), wildcard)
	if err == nil {
		t.Fatal("wait() succeeded without carrier or address")
	}
	for _, want := range []string{"no network link has carrier", "cable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if elapsed := f.t.Sub(f.start); elapsed < time.Minute || elapsed > 10*time.Minute {
		t.Fatalf("gave up after %s", elapsed)
	}
	if !strings.Contains(f.out.String(), "no interface has carrier") {
		t.Fatalf("output lacks no-carrier progress:\n%s", f.out.String())
	}
}

func TestHandoffWaitCarrierResetsNoCarrierGrace(t *testing.T) {
	f := &fakeHandoffWorld{addrAt: -1, links: []handoffLink{{"enp1s0", false}}, maxWait: 20 * time.Minute}
	w := f.waiter()
	w.links = func() ([]handoffLink, error) {
		// Carrier flaps up for the second before the grace would expire.
		up := f.t.Sub(f.start)%(handoffNoCarrierGrace-time.Second) < time.Second
		return []handoffLink{{"enp1s0", up}}, nil
	}

	_, err := w.wait(context.Background(), wildcard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait() error = %v, want it to outlast the grace while carrier returns", err)
	}
}
