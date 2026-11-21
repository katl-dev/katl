package agent

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	bootHealthUnit          = "katl-boot-health.service"
	bootCompleteTarget      = "katl-boot-complete.target"
	bootHealthSettledTarget = "katl-boot-health-settled.target"
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

func (s *Server) readBootHealthRun(ctx context.Context) bootHealthRun {
	if s.RunBootHealthStatus == nil {
		return bootHealthRunUnknown
	}
	result := s.RunBootHealthStatus(ctx, []string{"/usr/bin/systemctl", "show", bootHealthSettledTarget, bootHealthUnit, bootCompleteTarget, "--property=Id,LoadState,ActiveState,Job"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return bootHealthRunUnknown
	}
	units := map[string]map[string]string{}
	var current map[string]string
	scanner := bufio.NewScanner(bytes.NewReader(result.Stdout))
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
	settled, health, complete := units[bootHealthSettledTarget], units[bootHealthUnit], units[bootCompleteTarget]
	switch {
	case settled["LoadState"] != "loaded":
		return bootHealthRunUnknown
	case settled["Job"] != "":
		return bootHealthRunSettling
	case health["ActiveState"] == "failed":
		return bootHealthRunFailed
	// An inactive oneshot does not show whether it ran; the target it orders
	// before becomes active only after it succeeds.
	case complete["ActiveState"] == "active":
		return bootHealthRunSucceeded
	case health["Job"] != "" || health["ActiveState"] == "activating":
		return bootHealthRunStalled
	default:
		return bootHealthRunSkipped
	}
}

// nodeBootHealth refines the persisted boot health, which can describe an
// earlier boot of the same generation, with this boot's systemd state.
func (s *Server) nodeBootHealth(ctx context.Context) nodeBootHealth {
	health := readNodeBootHealth(s.Root)
	if health.SelectedGenerationID == "" {
		return health
	}
	switch s.readBootHealthRun(ctx) {
	case bootHealthRunSettling:
		health.State = nodeBootHealthPending
		health.Diagnostic = "boot health is running for this boot"
	case bootHealthRunStalled:
		health.State = nodeBootHealthPending
		health.Diagnostic = bootHealthUnit + " has not finished for this boot; inspect the units it requires"
	case bootHealthRunSkipped:
		health.State = nodeBootHealthFailed
		health.Diagnostic = bootHealthUnit + " did not run in this boot because a unit it requires did not start; inspect the boot journal"
	case bootHealthRunFailed:
		if health.State == nodeBootHealthHealthy {
			health.State = nodeBootHealthFailed
			health.Diagnostic = bootHealthUnit + " failed in this boot; inspect the boot journal"
		}
	}
	return health
}

// Boot health normally settles seconds after the agent starts. An unproven
// boot can wait 90 seconds for the management network, and the settle target
// stops waiting after 10 minutes.
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
	for s.readBootHealthRun(ctx) == bootHealthRunSettling {
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
