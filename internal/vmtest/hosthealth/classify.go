package hosthealth

import (
	"encoding/json"
	"strings"
)

// Outcome is the verdict of a VM test run.
type Outcome string

const (
	Pass         Outcome = "pass"
	Fail         Outcome = "fail"
	Inconclusive Outcome = "inconclusive"
)

// Verdict is an outcome with the evidence that justifies it.
type Verdict struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason"`
}

// Classify decides a run's outcome. A run that did not pass is inconclusive
// only when all of the following hold:
//
//   - go test ended in its own timeout panic, so the run was cut off rather
//     than reaching an assertion;
//   - no test reported a failure, so no deterministic failure is mixed in;
//   - the host health shows positive evidence of starvation.
//
// Every other failure, including one on a starved host, is a fail.
func Classify(passed bool, goTestLog string, h Health) Verdict {
	if passed {
		return Verdict{Pass, "go test passed and requested tests ran"}
	}
	text := plainOutput(goTestLog)
	if !strings.Contains(text, "panic: test timed out after") {
		return Verdict{Fail, "failure is not a go test timeout"}
	}
	if strings.Contains(text, "--- FAIL:") {
		return Verdict{Fail, "a test reported a failure"}
	}
	reason, starved := h.Starved()
	if !starved {
		return Verdict{Fail, "timed out without evidence of host starvation"}
	}
	return Verdict{Inconclusive, "go test timed out and " + reason}
}

// plainOutput returns the text of a go test log, unwrapping `go test -json`
// events into the output they carry.
func plainOutput(log string) string {
	var b strings.Builder
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "{") {
			var event struct{ Output string }
			if json.Unmarshal([]byte(line), &event) == nil {
				b.WriteString(event.Output)
				continue
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
