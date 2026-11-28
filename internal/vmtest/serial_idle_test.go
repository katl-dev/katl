package vmtest

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testIdle  = 45 * time.Second
	testBound = 5 * time.Minute
	testStep  = 2 * time.Second
)

// idleRun drives a serialIdleWatch on a fake clock, sampling block I/O from
// stats every testStep.
type idleRun struct {
	watch *serialIdleWatch
	start time.Time
	now   time.Time
	stats func(elapsed time.Duration) (blockStats, error)
}

func newIdleRun(stats func(time.Duration) (blockStats, error)) *idleRun {
	start := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	return &idleRun{watch: newSerialIdleWatch(testIdle, testBound, start), start: start, now: start, stats: stats}
}

// runUntil advances the clock to elapsed and returns the first expiry on the
// way, with the elapsed time at which it fired.
func (r *idleRun) runUntil(elapsed time.Duration) (string, time.Duration, bool) {
	for r.now.Sub(r.start) < elapsed {
		r.now = r.now.Add(testStep)
		stats, err := r.stats(r.now.Sub(r.start))
		r.watch.blockSample(r.now, stats, err)
		if summary, expired := r.watch.expired(r.now); expired {
			return summary, r.now.Sub(r.start), true
		}
	}
	return "", 0, false
}

func (r *idleRun) serial() {
	r.watch.serialProgress(r.now)
}

func steadyIO(time.Duration) (blockStats, error) {
	return blockStats{ReadReqs: 10, WriteReqs: 20}, nil
}

func advancingIO(elapsed time.Duration) (blockStats, error) {
	return blockStats{WriteReqs: uint64(elapsed / time.Second), WriteBytes: uint64(elapsed / time.Millisecond)}, nil
}

func TestSerialIdleWithoutIO(t *testing.T) {
	run := newIdleRun(steadyIO)

	summary, at, expired := run.runUntil(testBound)
	if !expired || at < testIdle || at >= testIdle+testStep {
		t.Fatalf("expired = %v at %s, want expiry at the %s idle timeout", expired, at, testIdle)
	}
	if !strings.Contains(summary, "serial idle timed out after 45s") || !strings.Contains(summary, "block I/O did not advance") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSerialIdleSampleError(t *testing.T) {
	run := newIdleRun(func(elapsed time.Duration) (blockStats, error) {
		if elapsed <= testStep {
			return blockStats{WriteReqs: 1}, nil
		}
		return blockStats{}, errors.New("domain is not running")
	})

	summary, at, expired := run.runUntil(testBound)
	if !expired || at >= testIdle+testStep {
		t.Fatalf("expired = %v at %s, want expiry at the %s idle timeout", expired, at, testIdle)
	}
	if !strings.Contains(summary, "block I/O did not advance") || !strings.Contains(summary, "sampling block I/O failed: domain is not running") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSerialIdleWithIOWithinBound(t *testing.T) {
	run := newIdleRun(advancingIO)

	if summary, at, expired := run.runUntil(testBound - testStep); expired {
		t.Fatalf("expired at %s with block I/O advancing: %q", at, summary)
	}
}

func TestSerialIdleWithIOPastBound(t *testing.T) {
	run := newIdleRun(advancingIO)

	summary, at, expired := run.runUntil(2 * testBound)
	if !expired || at < testBound || at >= testBound+testStep {
		t.Fatalf("expired = %v at %s, want expiry at the %s bound", expired, at, testBound)
	}
	if !strings.Contains(summary, "exceeded the 5m0s bound while guest block I/O kept advancing") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSerialIdleIOStopsAfterAdvancing(t *testing.T) {
	run := newIdleRun(func(elapsed time.Duration) (blockStats, error) {
		return advancingIO(min(elapsed, time.Minute))
	})

	summary, at, expired := run.runUntil(testBound)
	if !expired || at < time.Minute+testIdle || at >= time.Minute+testIdle+testStep {
		t.Fatalf("expired = %v at %s, want expiry %s after block I/O stopped", expired, at, testIdle)
	}
	if !strings.Contains(summary, "block I/O advanced during the") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSerialProgressResetsIdle(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		run := newIdleRun(steadyIO)
		if _, at, expired := run.runUntil(40 * time.Second); expired {
			t.Fatalf("expired at %s before serial progress", at)
		}
		run.serial()

		_, at, expired := run.runUntil(2 * testBound)
		if !expired || at < 40*time.Second+testIdle {
			t.Fatalf("expired = %v at %s, want expiry %s after the serial progress at 40s", expired, at, testIdle)
		}
	})

	t.Run("bound", func(t *testing.T) {
		run := newIdleRun(advancingIO)
		if _, at, expired := run.runUntil(4 * time.Minute); expired {
			t.Fatalf("expired at %s before serial progress", at)
		}
		run.serial()

		_, at, expired := run.runUntil(3 * testBound)
		if !expired || at < 4*time.Minute+testBound {
			t.Fatalf("expired = %v at %s, want expiry %s after the serial progress at 4m", expired, at, testBound)
		}
	})
}

func TestParseBlockStats(t *testing.T) {
	// virsh domblkstat without a device prints the domain-wide sum with an
	// empty device column.
	output := ` rd_req 6297
 rd_bytes 157736448
 wr_req 1573
 wr_bytes 41906176
 flush_operations 457
 rd_total_times 1690418474
 wr_total_times 2512125587
 flush_total_times 381166855

`
	got, err := parseBlockStats(output)
	if err != nil {
		t.Fatal(err)
	}
	want := blockStats{ReadReqs: 6297, ReadBytes: 157736448, WriteReqs: 1573, WriteBytes: 41906176, Flushes: 457}
	if got != want {
		t.Fatalf("parseBlockStats() = %+v, want %+v", got, want)
	}

	if _, err := parseBlockStats("error: no domain\n"); err == nil {
		t.Fatal("parseBlockStats() accepted output without counters")
	}
}
