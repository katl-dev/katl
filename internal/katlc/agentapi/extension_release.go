package agentapi

import (
	"fmt"
	"maps"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func ExtensionReleaseFromManifest(manifest *extensionrelease.Manifest) *ExtensionRelease {
	if manifest == nil {
		return nil
	}
	target := manifest.Target
	return &ExtensionRelease{
		Version:          target.Version,
		Architecture:     target.Architecture,
		Flavour:          target.Flavour,
		RuntimeInterface: target.RuntimeInterface,
		RuntimeSha256:    target.Kernel.RuntimeSHA256,
		KernelRelease:    target.Kernel.Release,
		Extensions:       maps.Clone(manifest.Extensions),
	}
}

func (release *ExtensionRelease) Manifest() (extensionrelease.Manifest, error) {
	if release == nil {
		return extensionrelease.Manifest{}, fmt.Errorf("node does not provide a release extension manifest; upgrade KatlOS before selecting release-owned extensions")
	}
	manifest := extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          release.Version,
			Architecture:     release.Architecture,
			Flavour:          release.Flavour,
			RuntimeInterface: release.RuntimeInterface,
			Kernel: kernelmodule.Target{
				Release:       release.KernelRelease,
				RuntimeSHA256: release.RuntimeSha256,
			},
		},
		Extensions: maps.Clone(release.Extensions),
	}
	return manifest, manifest.Validate()
}
