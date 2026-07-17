package vmtest

import "testing"

func TestParseLibvirtLease(t *testing.T) {
	output := []byte(`
 Expiry Time           MAC address         Protocol   IP address          Hostname   Client ID or DUID
---------------------------------------------------------------------------------------------------------
 2026-06-07 23:10:00  52:54:ab:cd:01:02  ipv4       192.168.122.42/24   cp-1       -
`)
	lease, ok := parseLibvirtLease(output, "52:54:ab:cd:01:02")
	if !ok {
		t.Fatal("parseLibvirtLease() did not find lease")
	}
	if lease.IPAddress != "192.168.122.42" || lease.MACAddress != "52:54:ab:cd:01:02" || lease.RawLine == "" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestParseLibvirtLeaseSelectsNewestLeaseForStableMAC(t *testing.T) {
	output := []byte(`
 Expiry Time           MAC address         Protocol   IP address          Hostname   Client ID or DUID
---------------------------------------------------------------------------------------------------------
 2026-06-07 23:10:00  52:54:ab:cd:01:02  ipv4       192.168.122.42/24   -          old
 2026-06-07 23:11:00  52:54:ab:cd:01:02  ipv4       192.168.122.43/24   -          current
`)
	lease, ok := parseLibvirtLease(output, "52:54:ab:cd:01:02")
	if !ok || lease.IPAddress != "192.168.122.43" {
		t.Fatalf("parseLibvirtLease() = %#v, %t", lease, ok)
	}
}

func TestParseLibvirtLeaseMissing(t *testing.T) {
	if lease, ok := parseLibvirtLease([]byte("no leases"), "52:54:ab:cd:01:02"); ok {
		t.Fatalf("parseLibvirtLease() = %#v, true", lease)
	}
}
