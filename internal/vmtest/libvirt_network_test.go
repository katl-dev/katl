package vmtest

import (
	"reflect"
	"testing"
)

func TestRoutedSubnets(t *testing.T) {
	// /proc/net/route: default via 192.168.1.1, 10.8.0.0/16 over a VPN,
	// and 192.168.1.0/24 on the LAN.
	data := []byte(`Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
tun0	0000080A	00000000	0001	0	0	0	0000FFFF	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`)
	var got []string
	for _, subnet := range routedSubnets(data) {
		got = append(got, subnet.String())
	}
	if want := []string{"10.8.0.0/16", "192.168.1.0/24"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subnets = %v, want %v", got, want)
	}
}

func TestParseLibvirtMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want LibvirtMetadata
	}{
		{
			name: "run-owned",
			data: `<vmtest xmlns="https://katlos.io/xmlns/vmtest/1" lease="/tmp/runs/a.lease">katl/vmtest</vmtest>`,
			want: LibvirtMetadata{Value: "katl/vmtest", Lease: "/tmp/runs/a.lease"},
		},
		{
			// virsh net-metadata prints the element without its namespace.
			name: "run-owned-without-namespace",
			data: `<vmtest lease="/tmp/runs/a.lease" run="a">katl/vmtest</vmtest>`,
			want: LibvirtMetadata{Value: "katl/vmtest", Lease: "/tmp/runs/a.lease", Run: "a"},
		},
		{
			name: "ownerless",
			data: `<vmtest xmlns:vmtest="https://katlos.io/xmlns/vmtest/1">katl/vmtest</vmtest>` + "\n",
			want: LibvirtMetadata{Value: "katl/vmtest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLibvirtMetadata([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("metadata = %+v, want %+v", got, tc.want)
			}
		})
	}
}
