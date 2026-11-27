// Package releaseversion owns the KatlOS version grammar of ADR-009.
package releaseversion

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Stage is the prerelease label of a version; the empty stage is a final.
type Stage string

const (
	Final     Stage = ""
	Dev       Stage = "dev"
	Alpha     Stage = "alpha"
	Beta      Stage = "beta"
	Candidate Stage = "rc"
)

// maxLength bounds a version so that every name derived from it stays usable.
const maxLength = 80

var grammar = regexp.MustCompile(`^([0-9]{4})\.([1-9]|1[0-2])\.(0|[1-9][0-9]*)(?:-(dev|alpha|beta|rc)\.(0|[1-9][0-9]*))?$`)

// Version is a canonical KatlOS version, without the tag's v prefix.
type Version struct {
	Year, Month, Patch uint64
	Stage              Stage
	// Number is the prerelease counter; it is zero for a final.
	Number uint64
}

// Parse accepts YYYY.M.PATCH[-dev.N|-alpha.N|-beta.N|-rc.N] with no zero
// padding and a calendar month. It accepts rc.0: whether a candidate number
// is usable is the release action's rule, not the grammar's.
func Parse(value string) (Version, error) {
	if value == "" {
		return Version{}, errors.New("release version is empty")
	}
	if len(value) > maxLength {
		return Version{}, fmt.Errorf("release version is longer than %d characters", maxLength)
	}
	match := grammar.FindStringSubmatch(value)
	if match == nil {
		return Version{}, fmt.Errorf("release version is not canonical KatlOS CalVer (YYYY.M.PATCH[-dev.N|-alpha.N|-beta.N|-rc.N]): %s", value)
	}
	var version Version
	fields := []*uint64{&version.Year, &version.Month, &version.Patch, &version.Number}
	for i, text := range []string{match[1], match[2], match[3], match[5]} {
		if text == "" {
			continue
		}
		n, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("release version has a number too large to compare: %s", value)
		}
		*fields[i] = n
	}
	version.Stage = Stage(match[4])
	return version, nil
}

// ParseTag parses a release tag, which is a version with a v prefix.
func ParseTag(tag string) (Version, error) {
	if len(tag) < 2 || tag[0] != 'v' {
		return Version{}, fmt.Errorf("release tag %q does not start with v", tag)
	}
	return Parse(tag[1:])
}

// IsFinal reports whether the version has no prerelease suffix.
func (v Version) IsFinal() bool { return v.Stage == Final }

// Base is the final version that the version precedes or is.
func (v Version) Base() Version { return Version{Year: v.Year, Month: v.Month, Patch: v.Patch} }

// Candidate is the release candidate n of the version's final.
func (v Version) Candidate(n uint64) Version {
	base := v.Base()
	base.Stage, base.Number = Candidate, n
	return base
}

func (v Version) String() string {
	core := fmt.Sprintf("%04d.%d.%d", v.Year, v.Month, v.Patch)
	if v.Stage == Final {
		return core
	}
	return fmt.Sprintf("%s-%s.%d", core, v.Stage, v.Number)
}

// Tag is the Git tag of the version.
func (v Version) Tag() string { return "v" + v.String() }
