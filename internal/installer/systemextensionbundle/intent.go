package systemextensionbundle

import "github.com/katl-dev/katl/internal/installer/manifest"

// Selection carries operator intent. The node resolves it against the runtime
// selected for the operation. Local file sources must be expanded by the client.
type Selection struct {
	Release       string                                `json:"release,omitempty" yaml:"release,omitempty"`
	Bundle        string                                `json:"bundle,omitempty" yaml:"bundle,omitempty"`
	State         string                                `json:"state,omitempty" yaml:"state,omitempty"`
	Configuration manifest.SystemExtensionConfiguration `json:"configuration,omitempty" yaml:"configuration,omitempty"`
	Units         []manifest.SystemExtensionUnit        `json:"units,omitempty" yaml:"units,omitempty"`
}
