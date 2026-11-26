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
)

type networkctlState struct {
	Interfaces []networkctlLink `json:"Interfaces"`
}

type networkctlLink struct {
	Name                     string    `json:"Name"`
	OperationalState         string    `json:"OperationalState"`
	CarrierState             string    `json:"CarrierState"`
	AddressState             string    `json:"AddressState"`
	NetworkFile              string    `json:"NetworkFile"`
	RequiredForOnline        bool      `json:"RequiredForOnline"`
	RequiredOperationalState [2]string `json:"RequiredOperationalStateForOnline"`
}

// Ready checks one networkd snapshot for configured management readiness.
func Ready(ctx context.Context, read func(context.Context) ([]byte, error)) (bool, string, error) {
	data, err := read(ctx)
	if err != nil {
		return false, "", err
	}

	var state networkctlState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, "", fmt.Errorf("decode networkctl state: %w", err)
	}

	var required, reasons []string
	routable := false
	for _, link := range state.Interfaces {
		if link.NetworkFile == "" || !link.RequiredForOnline {
			continue
		}
		required = append(required, link.Name)
		if !slices.Contains([]string{"carrier", "degraded-carrier", "enslaved"}, link.CarrierState) ||
			!link.meetsAddressMinimum() {
			reasons = append(reasons, fmt.Sprintf("systemd-networkd reports required link %s not online (carrier: %s, address: %s) (kernel carrier: %s)",
				link.Name, cmp.Or(link.CarrierState, "unknown"), cmp.Or(link.AddressState, "unknown"), kernelCarrier(link.Name)))
		}
		if link.OperationalState == "routable" {
			routable = true
		}
	}
	if len(required) == 0 {
		return false, "no systemd-networkd link is required for online", nil
	}
	if len(reasons) > 0 {
		return false, strings.Join(reasons, ". "), nil
	}
	if !routable {
		return false, "systemd-networkd reports no routable required link among " + strings.Join(required, ", "), nil
	}
	return true, "", nil
}

func kernelCarrier(name string) string {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return "unknown"
	}
	data, err := os.ReadFile(filepath.Join("/sys/class/net", name, "carrier"))
	if err != nil {
		// An administratively down interface can return EINVAL.
		return "unknown"
	}
	switch strings.TrimSpace(string(data)) {
	case "1":
		return "up"
	case "0":
		return "down"
	default:
		return "unknown"
	}
}

func (link networkctlLink) meetsAddressMinimum() bool {
	switch link.RequiredOperationalState[0] {
	case "routable":
		return link.AddressState == "routable"
	case "degraded":
		return link.AddressState == "degraded" || link.AddressState == "routable"
	default:
		return true
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
