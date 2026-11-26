// Package managementnetwork qualifies the node's configured management links.
package managementnetwork

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type networkctlState struct {
	Interfaces []networkctlLink `json:"Interfaces"`
}

type networkctlLink struct {
	Name              string  `json:"Name"`
	OperationalState  string  `json:"OperationalState"`
	OnlineState       *string `json:"OnlineState"`
	NetworkFile       string  `json:"NetworkFile"`
	RequiredForOnline bool    `json:"RequiredForOnline"`
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

	var required, offline []string
	routable := false
	for _, link := range state.Interfaces {
		if link.NetworkFile == "" || !link.RequiredForOnline {
			continue
		}
		required = append(required, link.Name)
		if link.OnlineState == nil || *link.OnlineState != "online" {
			offline = append(offline, link.Name)
		}
		if link.OperationalState == "routable" {
			routable = true
		}
	}
	if len(required) == 0 {
		return false, "no systemd-networkd link is required for online", nil
	}
	if len(offline) > 0 {
		return false, "required links are not online: " + strings.Join(offline, ", "), nil
	}
	if !routable {
		return false, "no required link is routable: " + strings.Join(required, ", "), nil
	}
	return true, "", nil
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
