package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
)

// versionCommand gives scripts the version grammar without a pattern of their
// own: "version VERSION" fails for a non-canonical version, "stable-tags
// TAG..." prints the arguments that are final release tags, in their order,
// and "bundle-name VERSION COMMIT" prints the artifact a build of the version
// at that full commit uploads.
func versionCommand(args []string, stdout io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "version":
		_, err := releaseversion.Parse(args[1])
		return err
	case args[0] == "require-candidate":
		if len(args) != 2 {
			return errors.New("usage: scripts/katl-release require-candidate VERSION")
		}
		version, err := publicationVersion(args[1])
		if err != nil {
			return err
		}
		if version.IsFinal() {
			return fmt.Errorf("%s is a final version. Automatic pre-release publication requires a release candidate", version)
		}
		return nil
	case len(args) == 2 && args[0] == "tag-name":
		version, err := validateVersion(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, version.Tag())
		return err
	case len(args) >= 1 && args[0] == "stable-tags":
		for _, tag := range args[1:] {
			if version, err := releaseversion.ParseTag(tag); err == nil && version.IsFinal() {
				fmt.Fprintln(stdout, tag)
			}
		}
		return nil
	case len(args) == 3 && args[0] == "bundle-name":
		version, err := releaseversion.Parse(args[1])
		if err != nil {
			return err
		}
		if !fullSHA.MatchString(args[2]) {
			return errors.New("COMMIT must be a full commit SHA")
		}
		_, err = fmt.Fprintln(stdout, releasestate.BundleName(version, args[2]))
		return err
	}
	return errors.New("usage: scripts/katl-release version VERSION\n       scripts/katl-release tag-name VERSION\n       scripts/katl-release stable-tags [TAG...]\n       scripts/katl-release bundle-name VERSION COMMIT")
}

// upgradeBaselineFile pins the previous final release. journeys reads it at
// the release branch head, and scripts/katl-release-artifacts through
// baseline-tag.
const upgradeBaselineFile = "scripts/production-upgrade-baseline.txt"

// parseUpgradeBaseline turns the content of upgradeBaselineFile into the
// pinned previous final release. The content is exactly one line holding a
// final release tag, with an optional trailing newline; surrounding
// whitespace, CRLF and further lines, blank or not, are refused, not trimmed.
func parseUpgradeBaseline(content []byte) (releaseversion.Version, error) {
	line := strings.TrimSuffix(string(content), "\n")
	previous, err := releaseversion.ParseTag(line)
	if err != nil || !previous.IsFinal() {
		return releaseversion.Version{}, fmt.Errorf("%s must contain one line with a final KatlOS release tag such as v2026.11.7, got %q", upgradeBaselineFile, line)
	}
	return previous, nil
}

// baselineTag prints the tag that pin content on stdin names, so scripts read
// the pin by the same rule as journeys.
func baselineTag(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: scripts/katl-release baseline-tag < " + upgradeBaselineFile)
	}
	content, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	previous, err := parseUpgradeBaseline(content)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, previous.Tag())
	return err
}

// compareFinal orders final versions by their release number.
func compareFinal(a, b releaseversion.Version) int {
	return cmp.Or(cmp.Compare(a.Year, b.Year), cmp.Compare(a.Month, b.Month), cmp.Compare(a.Patch, b.Patch))
}
