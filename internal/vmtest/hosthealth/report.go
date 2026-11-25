package hosthealth

import (
	"regexp"
	"strings"
	"time"
)

// Provisional thresholds. They are not yet calibrated on CI data, so nothing
// consumes the classification other than the recorded report.
const (
	// StealThreshold is the fraction of host CPU time stolen by the
	// hypervisor above which the host is considered to have withheld CPU.
	StealThreshold = 0.10
	// PressureThreshold is the fraction of elapsed time some task stalled on
	// CPU or memory above which the host is considered starved.
	PressureThreshold = 0.25
)

// Health is the difference between two samples plus signals read from guest
// serial logs.
type Health struct {
	Elapsed          time.Duration `json:"elapsed"`
	Load1End         float64       `json:"load1End"`
	MemAvailableKB   uint64        `json:"memAvailableKBEnd"`
	StealRatio       float64       `json:"stealRatio"`
	CPUPressure      float64       `json:"cpuPressure"`
	MemoryPressure   float64       `json:"memoryPressure"`
	ClocksourceWatch int           `json:"guestClocksourceWatchdog"`
	GuestStalls      int           `json:"guestStalls"`
}

// Between derives host health for the interval from start to end.
func Between(start, end Sample) Health {
	h := Health{
		Elapsed:        end.Time.Sub(start.Time),
		Load1End:       end.Load1,
		MemAvailableKB: end.MemAvailableKB,
	}
	if end.CPUTicks > start.CPUTicks {
		h.StealRatio = ratio(end.StealTicks-min(end.StealTicks, start.StealTicks), end.CPUTicks-start.CPUTicks)
	}
	if h.Elapsed > 0 {
		us := uint64(h.Elapsed / time.Microsecond)
		h.CPUPressure = ratio(end.CPUPressureUS-min(end.CPUPressureUS, start.CPUPressureUS), us)
		h.MemoryPressure = ratio(end.MemoryPressureUS-min(end.MemoryPressureUS, start.MemoryPressureUS), us)
	}
	return h
}

func ratio(n, d uint64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// The clocksource watchdog fires when the guest's clock sources disagree,
// which happens when a vCPU is not scheduled for long stretches. Soft
// lockups and RCU stalls are recorded as stalls but are not classification
// evidence, because a product kernel bug produces them too.
var (
	clocksourceWatchdog = regexp.MustCompile(`clocksource: timekeeping watchdog on CPU\d+: .*(unstable|skew|delayed)|Marking clocksource '[^']*' as unstable|clocksource: Long readout interval`)
	guestStall          = regexp.MustCompile(`BUG: soft lockup|rcu: INFO: \S+ (detected stalls|self-detected stall)`)
)

// ScanSerial counts guest stall signals in serial console text.
func (h *Health) ScanSerial(text string) {
	for _, line := range strings.Split(text, "\n") {
		switch {
		case clocksourceWatchdog.MatchString(line):
			h.ClocksourceWatch++
		case guestStall.MatchString(line):
			h.GuestStalls++
		}
	}
}

// Starved reports positive evidence that the host withheld CPU or memory.
// Load and available memory are recorded for diagnosis but are not evidence
// on their own: a busy host that still delivered the promised resources
// does not excuse a failure.
func (h Health) Starved() (reason string, ok bool) {
	switch {
	case h.StealRatio >= StealThreshold:
		return "host CPU steal above threshold", true
	case h.CPUPressure >= PressureThreshold:
		return "host CPU pressure above threshold", true
	case h.MemoryPressure >= PressureThreshold:
		return "host memory pressure above threshold", true
	case h.ClocksourceWatch > 0:
		return "guest clocksource watchdog fired", true
	}
	return "", false
}
