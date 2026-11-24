package vmtest

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net"
)

// addressPlan divides a network's host addresses, and is the one place that
// decides which addresses Katl plans for nodes and which dnsmasq may hand
// out. Planned nodes get reservations in [reservableStart, reservableEnd],
// skipping the gateway; the dynamic tail is the only DHCP range, so a guest
// without a reservation can never receive a planned address.
type addressPlan struct {
	gateway                        uint32
	reservableStart, reservableEnd uint32
	dynamicStart, dynamicEnd       uint32
}

// planAddresses gives the top quarter of the host addresses, for example .192
// to .254 of a /24, to dynamic DHCP.
func planAddresses(cidr *net.IPNet, gateway net.IP) (addressPlan, error) {
	first := ipToUint32(cidr.IP) + 1
	last := broadcast(cidr) - 1
	if broadcast(cidr) <= first {
		return addressPlan{}, fmt.Errorf("no free addresses in %s", cidr)
	}
	hosts := last - first + 1
	dynamic := hosts / 4
	if dynamic == 0 || hosts-dynamic < 2 {
		return addressPlan{}, fmt.Errorf("no free addresses in %s", cidr)
	}
	plan := addressPlan{
		gateway:         ipToUint32(gateway),
		reservableStart: first,
		reservableEnd:   last - dynamic,
		dynamicStart:    last - dynamic + 1,
		dynamicEnd:      last,
	}
	if plan.gateway >= plan.dynamicStart {
		return addressPlan{}, fmt.Errorf("gateway %s is inside the dynamic DHCP range of %s", gateway, cidr)
	}
	return plan, nil
}

// reservable reports whether address may be planned for a node.
func (plan addressPlan) reservable(address uint32) bool {
	return address >= plan.reservableStart && address <= plan.reservableEnd && address != plan.gateway
}

// libvirtNetworkGateway returns the address libvirt gives the host side of
// the network, which guests use as their gateway.
func libvirtNetworkGateway(data []byte) (net.IP, error) {
	var doc struct {
		IPs []networkIPElement `xml:"ip"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, element := range doc.IPs {
		if ip := net.ParseIP(element.Address).To4(); ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New("network has no IPv4 address")
}
