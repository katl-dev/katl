package networkdconfig

import (
	"fmt"
	"path"
	"strings"
)

const (
	Directory   = "/etc/systemd/network"
	DefaultPath = Directory + "/10-lan.network"
)

const DefaultContent = "[Match]\nType=ether\n\n[Network]\nDHCP=yes\n\n[DHCPv4]\nClientIdentifier=mac\nUseHostname=no\n\n[DHCPv6]\nUseHostname=no\n"

func IsPath(value string) bool {
	return strings.HasPrefix(value, Directory+"/")
}

func IsNetworkUnitPath(value string) bool {
	if !IsPath(value) {
		return false
	}
	relative := strings.TrimPrefix(value, Directory+"/")
	return !strings.Contains(relative, "/") && strings.HasSuffix(relative, ".network")
}

func ValidatePath(value string) error {
	if !IsPath(value) {
		return nil
	}
	relative := strings.TrimPrefix(value, Directory+"/")
	parts := strings.Split(relative, "/")
	switch len(parts) {
	case 1:
		if !validUnitName(parts[0]) {
			return fmt.Errorf("%q must name a .network, .netdev, or .link file", value)
		}
	case 2:
		if !validDropInDirectory(parts[0]) {
			return fmt.Errorf("%q must use a .network.d, .netdev.d, or .link.d drop-in directory", value)
		}
		if !validName(parts[1]) || !strings.HasSuffix(parts[1], ".conf") {
			return fmt.Errorf("%q must name a safe .conf drop-in", value)
		}
	default:
		return fmt.Errorf("%q must name a networkd file or one drop-in below it", value)
	}
	return nil
}

func validUnitName(name string) bool {
	if !validName(name) {
		return false
	}
	switch path.Ext(name) {
	case ".network", ".netdev", ".link":
		return true
	default:
		return false
	}
}

func validDropInDirectory(name string) bool {
	for _, suffix := range []string{".network.d", ".netdev.d", ".link.d"} {
		if strings.HasSuffix(name, suffix) {
			return validName(strings.TrimSuffix(name, ".d"))
		}
	}
	return false
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@+-", r) {
			continue
		}
		return false
	}
	return true
}
