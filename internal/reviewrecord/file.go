// Package reviewrecord validates verdict files and reads and writes review records
// stored in katl-queue PR comments.
package reviewrecord

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/queuemarker"
)

type Verdict string

const (
	Approved         Verdict = "approved"
	ChangesRequested Verdict = "changes-requested"
)

const reviewMarker = "review"

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type Record struct {
	Verdict Verdict `json:"verdict"`
	Head    string  `json:"head"`
	Base    string  `json:"base"`
}

func (r Record) validate() error {
	switch {
	case r.Verdict != Approved && r.Verdict != ChangesRequested:
		return errors.New("set verdict to approved or changes-requested")
	case !fullSHA.MatchString(r.Head):
		return errors.New("set head to the full 40-character reviewed commit SHA")
	case !fullSHA.MatchString(r.Base):
		return errors.New("set base to the full 40-character reviewed base SHA")
	default:
		return nil
	}
}

func (r *Record) normalizeSHAs() {
	r.Head = strings.ToLower(r.Head)
	r.Base = strings.ToLower(r.Base)
}

// File is a validated verdict file returned by Decode.
type File struct {
	data fileData
}

type fileData struct {
	Record
	Version  int       `json:"version"`
	PR       int       `json:"pr"`
	Summary  string    `json:"summary"`
	Findings []finding `json:"findings"`
}

type finding struct {
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

var severities = []string{"blocker", "major", "minor", "note"}

// PR is the pull request number recorded in the file.
func (f File) PR() int { return f.data.PR }

// Record is the verdict and the commits it covers.
func (f File) Record() Record { return f.data.Record }

// Decode reads and validates one versioned verdict file.
func Decode(src io.Reader) (File, error) {
	d := json.NewDecoder(src)
	d.DisallowUnknownFields()

	var f fileData
	if err := d.Decode(&f); err != nil {
		return File{}, fmt.Errorf("fix the verdict JSON: %w", err)
	}
	if err := d.Decode(new(json.RawMessage)); err != io.EOF {
		return File{}, errors.New("keep exactly one verdict JSON object in the file")
	}
	if err := f.validate(); err != nil {
		return File{}, err
	}
	f.normalizeSHAs()
	return File{data: f}, nil
}

func (f fileData) validate() error {
	switch {
	case f.Version != 1:
		return errors.New("set version to 1")
	case f.PR <= 0:
		return errors.New("set pr to the pull request number")
	}
	if err := f.Record.validate(); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(f.Summary) == "":
		return errors.New("provide a review summary")
	case f.Findings == nil:
		return errors.New("provide findings as a JSON array")
	case f.Verdict == ChangesRequested && len(f.Findings) == 0:
		return errors.New("add at least one finding for changes-requested")
	}
	for i, finding := range f.Findings {
		switch {
		case !slices.Contains(severities, finding.Severity):
			return fmt.Errorf("set finding %d severity to blocker, major, minor or note", i+1)
		case finding.Severity == "blocker" && f.Verdict == Approved:
			return fmt.Errorf("resolve finding %d or set verdict to changes-requested", i+1)
		case finding.Line < 0:
			return fmt.Errorf("set finding %d line to a positive line number or omit it", i+1)
		case strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Body) == "":
			return fmt.Errorf("provide a title and body for finding %d", i+1)
		}
	}
	return nil
}

// Comment renders a decoded file. Findings of equal severity retain file order.
func (f File) Comment() string {
	data := f.data
	var out strings.Builder
	fmt.Fprintf(&out, "## Review: %s (%.12s)\n\n%s\n", data.Verdict, data.Head, strings.TrimSpace(data.Summary))

	findings := slices.Clone(data.Findings)
	slices.SortStableFunc(findings, func(a, b finding) int {
		return cmp.Compare(slices.Index(severities, a.Severity), slices.Index(severities, b.Severity))
	})
	for _, finding := range findings {
		location := strings.TrimSpace(finding.Path)
		if finding.Line > 0 {
			if location == "" {
				location = fmt.Sprintf("line %d", finding.Line)
			} else {
				location = fmt.Sprintf("%s:%d", location, finding.Line)
			}
		}
		fmt.Fprintf(&out, "\n### %s: %s", finding.Severity, strings.TrimSpace(finding.Title))
		if location != "" {
			fmt.Fprintf(&out, " (%s)", location)
		}
		fmt.Fprintf(&out, "\n\n%s\n", strings.TrimSpace(finding.Body))
	}

	m := queuemarker.Marker{Kind: reviewMarker, Value: string(data.Verdict) + ":" + data.Head + ":" + data.Base}
	fmt.Fprintf(&out, "\n%s", m)
	return strings.ReplaceAll(out.String(), "\r\n", "\n")
}
