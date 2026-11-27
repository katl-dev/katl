// Package gotestlog describes the records emitted by go test.
package gotestlog

import (
	"regexp"
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
	return Event{Action: strings.ToLower(line[match[2]:match[3]]), Test: line[match[4]:match[5]]}, match[0]
}

var textResult = regexp.MustCompile(`--- (PASS|FAIL|SKIP): (\S+) \([0-9.]+s\)`)
