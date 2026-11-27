package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/katl-dev/katl/internal/releaseversion"
)

// versionCommand gives scripts the version grammar without a pattern of their
// own: "version VERSION" fails for a non-canonical version, and "stable-tags
// TAG..." prints the arguments that are final release tags, in their order.
func versionCommand(args []string, stdout io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "version":
		_, err := releaseversion.Parse(args[1])
		return err
	case len(args) >= 1 && args[0] == "stable-tags":
		for _, tag := range args[1:] {
			if version, err := releaseversion.ParseTag(tag); err == nil && version.IsFinal() {
				fmt.Fprintln(stdout, tag)
			}
		}
		return nil
	}
	return errors.New("usage: scripts/katl-release version VERSION\n       scripts/katl-release stable-tags [TAG...]")
}
