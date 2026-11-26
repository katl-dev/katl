package managementnetwork

import (
	"context"
	"strings"
	"testing"
)

func TestReadyUsesConfiguredRequiredLinks(t *testing.T) {
	for _, test := range []struct {
		name       string
		links      string
		ready      bool
		wantReason string
	}{
		{
			name:  "required management link",
			links: `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			ready: true,
		},
		{
			name:       "workload address cannot mask offline management",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier"},{"Name":"cni0","OnlineState":null,"OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			wantReason: "enp1s0",
		},
		{
			name:       "workload address cannot supply required route",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"degraded","CarrierState":"carrier","AddressState":"degraded"},{"Name":"cni0","OnlineState":null,"OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			wantReason: "no routable required link among enp1s0",
		},
		{
			name:  "optional secondary link is ignored",
			links: `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"},{"Name":"enp2s0","NetworkFile":"/etc/systemd/network/20-secondary.network","RequiredForOnline":false,"OnlineState":"offline","OperationalState":"no-carrier"}]}`,
			ready: true,
		},
		{
			name:       "required secondary link blocks readiness",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"/etc/systemd/network/10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"},{"Name":"enp2s0","NetworkFile":"/etc/systemd/network/20-secondary.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier"}]}`,
			wantReason: "enp2s0",
		},
		{
			name:       "no configured management link",
			links:      `{"Interfaces":[{"Name":"cni0","OnlineState":null,"OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			wantReason: "no systemd-networkd link",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, reason, err := Ready(t.Context(), func(context.Context) ([]byte, error) { return []byte(test.links), nil })
			if err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if ready != test.ready || !strings.Contains(reason, test.wantReason) {
				t.Fatalf("Ready() = %t, %q, want %t, reason containing %q", ready, reason, test.ready, test.wantReason)
			}
		})
	}
}
