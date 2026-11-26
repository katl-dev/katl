package managementnetwork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadyUsesNetworkdMinimum(t *testing.T) {
	raUnit := filepath.Join(t.TempDir(), "ra.network")
	if err := os.WriteFile(raUnit, []byte("[Link]\nRequiredForOnline=carrier\n\n[Network]\nIPv6AcceptRA=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const other = `{"Name":"enp3s0","NetworkFile":"other.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}`
	const member = `{"Name":"enp1s0","NetworkFile":"member.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"enslaved","CarrierState":"enslaved","AddressState":"off"}`
	const defaultBondMember = `{"Name":"enp1s0","NetworkFile":"member.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["enslaved","enslaved"],"OnlineState":"online","OperationalState":"enslaved","CarrierState":"enslaved","AddressState":"off"}`
	const bond = `{"Name":"bond0","NetworkFile":"bond.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}`
	const bondWithoutAddress = `{"Name":"bond0","NetworkFile":"bond.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`
	const managementWithoutAddress = `{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`
	const managementIPv6 = `{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable","IPv4AddressState":"off","IPv6AddressState":"routable"}`
	raWithoutAddress := fmt.Sprintf(`{"Name":"enp1s0","NetworkFile":%q,"RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`, raUnit)

	for _, tc := range []struct {
		name        string
		links       []string
		unavailable string
	}{
		{name: "addressless bond member", links: []string{member, bond}},
		{name: "addressless bond member with networkd default", links: []string{defaultBondMember, bond}},
		{name: "bond lost its address", links: []string{member, bondWithoutAddress, other}, unavailable: "bond0"},
		{name: "management online without an address", links: []string{managementWithoutAddress, other}, unavailable: "enp2s0"},
		{name: "IPv6-only management", links: []string{managementIPv6}},
		{name: "RA on a carrier-only link", links: []string{raWithoutAddress, other}},
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
				`{"Name":"enp2s0","NetworkFile":"management.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["degraded","routable"],"OnlineState":"offline","OperationalState":"degraded","CarrierState":"carrier","AddressState":"degraded"}`,
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
			ready, reason, err := Ready(t.Context(), func(context.Context) ([]byte, error) {
				return []byte(`{"Interfaces":[` + strings.Join(tc.links, ",") + `]}`), nil
			})
			if err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if wantReady := tc.unavailable == ""; ready != wantReady || !strings.Contains(reason, tc.unavailable) {
				t.Fatalf("Ready() = %t, %q, want %t, reason naming %q", ready, reason, wantReady, tc.unavailable)
			}
		})
	}
}
