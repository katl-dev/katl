package main

import (
	"errors"
	"fmt"
	"io"

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
