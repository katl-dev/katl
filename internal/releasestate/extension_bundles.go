package releasestate

import "github.com/katl-dev/katl/internal/flavour"

// ExtensionBundle identifies a workflow artifact with publication inputs for
// one kernel flavour.
type ExtensionBundle struct {
	Flavour string `json:"flavour"`
	Name    string `json:"name"`
}

// ExtensionBundles lists the publication inputs artifacts a build at commit
// must provide.
func ExtensionBundles(commit string) []ExtensionBundle {
	flavours := flavour.Supported()
	bundles := make([]ExtensionBundle, 0, len(flavours))
	for _, track := range flavours {
		bundles = append(bundles, ExtensionBundle{
			Flavour: track,
			Name:    "katl-extension-bundles-" + track + "-" + commit,
		})
	}
	return bundles
}
