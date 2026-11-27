package managementnetwork

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestObserveCarrierDisagreements(t *testing.T) {
	const noCarrier = `{"Name":"enp1s0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"off"}`
	for _, tc := range []struct {
		name      string
		link      string
		carrier   string
		operstate string
		want      []string
	}{
		{name: "networkd disagrees with kernel", link: noCarrier, carrier: "1\n", operstate: "up\n", want: []string{"enp1s0"}},
		{name: "kernel reports no carrier", link: noCarrier, carrier: "0\n", operstate: "down\n"},
		{name: "kernel operstate not up", link: noCarrier, carrier: "1\n", operstate: "lowerlayerdown\n"},
		{name: "kernel state unreadable", link: noCarrier},
		{
			name:    "link outside networkd",
			link:    `{"Name":"enp1s0","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier"}`,
			carrier: "1\n", operstate: "up\n",
		},
		{
			name:    "optional link",
			link:    `{"Name":"enp1s0","NetworkFile":"10-lan.network","RequiredForOnline":false,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier"}`,
			carrier: "1\n", operstate: "up\n",
		},
		{
			name:    "networkd sees carrier while awaiting an address",
			link:    `{"Name":"enp1s0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`,
			carrier: "1\n", operstate: "up\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfs := t.TempDir()
			dir := filepath.Join(sysfs, "class/net/enp1s0")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"carrier": tc.carrier, "operstate": tc.operstate} {
				if value == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, err := observe(t.Context(), func(context.Context) ([]byte, error) {
				return []byte(`{"Interfaces":[` + tc.link + `]}`), nil
			}, sysfs)
			if err != nil {
				t.Fatal(err)
			}
			if got.Ready {
				t.Fatalf("observe() ready for an offline link")
			}
			if !slices.Equal(got.CarrierDisagreements, tc.want) {
				t.Fatalf("CarrierDisagreements = %q, want %q", got.CarrierDisagreements, tc.want)
			}
		})
	}
}
