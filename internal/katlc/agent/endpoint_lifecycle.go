package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	"gopkg.in/yaml.v3"
)

const (
	endpointAdvertiserUnit    = "katl-app-bgp-api-vip.service"
	endpointAdvertiserCommand = "/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser"
	managedEndpointInterface  = "/usr/bin/networkctl"
	managedEndpointIP         = "/usr/bin/ip"
	managedEndpointKubectl    = "/usr/bin/kubectl"
)

type managedJoinEndpoint struct {
	VIP       string
	Interface string
	API       string
}

type managedJoinRoute struct {
	VIP     string
	NextHop string
	Device  string
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
func suspendManagedEndpointForJoin(ctx context.Context, root, discoveryPath string, run ToolRunner) (bool, *managedJoinRoute, error) {
	endpoint, configured, err := managedJoinEndpointConfig(root)
	if err != nil || !configured {
		return false, nil, err
	}
	if run == nil {
		return false, nil, fmt.Errorf("endpoint lifecycle runner is not configured")
	}
	if _, err := pauseManagedEndpoint(ctx, root, run); err != nil {
		return false, nil, err
	}
	result := run(ctx, []string{managedEndpointInterface, "down", endpoint.Interface}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, nil, fmt.Errorf("take managed control-plane endpoint off the local path: %s", toolFailure(result))
	}
	result = run(ctx, []string{managedEndpointIP, "address", "flush", "dev", endpoint.Interface, "to", endpoint.VIP}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return false, nil, fmt.Errorf("remove managed control-plane endpoint address from the local path: %s", toolFailure(result))
	}
	route, err := installManagedJoinRoute(ctx, root, discoveryPath, endpoint, run)
	if err != nil {
		return false, nil, err
	}
	result = run(ctx, []string{
		managedEndpointKubectl,
		"--kubeconfig", rootedRuntimePath(root, discoveryPath),
		"--server", endpoint.API,
		"--request-timeout=10s",
		"get", "--raw=/version",
	}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		cleanupErr := removeManagedJoinRoute(ctx, route, run)
		return false, nil, errors.Join(fmt.Errorf("verify managed control-plane endpoint through the join path: %s", toolFailure(result)), cleanupErr)
	}
	return true, route, nil
}

func installManagedJoinRoute(ctx context.Context, root, discoveryPath string, endpoint managedJoinEndpoint, run ToolRunner) (*managedJoinRoute, error) {
	discoveryHost, err := joinDiscoveryHost(root, discoveryPath)
	if err != nil {
		return nil, err
	}
	result := run(ctx, []string{managedEndpointIP, "-json", "route", "get", discoveryHost}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("inspect init-node route for managed control-plane join: %s", toolFailure(result))
	}
	var routes []struct {
		Destination string `json:"dst"`
		Gateway     string `json:"gateway"`
		Device      string `json:"dev"`
	}
	if err := json.Unmarshal(result.Stdout, &routes); err != nil || len(routes) != 1 {
		return nil, fmt.Errorf("inspect init-node route for managed control-plane join: invalid ip route response")
	}
	// A gateway means the init node is already reached through the fabric. The
	// direct override is only needed when the fabric would hairpin the stable
	// endpoint back onto the joining node's own link.
	device := strings.TrimSpace(routes[0].Device)
	if strings.TrimSpace(routes[0].Gateway) != "" {
		return nil, nil
	}
	nextHop, err := netip.ParseAddr(strings.TrimSpace(routes[0].Destination))
	if err != nil {
		return nil, fmt.Errorf("inspect init-node route for managed control-plane join: direct route has no IP destination")
	}
	vip, err := netip.ParsePrefix(endpoint.VIP)
	if err != nil {
		return nil, fmt.Errorf("inspect managed control-plane endpoint route: invalid VIP prefix")
	}
	if vip.Addr().Is4() != nextHop.Is4() {
		return nil, nil
	}
	if device == "" {
		return nil, fmt.Errorf("inspect init-node route for managed control-plane join: direct route has no device")
	}
	route := &managedJoinRoute{VIP: endpoint.VIP, NextHop: nextHop.String(), Device: device}
	result = run(ctx, []string{managedEndpointIP, "route", "add", route.VIP, "via", route.NextHop, "dev", route.Device}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("install temporary managed control-plane join route: %s", toolFailure(result))
	}
	return route, nil
}

func removeManagedJoinRoute(ctx context.Context, route *managedJoinRoute, run ToolRunner) error {
	if route == nil {
		return nil
	}
	result := run(ctx, []string{managedEndpointIP, "route", "del", route.VIP, "via", route.NextHop, "dev", route.Device}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("remove temporary managed control-plane join route: %s", toolFailure(result))
	}
	return nil
}

func joinDiscoveryHost(root, discoveryPath string) (string, error) {
	discoveryPath = strings.TrimSpace(discoveryPath)
	if !strings.HasPrefix(discoveryPath, "/run/katl/bootstrap-join/") || filepath.Base(discoveryPath) != "discovery.conf" {
		return "", fmt.Errorf("managed control-plane join requires an operation-scoped discovery kubeconfig")
	}
	data, err := os.ReadFile(rootedRuntimePath(root, discoveryPath))
	if err != nil {
		return "", fmt.Errorf("read managed control-plane join discovery kubeconfig: %w", err)
	}
	var config struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil || len(config.Clusters) != 1 {
		return "", fmt.Errorf("read managed control-plane join discovery kubeconfig: expected one cluster")
	}
	server, err := url.Parse(strings.TrimSpace(config.Clusters[0].Cluster.Server))
	if err != nil || server.Scheme != "https" || server.Hostname() == "" {
		return "", fmt.Errorf("read managed control-plane join discovery kubeconfig: invalid HTTPS server")
	}
	return server.Hostname(), nil
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
		API:       "https://" + net.JoinHostPort(config.Endpoint.Host, strconv.Itoa(config.Endpoint.Port)),
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
