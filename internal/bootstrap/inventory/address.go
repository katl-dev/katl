package inventory

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Host is a literal IP address or a host name. The zero Host is unset.
//
// The Parse functions validate new operator input: a host name must be a DNS
// name and an IPv6 address must not carry a zone. ParseConfiguredManagementAddress
// also accepts a host name that is not a DNS name but can still name a host.
// The Decode functions, and UnmarshalText, read values a release has already
// recorded, such as a saved context, a config bundle or an agent's report, and
// accept every host that earlier releases recorded, so upgrading never makes
// saved data unreadable.
type Host struct {
	addr netip.Addr
	name string
}

// ParseHost parses a literal IP address or a DNS name. A v4-mapped IPv6
// address is unmapped.
func ParseHost(value string) (Host, error) {
	return parseHost(value, hostDNSName)
}

// hostRule says which host names a parse accepts. Every rule accepts a
// literal IP address.
type hostRule int

const (
	// hostDNSName accepts a DNS name, and no IPv6 zone.
	hostDNSName hostRule = iota
	// hostAnyName also accepts a name that is not a DNS name but that
	// resolvers commonly resolve, such as cp_1.home.arpa, and no IPv6 zone.
	hostAnyName
	// hostRecorded accepts any value, as earlier releases recorded hosts
	// without validating them.
	hostRecorded
)

func parseHost(value string, rule hostRule) (Host, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Host{}, fmt.Errorf("host is required")
	}
	addr, err := netip.ParseAddr(value)
	switch {
	case err == nil && addr.Zone() == "":
		return Host{addr: addr.Unmap()}, nil
	case err == nil && rule != hostRecorded:
		return Host{}, fmt.Errorf("host %q must not carry an IPv6 zone", value)
	case rule == hostDNSName && !validDNSName(value):
		return Host{}, fmt.Errorf("host %q must be an IP address or a DNS name", value)
	case rule == hostAnyName && !validHostName(value):
		return Host{}, fmt.Errorf("host %q must be an IP address or a host name", value)
	}
	return Host{name: value}, nil
}

// HostFromAddr returns addr as a Host.
func HostFromAddr(addr netip.Addr) Host {
	return Host{addr: addr.Unmap()}
}

// Addr returns the host's literal address, or false for a DNS name or an
// unset host.
func (h Host) Addr() (netip.Addr, bool) {
	return h.addr, h.addr.IsValid()
}

// NotDNSName reports whether the host is a name that is not a valid DNS name,
// such as cp_1.home.arpa.
func (h Host) NotDNSName() bool {
	return h.name != "" && !validDNSName(h.name)
}

func (h Host) IsZero() bool {
	return !h.addr.IsValid() && h.name == ""
}

func (h Host) String() string {
	if h.addr.IsValid() {
		return h.addr.String()
	}
	return h.name
}

func (h Host) MarshalText() ([]byte, error) {
	return []byte(h.String()), nil
}

func (h *Host) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*h = Host{}
		return nil
	}
	parsed, err := parseHost(string(text), hostRecorded)
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// HostPort is a host and a port. The zero HostPort is unset.
type HostPort struct {
	Host Host
	Port uint16
}

// ParseHostPort parses host:port, with an IPv6 host in brackets.
func ParseHostPort(value string) (HostPort, error) {
	return parseHostPort(value, hostDNSName)
}

// DecodeHostPort parses a recorded host:port, accepting any host an earlier
// release recorded.
func DecodeHostPort(value string) (HostPort, error) {
	return parseHostPort(value, hostRecorded)
}

func parseHostPort(value string, rule hostRule) (HostPort, error) {
	value = strings.TrimSpace(value)
	hostText, portText, err := net.SplitHostPort(value)
	if err != nil || strings.Contains(value, "://") {
		return HostPort{}, fmt.Errorf("%q must be host:port", value)
	}
	host, err := parseHost(hostText, rule)
	if err != nil {
		return HostPort{}, err
	}
	port, err := parsePort(portText)
	if err != nil {
		return HostPort{}, fmt.Errorf("%q: %w", value, err)
	}
	return HostPort{Host: host, Port: port}, nil
}

func (e HostPort) IsZero() bool {
	return e.Host.IsZero()
}

func (e HostPort) String() string {
	if e.IsZero() {
		return ""
	}
	return net.JoinHostPort(e.Host.String(), strconv.Itoa(int(e.Port)))
}

// ManagementAddress returns the endpoint as a management address that names
// its port, as a saved context records it.
func (e HostPort) ManagementAddress() ManagementAddress {
	return ManagementAddress{Host: e.Host, Port: e.Port}
}

func (e HostPort) MarshalText() ([]byte, error) {
	return []byte(e.String()), nil
}

func (e *HostPort) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*e = HostPort{}
		return nil
	}
	parsed, err := DecodeHostPort(string(text))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// ManagementAddress is a node's management.address: a host, optionally with
// the agent port. The zero ManagementAddress is unset.
type ManagementAddress struct {
	Host Host
	// Port is zero when the address names no port, and the agent listens on
	// ManagementPort.
	Port uint16
}

