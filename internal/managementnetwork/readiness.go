// Package managementnetwork qualifies the node's configured management links.
package managementnetwork

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/retry"
)

type networkctlState struct {
	Interfaces []networkctlLink `json:"Interfaces"`
}

type networkctlLink struct {
	Name                     string     `json:"Name"`
	OperationalState         string     `json:"OperationalState"`
	CarrierState             string     `json:"CarrierState"`
	AddressState             string     `json:"AddressState"`
	OnlineState              string     `json:"OnlineState"`
	NetworkFile              string     `json:"NetworkFile"`
	RequiredForOnline        bool       `json:"RequiredForOnline"`
	RequiredOperationalState *[2]string `json:"RequiredOperationalStateForOnline"`
}

// Observation is one networkd snapshot evaluated for management readiness.
type Observation struct {
	Ready bool
	// Reason names the failed checks when the snapshot is not ready.
	Reason string
	// CarrierDisagreements lists unready required links that systemd-networkd
	// reports without carrier while sysfs reports carrier and operstate up.
	CarrierDisagreements []string
}

// PollBackoff paces repeated reads of networkd state by every watcher of the
// management links.
func PollBackoff() retry.Backoff {
	return retry.NewBackoff(time.Second, 5*time.Second)
}

// Ready checks one networkd snapshot for configured management readiness.
func Ready(ctx context.Context, read func(context.Context) ([]byte, error)) (bool, string, error) {
	return ready(ctx, read, "/sys")
}

// Observe evaluates one networkd snapshot against the kernel's link state.
func Observe(ctx context.Context, read func(context.Context) ([]byte, error)) (Observation, error) {
	return observe(ctx, read, "/sys")
}

func ready(ctx context.Context, read func(context.Context) ([]byte, error), sysfs string) (bool, string, error) {
	observation, err := observe(ctx, read, sysfs)
	return observation.Ready, observation.Reason, err
}

// observe evaluates a snapshot, reading each unready required link's kernel
// state beneath sysfs.
func observe(ctx context.Context, read func(context.Context) ([]byte, error), sysfs string) (Observation, error) {
	data, err := read(ctx)
	if err != nil {
		return Observation{}, err
	}

	var state networkctlState
	if err := json.Unmarshal(data, &state); err != nil {
		return Observation{}, fmt.Errorf("decode networkctl state: %w", err)
	}

	var observation Observation
	var required, reasons []string
	routable := false
	for _, link := range state.Interfaces {
		if link.NetworkFile == "" || !link.RequiredForOnline {
			continue
		}
		required = append(required, link.Name)
		if reason := link.unready(); reason != "" {
			carrier := kernelCarrier(sysfs, link.Name)
			reasons = append(reasons, fmt.Sprintf("%s (carrier: %s, address: %s) (kernel carrier: %s)",
				reason, cmp.Or(link.CarrierState, "unknown"), cmp.Or(link.AddressState, "unknown"), carrier))
			if link.reportsNoCarrier() && carrier == "up" && readLinkAttribute(sysfs, link.Name, "operstate") == "up" {
				observation.CarrierDisagreements = append(observation.CarrierDisagreements, link.Name)
			}
		}
		if link.OperationalState == "routable" {
			routable = true
		}
	}
	switch {
	case len(required) == 0:
		observation.Reason = "no systemd-networkd link is required for online"
	case len(reasons) > 0:
		observation.Reason = strings.Join(reasons, ". ")
	case !routable:
		observation.Reason = "systemd-networkd reports no routable required link among " + strings.Join(required, ", ")
	default:
		observation.Ready = true
	}
	return observation, nil
}

func (link networkctlLink) reportsNoCarrier() bool {
	return link.CarrierState == "no-carrier" || link.OperationalState == "no-carrier"
}

// unready names the first check the link fails, or returns "" when it is
// ready. networkd's verdict comes first; Katl's own carrier and address checks
// then apply to a link networkd reports online.
func (link networkctlLink) unready() string {
	addressReady, knownMinimum := link.meetsAddressMinimum()
	var reason string
	switch {
	case link.OnlineState != "online":
		reason = fmt.Sprintf("systemd-networkd reports required link %s not online", link.Name)
	case !link.meetsCarrierMinimum():
		reason = fmt.Sprintf("required link %s has no carrier although systemd-networkd reports it online", link.Name)
	case !addressReady:
		reason = fmt.Sprintf("required link %s lacks its required address although systemd-networkd reports it online", link.Name)
	default:
		return ""
	}
	if !knownMinimum {
		reason += fmt.Sprintf(" (unrecognised required operational minimum %q)", link.RequiredOperationalState[0])
	}
	return reason
}

// meetsCarrierMinimum applies the carrier check only when the link's minimum
// is carrier or higher in networkd's operational-state order. An omitted or
// unrecognised minimum keeps the check.
func (link networkctlLink) meetsCarrierMinimum() bool {
	if link.RequiredOperationalState != nil {
		switch link.RequiredOperationalState[0] {
		case "missing", "off", "no-carrier", "dormant", "degraded-carrier":
			return true
		}
	}
	return slices.Contains([]string{"carrier", "degraded-carrier", "enslaved"}, link.CarrierState)
}

func kernelCarrier(sysfs, name string) string {
	// An administratively down interface can fail the read with EINVAL.
	switch readLinkAttribute(sysfs, name, "carrier") {
	case "1":
		return "up"
	case "0":
		return "down"
	default:
		return "unknown"
	}
}

// readLinkAttribute returns a link's trimmed sysfs attribute, or "" when it
// cannot be read.
func readLinkAttribute(sysfs, name, attribute string) string {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(sysfs, "class/net", name, attribute))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (link networkctlLink) meetsAddressMinimum() (ready, knownMinimum bool) {
	// networkd applies device-specific defaults before publishing OnlineState.
	if link.RequiredOperationalState == nil {
		return true, true
	}
	switch link.RequiredOperationalState[0] {
	case "routable":
		return link.AddressState == "routable", true
	case "degraded":
		return link.AddressState == "degraded" || link.AddressState == "routable", true
	case "missing", "off", "no-carrier", "dormant", "degraded-carrier", "carrier", "enslaved":
		return true, true
	default:
		return false, false
	}
}

// ReadState returns the effective link state reported by systemd-networkd.
func ReadState(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "networkctl", "list", "--json=short", "--no-pager")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("networkctl list: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
