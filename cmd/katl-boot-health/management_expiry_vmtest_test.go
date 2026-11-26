//go:build linux && katl_vmtest

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func TestVMTestManagementExpiryIsScopedToArmedInvocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", dir)
	t.Setenv("INVOCATION_ID", "previous-invocation")
	previous, stopPrevious := vmtestTrialExpiry(func() {})
	previous("required link offline")
	stopPrevious()

	t.Setenv("INVOCATION_ID", "current-invocation")
	expired := make(chan struct{})
	observe, stop := vmtestTrialExpiry(func() { close(expired) })
	defer stop()
	path := filepath.Join(dir, "current-invocation")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expiry control before offline observation: %v, want absent", err)
	}

	observe("required link offline")
	if _, err := os.Lstat(filepath.Join(dir, "previous-invocation")); !os.IsNotExist(err) {
		t.Fatalf("expiry control for previous invocation: %v, want absent", err)
	}
	writer, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writer.Write([]byte{0})
	_ = writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("armed invocation did not expire")
	}
}

func TestManagementNetworkExpiryPreservesOfflineReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		inFlight bool
	}{
		{name: "between reads"},
		{name: "during a read", inFlight: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var expire func()
			var reported bool
			timing := managementTiming
			timing.pollInterval = time.Millisecond
			timing.reportInjectedExpiry = func() { reported = true }
			timing.trialExpiry = func(trigger func()) (func(string), func()) {
				expire = trigger
				return func(string) {
					if !test.inFlight {
						expire()
					}
				}, func() {}
			}
			oldRead := readNetworkctlState
			t.Cleanup(func() { readNetworkctlState = oldRead })
			reads := 0
			readNetworkctlState = func(ctx context.Context) ([]byte, error) {
				reads++
				if reads == 1 {
					return []byte(`{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline"}]}`), nil
				}
				expire()
				return nil, ctx.Err()
			}

			err := timing.wait(t.Context())
			const want = "configured management network is not ready after 90 seconds: required links are not online: management0"
			if err == nil || err.Error() != want {
				t.Fatalf("management network wait = %v, want %q", err, want)
			}
			if !reported {
				t.Fatal("effective injection was not reported")
			}
			if (reads > 1) != test.inFlight {
				t.Fatalf("networkctl ran %d times with in-flight expiry %t", reads, test.inFlight)
			}
		})
	}
}

func TestManagementNetworkDoesNotReportInjectionAfterDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldRead := readNetworkctlState
		t.Cleanup(func() { readNetworkctlState = oldRead })
		var expire func()
		timing := managementTiming
		timing.trialExpiry = func(trigger func()) (func(string), func()) {
			expire = trigger
			return nil, func() {}
		}
		timing.reportInjectedExpiry = func() { t.Error("reported injection after the real deadline won") }
		readNetworkctlState = func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			expire()
			return nil, ctx.Err()
		}

		const want = "configured management network is not ready after 90 seconds: network state has not been observed"
		if err := timing.wait(t.Context()); err == nil || err.Error() != want {
			t.Fatalf("management network wait = %v, want %q", err, want)
		}
	})
}
