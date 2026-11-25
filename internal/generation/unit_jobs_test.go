package generation

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runtimeUnits is the runtime image's unit graph as systemd assembles it from
// unit files, drop-ins and .wants/.requires directories. Units the image does
// not ship, such as systemd's own, appear only through those directories.
type runtimeUnits struct {
	// directives maps a unit to its directive values, drop-ins included.
	directives map[string]map[string][]string
}

func loadRuntimeUnits(t *testing.T) runtimeUnits {
	t.Helper()
	systemdRoot := filepath.Join(repoRoot(t), "mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system")
	units := runtimeUnits{directives: map[string]map[string][]string{}}
	add := func(unit, key string, values ...string) {
		if units.directives[unit] == nil {
			units.directives[unit] = map[string][]string{}
		}
		units.directives[unit][key] = append(units.directives[unit][key], values...)
	}
	parse := func(unit, path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && !strings.HasPrefix(key, "#") {
				add(unit, strings.TrimSpace(key), strings.Fields(value)...)
			}
		}
		return nil
	}
	err := filepath.WalkDir(systemdRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(systemdRoot, path)
		dir, name := filepath.Split(rel)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case dir == "":
			return parse(name, path)
		case strings.HasSuffix(dir, ".d") && strings.HasSuffix(name, ".conf"):
			return parse(strings.TrimSuffix(dir, ".d"), path)
		case strings.HasSuffix(dir, ".wants"):
			add(strings.TrimSuffix(dir, ".wants"), "Wants", name)
		case strings.HasSuffix(dir, ".requires"):
			add(strings.TrimSuffix(dir, ".requires"), "Requires", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return units
}

func (u runtimeUnits) values(unit, key string) []string {
	return u.directives[unit][key]
}

// exitsInactive reports whether unit is a oneshot service that is inactive
// once it has run, so any start job queued for it runs it again.
func (u runtimeUnits) exitsInactive(unit string) bool {
	return strings.HasSuffix(unit, ".service") && slices.Contains(u.values(unit, "Type"), "oneshot") &&
		!slices.Contains(u.values(unit, "RemainAfterExit"), "yes")
}

// dependents returns the units whose key directives name unit.
func (u runtimeUnits) dependents(unit string, keys ...string) []string {
	var found []string
	for name := range u.directives {
		for _, key := range keys {
			if slices.Contains(u.values(name, key), unit) {
				found = append(found, name)
				break
			}
		}
	}
	return found
}

// closure walks next from start and returns every unit reached, start included.
func closure(start []string, next func(string) []string) map[string]bool {
	reached := map[string]bool{}
	queue := slices.Clone(start)
	for len(queue) > 0 {
		unit := queue[0]
		queue = queue[1:]
		if reached[unit] {
			continue
		}
		reached[unit] = true
		queue = append(queue, next(unit)...)
	}
	return reached
}

// rerunByRestart returns the oneshot services `systemctl restart unit` runs
// again. The restart try-restarts every active unit that Requires=, BindsTo=
// or is PartOf= a restarted unit, and the start half of each restart queues a
// start job for everything the restarted units pull in, transitively, which
// runs a oneshot that exited inactive.
func (u runtimeUnits) rerunByRestart(unit string) []string {
	restarted := closure([]string{unit}, func(name string) []string {
		var active []string
		for _, dependent := range u.dependents(name, "Requires", "BindsTo", "PartOf") {
			// try-restart leaves an inactive unit alone.
			if !u.exitsInactive(dependent) {
				active = append(active, dependent)
			}
		}
		return active
	})
	started := closure(sortedKeys(restarted), func(name string) []string {
		var pulled []string
		for _, key := range []string{"Requires", "Wants", "BindsTo", "Requisite"} {
			pulled = append(pulled, u.values(name, key)...)
		}
		return pulled
	})
	var rerun []string
	for _, name := range sortedKeys(started) {
		if u.exitsInactive(name) {
			rerun = append(rerun, name)
		}
	}
	return rerun
}

// reloaded returns the units `systemctl reload unit` reloads. A reload
// propagates only through PropagatesReloadTo= and ReloadPropagatedFrom= and
// queues no start jobs.
func (u runtimeUnits) reloaded(unit string) []string {
	return sortedKeys(closure([]string{unit}, func(name string) []string {
		return append(slices.Clone(u.values(name, "PropagatesReloadTo")), u.dependents(name, "ReloadPropagatedFrom")...)
	}))
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Management changes made on the node, credential rotation and a trial revert,
// reload katlc-agent with systemctl reload-or-restart instead of restarting
// it. A restart reruns the generation activation the agent requires, and a
// failed rerun would keep the agent from starting again.
func TestAgentReloadRerunsNoUnit(t *testing.T) {
	units := loadRuntimeUnits(t)

	// The walk must see why a restart is unsafe, or its clean reload result
	// would prove nothing.
	if rerun := units.rerunByRestart("katlc-agent.service"); !slices.Contains(rerun, "katl-generation-activate.service") {
		t.Fatalf("restarting katlc-agent reruns %q; the walk no longer sees the generation activation it requires", rerun)
	}

	// reload-or-restart restarts a unit that cannot reload.
	if !slices.Contains(units.values("katlc-agent.service", "Type"), "notify-reload") && len(units.values("katlc-agent.service", "ExecReload")) == 0 {
		t.Fatal("katlc-agent.service cannot reload, so reload-or-restart would restart it")
	}
	if reloaded := units.reloaded("katlc-agent.service"); !slices.Equal(reloaded, []string{"katlc-agent.service"}) {
		t.Fatalf("reloading katlc-agent reloads %q; want only the agent", reloaded)
	}
}
