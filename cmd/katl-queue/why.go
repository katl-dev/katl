package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/gotestlog"
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

// testLogExcerpt ranks output only after associating it with a failure: a
// failed test, or a failed package, which owns its package-level output and
// the output of tests that never reported a result.
func testLogExcerpt(log []byte, limit int) []string {
	var structured jsonTestLog
	var plain textTestLog
	var failed []string
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if output, ok := structured.add(line); ok {
			failed = append(failed, output...)
			continue
		}
		failed = append(failed, plain.add(line)...)
	}

	excerpt := rankExcerpt(failed, limit)
	if len(excerpt) > 0 && rankLine(excerpt[0]) >= rankGeneric {
		return nil
	}
	return excerpt
}

// jsonTestID names an output owner in a go test -json stream. An empty name
// is the package itself; build output is owned by its import path.
type jsonTestID struct{ pkg, name string }

type jsonTestLog struct {
	pending map[jsonTestID][]string
	order   []jsonTestID
}

// add reports whether line is a go test -json event and returns newly
// confirmed failure output.
func (l *jsonTestLog) add(line string) ([]string, bool) {
	if !strings.HasPrefix(line, "{") {
		return nil, false
	}
	var event gotestlog.Event
	if json.Unmarshal([]byte(line), &event) != nil || event.Action == "" {
		return nil, false
	}

	switch {
	case event.Action == "output" || event.Action == "build-output":
		id := jsonTestID{event.Package, event.Test}
		if event.Action == "build-output" {
			id = jsonTestID{pkg: event.ImportPath}
		}
		if l.pending == nil {
			l.pending = map[jsonTestID][]string{}
		}
		if _, ok := l.pending[id]; !ok && !slices.Contains(l.order, id) {
			l.order = append(l.order, id)
		}
		l.pending[id] = append(l.pending[id], strings.Split(strings.TrimSuffix(event.Output, "\n"), "\n")...)
	case event.IsResult() && event.Test != "":
		id := jsonTestID{event.Package, event.Test}
		output := l.pending[id]
		delete(l.pending, id)
		if event.Action == "fail" {
			return output, true
		}
	case event.IsResult():
		owned := func(id jsonTestID) bool {
			return id.pkg == event.Package || (event.FailedBuild != "" && id.pkg == event.FailedBuild)
		}
		var output []string
		for _, id := range l.order {
			if lines, ok := l.pending[id]; ok && owned(id) {
				output = append(output, lines...)
				delete(l.pending, id)
			}
		}
		l.order = slices.DeleteFunc(l.order, owned)
		if event.Action == "fail" {
			return output, true
		}
	}
	return nil, true
}

type textTestResult struct {
	indent int
	failed bool
}

// textTestLog follows one package at a time: go test prints a package's
// output before its "ok" or "FAIL" line.
type textTestLog struct {
	active  string
	pending map[string][]string
	order   []string
	results []textTestResult
	// unindented holds column-0 lines, such as panics, build errors and
	// TestMain output, which belong to the package if it fails.
	unindented []string
}

// add returns newly confirmed failure output. Verbose output can arrive
// before a result, while buffered output follows its result at a deeper
// indentation. A nested result owns only its own indented output.
func (l *textTestLog) add(line string) []string {
	if control, ok := strings.CutPrefix(line, "=== "); ok {
		action, name, _ := strings.Cut(control, " ")
		switch action {
		case "RUN", "CONT", "NAME":
			l.active = strings.TrimSpace(name)
		case "PAUSE":
			l.active = ""
		}
		l.results = l.results[:0]
		return nil
	}

	trimmed := strings.TrimLeft(line, " \t")
	indent := len(line) - len(trimmed)
	for len(l.results) > 0 && l.results[len(l.results)-1].indent >= indent {
		l.results = l.results[:len(l.results)-1]
	}
	if result, offset := gotestlog.MatchTextResult(line); offset == indent {
		l.active = ""
		failed := result.Action == "fail"
		l.results = append(l.results, textTestResult{indent, failed})
		output := l.pending[result.Test]
		delete(l.pending, result.Test)
		if failed {
			return append(output, trimmed)
		}
		return nil
	}

	if strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "ok  \t") {
		*l = textTestLog{}
		return nil
	}
	if packageFailLine.MatchString(line) {
		output := l.unindented
		for _, name := range l.order {
			output = append(output, l.pending[name]...)
		}
		*l = textTestLog{}
		return append(output, line)
	}

	if indent == 0 {
		l.unindented = append(l.unindented, line)
	}
	if len(l.results) > 0 && l.results[len(l.results)-1].failed {
		return []string{trimmed}
	}
	if l.active != "" {
		if l.pending == nil {
			l.pending = map[string][]string{}
		}
		if _, ok := l.pending[l.active]; !ok && !slices.Contains(l.order, l.active) {
			l.order = append(l.order, l.active)
		}
		l.pending[l.active] = append(l.pending[l.active], trimmed)
	}
	return nil
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
