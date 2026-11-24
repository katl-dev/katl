package main

import (
	"bufio"
	"bytes"
	"regexp"
	"slices"
	"strings"
)

var (
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
	errorLine    = regexp.MustCompile(`(?i)(##\[error\]|\berror\b|--- FAIL|^FAIL\b|panic:|\bfailed\b)`)
	envLine      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*: `)
)

// errorExcerpts picks the first limit error-looking lines for each job from
// `gh run view --log-failed` output, whose lines are "job<TAB>step<TAB>log".
// Echoed shell commands and step environment listings are skipped: they
// quote configuration rather than report errors. Repeated lines are reported
// once.
func errorExcerpts(log []byte, limit int) map[string][]string {
	excerpts := map[string][]string{}
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
		text = strings.TrimSpace(ansiEscape.ReplaceAllString(text, ""))
		if !errorLine.MatchString(text) || envLine.MatchString(text) ||
			slices.Contains(excerpts[job], text) || len(excerpts[job]) >= limit {
			continue
		}
		excerpts[job] = append(excerpts[job], text)
	}
	return excerpts
}
