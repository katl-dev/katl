//go:build linux && katl_vmtest

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type managementExpiryHooks struct {
	trialExpiry          func(expire func()) (observedOffline func(string), stop func())
	reportInjectedExpiry func()
}

type managementExpiryTrial struct {
	observedOffline      func(string)
	reportInjectedExpiry func()
}

var errInjectedManagementExpiry = fmt.Errorf("injected management network expiry: %w", context.DeadlineExceeded)

func (hooks managementExpiryHooks) start(ctx context.Context, cancel context.CancelFunc) (context.Context, context.CancelFunc, managementExpiryTrial) {
	if hooks.trialExpiry == nil {
		return ctx, cancel, managementExpiryTrial{}
	}
	healthCtx, expire := context.WithCancelCause(ctx)
	observedOffline, stop := hooks.trialExpiry(func() { expire(errInjectedManagementExpiry) })
	return healthCtx, func() {
			cancel()
			stop()
		}, managementExpiryTrial{
			observedOffline:      observedOffline,
			reportInjectedExpiry: hooks.reportInjectedExpiry,
		}
}

func (trial managementExpiryTrial) observeOffline(reason string) {
	if trial.observedOffline != nil {
		trial.observedOffline(reason)
	}
}

func (trial managementExpiryTrial) report(cause error) {
	if cause == errInjectedManagementExpiry && trial.reportInjectedExpiry != nil {
		trial.reportInjectedExpiry()
	}
}

func init() {
	managementTiming.trialExpiry = vmtestTrialExpiry
	managementTiming.reportInjectedExpiry = func() {
		fmt.Fprintln(os.Stderr, "vmtest management network expiry injected")
	}
}

func vmtestTrialExpiry(expire func()) (func(string), func()) {
	dir, invocation := os.Getenv("RUNTIME_DIRECTORY"), os.Getenv("INVOCATION_ID")
	path := filepath.Join(dir, invocation)
	var pipe *os.File
	var attempted bool
	var reportedReason string
	return func(reason string) {
			if !attempted {
				attempted = true
				if dir == "" || invocation == "" {
					fmt.Fprintln(os.Stderr, "vmtest management network expiry needs a systemd runtime directory and invocation")
					return
				}
				if err := unix.Mkfifo(path, 0o600); err != nil {
					fmt.Fprintf(os.Stderr, "create vmtest management network expiry FIFO: %v\n", err)
					return
				}
				// Opening both ends allows the reader to arm before a writer arrives.
				// Nonblocking mode lets Close interrupt the read on normal completion.
				var err error
				pipe, err = os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
				if err != nil {
					_ = os.Remove(path)
					fmt.Fprintf(os.Stderr, "open vmtest management network expiry FIFO: %v\n", err)
					return
				}
				go func(reader *os.File) {
					var trigger [1]byte
					if _, err := reader.Read(trigger[:]); err == nil {
						expire()
					}
				}(pipe)
			}
			if pipe != nil && reportedReason != reason {
				reportedReason = reason
				fmt.Fprintf(os.Stderr, "vmtest management network expiry armed: %s\n", reason)
			}
		}, func() {
			if pipe != nil {
				_ = pipe.Close()
				_ = os.Remove(path)
			}
		}
}
