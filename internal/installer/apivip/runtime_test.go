package apivip

import (
	"encoding/binary"
	"net/netip"
	"syscall"
	"testing"
)

func TestNetlinkAddressRequestAndOwnership(t *testing.T) {
	for _, prefix := range []string{"10.40.0.10/32", "2001:db8::10/128"} {
		t.Run(prefix, func(t *testing.T) {
			address := netip.MustParsePrefix(prefix)
			request := marshalNetlinkAddressRequest(17, address, true)
			if got := binary.NativeEndian.Uint16(request[4:6]); got != syscall.RTM_NEWADDR {
				t.Fatalf("message type = %d", got)
			}
			owned, err := netlinkAddressOwned(request, 17, address)
			if err != nil || !owned {
				t.Fatalf("owned = %t, error = %v", owned, err)
			}
			owned, err = netlinkAddressOwned(request, 18, address)
			if err != nil || owned {
				t.Fatalf("wrong-interface owned = %t, error = %v", owned, err)
			}
			release := marshalNetlinkAddressRequest(17, address, false)
			if got := binary.NativeEndian.Uint16(release[4:6]); got != syscall.RTM_DELADDR {
				t.Fatalf("release message type = %d", got)
			}
		})
	}
}
