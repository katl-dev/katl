// Package extensionrelease describes release-owned extension selections.
package extensionrelease

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

// Target identifies the runtime being prepared, independently of the kernel
// executing the resolver or the workstation's installation defaults.
type Target struct {
	Version          string              `json:"version" yaml:"version"`
	Architecture     string              `json:"architecture" yaml:"architecture"`
	Flavour          string              `json:"flavour" yaml:"flavour"`
	RuntimeInterface string              `json:"runtimeInterface" yaml:"runtimeInterface"`
	Kernel           kernelmodule.Target `json:"kernel" yaml:"kernel"`
}

type Manifest struct {
	Target     Target            `json:"target" yaml:"target"`
	Extensions map[string]string `json:"extensions" yaml:"extensions"`
}

func ValidateRepository(value string) error {
	ref, err := reference.ParseNamed(value)
	if err != nil || !reference.IsNameOnly(ref) {
		return fmt.Errorf("release extension %q must be a full OCI repository without a tag or digest", value)
	}
	return nil
}

func (target Target) Validate() error {
	if strings.TrimSpace(target.Version) == "" || strings.TrimSpace(target.Architecture) == "" || strings.TrimSpace(target.RuntimeInterface) == "" {
		return fmt.Errorf("release version, architecture, and runtime interface are required")
	}
	canonical, err := flavour.Normalize(target.Flavour)
	if err != nil || target.Flavour != canonical {
		return fmt.Errorf("release target requires an explicit supported kernel flavor")
	}
	return target.Kernel.Validate()
}

func (target Target) ValidateRuntime(version, architecture, kernelFlavour, runtimeInterface, runtimeSHA256 string) error {
	if err := target.Validate(); err != nil {
		return err
	}
	canonical, err := flavour.Normalize(kernelFlavour)
	if err != nil {
		return err
	}
	if target.Version != version || target.Architecture != architecture || target.Flavour != canonical || target.RuntimeInterface != runtimeInterface || target.Kernel.RuntimeSHA256 != runtimeSHA256 {
		return fmt.Errorf("extension release target does not match the selected runtime")
	}
	return nil
}

func (manifest Manifest) Validate() error {
	if err := manifest.Target.Validate(); err != nil {
		return err
	}
	for repository, value := range manifest.Extensions {
		if err := ValidateRepository(repository); err != nil {
			return err
		}
		ref, err := payloadbundle.ParseReference(value)
		if err != nil {
			return fmt.Errorf("release extension %q: %w", repository, err)
		}
		if payloadbundle.ManifestDigest(ref) == "" {
			return fmt.Errorf("release extension %q must be pinned by OCI digest", repository)
		}
		if ref.Name() != repository {
			return fmt.Errorf("release extension %q must select an artifact in the same repository", repository)
		}
	}
	return nil
}

func (manifest Manifest) Resolve(target Target, repository string) (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	if manifest.Target != target {
		return "", fmt.Errorf("extension release manifest does not match the target runtime")
	}
	ref, ok := manifest.Extensions[repository]
	if !ok {
		return "", fmt.Errorf("extension %q is unavailable for KatlOS %s (%s/%s); select a release that provides it or remove the selection with node upgrade --apply-config", repository, target.Version, target.Architecture, target.Flavour)
	}
	return ref, nil
}
