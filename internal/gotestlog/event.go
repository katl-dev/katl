// Package gotestlog describes the records emitted by go test.
package gotestlog

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Event is a record from go test -json. Build output carries ImportPath
// instead of Package, and a package that failed to build names that import
// path in FailedBuild.
type Event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	Elapsed     float64
	ImportPath  string
	FailedBuild string
}

// IsResult reports whether the event ends a test or package run.
func (e Event) IsResult() bool {
	return e.Action == "pass" || e.Action == "fail" || e.Action == "skip"
}

// MatchTextResult finds a result header and returns its byte offset. A writer
// can leave an unterminated line before the header. The duration distinguishes
// headers from incidental mentions of a result.
func MatchTextResult(line string) (Event, int) {
	match := textResult.FindStringSubmatchIndex(line)
	if match == nil {
		return Event{}, -1
	}
	elapsed, _ := strconv.ParseFloat(line[match[6]:match[7]], 64)
	return Event{
		Action:  strings.ToLower(line[match[2]:match[3]]),
		Test:    line[match[4]:match[5]],
		Elapsed: elapsed,
	}, match[0]
}

var textResult = regexp.MustCompile(`--- (PASS|FAIL|SKIP): (\S+) \(([0-9.]+)s\)`)

// ParseTestResult returns the test result a go test log line records, in
// either -json or -v text form. A JSON event is never matched as text: its
// Output field quotes the header of a result that has its own event.
func ParseTestResult(line string) (Event, bool) {
	if strings.HasPrefix(line, "{") {
		var event Event
		if json.Unmarshal([]byte(line), &event) == nil {
			return event, event.IsResult() && event.Test != ""
		}
	}
	event, offset := MatchTextResult(line)
	return event, offset >= 0
}