// ParseManagementAddress parses a host, host:port, or tcp://host[:port].
func ParseManagementAddress(value string) (ManagementAddress, error) {
	return parseManagementAddress(value, hostDNSName)
}

// ParseConfiguredManagementAddress parses a cluster configuration's
// management.address. Besides what ParseManagementAddress accepts, it accepts
// a host name that is not a DNS name, such as cp_1.home.arpa, which earlier
// releases accepted and resolvers commonly resolve. Host.NotDNSName reports
// such a host, so that the caller can warn about it.
func ParseConfiguredManagementAddress(value string) (ManagementAddress, error) {
	return parseManagementAddress(value, hostAnyName)
}

// DecodeManagementAddress parses a recorded management address. Earlier
// releases recorded management.address without validating it, so a value
// that does not parse is kept verbatim as its host: it loads, and the node
// is unreachable until the address is corrected, as it was before. An empty
// value is unset.
func DecodeManagementAddress(value string) ManagementAddress {
	value = strings.TrimSpace(value)
	if value == "" {
		return ManagementAddress{}
	}
	if address, err := parseManagementAddress(value, hostRecorded); err == nil {
		return address
	}
	return ManagementAddress{Host: Host{name: value}}
}

func parseManagementAddress(value string, rule hostRule) (ManagementAddress, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ManagementAddress{}, fmt.Errorf("management address is required")
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return ManagementAddress{}, fmt.Errorf("management address %q is invalid: %w", value, err)
		}
		if parsed.Scheme != "tcp" {
			return ManagementAddress{}, fmt.Errorf("management address %q URL scheme must be tcp", value)
		}
		if parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return ManagementAddress{}, fmt.Errorf("management address %q must be a tcp:// host or host:port without credentials, path, query, or fragment", value)
		}
		value = parsed.Host
	}
	bare := value
	if strings.HasPrefix(bare, "[") && strings.HasSuffix(bare, "]") {
		bare = bare[1 : len(bare)-1]
	}
	if _, err := netip.ParseAddr(bare); err == nil || !strings.Contains(value, ":") {
		host, err := parseHost(bare, rule)
		if err != nil {
			return ManagementAddress{}, fmt.Errorf("management address %q: %w", value, err)
		}
		return ManagementAddress{Host: host}, nil
	}
	hostText, portText, err := net.SplitHostPort(value)
	if err != nil {
		return ManagementAddress{}, fmt.Errorf("management address %q must be an IP, hostname, host:port, or tcp:// URL", value)
	}
	host, err := parseHost(hostText, rule)
	if err != nil {
		return ManagementAddress{}, fmt.Errorf("management address %q: %w", value, err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return ManagementAddress{}, fmt.Errorf("management address %q: %w", value, err)
	}
	return ManagementAddress{Host: host, Port: port}, nil
}

func (a ManagementAddress) IsZero() bool {
	return a.Host.IsZero()
}

// Endpoint returns where the agent listens: the address's own port, or
// ManagementPort.
func (a ManagementAddress) Endpoint() HostPort {
	if a.IsZero() {
		return HostPort{}
	}
	port := a.Port
	if port == 0 {
		port = ManagementPort
	}
	return HostPort{Host: a.Host, Port: port}
}

// String returns the address as configured: a bare host when it names no
// port.
func (a ManagementAddress) String() string {
	if a.Port == 0 {
		return a.Host.String()
	}
	return net.JoinHostPort(a.Host.String(), strconv.Itoa(int(a.Port)))
}

func (a ManagementAddress) MarshalText() ([]byte, error) {
	return []byte(a.String()), nil
}

func (a *ManagementAddress) UnmarshalText(text []byte) error {
	*a = DecodeManagementAddress(string(text))
	return nil
}

// ParseKubernetesAddress parses a node's kubernetes.address, the literal
// unicast IP assigned to the node itself. An empty value is unset. A
// v4-mapped IPv6 address is unmapped.
func ParseKubernetesAddress(value string) (netip.Addr, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q must be a literal unicast IP address", value)
	}
	if err := validKubernetesAddr(addr); err != nil {
		return netip.Addr{}, fmt.Errorf("%q must be a literal unicast IP address", value)
	}
	return addr.Unmap(), nil
}

func validKubernetesAddr(addr netip.Addr) error {
	if !addr.IsValid() || addr.Zone() != "" || !addr.IsGlobalUnicast() {
		return fmt.Errorf("%s must be a literal unicast IP address", addr)
	}
	return nil
}

func parsePort(value string) (uint16, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port must be a number from 1 to 65535")
	}
	return uint16(port), nil
}

var dnsNameLabelPattern = regexp.MustCompile(`(?i)^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func validDNSName(value string) bool {
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !dnsNameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

var hostNameLabelPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// validHostName accepts a DNS name, or a name whose labels also contain
// underscores or begin or end with a hyphen.
func validHostName(value string) bool {
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !hostNameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}
