package controlplaneendpoint

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

const DefaultPort = 6443

var dnsLabelRE = regexp.MustCompile(`(?i)^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// Config is the complete operator-owned control-plane endpoint intent. The
// absence of Advertisement means that the endpoint is externally owned.
type Config struct {
	Host          string         `yaml:"host" json:"host"`
	Port          int            `yaml:"port,omitempty" json:"port,omitempty"`
	Advertisement *Advertisement `yaml:"advertisement,omitempty" json:"advertisement,omitempty"`
}

type Advertisement struct {
	VIP string `yaml:"vip" json:"vip"`
}

type Plan struct {
	Config    Config `json:"config"`
	Endpoint  string `json:"endpoint"`
	VIPPrefix string `json:"vipPrefix,omitempty"`
}

func Normalize(input Config) (Plan, error) {
	config := input
	config.Host = strings.TrimSpace(config.Host)
	if err := validateHost(config.Host); err != nil {
		return Plan{}, err
	}
	if config.Port == 0 {
		config.Port = DefaultPort
	}
	if config.Port < 1 || config.Port > 65535 {
		return Plan{}, fmt.Errorf("controlPlaneEndpoint.port must be between 1 and 65535")
	}

	plan := Plan{
		Config:   config,
		Endpoint: net.JoinHostPort(config.Host, strconv.Itoa(config.Port)),
	}
	if config.Advertisement == nil {
		return plan, nil
	}

	advertisement := *config.Advertisement
	vip, err := validateVIP(advertisement.VIP)
	if err != nil {
		return Plan{}, err
	}
	advertisement.VIP = vip.String()
	plan.VIPPrefix = netip.PrefixFrom(vip, 32).String()
	if hostIP, err := netip.ParseAddr(config.Host); err == nil && hostIP != vip {
		return Plan{}, fmt.Errorf("controlPlaneEndpoint.host IP %q must equal advertisement.vip %q", hostIP, vip)
	}
	config.Advertisement = &advertisement
	plan.Config = config
	return plan, nil
}

func Managed(config Config) bool {
	return config.Advertisement != nil
}

func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("controlPlaneEndpoint.host is required")
	}
	if strings.Contains(host, "://") || strings.ContainsAny(host, `/\\`) {
		return fmt.Errorf("controlPlaneEndpoint.host must be a DNS name or IP address, not a URL or path")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
			return fmt.Errorf("controlPlaneEndpoint.host %q is not a usable address", host)
		}
		return nil
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") {
		return fmt.Errorf("controlPlaneEndpoint.host %q is not a valid DNS name", host)
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabelRE.MatchString(label) {
			return fmt.Errorf("controlPlaneEndpoint.host %q is not a valid DNS name", host)
		}
	}
	return nil
}

func validateVIP(value string) (netip.Addr, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "/") {
		return netip.Addr{}, fmt.Errorf("controlPlaneEndpoint.advertisement.vip must be a bare IPv4 address, not CIDR notation")
	}
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("controlPlaneEndpoint.advertisement.vip must be a bare IPv4 address")
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr == netip.MustParseAddr("255.255.255.255") {
		return netip.Addr{}, fmt.Errorf("controlPlaneEndpoint.advertisement.vip %q is not a usable routed address", value)
	}
	return addr, nil
}
