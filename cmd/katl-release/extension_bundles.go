package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/katl-dev/katl/internal/releasestate"
)

func extensionBundles(args []string, stdout io.Writer) error {
	if len(args) != 1 || !fullSHA.MatchString(args[0]) {
		return errors.New("usage: scripts/katl-release extension-bundles COMMIT (full commit SHA)")
	}
	return json.NewEncoder(stdout).Encode(struct {
		Bundles []releasestate.ExtensionBundle `json:"bundles"`
	}{Bundles: releasestate.ExtensionBundles(args[0])})
}
