package networkdconfig

import (
	"path"
	"sort"
	"strings"
)

// stackingKeys are the [Network] settings that make networkd create or
// enslave a netdev when it configures the matching link.
var stackingKeys = map[string]bool{
	"VLAN": true, "MACVLAN": true, "MACVTAP": true, "IPVLAN": true, "IPVTAP": true,
	"VXLAN": true, "Tunnel": true, "Bond": true, "Bridge": true, "VRF": true,
}

// Reference is a .network unit that stacks or enslaves a netdev.
type Reference struct {
	// Key is the [Network] setting, such as VLAN or Bond.
	Key string
	// Names are the unit's [Match] Name= patterns. They are empty when the
	// unit matches links by anything other than name, so Katl cannot tell
	// which links it applies to.
	Names []string
}

// References returns, per netdev name, the .network units that reference it.
func References(files map[string]string) map[string][]Reference {
	references := map[string][]Reference{}
	for _, settings := range mergedUnits(files, ".network") {
		var names []string
		nameOnly := true
		for _, setting := range settings {
			if value, ok := strings.CutPrefix(setting, "[Match]Name="); ok {
				names = append(names, strings.Fields(value)...)
			} else if strings.HasPrefix(setting, "[Match]") {
				nameOnly = false
			}
		}
		for _, name := range names {
			nameOnly = nameOnly && !strings.HasPrefix(name, "!")
		}
		if !nameOnly {
			names = nil
		}
		for _, setting := range settings {
			assignment, ok := strings.CutPrefix(setting, "[Network]")
			if !ok {
				continue
			}
			key, value, _ := strings.Cut(assignment, "=")
			if !stackingKeys[key] {
				continue
			}
			for _, name := range strings.Fields(value) {
				references[name] = append(references[name], Reference{Key: key, Names: names})
			}
		}
	}
	return references
}

// MatchesName reports whether a link name matches one of the patterns.
func MatchesName(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

// mergedUnits returns the flattened settings of each top-level unit with the
// given suffix followed by its drop-ins, in networkd's file-name order.
func mergedUnits(files map[string]string, suffix string) [][]string {
	var units []string
	dropIns := map[string][]string{}
	for filePath := range files {
		if !IsPath(filePath) {
			continue
		}
		relative := strings.TrimPrefix(filePath, Directory+"/")
		directory, name, nested := strings.Cut(relative, "/")
		switch {
		case !nested && strings.HasSuffix(relative, suffix):
			units = append(units, filePath)
		case nested && strings.HasSuffix(directory, suffix+".d") && strings.HasSuffix(name, ".conf"):
			unit := strings.TrimSuffix(directory, ".d")
			dropIns[unit] = append(dropIns[unit], filePath)
		}
	}
	sort.Slice(units, func(i, j int) bool { return path.Base(units[i]) < path.Base(units[j]) })
	out := make([][]string, 0, len(units))
	for _, unit := range units {
		paths := dropIns[path.Base(unit)]
		sort.Slice(paths, func(i, j int) bool { return path.Base(paths[i]) < path.Base(paths[j]) })
		var settings []string
		for _, filePath := range append([]string{unit}, paths...) {
			settings = append(settings, unitSettings(files[filePath])...)
		}
		out = append(out, settings)
	}
	return out
}
