package configapply

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

type RenderNodeRequest struct {
	NodeName       string
	Manifest       manifest.Manifest
	SourceID       string
	DesiredVersion string
	ApplyMode      string
}

type renderedNodeConfigurationChange struct {
	APIVersion string                              `yaml:"apiVersion"`
	Kind       string                              `yaml:"kind"`
	Metadata   renderedNodeConfigurationMetadata   `yaml:"metadata"`
	Apply      Apply                               `yaml:"apply"`
	Spec       renderedNodeConfigurationChangeSpec `yaml:"spec"`
}

type renderedNodeConfigurationMetadata struct {
	SourceID       string `yaml:"sourceID"`
	DesiredVersion string `yaml:"desiredVersion"`
}

type renderedNodeConfigurationChangeSpec struct {
	NodeOverrides map[string]renderedNodeConfigurationOverlay `yaml:"nodeOverrides"`
}

type renderedNodeConfigurationOverlay struct {
	Identity   renderedNodeIdentity       `yaml:"identity"`
	SystemRole string                     `yaml:"systemRole,omitempty"`
	Networkd   manifest.NetworkdConfig    `yaml:"networkd"`
	Kubernetes *manifest.KubernetesConfig `yaml:"kubernetes,omitempty"`
}

type renderedNodeIdentity struct {
	Hostname       string   `yaml:"hostname"`
	AuthorizedKeys []string `yaml:"authorizedKeys"`
}

func RenderNodeConfigurationChange(request RenderNodeRequest) ([]byte, error) {
	nodeName := strings.TrimSpace(request.NodeName)
	if nodeName == "" {
		return nil, fmt.Errorf("selected node name is required")
	}
	sourceID, err := cleanAuditSegment("sourceID", request.SourceID)
	if err != nil {
		return nil, err
	}
	desiredVersion, err := cleanDesiredVersion(request.DesiredVersion)
	if err != nil {
		return nil, err
	}
	applyMode, err := normalizeRequestedMode(request.ApplyMode)
	if err != nil {
		return nil, err
	}

	node := request.Manifest.Node
	document := renderedNodeConfigurationChange{
		APIVersion: NodeConfigurationChangeAPIVersion,
		Kind:       NodeConfigurationChangeKind,
		Metadata: renderedNodeConfigurationMetadata{
			SourceID:       sourceID,
			DesiredVersion: desiredVersion,
		},
		Apply: Apply{Mode: applyMode},
		Spec: renderedNodeConfigurationChangeSpec{
			NodeOverrides: map[string]renderedNodeConfigurationOverlay{
				nodeName: {
					Identity: renderedNodeIdentity{
						Hostname:       node.Identity.Hostname,
						AuthorizedKeys: append([]string{}, node.Identity.SSH.AuthorizedKeys...),
					},
					SystemRole: node.SystemRole,
					Networkd:   node.Networkd,
					Kubernetes: &node.Kubernetes,
				},
			},
		},
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("marshal node configuration change: %w", err)
	}
	return data, nil
}
