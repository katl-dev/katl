package networkdconfig

import (
	"bufio"
	"strings"
)

// Netdev is a virtual device declared by a .netdev unit in Directory.
type Netdev struct {
	Name string
	Kind string
	// Definition is the effective unit and drop-in content without comments,
	// blank lines, or cosmetic settings. networkd applies it only when it
	// creates the device, so any difference requires recreation.
	Definition string
	// VLANID is the [VLAN] Id of a VLAN device.
	VLANID string
	// Conditional reports a [Match] section, which can keep networkd from
	// creating the device on this host.
	Conditional bool
}

// Netdevs returns the virtual devices declared by .netdev units and their
// drop-ins, keyed by device name. files maps absolute paths to content; paths
// outside Directory are ignored.
func Netdevs(files map[string]string) map[string]Netdev {
	netdevs := map[string]Netdev{}
	// networkd orders units by file name; the first unit naming a device wins.
	for _, settings := range mergedUnits(files, ".netdev") {
		netdev := Netdev{Definition: strings.Join(settings, "\n")}
		for _, setting := range settings {
			switch {
			case strings.HasPrefix(setting, "[NetDev]Name="):
				netdev.Name = strings.TrimPrefix(setting, "[NetDev]Name=")
			case strings.HasPrefix(setting, "[NetDev]Kind="):
				netdev.Kind = strings.TrimPrefix(setting, "[NetDev]Kind=")
			case strings.HasPrefix(setting, "[VLAN]Id="):
				netdev.VLANID = strings.TrimPrefix(setting, "[VLAN]Id=")
			case strings.HasPrefix(setting, "[Match]"):
				netdev.Conditional = true
			}
		}
		if netdev.Name == "" {
			continue
		}
		if _, exists := netdevs[netdev.Name]; !exists {
			netdevs[netdev.Name] = netdev
		}
	}
	return netdevs
}

// unitSettings flattens a systemd unit into section-qualified assignments.
func unitSettings(content string) []string {
	var settings []string
	section := ""
	pending := ""
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if pending != "" {
			line = pending + " " + line
			pending = ""
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			settings = append(settings, section+line)
			continue
		}
		key = strings.TrimSpace(key)
		if section == "[NetDev]" && key == "Description" {
			continue
		}
		settings = append(settings, section+key+"="+strings.TrimSpace(value))
	}
	if pending != "" {
		settings = append(settings, section+pending)
	}
	return settings
}
