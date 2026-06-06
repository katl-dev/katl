package configapply

import (
	"context"
	"fmt"
	"io"

	"github.com/zariel/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

type nodeConfigurationChangeDocument struct {
	APIVersion string                          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string                          `json:"kind" yaml:"kind"`
	Metadata   nodeConfigurationChangeMetadata `json:"metadata" yaml:"metadata"`
	Apply      Apply                           `json:"apply" yaml:"apply"`
	Spec       nodeConfigurationChangeSpec     `json:"spec" yaml:"spec"`
}

type nodeConfigurationChangeMetadata struct {
	SourceID       string `json:"sourceID" yaml:"sourceID"`
	DesiredVersion string `json:"desiredVersion" yaml:"desiredVersion"`
}

type nodeConfigurationChangeSpec struct {
	ClusterDefaults     nodeConfigurationOverlay            `json:"clusterDefaults,omitempty" yaml:"clusterDefaults,omitempty"`
	SystemRoleOverrides map[string]nodeConfigurationOverlay `json:"systemRoleOverrides,omitempty" yaml:"systemRoleOverrides,omitempty"`
	NodeOverrides       map[string]nodeConfigurationOverlay `json:"nodeOverrides,omitempty" yaml:"nodeOverrides,omitempty"`
}

type nodeConfigurationOverlay struct {
	Identity      *IdentityOverlay           `json:"identity,omitempty" yaml:"identity,omitempty"`
	SystemRole    string                     `json:"systemRole,omitempty" yaml:"systemRole,omitempty"`
	Networkd      *manifest.NetworkdConfig   `json:"networkd,omitempty" yaml:"networkd,omitempty"`
	Kubernetes    *manifest.KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	LivePreflight map[string]bool            `json:"livePreflight,omitempty" yaml:"livePreflight,omitempty"`
}

func DecodeNodeConfigurationChange(reader io.Reader, base TrustedBundleRequest) (TrustedBundleRequest, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var document nodeConfigurationChangeDocument
	if err := decoder.Decode(&document); err != nil {
		return TrustedBundleRequest{}, fmt.Errorf("decode node configuration change: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return TrustedBundleRequest{}, fmt.Errorf("decode node configuration change: multiple YAML documents")
	}
	if document.APIVersion != NodeConfigurationChangeAPIVersion {
		return TrustedBundleRequest{}, fmt.Errorf("apiVersion must be %s", NodeConfigurationChangeAPIVersion)
	}
	if document.Kind != NodeConfigurationChangeKind {
		return TrustedBundleRequest{}, fmt.Errorf("kind must be %s", NodeConfigurationChangeKind)
	}

	request := base
	request.SourceID = document.Metadata.SourceID
	request.DesiredVersion = document.Metadata.DesiredVersion
	request.ApplyMode = document.Apply.Mode
	request.ClusterDefaults = document.Spec.ClusterDefaults.nodeOverlay()
	request.SystemRoleOverrides = nodeOverlayMap(document.Spec.SystemRoleOverrides)
	request.NodeOverrides = nodeOverlayMap(document.Spec.NodeOverrides)
	return request, nil
}

func ApplyNodeConfigurationChange(ctx context.Context, reader io.Reader, base TrustedBundleRequest) (TrustedBundleResult, error) {
	request, err := DecodeNodeConfigurationChange(reader, base)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	return ApplyTrustedBundle(ctx, request)
}

func nodeOverlayMap(overlays map[string]nodeConfigurationOverlay) map[string]NodeOverlay {
	if len(overlays) == 0 {
		return nil
	}
	out := make(map[string]NodeOverlay, len(overlays))
	for name, overlay := range overlays {
		out[name] = overlay.nodeOverlay()
	}
	return out
}

func (overlay nodeConfigurationOverlay) nodeOverlay() NodeOverlay {
	return NodeOverlay{
		Identity:      overlay.Identity,
		SystemRole:    overlay.SystemRole,
		Networkd:      overlay.Networkd,
		Kubernetes:    overlay.Kubernetes,
		LivePreflight: overlay.LivePreflight,
	}
}
