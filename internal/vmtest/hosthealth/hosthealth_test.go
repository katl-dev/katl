package hosthealth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const timeoutLog = "=== RUN   TestA\npanic: test timed out after 30m0s\n\trunning tests:\n\t\tTestA (30m0s)\n"

func starved() Health { return Health{StealRatio: 0.5} }

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		passed bool
		log    string
		health Health
		want   Outcome
	}{
		{"pass on starved host", true, "--- PASS: TestA (1.0s)\n", starved(), Pass},
		{"timeout on starved host", false, timeoutLog, starved(), Inconclusive},
		{"timeout with pressure", false, timeoutLog, Health{CPUPressure: 0.6}, Inconclusive},
		{"timeout with clocksource watchdog", false, timeoutLog, Health{ClocksourceWatch: 1}, Inconclusive},
		{"timeout on healthy host", false, timeoutLog, Health{StealRatio: 0.01, Load1End: 40, GuestStalls: 3}, Fail},
		{"assertion failure on starved host", false, "--- FAIL: TestA (1.0s)\n    a_test.go:3: got 1, want 2\n", starved(), Fail},
		{"timeout beside a failed test", false, "--- FAIL: TestB (1.0s)\n" + timeoutLog, starved(), Fail},
		{"build failure on starved host", false, "FAIL pkg [build failed]\n", starved(), Fail},
		{"json timeout on starved host", false, `{"Action":"output","Output":"panic: test timed out after 5m0s\n"}` + "\n", starved(), Inconclusive},
		{"json failure beside timeout", false, `{"Action":"output","Test":"TestB","Output":"--- FAIL: TestB (1.00s)\n"}` + "\n" +
			`{"Action":"output","Output":"panic: test timed out after 5m0s\n"}` + "\n", starved(), Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.passed, tt.log, tt.health); got.Outcome != tt.want {
				t.Fatalf("outcome = %s (%s), want %s", got.Outcome, got.Reason, tt.want)
			}
		})
	}
}

func TestBetween(t *testing.T) {
	t0 := time.Unix(1000, 0)
	start := Sample{Time: t0, CPUTicks: 1000, StealTicks: 100, CPUPressureUS: 5_000_000, MemoryPressureUS: 1}
	end := Sample{Time: t0.Add(100 * time.Second), CPUTicks: 2000, StealTicks: 300, CPUPressureUS: 55_000_000, MemoryPressureUS: 1, Load1: 3}

	h := Between(start, end)

	if h.StealRatio != 0.2 {
		t.Errorf("steal = %v, want 0.2", h.StealRatio)
	}
	if h.CPUPressure != 0.5 {
		t.Errorf("cpu pressure = %v, want 0.5", h.CPUPressure)
	}
	if h.MemoryPressure != 0 {
		t.Errorf("memory pressure = %v, want 0", h.MemoryPressure)
	}
	if _, ok := h.Starved(); !ok {
		t.Error("expected starved")
	}
}

func TestScanSerial(t *testing.T) {
	var h Health
	h.ScanSerial(`[    1.0] systemd[1]: Started foo.
[   40.2] clocksource: timekeeping watchdog on CPU1: Marking clocksource 'tsc' as unstable because the skew is too large:
[   40.2] clocksource: Switched to clocksource kvm-clock
[   90.0] watchdog: BUG: soft lockup - CPU#0 stuck for 22s!
[   91.0] rcu: INFO: rcu_sched detected stalls on CPUs/tasks:
`)
	if h.ClocksourceWatch != 1 || h.GuestStalls != 2 {
		t.Fatalf("watchdog=%d stalls=%d, want 1 and 2", h.ClocksourceWatch, h.GuestStalls)
	}
	if _, ok := (Health{GuestStalls: 2}).Starved(); ok {
		t.Error("guest stalls alone must not count as starvation")
	}
}

func TestReadSample(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("loadavg", "0.96 2.25 3.06 1/2349 3651410\n")
	write("meminfo", "MemTotal: 100 kB\nMemAvailable:    4096 kB\n")
	write("stat", "cpu  10 1 2 100 3 0 4 5 99 99\ncpu0 1 1 1 1 1 1 1 1 1 1\n")
	write("pressure/cpu", "some avg10=0.00 avg60=0.00 avg300=0.19 total=777\n")

	s, err := ReadSample(root, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}

	// total sums user..steal (8 columns), excluding guest columns.
	if s.CPUTicks != 125 || s.StealTicks != 5 {
		t.Errorf("ticks = %d steal %d, want 125 and 5", s.CPUTicks, s.StealTicks)
	}
	if s.Load1 != 0.96 || s.MemAvailableKB != 4096 || s.CPUPressureUS != 777 {
		t.Errorf("sample = %+v", s)
	}
	if s.MemoryPressureUS != 0 {
		t.Errorf("missing pressure file must read as zero, got %d", s.MemoryPressureUS)
	}
}
