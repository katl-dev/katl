package managementnetwork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadyAddressRoles(t *testing.T) {
	dir := t.TempDir()
	writeNetwork := func(name, config string) string {
		t.Helper()
		path := filepath.Join(dir, name+".network")
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	member := writeNetwork("member", "[Network]\nBond=bond0\n")
	bridgePort := writeNetwork("bridge-port", "[Network]\nBridge=br0\n")
	carrierOnly := writeNetwork("carrier", "[Network]\nLinkLocalAddressing=no\n")
	bond := writeNetwork("bond", "[Match]\nName=bond0\n")
	bondAddress := writeNetwork("bond-address", "[Network]\nDHCP=ipv4\n")
	management := writeNetwork("management", "[Address]\nAddress=192.0.2.10/24\n")
	managementV6 := writeNetwork("management-v6", "[Network]\nIPv6AcceptRA=yes\n")
	addressless := func(name, file, state string) string {
		return fmt.Sprintf(`{"Name":%q,"NetworkFile":%q,"RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":%q,"CarrierState":%q,"AddressState":"off"}`, name, file, state, state)
	}
	routable := func(name, file, family string) string {
		return fmt.Sprintf(`{"Name":%q,"NetworkFile":%q,"RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable",%s}`, name, file, family)
	}
	const ipv4 = `"IPv4AddressState":"routable","IPv6AddressState":"off"`
	const ipv6 = `"IPv4AddressState":"off","IPv6AddressState":"routable"`
	other := routable("enp3s0", management, ipv4)
	port := addressless("enp1s0", member, "enslaved")
	withBondAddress := func(link string) string {
		return strings.TrimSuffix(link, "}") + fmt.Sprintf(`,"NetworkFileDropins":[%q]}`, bondAddress)
	}
	for _, tc := range []struct {
		name        string
		links       []string
		unavailable string
	}{
		{name: "addressless bond member", links: []string{port, withBondAddress(routable("bond0", bond, ipv4)), other}},
		{name: "bond lost its address", links: []string{port, withBondAddress(addressless("bond0", bond, "carrier")), other}, unavailable: "bond0"},
		{name: "management lost its address", links: []string{addressless("enp2s0", management, "carrier"), other}, unavailable: "enp2s0"},
		{name: "IPv6-only management", links: []string{port, routable("bond0", managementV6, ipv6)}},
		{name: "addressless bridge port", links: []string{addressless("enp1s0", bridgePort, "enslaved"), routable("br0", management, ipv4)}},
		{name: "addressless carrier-only link", links: []string{addressless("enp1s0", carrierOnly, "carrier"), other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready, reason, err := Ready(t.Context(), func(context.Context) ([]byte, error) {
				return []byte(`{"Interfaces":[` + strings.Join(tc.links, ",") + `]}`), nil
			})
			wantReason := ""
			if tc.unavailable != "" {
				wantReason = "required links have no usable address: " + tc.unavailable
			}
			if err != nil || ready != (wantReason == "") || reason != wantReason {
				t.Fatalf("Ready() = %t, %q, %v, want %t, %q, nil", ready, reason, err, wantReason == "", wantReason)
			}
		})
	}
}
