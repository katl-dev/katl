package agent

import (
	"bufio"
	"bytes"
	"context"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
func (s *Server) readBootHealthRun(ctx context.Context) (run bootHealthRun, volumesFailed bool, statusText string) {
	if s.RunBootHealthStatus == nil {
		return bootHealthRunUnknown, false, ""
	}
	result := s.RunBootHealthStatus(ctx, []string{"/usr/bin/systemctl", "show", bootHealthSettledTarget, bootHealthUnit, bootCompleteTarget, volumesActivateUnit, "--property=Id,LoadState,ActiveState,Job,StatusText"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return bootHealthRunUnknown, false, ""
	}
	units := systemdUnitProperties(result.Stdout)
	settled, health, complete := units[bootHealthSettledTarget], units[bootHealthUnit], units[bootCompleteTarget]
	volumesFailed = units[volumesActivateUnit]["ActiveState"] == "failed"
	switch {
	case settled["LoadState"] != "loaded":
		return bootHealthRunUnknown, volumesFailed, ""
	case settled["Job"] != "":
		return bootHealthRunSettling, volumesFailed, health["StatusText"]
	case health["ActiveState"] == "failed":
		return bootHealthRunFailed, volumesFailed, health["StatusText"]
	// An inactive oneshot does not show whether it ran; the target it orders
	// before becomes active only after it succeeds.
	case complete["ActiveState"] == "active":
		return bootHealthRunSucceeded, volumesFailed, ""
	case health["Job"] != "" || health["ActiveState"] == "activating":
		return bootHealthRunStalled, volumesFailed, health["StatusText"]
	default:
		return bootHealthRunSkipped, volumesFailed, ""
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

func (s *Server) failedBootHealthDependencies(ctx context.Context) []string {
	requires := s.RunBootHealthStatus(ctx, []string{"/usr/bin/systemctl", "show", "--property=Requires", "--value", bootHealthUnit}, nil)
	required := strings.Fields(string(requires.Stdout))
	if requires.Err != nil || requires.ExitStatus != 0 || len(required) == 0 {
		return nil
	}
	states := s.RunBootHealthStatus(ctx, append([]string{"/usr/bin/systemctl", "show", "--property=Id,ActiveState", "--"}, required...), nil)
	// systemctl can report a missing unit alongside properties of loaded units.
	var failed []string
	for unit, properties := range systemdUnitProperties(states.Stdout) {
		if properties["ActiveState"] == "failed" {
			failed = append(failed, unit)
		}
	}
	slices.Sort(failed)
	return failed
}

// nodeBootHealth refines the persisted boot health, which can describe an
// earlier boot of the same generation, with this boot's systemd state.
func (s *Server) nodeBootHealth(ctx context.Context) nodeBootHealth {
	health := readNodeBootHealth(s.Root)
	if health.SelectedGenerationID == "" {
		return health
	}
	run, volumesFailed, statusText := s.readBootHealthRun(ctx)
	var diagnostics []string
	if health.State == nodeBootHealthFailed || health.State == nodeBootHealthUnknown {
		diagnostics = append(diagnostics, health.Diagnostic)
	}
	switch run {
	case bootHealthRunSettling:
		health.State = nodeBootHealthPending
		health.Diagnostic = "boot health is running for this boot"
	case bootHealthRunStalled:
		health.State = nodeBootHealthPending
		health.Diagnostic = bootHealthUnit + " has not finished for this boot; inspect the units it requires"
	}
	if statusText != "" {
		if run == bootHealthRunFailed {
			diagnostics = append(diagnostics, "boot health last reported: "+statusText)
		} else {
			health.Diagnostic = "boot health is " + statusText
			diagnostics = append(diagnostics, health.Diagnostic)
		}
	}
	if volumesFailed {
		diagnostics = append(diagnostics, volumesActivateUnit+" failed in this boot, so volumes are unmounted. See the volume status and boot journal")
	}
	if run == bootHealthRunFailed || run == bootHealthRunSkipped {
		unitDiagnostic := bootHealthUnit + " failed in this boot"
		if run == bootHealthRunSkipped {
			unitDiagnostic = bootHealthUnit + " did not run in this boot"
		}
		if failed := s.failedBootHealthDependencies(ctx); len(failed) > 0 {
			unitDiagnostic += ". Required units failed: " + strings.Join(failed, ", ") + ". Run 'systemctl --failed' on the node"
		} else {
			unitDiagnostic += ". Inspect 'journalctl -b -u katl-boot-health' on the node"
		}
		diagnostics = append(diagnostics, unitDiagnostic+". Fix the cause, for example with 'katlctl cluster apply', then run 'katlctl node reboot <node>'")
	}
	if volumesFailed || run == bootHealthRunFailed || run == bootHealthRunSkipped {
		health.State = nodeBootHealthFailed
		health.Diagnostic = strings.Join(diagnostics, ". ")
	}
	return health
}

// Boot health normally settles seconds after the agent starts. An unproven
// boot with a known-good fallback waits up to 90 seconds for the management
// network. An unproven boot without a fallback waits for the network to come
// up. The settle target stops waiting after 10 minutes.
var (
	bootHealthAdmissionWait     = 5 * time.Minute
	bootHealthAdmissionInterval = 250 * time.Millisecond
	bootHealthReplyMargin       = 2 * time.Second
)

// awaitBootHealth holds a mutation until boot health settles for this boot.
// Boot health rejects a boot whose pending trial changed underneath it and
// resets the default to the booted generation, so a mutation admitted earlier
// could trigger recovery or undo a live promotion. Boot health settles with
// any result, including when it cannot run, so a broken boot never blocks the
// mutations that repair it.
func (s *Server) awaitBootHealth(ctx context.Context) error {
	deadline := time.Now().Add(bootHealthAdmissionWait)
	// Reply before the caller's deadline so it sees why it was refused.
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Add(-bootHealthReplyMargin).Before(deadline) {
		deadline = callerDeadline.Add(-bootHealthReplyMargin)
	}
	for run, _, _ := s.readBootHealthRun(ctx); run == bootHealthRunSettling; run, _, _ = s.readBootHealthRun(ctx) {
		if !time.Now().Before(deadline) {
			return status.Error(codes.Unavailable, "node is completing boot health for this boot; retry when `katlctl node status` no longer reports health pending")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootHealthAdmissionInterval):
		}
	}
	return nil
}
