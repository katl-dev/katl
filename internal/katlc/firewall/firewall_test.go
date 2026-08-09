package firewall

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsureSnapshotsHostInterfaces(t *testing.T) {
	var calls [][]string
	var applied string
	err := Ensure(context.Background(), Config{
		Port:       9443,
		Interfaces: []net.Interface{{Name: "enp1s0"}, {Name: "lo"}, {Name: "enp1s0"}},
		Run: func(_ context.Context, args []string, stdin []byte) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(calls) == 1 {
				return nil, errors.New("table absent")
			}
			applied = string(stdin)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"list", "table", "inet", TableName}, {"-f", "-"}}) {
		t.Fatalf("calls = %#v", calls)
	}
	for _, want := range []string{`elements = { "enp1s0", "lo" }`, "tcp dport 9443", "iifname != @host_interfaces drop"} {
		if !strings.Contains(applied, want) {
			t.Fatalf("rules missing %q:\n%s", want, applied)
		}
	}
}

func TestEnsureDoesNotAdoptInterfacesWhenTableExists(t *testing.T) {
	calls := 0
	err := Ensure(context.Background(), Config{
		Port:       9443,
		Interfaces: []net.Interface{{Name: "enp1s0"}, {Name: "cilium_host"}},
		Run: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
			calls++
			return []byte("table exists"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestEnsureReusesBootSnapshotIfRulesDisappear(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "management-interfaces")
	apply := func(interfaces []net.Interface) string {
		t.Helper()
		var rules string
		err := Ensure(context.Background(), Config{
			Port: 9443, Interfaces: interfaces, SnapshotPath: snapshot,
			Run: func(_ context.Context, args []string, stdin []byte) ([]byte, error) {
				if args[0] == "list" {
					return nil, errors.New("table absent")
				}
				rules = string(stdin)
				return nil, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return rules
	}
	first := apply([]net.Interface{{Name: "lo"}, {Name: "enp1s0"}})
	second := apply([]net.Interface{{Name: "lo"}, {Name: "enp1s0"}, {Name: "cilium_host"}, {Name: "veth1234"}})
	if first != second || strings.Contains(second, "cilium") || strings.Contains(second, "veth") {
		t.Fatalf("restored rules adopted workload interfaces:\n%s", second)
	}
}
