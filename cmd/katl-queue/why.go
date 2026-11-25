package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
	envLine      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*: `)
)

// Excerpt ranks, best first. A go test failure explains a failed job better
// than anything printed before the tests started, so lower ranks are kept in
// preference to higher ones when the excerpt is full.
const (
	rankTestFailure = iota // --- FAIL
	rankPanic              // panic:
	rankAssertion          // foo_test.go:123: ...
	rankPackageFail        // FAIL <pkg>
	rankGeneric            // anything else that reads as an error
	rankNone
)

var (
	testFailureLine = regexp.MustCompile(`^--- FAIL\b`)
	panicLine       = regexp.MustCompile(`^panic:`)
	assertionLine   = regexp.MustCompile(`^\S+_test\.go:\d+:`)
	packageFailLine = regexp.MustCompile(`^FAIL\s+\S+`)
	genericLine     = regexp.MustCompile(`(?i)(##\[error\]|\berror\b|\bfailed\b)`)
)

// benignSetup lists complete lines that setup steps print on their expected
// path. Each entry must match an entire line so that a real failure which
// merely mentions the same words is still reported.
var benignSetup = []*regexp.Regexp{
	// The hosted-vmtest action probes for libvirt's default pool before
	// defining it (#516).
	regexp.MustCompile(`^error: failed to get pool 'default'$`),
	regexp.MustCompile(`^error: Storage pool not found: no storage pool with matching name 'default'$`),
}

func rankLine(line string) int {
	switch {
	case testFailureLine.MatchString(line):
		return rankTestFailure
	case panicLine.MatchString(line):
		return rankPanic
	case assertionLine.MatchString(line):
		return rankAssertion
	case packageFailLine.MatchString(line):
		return rankPackageFail
	case slices.ContainsFunc(benignSetup, func(re *regexp.Regexp) bool { return re.MatchString(line) }):
		return rankNone
	case envLine.MatchString(line), !genericLine.MatchString(line):
		return rankNone
	}
	return rankGeneric
}

// rankExcerpt picks the limit most informative error lines from log lines in
// the order they were printed. Lines of equal rank keep their order, and
// repeated lines are reported once. Generic errors are dropped when go test
// failures are present: they are setup output, not the cause.
func rankExcerpt(lines []string, limit int) []string {
	var seen []string
	var ranked [rankNone][]string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		rank := rankLine(line)
		if rank == rankNone || slices.Contains(seen, line) {
			continue
		}
		seen = append(seen, line)
		ranked[rank] = append(ranked[rank], line)
	}
	var excerpt []string
	for rank, group := range ranked {
		if rank == rankGeneric && len(excerpt) > 0 {
			break
		}
		excerpt = append(excerpt, group...)
	}
	if len(excerpt) > limit {
		excerpt = excerpt[:limit]
	}
	return excerpt
}

// explainsTestFailure reports whether an excerpt says how a test failed, not
// only which one did.
func explainsTestFailure(excerpt []string) bool {
	return slices.ContainsFunc(excerpt, func(line string) bool {
		return rankLine(line) == rankPanic || rankLine(line) == rankAssertion
	})
}

// errorExcerpts picks error lines for each job from `gh run view
// --log-failed` output, whose lines are "job<TAB>step<TAB>log". Echoed shell
// commands and step environment listings are skipped: they quote
// configuration rather than report errors.
func errorExcerpts(log []byte, limit int) map[string][]string {
	jobLines := map[string][]string{}
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		job, rest, ok := strings.Cut(scanner.Text(), "\t")
		if !ok {
			continue
		}
		_, text, ok := strings.Cut(rest, "\t")
		if !ok || strings.Contains(text, "\x1b[36;1m") {
			continue
		}
		text = strings.TrimPrefix(text, "\ufeff")
		text = logTimestamp.ReplaceAllString(text, "")
		text = ansiEscape.ReplaceAllString(text, "")
		jobLines[job] = append(jobLines[job], text)
	}
	excerpts := map[string][]string{}
	for job, lines := range jobLines {
		if excerpt := rankExcerpt(lines, limit); len(excerpt) > 0 {
			excerpts[job] = excerpt
		}
	}
	return excerpts
}

// testLogExcerpt picks failure lines from a go test log, which is either plain
// text or the `go test -json` event stream.
func testLogExcerpt(log []byte, limit int) []string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			var event struct{ Output string }
			if json.Unmarshal([]byte(line), &event) == nil {
				lines = append(lines, strings.Split(event.Output, "\n")...)
				continue
			}
		}
		lines = append(lines, line)
	}
	var tests []string
	for _, line := range rankExcerpt(lines, len(lines)) {
		if rankLine(line) < rankGeneric {
			tests = append(tests, line)
		}
	}
	if len(tests) > limit {
		tests = tests[:limit]
	}
	return tests
}

var testLogName = regexp.MustCompile(`^go-test\.(log|jsonl)$`)

// diagnosticsArtifacts selects, from newline-separated artifact names, those
// that carry the logs of one run. Workflows name them with the run ID, which
// excludes the large input and release artifacts named for a commit.
func diagnosticsArtifacts(names string, runID int64) []string {
	suffix := "-" + strconv.FormatInt(runID, 10)
	var selected []string
	for _, name := range strings.Fields(names) {
		if strings.HasSuffix(name, suffix) {
			selected = append(selected, name)
		}
	}
	return selected
}
