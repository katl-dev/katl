package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	handoffAddressPollInterval = 250 * time.Millisecond
	// A link that has carrier is given as long as DHCP needs, because switch
	// spanning-tree forwarding delay and slow servers can exceed any fixed limit
	// and the installer has nothing else to do. Without carrier no address can
	// arrive until a cable is attached, so the wait ends after this grace.
	handoffNoCarrierGrace      = 5 * time.Minute
	handoffAddressProgressEach = 30 * time.Second
)

// handoffLink is the carrier state of one network interface.
type handoffLink struct {
	Name    string
	Carrier bool
}

// handoffWaiter waits for an address to announce the handoff API on. Its
// inputs are injectable so the waiting policy is testable without real time or
// interfaces.
type handoffWaiter struct {
	detectHost func() (string, error)
	links      func() ([]handoffLink, error)
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
	out        io.Writer
}

func waitHandoffAnnouncementBaseURL(ctx context.Context, addr net.Addr, out io.Writer) (string, error) {
	w := handoffWaiter{
		detectHost: handoffAnnouncementHost,
		links:      handoffLinks,
		now:        time.Now,
		sleep:      sleepContext,
		out:        out,
	}
	return w.wait(ctx, addr)
}

// wait returns the handoff base URL once a host address exists. It reports
// progress at the start, whenever the set of waiting interfaces changes, and
// periodically with the elapsed time.
func (w handoffWaiter) wait(ctx context.Context, addr net.Addr) (string, error) {
	start := w.now()
	var noCarrierSince time.Time
	var lastReport time.Time
	var lastState string
	for {
		baseURL, err := handoffAnnouncementBaseURLWithHost(addr, w.detectHost)
		if err == nil {
			if !lastReport.IsZero() {
				fmt.Fprintf(w.out, "katlos-install: network address acquired after %s\n", w.now().Sub(start).Round(time.Second))
			}
			return baseURL, nil
		}

		now := w.now()
		links, linkErr := w.links()
		var carrier []string
		for _, link := range links {
			if link.Carrier {
				carrier = append(carrier, link.Name)
			}
		}

		var state string
		if len(carrier) > 0 {
			noCarrierSince = time.Time{}
			state = "waiting for a DHCP address on " + strings.Join(carrier, ", ")
		} else {
			if noCarrierSince.IsZero() {
				noCarrierSince = now
			}
			if linkErr == nil && now.Sub(noCarrierSince) >= handoffNoCarrierGrace {
				return "", fmt.Errorf("no network link has carrier after %s: connect a network cable or check the switch port and the DHCP server, then reboot the installer: %w",
					now.Sub(noCarrierSince).Round(time.Second), err)
			}
			state = "waiting for a network link; no interface has carrier, check the cable and switch port"
		}

		if state != lastState || now.Sub(lastReport) >= handoffAddressProgressEach {
			msg := state
			if elapsed := now.Sub(start); elapsed >= time.Second {
				msg += fmt.Sprintf(" (%s elapsed)", elapsed.Round(time.Second))
			}
			reportInstallerProgress(w.out, msg, false)
			lastState, lastReport = state, now
		}

		if err := w.sleep(ctx, handoffAddressPollInterval); err != nil {
			return "", err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// handoffLinks reports carrier for non-loopback interfaces from the kernel's
// RUNNING flag, which is the carrier state systemd-networkd also reports.
func handoffLinks() ([]handoffLink, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.New("list network interfaces: " + err.Error())
	}
	var links []handoffLink
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		links = append(links, handoffLink{
			Name:    iface.Name,
			Carrier: iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagRunning != 0,
		})
	}
	return links, nil
}
