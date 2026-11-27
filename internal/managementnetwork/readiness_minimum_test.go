package managementnetwork

import (
	"context"
	"strings"
	"testing"
)

func TestReadyUsesNetworkdMinimum(t *testing.T) {
	const other = `{"Name":"enp3s0","NetworkFile":"other.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}`
	const member = `{"Name":"enp1s0","NetworkFile":"member.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"enslaved","CarrierState":"enslaved","AddressState":"off"}`
	const defaultBondMember = `{"Name":"enp1s0","NetworkFile":"member.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"enslaved","CarrierState":"enslaved","AddressState":"off"}`
	const unenslavedMember = `{"Name":"enp1s0","NetworkFile":"member.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["enslaved","enslaved"],"OnlineState":"offline","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`
	const bond = `{"Name":"bond0","NetworkFile":"bond.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}`
	const bondWithoutAddress = `{"Name":"bond0","NetworkFile":"bond.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`
	const managementWithoutAddress = `{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`
	const managementIPv6 = `{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable","IPv4AddressState":"off","IPv6AddressState":"routable"}`
	const raWithoutAddress = `{"Name":"enp1s0","NetworkFile":"ra.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`

	for _, tc := range []struct {
		name        string
		links       []string
		unavailable string
	}{
		{name: "addressless bond member", links: []string{member, bond}},
		{name: "addressless bond member with omitted range", links: []string{defaultBondMember, bond}},
		{name: "carrier on an unenslaved member", links: []string{unenslavedMember, bond}, unavailable: "enp1s0"},
		{name: "bond lost its address", links: []string{member, bondWithoutAddress, other}, unavailable: "bond0"},
		{name: "management online without an address", links: []string{managementWithoutAddress, other}, unavailable: "enp2s0"},
		{name: "IPv6-only management", links: []string{managementIPv6}},
		{
			name: "IPv4 loss with routable IPv6",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"RequiredFamilyForOnline":"ipv4","OnlineState":"offline","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable","IPv4AddressState":"off","IPv6AddressState":"routable"}`,
			},
			unavailable: "enp2s0",
		},
		{
			name: "addressless Ethernet with omitted range",
			links: []string{
				`{"Name":"enp2s0","Type":"ether","NetworkFile":"management.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`,
				other,
			},
			unavailable: "enp2s0",
		},
		{name: "RA on a carrier-only link", links: []string{raWithoutAddress, other}},
		{
			name: "carrier minimum rejects no carrier",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"off"}`,
				other,
			},
			unavailable: "enp2s0",
		},
		{
			name: "no-carrier minimum accepts no carrier",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["no-carrier","routable"],"OnlineState":"online","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"off"}`,
				other,
			},
		},
		{
			name: "degraded minimum rejects no address",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["degraded","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`,
				other,
			},
			unavailable: "enp2s0",
		},
		{
			name: "degraded minimum accepts link-local address",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["degraded","routable"],"OnlineState":"online","OperationalState":"degraded","CarrierState":"carrier","AddressState":"degraded"}`,
				other,
			},
		},
		{
			name: "routable minimum rejects link-local address",
			links: []string{
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"degraded","CarrierState":"carrier","AddressState":"degraded"}`,
				other,
			},
			unavailable: "enp2s0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, err := ready(t.Context(), func(context.Context) ([]byte, error) {
				return []byte(`{"Interfaces":[` + strings.Join(tc.links, ",") + `]}`), nil
			}, t.TempDir())
			if err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if wantReady := tc.unavailable == ""; got != wantReady || !strings.Contains(reason, tc.unavailable) {
				t.Fatalf("Ready() = %t, %q, want %t, reason naming %q", got, reason, wantReady, tc.unavailable)
			}
		})
	}
}
