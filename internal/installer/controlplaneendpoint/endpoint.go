package controlplaneendpoint

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultPort              = 6443
	DefaultRouteExchangePort = 179
)

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
	BGP *BGP   `yaml:"bgp,omitempty" json:"bgp,omitempty"`
}

type BGP struct {
	LocalASN      uint32          `yaml:"localASN" json:"localASN"`
	Peers         []Peer          `yaml:"peers" json:"peers"`
	RouteExchange []RouteExchange `yaml:"routeExchange,omitempty" json:"routeExchange,omitempty"`
}

type Peer struct {
	Address string `yaml:"address" json:"address"`
	ASN     uint32 `yaml:"asn" json:"asn"`
}

type RouteExchange struct {
	Name           string           `yaml:"name" json:"name"`
	ListenPort     int              `yaml:"listenPort,omitempty" json:"listenPort,omitempty"`
	PeerASN        uint32           `yaml:"peerASN,omitempty" json:"peerASN,omitempty"`
	ExportToFabric []PrefixEnvelope `yaml:"exportToFabric,omitempty" json:"exportToFabric,omitempty"`
}

type PrefixEnvelope struct {
	CIDR         string `yaml:"cidr" json:"cidr"`
	PrefixLength *int   `yaml:"prefixLength,omitempty" json:"prefixLength,omitempty"`
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Plan struct {
	Config    Config    `json:"config"`
	Endpoint  string    `json:"endpoint"`
	VIPPrefix string    `json:"vipPrefix,omitempty"`
	Warnings  []Warning `json:"warnings,omitempty"`
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
	if advertisement.BGP == nil {
		return Plan{}, fmt.Errorf("controlPlaneEndpoint.advertisement.bgp is required")
	}
	bgp, warnings, err := normalizeBGP(*advertisement.BGP, vip)
	if err != nil {
		return Plan{}, err
	}
	advertisement.BGP = &bgp
	config.Advertisement = &advertisement
	plan.Config = config
	plan.Warnings = warnings
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

func normalizeBGP(input BGP, vip netip.Addr) (BGP, []Warning, error) {
	bgp := input
	if err := validateASN("controlPlaneEndpoint.advertisement.bgp.localASN", bgp.LocalASN); err != nil {
		return BGP{}, nil, err
	}
	if len(bgp.Peers) == 0 {
		return BGP{}, nil, fmt.Errorf("controlPlaneEndpoint.advertisement.bgp.peers must not be empty")
	}
	seenPeers := map[netip.Addr]struct{}{}
	for i := range bgp.Peers {
		path := fmt.Sprintf("controlPlaneEndpoint.advertisement.bgp.peers[%d]", i)
		addr, err := usableIPv4(bgp.Peers[i].Address)
		if err != nil {
			return BGP{}, nil, fmt.Errorf("%s.address must be a usable IPv4 address", path)
		}
		if _, exists := seenPeers[addr]; exists {
			return BGP{}, nil, fmt.Errorf("%s.address %q duplicates another peer", path, addr)
		}
		seenPeers[addr] = struct{}{}
		bgp.Peers[i].Address = addr.String()
		if err := validateASN(path+".asn", bgp.Peers[i].ASN); err != nil {
			return BGP{}, nil, err
		}
	}
	sort.Slice(bgp.Peers, func(i, j int) bool {
		return bgp.Peers[i].Address < bgp.Peers[j].Address
	})

	exchanges, warnings, err := normalizeRouteExchanges(bgp.RouteExchange, bgp.LocalASN, vip)
	if err != nil {
		return BGP{}, nil, err
	}
	bgp.RouteExchange = exchanges
	return bgp, warnings, nil
}

func normalizeRouteExchanges(input []RouteExchange, localASN uint32, vip netip.Addr) ([]RouteExchange, []Warning, error) {
	exchanges := append([]RouteExchange(nil), input...)
	seenNames := map[string]struct{}{}
	seenPorts := map[int]struct{}{}
	var warnings []Warning
	for i := range exchanges {
		path := fmt.Sprintf("controlPlaneEndpoint.advertisement.bgp.routeExchange[%d]", i)
		exchange := &exchanges[i]
		exchange.Name = strings.TrimSpace(exchange.Name)
		if len(exchange.Name) > 63 || !dnsLabelRE.MatchString(exchange.Name) {
			return nil, nil, fmt.Errorf("%s.name %q must be a DNS-label-style name", path, exchange.Name)
		}
		if _, exists := seenNames[exchange.Name]; exists {
			return nil, nil, fmt.Errorf("%s.name %q duplicates another route exchange", path, exchange.Name)
		}
		seenNames[exchange.Name] = struct{}{}
		if exchange.ListenPort == 0 {
			if len(exchanges) != 1 {
				return nil, nil, fmt.Errorf("%s.listenPort is required when more than one route exchange is configured", path)
			}
			exchange.ListenPort = DefaultRouteExchangePort
		}
		if exchange.ListenPort < 1 || exchange.ListenPort > 65535 {
			return nil, nil, fmt.Errorf("%s.listenPort must be between 1 and 65535", path)
		}
		if _, exists := seenPorts[exchange.ListenPort]; exists {
			return nil, nil, fmt.Errorf("%s.listenPort %d duplicates another route exchange", path, exchange.ListenPort)
		}
		seenPorts[exchange.ListenPort] = struct{}{}
		if exchange.PeerASN == 0 {
			exchange.PeerASN = localASN
		}
		if err := validateASN(path+".peerASN", exchange.PeerASN); err != nil {
			return nil, nil, err
		}
		envelopes, includesVIP, err := normalizeEnvelopes(path+".exportToFabric", exchange.ExportToFabric, vip)
		if err != nil {
			return nil, nil, err
		}
		exchange.ExportToFabric = envelopes
		if includesVIP {
			warnings = append(warnings, Warning{
				Code:    "route-exchange-includes-api-vip",
				Message: fmt.Sprintf("route exchange %q may export the API VIP independently of Katl's health gate", exchange.Name),
			})
		}
	}
	sort.Slice(exchanges, func(i, j int) bool { return exchanges[i].Name < exchanges[j].Name })
	return exchanges, warnings, nil
}

func normalizeEnvelopes(path string, input []PrefixEnvelope, vip netip.Addr) ([]PrefixEnvelope, bool, error) {
	byKey := map[string]PrefixEnvelope{}
	includesVIP := false
	for i, envelope := range input {
		field := fmt.Sprintf("%s[%d]", path, i)
		prefix, err := netip.ParsePrefix(strings.TrimSpace(envelope.CIDR))
		if err != nil || !prefix.Addr().Is4() {
			return nil, false, fmt.Errorf("%s.cidr must be an IPv4 CIDR", field)
		}
		prefix = prefix.Masked()
		envelope.CIDR = prefix.String()
		if envelope.PrefixLength != nil {
			length := *envelope.PrefixLength
			if length < prefix.Bits() || length > 32 {
				return nil, false, fmt.Errorf("%s.prefixLength must be between %d and 32", field, prefix.Bits())
			}
		}
		if prefix.Contains(vip) && (envelope.PrefixLength == nil || *envelope.PrefixLength == 32) {
			includesVIP = true
		}
		key := envelope.CIDR + "/any"
		if envelope.PrefixLength != nil {
			key = envelope.CIDR + "/" + strconv.Itoa(*envelope.PrefixLength)
		}
		byKey[key] = envelope
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]PrefixEnvelope, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out, includesVIP, nil
}

func usableIPv4(value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr == netip.MustParseAddr("255.255.255.255") {
		return netip.Addr{}, fmt.Errorf("not usable IPv4")
	}
	return addr, nil
}

func validateASN(path string, asn uint32) error {
	switch asn {
	case 0, 23456, 65535, 4294967295:
		return fmt.Errorf("%s %d is reserved and cannot be used", path, asn)
	default:
		return nil
	}
}
