package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

const (
	endpointAdvertiserUnit    = "katl-app-bgp-api-vip.service"
	endpointAdvertiserCommand = "/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser"
	managedEndpointInterface  = "/usr/bin/networkctl"
	managedEndpointIP         = "/usr/bin/ip"
)

type managedJoinEndpoint struct {
	VIP       string
	Interface string
}

func pauseManagedEndpoint(ctx context.Context, root string, run ToolRunner) (bool, error) {
	configured, err := managedEndpointConfigured(root)
	if err != nil || !configured {
		return false, err
	}
	if run == nil {
		return false, fmt.Errorf("endpoint lifecycle runner is not configured")
	}
	result := run(ctx, []string{"systemctl", "stop", endpointAdvertiserUnit}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, fmt.Errorf("stop managed control-plane endpoint: %s", toolFailure(result))
	}
	result = run(ctx, []string{endpointAdvertiserCommand, "withdraw"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, fmt.Errorf("confirm managed control-plane endpoint withdrawal: %s", toolFailure(result))
	}
	return true, nil
}

func resumeManagedEndpoint(ctx context.Context, root string, run ToolRunner) error {
	configured, err := managedEndpointConfigured(root)
	if err != nil || !configured {
		return err
	}
	if run == nil {
		return fmt.Errorf("endpoint lifecycle runner is not configured")
	}
	result := run(ctx, []string{"systemctl", "start", endpointAdvertiserUnit}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("resume managed control-plane endpoint: %s", toolFailure(result))
	}
	return nil
}

// suspendManagedEndpointForJoin keeps a joining control-plane node from
// capturing stable-endpoint traffic before its local API server exists. The
// generated endpoint uses a Katl-owned dummy device, so taking that device
// offline does not disturb the node's management network.
func suspendManagedEndpointForJoin(ctx context.Context, root string, run ToolRunner) (bool, error) {
	endpoint, configured, err := managedJoinEndpointConfig(root)
	if err != nil || !configured {
		return false, err
	}
	if run == nil {
		return false, fmt.Errorf("endpoint lifecycle runner is not configured")
	}
	if _, err := pauseManagedEndpoint(ctx, root, run); err != nil {
		return false, err
	}
	result := run(ctx, []string{managedEndpointInterface, "down", endpoint.Interface}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, fmt.Errorf("take managed control-plane endpoint off the local path: %s", toolFailure(result))
	}
	result = run(ctx, []string{managedEndpointIP, "address", "flush", "dev", endpoint.Interface, "to", endpoint.VIP}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, fmt.Errorf("remove managed control-plane endpoint address from the local path: %s", toolFailure(result))
	}
	return true, nil
}

func resumeManagedEndpointAfterJoin(ctx context.Context, root string, run ToolRunner) error {
	endpoint, configured, err := managedJoinEndpointConfig(root)
	if err != nil || !configured {
		return err
	}
	if run == nil {
		return fmt.Errorf("endpoint lifecycle runner is not configured")
	}
	result := run(ctx, []string{managedEndpointInterface, "up", endpoint.Interface}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("restore managed control-plane endpoint interface: %s", toolFailure(result))
	}
	// networkctl applies the generated desired state asynchronously. Replace the
	// exact generated host address as an idempotent, immediate handoff so the
	// stable endpoint is usable for post-kubeadm health checks.
	result = run(ctx, []string{managedEndpointIP, "address", "replace", endpoint.VIP, "dev", endpoint.Interface}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("restore managed control-plane endpoint address: %s", toolFailure(result))
	}
	return resumeManagedEndpoint(ctx, root, run)
}

func managedJoinEndpointConfig(root string) (managedJoinEndpoint, bool, error) {
	configured, err := managedEndpointConfigured(root)
	if err != nil || !configured {
		return managedJoinEndpoint{}, false, err
	}
	file, err := os.Open(rootedRuntimePath(root, bgpapivip.ConfigPath))
	if err != nil {
		return managedJoinEndpoint{}, false, fmt.Errorf("open managed control-plane endpoint configuration: %w", err)
	}
	object, decodeErr := bgpapivip.Decode(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return managedJoinEndpoint{}, false, errors.Join(decodeErr, closeErr)
	}
	config, err := bgpapivip.Normalize(object.Spec)
	if err != nil {
		return managedJoinEndpoint{}, false, fmt.Errorf("normalize managed control-plane endpoint configuration: %w", err)
	}
	if config.VIPInterface.Kind != "dummy" {
		return managedJoinEndpoint{}, false, fmt.Errorf("managed control-plane join requires a Katl-owned dummy VIP interface, got %q", config.VIPInterface.Kind)
	}
	return managedJoinEndpoint{
		VIP:       strings.TrimSpace(config.Endpoint.VIP),
		Interface: strings.TrimSpace(config.VIPInterface.Name),
	}, true, nil
}

func managedEndpointConfigured(root string) (bool, error) {
	_, err := os.Stat(rootedRuntimePath(root, bgpapivip.AdvertisementEnabledPath))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect managed control-plane endpoint: %w", err)
	}
}
