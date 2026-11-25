package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/installmedia"
	"gopkg.in/yaml.v3"
)

const (
	handoffAddressPollInterval = 250 * time.Millisecond
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

// wait returns the handoff base URL once a host address exists. It has no
// deadline: katlos-install.service does not restart, so giving up would close
// the handoff API and force a reboot, while waiting recovers by itself when a
// cable or DHCP server returns. It reports progress at the start, whenever the
// waiting state changes, and periodically with the elapsed time.
func (w handoffWaiter) wait(ctx context.Context, addr net.Addr) (string, error) {
	start := w.now()
	var lastReport time.Time
	var lastState string
	for {
		baseURL, err := handoffAnnouncementBaseURLWithHost(addr, w.detectHost)
		if err == nil {
			if !lastReport.IsZero() {
				reportInstallerProgress(w.out, fmt.Sprintf("network address acquired after %s", w.now().Sub(start).Round(time.Second)), false)
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
		switch {
		case linkErr != nil:
			state = "waiting for a network address; " + linkErr.Error()
		case len(carrier) > 0:
			state = "waiting for a DHCP address on " + strings.Join(carrier, ", ")
		default:
			state = "waiting for a network link; no interface has carrier, connect a cable or check the switch port"
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
// RUNNING flag, which reflects the interface's operational state and is a
// close but not exact match for the carrier state systemd-networkd reports.
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

// waitForRemoteInputs blocks until an address exists when the automatic
// install must fetch anything over the network, and returns immediately for
// all-local inputs so offline media installs never wait.
func waitForRemoteInputs(input installer.BootInput, media installmedia.Media, wait func() (string, error)) error {
	if !automaticInstallNeedsNetwork(input, media) {
		return nil
	}
	_, err := wait()
	return err
}

// automaticInstallNeedsNetwork reports whether the install reads a remote
// source: a bundle or manifest URL, or a KatlOS image URL in the manifest the
// install will run. An unreadable local input reports false so the install
// itself reports the error instead of hanging on the network.
func automaticInstallNeedsNetwork(input installer.BootInput, media installmedia.Media) bool {
	switch {
	case input.BundleURL != "" || input.ManifestURL != "":
		return true
	case input.BundlePath != "":
		selected, err := configbundle.ReadSelectedNodeFile(input.BundlePath, configbundle.ReadOptions{
			ExpectedDigest:     input.BundleDigest,
			NodeName:           input.NodeName,
			DefaultKatlosImage: media.Image,
		})
		if err != nil {
			return false
		}
		return !selected.KatlosImageFromMedia && selected.InstallManifest.KatlosImage.URL != ""
	case input.ManifestPath != "":
		data, err := os.ReadFile(input.ManifestPath)
		if err != nil {
			return false
		}
		var m struct {
			KatlosImage struct {
				URL string `yaml:"url"`
			} `yaml:"katlosImage"`
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			return false
		}
		return strings.TrimSpace(m.KatlosImage.URL) != ""
	}
	return false
}
