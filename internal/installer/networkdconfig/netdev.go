package networkdconfig

import (
	"bufio"
	"path"
	"sort"
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
}

// Netdevs returns the virtual devices declared by .netdev units and their
// drop-ins, keyed by device name. files maps absolute paths to content; paths
// outside Directory are ignored.
func Netdevs(files map[string]string) map[string]Netdev {
	var units []string
	dropIns := map[string][]string{}
	for filePath := range files {
		if !IsPath(filePath) {
			continue
		}
		relative := strings.TrimPrefix(filePath, Directory+"/")
		directory, name, nested := strings.Cut(relative, "/")
		switch {
		case !nested && strings.HasSuffix(relative, ".netdev"):
			units = append(units, filePath)
		case nested && strings.HasSuffix(directory, ".netdev.d") && strings.HasSuffix(name, ".conf"):
			unit := strings.TrimSuffix(directory, ".d")
			dropIns[unit] = append(dropIns[unit], filePath)
		}
	}
	// networkd orders units by file name; the first unit naming a device wins.
	sort.Slice(units, func(i, j int) bool { return path.Base(units[i]) < path.Base(units[j]) })
	netdevs := map[string]Netdev{}
	for _, unit := range units {
		paths := dropIns[path.Base(unit)]
		sort.Slice(paths, func(i, j int) bool { return path.Base(paths[i]) < path.Base(paths[j]) })
		var settings []string
		for _, filePath := range append([]string{unit}, paths...) {
			settings = append(settings, unitSettings(files[filePath])...)
		}
		netdev := Netdev{Definition: strings.Join(settings, "\n")}
		for _, setting := range settings {
			switch {
			case strings.HasPrefix(setting, "[NetDev]Name="):
				netdev.Name = strings.TrimPrefix(setting, "[NetDev]Name=")
			case strings.HasPrefix(setting, "[NetDev]Kind="):
				netdev.Kind = strings.TrimPrefix(setting, "[NetDev]Kind=")
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
