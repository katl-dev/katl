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

// restartedBy returns the units `systemctl restart unit` restarts: unit, and
// every active unit that Requires=, BindsTo= or is PartOf= a restarted unit.
func (u runtimeUnits) restartedBy(unit string) map[string]bool {
	return closure([]string{unit}, func(name string) []string {
		var active []string
		for _, dependent := range u.dependents(name, "Requires", "BindsTo", "PartOf") {
			// try-restart leaves an inactive unit alone.
			if !u.exitsInactive(dependent) {
				active = append(active, dependent)
			}
		}
		return active
	})
}

// rerunByRestart returns the oneshot services `systemctl restart unit` runs
// again. The start half of each restart queues start jobs for what the
// restarted units pull in. A start job for an active unit is dropped as
// redundant, together with the jobs only it pulled in, so the walk continues
// only through units that are actually started: the restarted units and
// oneshots that exited inactive, which run again. A crash under Restart= and
// reload-or-restart of a stopped unit queue the same start jobs.
func (u runtimeUnits) rerunByRestart(unit string) []string {
	restarted := u.restartedBy(unit)
	started := closure(sortedKeys(restarted), func(name string) []string {
		if !restarted[name] && !u.exitsInactive(name) {
			return nil
		}
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

// Generation activation runs when systemd-sysext.service or
// systemd-confext.service starts, and at no other time. Restarting
// katlc-agent, which systemd also does when the agent crashes and on
// reload-or-restart of a stopped agent, must not rerun it: a failed rerun
// would keep the agent down and leave the node unreachable.
func TestAgentRerunsNoUnit(t *testing.T) {
	const agent, activation = "katlc-agent.service", "katl-generation-activate.service"
	units := loadRuntimeUnits(t)

	// The walk must see the rerun it guards against, or its clean result for
	// the agent would prove nothing.
	for _, extensions := range []string{"systemd-sysext.service", "systemd-confext.service"} {
		if rerun := units.rerunByRestart(extensions); !slices.Contains(rerun, activation) {
			t.Errorf("restarting %s reruns %q, not %s", extensions, rerun, activation)
		}
		if units.restartedBy(extensions)[agent] {
			t.Errorf("restarting %s restarts %s; the agent restarts the extensions during bootstrap", extensions, agent)
		}
	}

	// katl-kubeadm-ready.target requires the agent, so restarting the agent
	// restarts the target, which reruns this check. It only prints a marker
	// once the mounts it requires are up, so it cannot fail where its first
	// run passed.
	harmless := []string{"katl-state-projection-check.service"}
	for _, rerun := range units.rerunByRestart(agent) {
		if !slices.Contains(harmless, rerun) {
			t.Errorf("restarting %s reruns %s", agent, rerun)
		}
	}

	// reload-or-restart restarts a unit that cannot reload.
	if !slices.Contains(units.values(agent, "Type"), "notify-reload") && len(units.values(agent, "ExecReload")) == 0 {
		t.Errorf("%s cannot reload, so reload-or-restart would restart it", agent)
	}
	if reloaded := units.reloaded(agent); !slices.Equal(reloaded, []string{agent}) {
		t.Errorf("reloading %s reloads %q; want only the agent", agent, reloaded)
	}
}

// The agent no longer pulls in generation activation, so the boot must still
// order the agent after it, and still fail boot health when it fails.
func TestAgentFollowsGenerationActivation(t *testing.T) {
	const activation = "katl-generation-activate.service"
	units := loadRuntimeUnits(t)

	if !slices.Contains(units.values("katlc-agent.service", "After"), activation) {
		t.Errorf("katlc-agent.service is not ordered after %s", activation)
	}

	required := closure([]string{"katl-boot-health.service"}, func(name string) []string {
		return append(slices.Clone(units.values(name, "Requires")), units.values(name, "BindsTo")...)
	})
	if !required[activation] {
		t.Errorf("katl-boot-health.service does not require %s, so a failed activation would not fail boot health", activation)
	}
}
