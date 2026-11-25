// Package hosthealth records whether the machine running a VM test run kept
// the resources the run depended on, and classifies the run's outcome from
// that evidence.
package hosthealth

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Sample is a point-in-time reading of host counters. Counters are cumulative
// since boot, so a run's health is the difference between two samples.
type Sample struct {
	Time time.Time `json:"time"`
	// Load1 is the one-minute load average.
	Load1 float64 `json:"load1"`
	// MemAvailableKB is MemAvailable from /proc/meminfo.
	MemAvailableKB uint64 `json:"memAvailableKB"`
	// CPUTicks and StealTicks are the summed and stolen jiffies from /proc/stat.
	CPUTicks   uint64 `json:"cpuTicks"`
	StealTicks uint64 `json:"stealTicks"`
	// CPUPressureUS and MemoryPressureUS are the cumulative microseconds in
	// which some task stalled on that resource. They are zero when the kernel
	// has no pressure stall information.
	CPUPressureUS    uint64 `json:"cpuPressureUS"`
	MemoryPressureUS uint64 `json:"memoryPressureUS"`
}

// ReadSample reads counters from a /proc-shaped directory. Missing pressure
// files are not an error: an unavailable signal is not evidence of starvation.
func ReadSample(procRoot string, now time.Time) (Sample, error) {
	s := Sample{Time: now}

	loadavg, err := os.ReadFile(filepath.Join(procRoot, "loadavg"))
	if err != nil {
		return s, err
	}
	if s.Load1, err = parseLoad1(string(loadavg)); err != nil {
		return s, err
	}

	meminfo, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return s, err
	}
	s.MemAvailableKB = parseMemAvailable(string(meminfo))

	stat, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return s, err
	}
	if s.CPUTicks, s.StealTicks, err = parseCPUStat(string(stat)); err != nil {
		return s, err
	}

	s.CPUPressureUS = readPressure(filepath.Join(procRoot, "pressure", "cpu"))
	s.MemoryPressureUS = readPressure(filepath.Join(procRoot, "pressure", "memory"))
	return s, nil
}

func parseLoad1(text string) (float64, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0, fmt.Errorf("loadavg is empty")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func parseMemAvailable(text string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		v, _ := strconv.ParseUint(fields[0], 10, 64)
		return v
	}
	return 0
}

// parseCPUStat sums the aggregate "cpu" line and returns its steal column.
func parseCPUStat(text string) (total, steal uint64, err error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		for i, f := range fields[1:] {
			// guest and guest_nice (columns 9 and 10) are already counted in
			// user and nice.
			if i >= 8 {
				break
			}
			v, perr := strconv.ParseUint(f, 10, 64)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse /proc/stat cpu line: %w", perr)
			}
			total += v
			if i == 7 {
				steal = v
			}
		}
		return total, steal, nil
	}
	return 0, 0, fmt.Errorf("/proc/stat has no cpu line")
}

func readPressure(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return parsePressureSome(string(data))
}

// parsePressureSome returns total= from the "some" line.
func parsePressureSome(text string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "total="); ok {
				n, _ := strconv.ParseUint(v, 10, 64)
				return n
			}
		}
	}
	return 0
}
