package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	directJoinHostsMarker = "# katl-direct-control-plane-join"
	directJoinNFT         = "/usr/sbin/nft"
	directJoinNFTTable    = "katl_direct_join"
	directJoinMount       = "/usr/bin/mount"
	directJoinUnmount     = "/usr/bin/umount"
)

type directControlPlaneJoinPath struct {
	hostsPath    string
	hostsMounted bool
	nft          bool
}

func installDirectControlPlaneJoinPath(ctx context.Context, root, canonicalEndpoint, discoveryPath string, run ToolRunner) (*directControlPlaneJoinPath, error) {
	canonicalHost, canonicalPort, err := splitJoinEndpoint(canonicalEndpoint)
	if err != nil {
		return nil, fmt.Errorf("canonical control-plane endpoint: %w", err)
	}
	discoveryHost, discoveryPort, err := readJoinDiscoveryEndpoint(root, discoveryPath)
	if err != nil {
		return nil, err
	}
	discoveryAddress, err := resolveJoinAddress(ctx, discoveryHost)
	if err != nil {
		return nil, err
	}

	path := &directControlPlaneJoinPath{}
	sourceAddress, err := netip.ParseAddr(canonicalHost)
	if err != nil {
		if strings.ContainsAny(canonicalHost, " \t\r\n#") {
			return nil, fmt.Errorf("canonical control-plane endpoint host %q is invalid", canonicalHost)
		}
		if run == nil {
			return nil, fmt.Errorf("direct control-plane join path runner is required")
		}
		hostsPath, err := mountDirectJoinHosts(ctx, root, discoveryPath, discoveryAddress.String()+" "+canonicalHost+" "+directJoinHostsMarker, run)
		if err != nil {
			return nil, err
		}
		path.hostsPath = hostsPath
		path.hostsMounted = true
		sourceAddress = discoveryAddress
	}

	if sourceAddress == discoveryAddress && canonicalPort == discoveryPort {
		return path, nil
	}
	if sourceAddress.Is4() != discoveryAddress.Is4() {
		cleanupErr := path.cleanup(ctx, root, run)
		return nil, errors.Join(fmt.Errorf("canonical and direct control-plane endpoints use different address families"), cleanupErr)
	}
	if run == nil {
		cleanupErr := path.cleanup(ctx, root, nil)
		return nil, errors.Join(fmt.Errorf("direct control-plane join path runner is required"), cleanupErr)
	}
	if err := runDirectJoinNFT(ctx, run, "destroy", "table", "inet", directJoinNFTTable); err != nil {
		cleanupErr := path.cleanup(ctx, root, run)
		return nil, errors.Join(fmt.Errorf("remove stale direct control-plane join redirect: %w", err), cleanupErr)
	}
	if err := runDirectJoinNFT(ctx, run, "add", "table", "inet", directJoinNFTTable); err != nil {
		cleanupErr := path.cleanup(ctx, root, run)
		return nil, errors.Join(fmt.Errorf("create direct control-plane join redirect table: %w", err), cleanupErr)
	}
	path.nft = true
	commands := [][]string{
		{"add", "chain", "inet", directJoinNFTTable, "output", "{ type nat hook output priority dstnat; policy accept; }"},
		{"add", "rule", "inet", directJoinNFTTable, "output", joinAddressFamily(sourceAddress), "daddr", sourceAddress.String(), "tcp", "dport", strconv.Itoa(canonicalPort), "dnat", "to", net.JoinHostPort(discoveryAddress.String(), strconv.Itoa(discoveryPort))},
	}
	for _, command := range commands {
		if err := runDirectJoinNFT(ctx, run, command...); err != nil {
			cleanupErr := path.cleanup(ctx, root, run)
			return nil, errors.Join(fmt.Errorf("configure direct control-plane join redirect: %w", err), cleanupErr)
		}
	}
	return path, nil
}

