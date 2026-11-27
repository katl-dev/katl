// Package queuemarker owns the hidden marker grammar for katl-queue comments.
package queuemarker

import (
	"regexp"
	"strings"
)

// Marker is a hidden line in a katl-queue comment. It lets a later pass
// recognise what the comment records without parsing its text.
type Marker struct {
	Kind, Value string
}

func (m Marker) String() string {
	return "<!-- katl-queue:" + m.Kind + " " + m.Value + " -->"
}

var markerLine = regexp.MustCompile(`^<!-- katl-queue:([a-z-]+) (\S+) -->$`)

// Parse returns the markers on standalone lines in text.
func Parse(text string) []Marker {
	var out []Marker
	for _, line := range strings.Split(text, "\n") {
		if m := markerLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, Marker{Kind: m[1], Value: m[2]})
		}
	}
	return out
}
