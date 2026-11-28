package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/katl-dev/katl/internal/releasestate"
)

func extensionBundles(args []string, stdout io.Writer) error {
	if len(args) != 1 || !fullSHA.MatchString(args[0]) {
		return errors.New("usage: scripts/katl-release extension-bundles COMMIT (full commit SHA)")
	}
	bundles := releasestate.ExtensionBundles(args[0])
	names := make([]string, 0, len(bundles))
	for _, bundle := range bundles {
		names = append(names, bundle.Name)
	}
	// download-artifact accepts one minimatch pattern for several names.
	pattern := strings.Join(names, ",")
	if len(names) > 1 {
		pattern = "{" + pattern + "}"
	}
	return json.NewEncoder(stdout).Encode(struct {
		Pattern string                         `json:"pattern"`
		Bundles []releasestate.ExtensionBundle `json:"bundles"`
	}{Pattern: pattern, Bundles: bundles})
}
