package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
)

// HandoffHook runs once, when the installer's handoff API at the VM's
// reserved address is waiting for configuration. That API state is the
// readiness signal; the serial console is evidence only.
type HandoffHook struct {
	Name string
	Run  func(context.Context, HandoffEvent) error
}

type HandoffEvent struct {
	Result Result
	Config VMConfig
	Plan   VMPlan
	// Base is the handoff API's base URL at the reserved address.
	Base string
}

// handoffProbeTimeout bounds one status request. While the installer waits
// for configuration, each status request rediscovers its disks.
const handoffProbeTimeout = 5 * time.Second

// handoffBase returns the handoff API's base URL at the VM's reserved address.
func handoffBase(config VMConfig) (string, error) {
	address := config.Network.Address
	if address == "" {
		return "", errors.New("reaching the installer handoff API requires a reserved address: attach the VM with its world node's VMNetwork()")
	}
	return "http://" + net.JoinHostPort(address, handoff.DefaultPort), nil
}

// probeHandoff reports whether the installer's handoff API at base is waiting
// for configuration. The API already serves while a boot-provided automatic
// install runs, but accepts configuration only once that install has been
// refused before disk mutation.
func probeHandoff(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, handoffProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/status", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("handoff API status = %s", response.Status)
	}
	var status handoff.HandoffStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return fmt.Errorf("decode handoff API status: %w", err)
	}
	if status.State != handoff.HandoffWaiting {
		return fmt.Errorf("handoff API state = %q, want %q", status.State, handoff.HandoffWaiting)
	}
	return nil
}

// handoffWaiting probes the handoff API once.
func (h *VMHandle) handoffWaiting(base string) bool {
	probe := h.runner.handoffProbe
	if probe == nil {
		probe = probeHandoff
	}
	return probe(h.ctx, base) == nil
}

// runHandoffHooks runs each handoff hook whose API is waiting for
// configuration.
func (h *VMHandle) runHandoffHooks(hooksRun []bool) error {
	for i, hook := range h.Config.HandoffHooks {
		if hooksRun[i] {
			continue
		}
		base, err := handoffBase(h.Config)
		if err != nil {
			return fmt.Errorf("handoff hook %q: %w", hook.Name, err)
		}
		if !h.handoffWaiting(base) {
			continue
		}
		if err := hook.Run(h.ctx, HandoffEvent{Result: h.Result, Config: h.Config, Plan: h.Plan, Base: base}); err != nil {
			return fmt.Errorf("handoff hook %q failed: %w", hook.Name, err)
		}
		hooksRun[i] = true
	}
	return nil
}

// WaitForHandoffAPI blocks until the installer's handoff API at the VM's
// reserved address is waiting for configuration and returns its base URL.
func (h *VMHandle) WaitForHandoffAPI(interval time.Duration) (string, error) {
	base, err := handoffBase(h.Config)
	if err != nil {
		return "", err
	}
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if h.handoffWaiting(base) {
			return base, nil
		}
		select {
		case <-h.done:
			waitErr := h.Wait()
			if waitErr == nil {
				waitErr = errors.New("libvirt domain exited")
			}
			return "", fmt.Errorf("installer handoff API never waited for configuration at %s: %w", base, waitErr)
		case <-h.ctx.Done():
			return "", fmt.Errorf("installer handoff API never waited for configuration at %s: %w", base, context.Cause(h.ctx))
		case <-ticker.C:
		}
	}
}
