package vmtest

import "strings"

// consoleText returns serial console output with every line that a kernel
// record interrupted rejoined, and the record moved to its own line before it.
//
// The kernel writes each record straight to the UART, while userspace console
// output, such as journald's forwarding, drains through the tty buffer. A
// record can therefore land anywhere inside a userspace line. The record is
// written whole and ends with its own newline, so the interrupted line's text
// is left before the record and its remainder starts the next raw line.
// Several records can interrupt one line, including back to back.
//
// Matching on the rejoined text keeps every substring of the raw output that
// spans neither a newline nor a mid-line kernel timestamp. An unterminated
// final line is kept as observed, because its remainder may still arrive.
func consoleText(raw string) string {
	var text strings.Builder
	text.Grow(len(raw))
	interrupted := ""
	for raw != "" {
		line, rest, terminated := strings.Cut(raw, "\n")
		raw = rest
		line = interrupted + line
		interrupted = ""
		if !terminated {
			text.WriteString(line)
			break
		}
		if start := kernelRecordStart(line); start > 0 {
			interrupted = line[:start]
			line = line[start:]
		}
		text.WriteString(line)
		text.WriteByte('\n')
	}
	text.WriteString(interrupted)
	return text.String()
}

// kernelRecordStart returns the index of the first console timestamp, such as
// "[    4.803472] ", after the start of line, or -1. The kernel prints its
// timestamp as %5lu.%06lu in brackets.
func kernelRecordStart(line string) int {
	for i := 1; i < len(line); i++ {
		if line[i] == '[' && isConsoleTimestamp(line[i:]) {
			return i
		}
	}
	return -1
}

func isConsoleTimestamp(s string) bool {
	i := 1
	for i < len(s) && s[i] == ' ' {
		i++
	}
	seconds := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == seconds || i >= len(s) || s[i] != '.' {
		return false
	}
	i++
	fraction := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i-fraction == 6 && strings.HasPrefix(s[i:], "] ")
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
