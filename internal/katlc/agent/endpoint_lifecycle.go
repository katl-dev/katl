package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

const (
	endpointAdvertiserUnit    = "katl-app-bgp-api-vip.service"
	endpointAdvertiserCommand = "/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser"
)

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
