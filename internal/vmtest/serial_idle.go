package vmtest

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// serialIOBound limits how long a guest may run without serial output while
// its block I/O keeps advancing. Installer steps that stay silent normally
// finish within seconds, and a slow hosted-runner disk has been seen to stretch
// one past the 45 s idle timeout; five minutes leaves several times that margin
// while still failing a guest stuck in a write loop well before the phase
// timeout.
const serialIOBound = 5 * time.Minute

// blockStatsInterval is how often the idle loop samples guest block I/O.
const blockStatsInterval = 2 * time.Second

// blockStatsTimeout bounds one sample so a hung virsh cannot stall the loop.
const blockStatsTimeout = 5 * time.Second

// blockStats holds a domain's cumulative block I/O counters summed over all of
// its disks. Elapsed-time counters are excluded: only completed requests and
// bytes are evidence that the guest is doing I/O.
type blockStats struct {
	ReadReqs   uint64
	ReadBytes  uint64
	WriteReqs  uint64
	WriteBytes uint64
	Flushes    uint64
}

// blockStatsSource marks executors that can sample the running domain's block
// I/O counters.
type blockStatsSource interface {
	blockStats(ctx context.Context) (blockStats, error)
}

// blockStats reads the counters through virsh domblkstat without a device,
// which libvirt answers with the sum over every disk of the domain.
func (e LibvirtVMExecutor) blockStats(ctx context.Context) (blockStats, error) {
	output, err := e.virshOutput(ctx, "domblkstat", e.DomainName)
	if err != nil {
		return blockStats{}, err
	}
	return parseBlockStats(output)
}

// parseBlockStats reads virsh domblkstat output, whose lines end with a
// counter name and its value, optionally after a device name.
func parseBlockStats(output string) (blockStats, error) {
	var stats blockStats
	counters := map[string]*uint64{
		"rd_req":           &stats.ReadReqs,
		"rd_bytes":         &stats.ReadBytes,
		"wr_req":           &stats.WriteReqs,
		"wr_bytes":         &stats.WriteBytes,
		"flush_operations": &stats.Flushes,
	}
	found := false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		target, ok := counters[fields[len(fields)-2]]
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			return blockStats{}, fmt.Errorf("parse domblkstat %s: %w", fields[len(fields)-2], err)
		}
		*target = value
		found = true
	}
	if !found {
		return blockStats{}, fmt.Errorf("domblkstat reported no block I/O counters: %q", strings.TrimSpace(output))
	}
	return stats, nil
}

// serialIdleWatch decides when a guest without serial output counts as stuck.
// Advancing block I/O defers the idle timeout, but never past bound from the
// last serial output.
type serialIdleWatch struct {
	idle  time.Duration
	bound time.Duration

	lastSerial   time.Time
	lastActivity time.Time

	baseline    blockStats
	hasBaseline bool
	// ioAdvanced and sampleErr describe the window since the last serial output.
	ioAdvanced bool
	sampleErr  error
}

func newSerialIdleWatch(idle, bound time.Duration, now time.Time) *serialIdleWatch {
	return &serialIdleWatch{idle: idle, bound: bound, lastSerial: now, lastActivity: now}
}

func (w *serialIdleWatch) serialProgress(now time.Time) {
	w.lastSerial = now
	w.lastActivity = now
	w.ioAdvanced = false
	w.sampleErr = nil
}

// blockSample records one sample. A failed sample is not progress, and the
// baseline stays so the next successful sample is compared against it.
func (w *serialIdleWatch) blockSample(now time.Time, stats blockStats, err error) {
	if err != nil {
		w.sampleErr = err
		return
	}
	if w.hasBaseline && stats != w.baseline {
		w.lastActivity = now
		w.ioAdvanced = true
	}
	w.baseline = stats
	w.hasBaseline = true
}

// expired returns the failure message once the guest has been idle for the
// idle timeout, or silent for the bound while its block I/O advanced.
func (w *serialIdleWatch) expired(now time.Time) (string, bool) {
	silent := now.Sub(w.lastSerial)
	var summary string
	switch {
	case w.bound > 0 && silent >= w.bound:
		summary = fmt.Sprintf("libvirt domain serial idle exceeded the %s bound while guest block I/O kept advancing", w.bound)
	case now.Sub(w.lastActivity) >= w.idle:
		switch {
		case w.ioAdvanced:
			summary = fmt.Sprintf("libvirt domain serial idle timed out after %s; guest block I/O advanced during the %s without serial output, then stopped for %s", w.idle, silent.Round(time.Second), w.idle)
		case !w.hasBaseline:
			summary = fmt.Sprintf("libvirt domain serial idle timed out after %s; guest block I/O was not sampled", w.idle)
		default:
			summary = fmt.Sprintf("libvirt domain serial idle timed out after %s; guest block I/O did not advance", w.idle)
		}
	default:
		return "", false
	}
	if w.sampleErr != nil {
		summary += fmt.Sprintf("; sampling block I/O failed: %v", w.sampleErr)
	}
	return summary, true
}
