package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func upgradePreparationUnit(work string) string {
	return "katl-host-upgrade-prepare-" + cleanID(filepath.Base(work)) + ".service"
}

func stopUpgradePreparation(unit string, run ToolRunner) error {
	if run == nil {
		return fmt.Errorf("cannot verify preparation unit %s without a command runner", unit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop := run(ctx, []string{"systemctl", "stop", unit}, nil)
	var stopErr error
	if stop.Err != nil || stop.ExitStatus != 0 {
		stopErr = hostUpgradeCommandError{name: "stop " + unit, result: stop}
	}
	// systemd-run can fail after submission. Only manager state proves that
	// private namespace consumers have ended, including an already collected unit.
	show := run(ctx, []string{"systemctl", "show", "--property=LoadState", "--property=ActiveState", "--", unit}, nil)
	if show.Err != nil || show.ExitStatus != 0 {
		return errors.Join(stopErr, hostUpgradeCommandError{name: "inspect " + unit, result: show})
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(show.Stdout)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return errors.Join(stopErr, fmt.Errorf("preparation unit %s returned incomplete state", unit))
		}
		if _, exists := properties[key]; exists {
			return errors.Join(stopErr, fmt.Errorf("preparation unit %s returned duplicate state %s", unit, key))
		}
		properties[key] = value
	}
	if properties["LoadState"] == "" || properties["ActiveState"] != "inactive" {
		return errors.Join(stopErr, fmt.Errorf("preparation unit %s is not proven inactive: LoadState=%q ActiveState=%q; retain image resources", unit, properties["LoadState"], properties["ActiveState"]))
	}
	return nil
}
