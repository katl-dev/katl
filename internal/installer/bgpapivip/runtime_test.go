package bgpapivip

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"syscall"
	"testing"
)

func TestCommandBirdClientReportsBoundedPeerState(t *testing.T) {
	config := minimalConfig()
	config, err := Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	config.RouteExchange = []RouteExchange{{Name: "cilium"}}
	runner := &recordingCommandRunner{outputs: [][]byte{[]byte(`Name Proto Table State Since Info
katl_fabric_router_a BGP katl_fabric up 12:00:00 Established
	Local address: 10.0.0.11
	Routes: 0 imported, 1 exported, 0 preferred
katl_exchange_cilium BGP katl_exchange_cilium_table up 12:00:00 Established
	Routes: 3 imported, 0 exported, 3 preferred
katl_exchange_cilium_to_fabric Pipe katl_exchange_cilium_table up 12:00:00
	Routes: 0 imported, 3 exported, 3 preferred
`), []byte(`Table katl_fabric:
10.40.0.10/32  unicast [katl_api 12:00:00] * (240)
`)}}
	client := CommandBirdClient{Runner: runner, Config: config}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ControlSocketReady || !status.RouteOriginated || len(status.Peers) != 3 || status.Peers[0].SessionState != "established" || status.Peers[0].ExportedRoutes != 1 || status.RouterID != "10.0.0.11" {
		t.Fatalf("status = %#v", status)
	}
	if status.Peers[1].AcceptedRoutes != 3 || status.Peers[2].ExportedRoutes != 3 {
		t.Fatalf("route exchange status = %#v", status.Peers[1:])
	}
	want := [][]string{
		{"birdc", "-s", BirdControlSocketPath, "show", "protocols", "all"},
		{"birdc", "-s", BirdControlSocketPath, "show", "route", "table", "katl_fabric", "for", "10.40.0.10/32", "protocol", "katl_api"},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
}

func TestCommandBirdClientFailsClosedWhenControlSocketIsUnavailable(t *testing.T) {
	runner := &recordingCommandRunner{err: errors.New("exit status 1")}
	client := CommandBirdClient{Runner: runner}
	status, err := client.Status(context.Background())
	if err == nil || status.ControlSocketReady || status.ProcessActive {
		t.Fatalf("status = %#v, error = %v", status, err)
	}
}

func TestCommandBirdClientTreatsMissingDirectRouteAsReadyAndWithdrawn(t *testing.T) {
	config, err := Normalize(minimalConfig())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingCommandRunner{
		outputs: [][]byte{
			[]byte("katl_api Direct --- up\n"),
			[]byte("BIRD 3.3.1 ready.\nNetwork not found\n"),
		},
		errs: []error{nil, errors.New("exit status 1")},
	}
	client := CommandBirdClient{Runner: runner, Config: config}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ProcessActive || !status.ControlSocketReady || status.RouteOriginated || status.FailureReason != "" {
		t.Fatalf("status = %#v", status)
	}
}

func TestNetlinkAddressRequestAndOwnership(t *testing.T) {
	for _, prefix := range []string{"10.40.0.10/32", "2001:db8::10/128"} {
		t.Run(prefix, func(t *testing.T) {
			address := netip.MustParsePrefix(prefix)
			request := marshalNetlinkAddressRequest(17, address, true)
			if got := binary.NativeEndian.Uint16(request[4:6]); got != syscall.RTM_NEWADDR {
				t.Fatalf("message type = %d, want RTM_NEWADDR", got)
			}
			flags := binary.NativeEndian.Uint16(request[6:8])
			if flags&syscall.NLM_F_ACK == 0 || flags&syscall.NLM_F_CREATE == 0 || flags&syscall.NLM_F_EXCL == 0 {
				t.Fatalf("message flags = %#x", flags)
			}
			owned, err := netlinkAddressOwned(request, 17, address)
			if err != nil {
				t.Fatal(err)
			}
			if !owned {
				t.Fatalf("%s was not found in encoded route netlink state", prefix)
			}
			owned, err = netlinkAddressOwned(request, 18, address)
			if err != nil {
				t.Fatal(err)
			}
			if owned {
				t.Fatalf("%s matched the wrong interface", prefix)
			}

			release := marshalNetlinkAddressRequest(17, address, false)
			if got := binary.NativeEndian.Uint16(release[4:6]); got != syscall.RTM_DELADDR {
				t.Fatalf("release message type = %d, want RTM_DELADDR", got)
			}
		})
	}
}

type recordingCommandRunner struct {
	outputs  [][]byte
	errs     []error
	err      error
	commands [][]string
}

func (r *recordingCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	var output []byte
	if len(r.outputs) > 0 {
		output = r.outputs[0]
		r.outputs = r.outputs[1:]
	}
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		return output, err
	}
	return output, r.err
}
