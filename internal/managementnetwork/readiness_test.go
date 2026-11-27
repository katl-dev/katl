package managementnetwork

import (
	"context"
	"os"
	"path/filepath"
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
			name:       "unknown explicit minimum leaves readiness unconfirmed",
			links:      `{"Interfaces":[{"Name":"enp1s0","NetworkFile":"lan.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["future-state","routable"],"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			wantReason: `unrecognised required operational minimum "future-state"`,
		},
		{
			name:       "no configured management link",
			links:      `{"Interfaces":[{"Name":"cni0","OnlineState":null,"OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`,
			wantReason: "no systemd-networkd link",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason, err := ready(t.Context(), func(context.Context) ([]byte, error) { return []byte(test.links), nil }, t.TempDir())
			if err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if got != test.ready || !strings.Contains(reason, test.wantReason) {
				t.Fatalf("Ready() = %t, %q, want %t, reason containing %q", got, reason, test.ready, test.wantReason)
			}
		})
	}
}

func TestReadyReasonNamesFailedCheck(t *testing.T) {
	for _, test := range []struct {
		name          string
		link          string
		kernelCarrier string
		wantReason    string
	}{
		{
			name:          "networkd offline",
			link:          `{"Name":"enp1s0","NetworkFile":"lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"off"}`,
			kernelCarrier: "0\n",
			wantReason:    "systemd-networkd reports required link enp1s0 not online (carrier: no-carrier, address: off) (kernel carrier: down)",
		},
		{
			name:          "online without carrier",
			link:          `{"Name":"enp1s0","NetworkFile":"lan.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["carrier","routable"],"OnlineState":"online","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"off"}`,
			kernelCarrier: "1\n",
			wantReason:    "required link enp1s0 has no carrier although systemd-networkd reports it online (carrier: no-carrier, address: off) (kernel carrier: up)",
		},
		{
			name:       "online without address and unreadable kernel carrier",
			link:       `{"Name":"enp1s0","NetworkFile":"lan.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"}`,
			wantReason: "required link enp1s0 lacks its required address although systemd-networkd reports it online (carrier: carrier, address: off) (kernel carrier: unknown)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sysfs := t.TempDir()
			if test.kernelCarrier != "" {
				dir := filepath.Join(sysfs, "class/net/enp1s0")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "carrier"), []byte(test.kernelCarrier), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, reason, err := ready(t.Context(), func(context.Context) ([]byte, error) {
				return []byte(`{"Interfaces":[` + test.link + `]}`), nil
			}, sysfs)
			if err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if got || reason != test.wantReason {
				t.Fatalf("Ready() = %t, %q, want false, %q", got, reason, test.wantReason)
			}
		})
	}
}