func (path *directControlPlaneJoinPath) cleanup(ctx context.Context, root string, run ToolRunner) error {
	if path == nil {
		return nil
	}
	var err error
	if path.nft {
		if run == nil {
			err = errors.Join(err, fmt.Errorf("remove direct control-plane join redirect: runner is required"))
		} else if cleanupErr := runDirectJoinNFT(ctx, run, "destroy", "table", "inet", directJoinNFTTable); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove direct control-plane join redirect: %w", cleanupErr))
		}
	}
	if path.hostsMounted {
		result := run(ctx, []string{directJoinUnmount, rootedRuntimePath(root, "/etc/hosts")}, func(int) {})
		if result.Err != nil || result.ExitStatus != 0 {
			err = errors.Join(err, fmt.Errorf("unmount direct control-plane join hosts file: %s", toolFailure(result)))
		} else {
			path.hostsMounted = false
		}
	}
	if path.hostsPath != "" && !path.hostsMounted {
		if cleanupErr := os.Remove(path.hostsPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove direct control-plane join hosts file: %w", cleanupErr))
		} else {
			path.hostsPath = ""
		}
	}
	return err
}

func mountDirectJoinHosts(ctx context.Context, root, discoveryPath, entry string, run ToolRunner) (string, error) {
	target := rootedRuntimePath(root, "/etc/hosts")
	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("read hosts file for direct control-plane join: %w", err)
	}
	output := directJoinHostsData(data, entry)
	hostsPath := rootedRuntimePath(root, filepath.Join(filepath.Dir(discoveryPath), "hosts"))
	if err := os.WriteFile(hostsPath, output, 0o600); err != nil {
		return "", fmt.Errorf("write direct control-plane join hosts file: %w", err)
	}
	// The bind mount keeps the immutable runtime untouched and scopes the
	// canonical-name override to this kubeadm operation.
	result := run(ctx, []string{directJoinMount, "--bind", hostsPath, target}, func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		removeErr := os.Remove(hostsPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return "", errors.Join(fmt.Errorf("mount direct control-plane join hosts file: %s", toolFailure(result)), removeErr)
	}
	return hostsPath, nil
}

func directJoinHostsData(data []byte, entry string) []byte {
	var output bytes.Buffer
	for len(data) > 0 {
		line := data
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line = data[:index+1]
			data = data[index+1:]
		} else {
			data = nil
		}
		if !bytes.Contains(line, []byte(directJoinHostsMarker)) {
			_, _ = output.Write(line)
		}
	}
	if entry != "" {
		if output.Len() > 0 && output.Bytes()[output.Len()-1] != '\n' {
			_ = output.WriteByte('\n')
		}
		_, _ = output.WriteString(entry)
		_ = output.WriteByte('\n')
	}
	return output.Bytes()
}

func splitJoinEndpoint(endpoint string) (string, int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(endpoint))
	if err != nil || strings.TrimSpace(host) == "" {
		return "", 0, fmt.Errorf("endpoint %q must include a host and port", endpoint)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("endpoint %q has an invalid port", endpoint)
	}
	return host, port, nil
}

func readJoinDiscoveryEndpoint(root, discoveryPath string) (string, int, error) {
	discoveryPath = strings.TrimSpace(discoveryPath)
	if !strings.HasPrefix(discoveryPath, "/run/katl/bootstrap-join/") || filepath.Base(discoveryPath) != "discovery.conf" {
		return "", 0, fmt.Errorf("direct control-plane join requires an operation-scoped discovery kubeconfig")
	}
	data, err := os.ReadFile(rootedRuntimePath(root, discoveryPath))
	if err != nil {
		return "", 0, fmt.Errorf("read direct control-plane join discovery kubeconfig: %w", err)
	}
	var config struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil || len(config.Clusters) != 1 {
		return "", 0, fmt.Errorf("read direct control-plane join discovery kubeconfig: expected one cluster")
	}
	server, err := url.Parse(strings.TrimSpace(config.Clusters[0].Cluster.Server))
	if err != nil || server.Scheme != "https" || server.Hostname() == "" {
		return "", 0, fmt.Errorf("read direct control-plane join discovery kubeconfig: invalid HTTPS server")
	}
	return splitJoinEndpoint(server.Host)
}

func resolveJoinAddress(ctx context.Context, host string) (netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return address, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve direct control-plane join endpoint %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf("resolve direct control-plane join endpoint %q: no addresses", host)
	}
	return addresses[0].Unmap(), nil
}

func runDirectJoinNFT(ctx context.Context, run ToolRunner, args ...string) error {
	argv := slices.Concat([]string{directJoinNFT}, args)
	result := run(ctx, argv, func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("%s", toolFailure(result))
	}
	return nil
}

func joinAddressFamily(address netip.Addr) string {
	if address.Is4() {
		return "ip"
	}
	return "ip6"
}
