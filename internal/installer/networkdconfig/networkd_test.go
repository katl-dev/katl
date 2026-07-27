package networkdconfig

import "testing"

func TestValidatePathAcceptsUnitsAndDropIns(t *testing.T) {
	for _, value := range []string{
		"/etc/systemd/network/20-bond0.network",
		"/etc/systemd/network/20-bond0.netdev",
		"/etc/systemd/network/10-uplink.link",
		"/etc/systemd/network/20-bond0.network.d/50-address.conf",
		"/etc/systemd/network/20-bond0.netdev.d/50-options.conf",
		"/etc/systemd/network/10-uplink.link.d/50-options.conf",
	} {
		if err := ValidatePath(value); err != nil {
			t.Errorf("ValidatePath(%q) error = %v", value, err)
		}
	}
}

func TestValidatePathRejectsUnsupportedNetworkTreeEntries(t *testing.T) {
	for _, value := range []string{
		"/etc/systemd/network/20-bond0.conf",
		"/etc/systemd/network/20-bond0.network.d/address.txt",
		"/etc/systemd/network/20-bond0.network.d/nested/50-address.conf",
		"/etc/systemd/network/20 bond0.network",
	} {
		if err := ValidatePath(value); err == nil {
			t.Errorf("ValidatePath(%q) error = nil", value)
		}
	}
}

func TestIsNetworkUnitPathDistinguishesBaseUnitsFromAuxiliaryFiles(t *testing.T) {
	if !IsNetworkUnitPath("/etc/systemd/network/20-bond0.network") {
		t.Fatal("base .network unit was not recognized")
	}
	for _, value := range []string{
		"/etc/systemd/network/20-bond0.netdev",
		"/etc/systemd/network/20-bond0.network.d/50-address.conf",
		"/etc/example.network",
	} {
		if IsNetworkUnitPath(value) {
			t.Errorf("IsNetworkUnitPath(%q) = true", value)
		}
	}
}
