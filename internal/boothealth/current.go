// Package boothealth reads health for the running KatlOS boot.
package boothealth

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/managementnetwork"
)

type Reader struct {
	Root        string
	RunCommand  func(context.Context, []string) ([]byte, error)
	ReadNetwork func(context.Context) ([]byte, error)
}

func (r Reader) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"/usr/bin/systemctl"}, args...)
	if r.RunCommand != nil {
		return r.RunCommand(ctx, argv)
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
}

// Settling reports whether boot health still has a systemd start job.
func (r Reader) Settling(ctx context.Context) bool {
	run, _ := r.readBootHealthRun(ctx)
	return run == bootHealthRunSettling
}

const (
	bootHealthUnit          = "katl-boot-health.service"
	bootCompleteTarget      = "katl-boot-complete.target"
	bootHealthSettledTarget = "katl-boot-health-settled.target"
	volumesActivateUnit     = "katl-volumes-activate.service"
)

// bootHealthRun is boot health's progress in the running boot. systemd unit
// state is boot-scoped, so it cannot describe an earlier boot.
type bootHealthRun int

const (
	// bootHealthRunUnknown means systemd did not describe the units, as on a
	// host that is not running KatlOS.
	bootHealthRunUnknown bootHealthRun = iota
	// bootHealthRunSettling means the settle target still has a start job.
	bootHealthRunSettling
	// bootHealthRunStalled means the settle target gave up waiting while boot
	// health is still queued or running.
	bootHealthRunStalled
	// bootHealthRunSkipped means boot health's start job finished without
	// running it, typically because a required unit failed.
	bootHealthRunSkipped
	bootHealthRunFailed
	bootHealthRunSucceeded
)

// Volume activation is independent of the generation's boot health check.
func (r Reader) readBootHealthRun(ctx context.Context) (run bootHealthRun, volumesFailed bool) {
	output, err := r.systemctl(ctx, "show", bootHealthSettledTarget, bootHealthUnit, bootCompleteTarget, volumesActivateUnit, "--property=Id,LoadState,ActiveState,Job")
	if err != nil {
		return bootHealthRunUnknown, false
	}
	units := systemdUnitProperties(output)
	settled, health, complete := units[bootHealthSettledTarget], units[bootHealthUnit], units[bootCompleteTarget]
	volumesFailed = units[volumesActivateUnit]["ActiveState"] == "failed"
	switch {
	case settled["LoadState"] != "loaded":
		return bootHealthRunUnknown, false
	case settled["Job"] != "":
		return bootHealthRunSettling, false
	case health["ActiveState"] == "failed":
		return bootHealthRunFailed, volumesFailed
	// An inactive oneshot does not show whether it ran; the target it orders
	// before becomes active only after it succeeds.
	case complete["ActiveState"] == "active":
		return bootHealthRunSucceeded, volumesFailed
	case health["Job"] != "" || health["ActiveState"] == "activating":
		return bootHealthRunStalled, volumesFailed
	default:
		return bootHealthRunSkipped, volumesFailed
	}
}

func systemdUnitProperties(output []byte) map[string]map[string]string {
	units := map[string]map[string]string{}
	var current map[string]string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			current = nil
			continue
		}
		if current == nil {
			current = map[string]string{}
		}
		current[key] = strings.TrimSpace(value)
		if key == "Id" {
			units[current[key]] = current
		}
	}
	return units
}

func (r Reader) failedBootHealthDependencies(ctx context.Context) []string {
	requires, err := r.systemctl(ctx, "show", "--property=Requires", "--value", bootHealthUnit)
	required := strings.Fields(string(requires))
	if err != nil || len(required) == 0 {
		return nil
	}
	states, _ := r.systemctl(ctx, append([]string{"show", "--property=Id,ActiveState", "--"}, required...)...)
	// systemctl can report a missing unit alongside properties of loaded units.
	var failed []string
	for unit, properties := range systemdUnitProperties(states) {
		if properties["ActiveState"] == "failed" {
			failed = append(failed, unit)
		}
	}
	slices.Sort(failed)
	return failed
}

// Read refines the persisted boot health, which can describe an
// earlier boot of the same generation, with this boot's systemd state.
func (r Reader) Read(ctx context.Context) Status {
	health := ReadPersisted(r.Root)
	if health.SelectedGenerationID == "" {
		return health
	}
	run, volumesFailed := r.readBootHealthRun(ctx)
	var diagnostics []string
	if health.State == Failed || health.State == Unknown {
		diagnostics = append(diagnostics, health.Diagnostic)
	}
	switch run {
	case bootHealthRunUnknown:
		health.State = Unknown
		diagnostics = append(diagnostics, "cannot read boot health for this boot from systemd")
		health.Diagnostic = strings.Join(diagnostics, ". ")
	case bootHealthRunSettling:
		health.State = Pending
		health.Diagnostic = "boot health is running for this boot"
	case bootHealthRunStalled:
		health.State = Pending
		health.Diagnostic = bootHealthUnit + " has not finished for this boot; inspect the units it requires"
	}
	if volumesFailed {
		diagnostics = append(diagnostics, volumesActivateUnit+" failed in this boot, so volumes are unmounted. See the volume status and boot journal")
	}
	if run == bootHealthRunFailed || run == bootHealthRunSkipped {
		unitDiagnostic := bootHealthUnit + " failed in this boot"
		if run == bootHealthRunSkipped {
			unitDiagnostic = bootHealthUnit + " did not run in this boot"
		}
		if failed := r.failedBootHealthDependencies(ctx); len(failed) > 0 {
			unitDiagnostic += ". Required units failed: " + strings.Join(failed, ", ") + ". Run 'systemctl --failed' on the node"
		} else {
			unitDiagnostic += ". Inspect 'journalctl -b -u katl-boot-health' on the node"
		}
		diagnostics = append(diagnostics, unitDiagnostic+". Fix the cause, for example with 'katlctl cluster apply', then run 'katlctl node reboot <node>'")
	}
	if volumesFailed || run == bootHealthRunFailed || run == bootHealthRunSkipped {
		health.State = Failed
		health.Diagnostic = strings.Join(diagnostics, ". ")
	}
	if run == bootHealthRunSucceeded && health.State == Healthy {
		// A live observation reports reachability without changing boot selection.
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		readNetwork := r.ReadNetwork
		if readNetwork == nil {
			readNetwork = managementnetwork.ReadState
		}
		ready, reason, err := managementnetwork.Ready(probeCtx, readNetwork)
		switch {
		case err != nil:
			health.State = Unknown
			health.Diagnostic = "cannot read management link state from systemd-networkd: " + err.Error()
		case !ready:
			health.State = Unhealthy
			health.Diagnostic = "management network unavailable: " + reason +
				". Check the link and the node's network configuration, then run 'katlctl node status <node>' again"
		}
	}
	return health
}
