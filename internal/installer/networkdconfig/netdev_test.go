package networkdconfig

import "testing"

func TestNetdevs(t *testing.T) {
	files := map[string]string{
		"/etc/systemd/network/20-bond0.netdev":                "[NetDev]\nName=bond0\nKind=bond\n\n[Bond]\nMode=802.3ad\n",
		"/etc/systemd/network/20-bond0.netdev.d/50-mode.conf": "# prefer failover\n[Bond]\nMode=active-backup\n",
		"/etc/systemd/network/30-vlan.netdev":                 "[NetDev]\nDescription=uplink \\\n  VLAN\nName=bond0.20\nKind=vlan\n[VLAN]\nId=20\n",
		"/etc/systemd/network/10-dup.netdev":                  "[NetDev]\nName=bond0.20\nKind=vlan\n[VLAN]\nId=99\n",
		"/etc/systemd/network/40-unnamed.netdev":              "[NetDev]\nKind=dummy\n",
		"/etc/systemd/network/20-bond0.network":               "[Match]\nName=bond0\n",
		"/usr/lib/systemd/network/50-other.netdev":            "[NetDev]\nName=other0\nKind=dummy\n",
	}
	netdevs := Netdevs(files)
	if len(netdevs) != 2 {
		t.Fatalf("netdevs = %#v, want bond0 and bond0.20", netdevs)
	}
	bond := netdevs["bond0"]
	if bond.Kind != "bond" || bond.Definition != "[NetDev]Name=bond0\n[NetDev]Kind=bond\n[Bond]Mode=802.3ad\n[Bond]Mode=active-backup" {
		t.Fatalf("bond0 = %#v", bond)
	}
	if vlan := netdevs["bond0.20"]; vlan.Definition != "[NetDev]Name=bond0.20\n[NetDev]Kind=vlan\n[VLAN]Id=99" {
		t.Fatalf("bond0.20 = %#v, want the first unit by file name", vlan)
	}
}
