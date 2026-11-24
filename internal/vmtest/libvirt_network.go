package vmtest

import (
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"strings"
)

// OwnedNetwork is a libvirt network that one world creates, reserves
// addresses in, and removes. Its metadata names the owning world so cleanup
// never touches networks it did not create. It forwards through host NAT
// unless Isolated, which leaves only the host and guests on it and gives the
// guests no default route.
type OwnedNetwork struct {
	Name     string
	Owner    string
	CIDR     string
	Gateway  string
	Isolated bool
	Hosts    []DHCPHost
}

// DHCPHost reserves Address for MACAddress. Reservations use infinite leases,
// so a changed reservation reaches a guest only when it next discovers an
// address, normally at boot.
type DHCPHost struct {
	Name       string
	MACAddress string
	Address    string
}

type networkXML struct {
	XMLName  xml.Name         `xml:"network"`
	Name     string           `xml:"name"`
	Metadata domainMetadata   `xml:"metadata"`
	Forward  *networkForward  `xml:"forward"`
	IP       networkIPElement `xml:"ip"`
}

type networkForward struct {
	Mode string `xml:"mode,attr"`
}

type networkIPElement struct {
	Address string      `xml:"address,attr"`
	Prefix  int         `xml:"prefix,attr,omitempty"`
	Netmask string      `xml:"netmask,attr,omitempty"`
	DHCP    networkDHCP `xml:"dhcp"`
}

type networkDHCP struct {
	Ranges []networkDHCPRange `xml:"range"`
	Hosts  []networkDHCPHost  `xml:"host"`
}

type networkDHCPRange struct {
	Start string       `xml:"start,attr"`
	End   string       `xml:"end,attr"`
	Lease networkLease `xml:"lease"`
}

type networkDHCPHost struct {
	XMLName xml.Name     `xml:"host"`
	MAC     string       `xml:"mac,attr"`
	Name    string       `xml:"name,attr,omitempty"`
	IP      string       `xml:"ip,attr"`
	Lease   networkLease `xml:"lease"`
}

type networkLease struct {
	// Expiry zero is an infinite lease in libvirt.
	Expiry int `xml:"expiry,attr"`
}

// XML renders the libvirt network definition.
func (network OwnedNetwork) XML() (string, error) {
	if strings.TrimSpace(network.Name) == "" || strings.TrimSpace(network.Owner) == "" {
		return "", errors.New("network name and owner are required")
	}
	ip, cidr, err := net.ParseCIDR(network.CIDR)
	if err != nil || ip.To4() == nil {
		return "", fmt.Errorf("network CIDR %q must be IPv4", network.CIDR)
	}
	gateway := net.ParseIP(network.Gateway).To4()
	if gateway == nil || !cidr.Contains(gateway) {
		return "", fmt.Errorf("network gateway %q is outside %s", network.Gateway, network.CIDR)
	}
	prefix, _ := cidr.Mask.Size()
	start := ipToUint32(cidr.IP) + 1
	end := broadcast(cidr) - 1
	if ipToUint32(gateway) == start {
		start++
	}
	if end <= start {
		return "", fmt.Errorf("network CIDR %q has no DHCP range", network.CIDR)
	}
	doc := networkXML{
		Name:     network.Name,
		Metadata: domainMetadata{VMTest: domainVMTest{Value: network.Owner}},
		IP: networkIPElement{
			Address: gateway.String(),
			Prefix:  prefix,
			DHCP: networkDHCP{Ranges: []networkDHCPRange{{
				Start: uint32ToIP(start).String(),
				End:   uint32ToIP(end).String(),
			}}},
		},
	}
	if !network.Isolated {
		doc.Forward = &networkForward{Mode: "nat"}
	}
	for _, host := range network.Hosts {
		element, err := host.element(cidr)
		if err != nil {
			return "", err
		}
		doc.IP.DHCP.Hosts = append(doc.IP.DHCP.Hosts, element)
	}
	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// HostXML renders the reservation for virsh net-update ip-dhcp-host.
func (network OwnedNetwork) HostXML(host DHCPHost) (string, error) {
	_, cidr, err := net.ParseCIDR(network.CIDR)
	if err != nil {
		return "", fmt.Errorf("network CIDR %q is invalid: %w", network.CIDR, err)
	}
	element, err := host.element(cidr)
	if err != nil {
		return "", err
	}
	data, err := xml.Marshal(element)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (host DHCPHost) element(cidr *net.IPNet) (networkDHCPHost, error) {
	if _, err := net.ParseMAC(host.MACAddress); err != nil {
		return networkDHCPHost{}, fmt.Errorf("DHCP host %q MAC address: %w", host.Name, err)
	}
	ip := net.ParseIP(host.Address).To4()
	if ip == nil || !cidr.Contains(ip) {
		return networkDHCPHost{}, fmt.Errorf("DHCP host %q address %q is outside %s", host.Name, host.Address, cidr)
	}
	return networkDHCPHost{MAC: strings.ToLower(host.MACAddress), Name: host.Name, IP: ip.String()}, nil
}

// LibvirtNetworkIPv4 returns the IPv4 subnet a libvirt network definition
// serves, or nil when it has none.
func LibvirtNetworkIPv4(data []byte) (*net.IPNet, error) {
	var doc struct {
		IPs []networkIPElement `xml:"ip"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, element := range doc.IPs {
		ip := net.ParseIP(element.Address).To4()
		if ip == nil {
			continue
		}
		mask := net.CIDRMask(element.Prefix, 32)
		if element.Netmask != "" {
			parsed := net.ParseIP(element.Netmask).To4()
			if parsed == nil {
				return nil, fmt.Errorf("network netmask %q is invalid", element.Netmask)
			}
			mask = net.IPMask(parsed)
		}
		return &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
	}
	return nil, nil
}

// ChooseNetworkCIDR picks a /24 in 10.200.0.0/13 that overlaps none of
// inUse. The seed makes the first candidate stable for one owner, so a
// recreated world normally keeps its addresses.
func ChooseNetworkCIDR(seed string, inUse []*net.IPNet) (string, string, error) {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	const base, blocks = 10<<24 | 200<<16, 8 * 256
	offset := hash.Sum32() % blocks
	for i := uint32(0); i < blocks; i++ {
		network := base + ((offset+i)%blocks)<<8
		candidate := &net.IPNet{IP: uint32ToIP(network).To4(), Mask: net.CIDRMask(24, 32)}
		if overlapsAny(candidate, inUse) {
			continue
		}
		var gateway [4]byte
		binary.BigEndian.PutUint32(gateway[:], network+1)
		return candidate.String(), net.IP(gateway[:]).String(), nil
	}
	return "", "", errors.New("no free /24 in 10.200.0.0/13 for a libvirt network")
}

func overlapsAny(candidate *net.IPNet, inUse []*net.IPNet) bool {
	for _, network := range inUse {
		if network == nil {
			continue
		}
		if network.Contains(candidate.IP) || candidate.Contains(network.IP) {
			return true
		}
	}
	return false
}

// LibvirtNetworkHosts returns the DHCP reservations of a libvirt network
// definition.
func LibvirtNetworkHosts(data []byte) ([]DHCPHost, error) {
	var doc struct {
		IPs []networkIPElement `xml:"ip"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var hosts []DHCPHost
	for _, element := range doc.IPs {
		for _, host := range element.DHCP.Hosts {
			hosts = append(hosts, DHCPHost{Name: host.Name, MACAddress: strings.ToLower(host.MAC), Address: host.IP})
		}
	}
	return hosts, nil
}
