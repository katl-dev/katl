package configapply

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

type RenderNodeRequest struct {
	NodeName                string
	Manifest                manifest.Manifest
	KubeadmConfigs          map[string]kubeadmconfig.Plan
	SourceID                string
	DesiredVersion          string
	ApplyMode               string
	KubeadmOnly             bool
	SystemExtensionPayloads []SystemExtensionPayload
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
	NodeOverrides           map[string]renderedNodeConfigurationOverlay `yaml:"nodeOverrides"`
	KubeadmConfigs          map[string]inlineKubeadmConfig              `yaml:"kubeadmConfigs,omitempty"`
	SystemExtensionPayloads []SystemExtensionPayload                    `yaml:"systemExtensionPayloads,omitempty"`
}

type renderedNodeConfigurationOverlay struct {
	Identity             *renderedNodeIdentity        `yaml:"identity,omitempty"`
	SystemRole           string                       `yaml:"systemRole,omitempty"`
	Kernel               *manifest.KernelConfig       `yaml:"kernel"`
	HostConfiguration    *manifest.HostConfiguration  `yaml:"hostConfiguration"`
	SystemExtensions     *[]manifest.SystemExtension  `yaml:"systemExtensions"`
	Volumes              *[]manifest.Volume           `yaml:"volumes"`
	Kubernetes           *manifest.KubernetesConfig   `yaml:"kubernetes,omitempty"`
	ControlPlaneEndpoint *controlPlaneEndpointOverlay `yaml:"controlPlaneEndpoint,omitempty"`
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
	systemExtensions := append([]manifest.SystemExtension(nil), node.SystemExtensions...)
	volumes := append([]manifest.Volume(nil), request.Manifest.Install.Volumes...)
	kernel := manifest.KernelConfig{CommandLine: slices.Clone(node.Kernel.CommandLine)}
	kubeadmConfigs, err := renderKubeadmConfigs(node.Kubernetes.Kubeadm.ConfigRef, request.KubeadmConfigs)
	if err != nil {
		return nil, err
	}
	overlay := renderedNodeConfigurationOverlay{
		Identity: &renderedNodeIdentity{
			Hostname:       node.Identity.Hostname,
			AuthorizedKeys: append([]string{}, node.Identity.SSH.AuthorizedKeys...),
		},
		SystemRole:           node.SystemRole,
		Kernel:               &kernel,
		HostConfiguration:    &node.HostConfiguration,
		SystemExtensions:     &systemExtensions,
		Volumes:              &volumes,
		Kubernetes:           &node.Kubernetes,
		ControlPlaneEndpoint: renderedControlPlaneEndpoint(node.ControlPlaneEndpoint),
	}
	if request.KubeadmOnly {
		overlay = renderedNodeConfigurationOverlay{Kubernetes: &node.Kubernetes}
	}
	document := renderedNodeConfigurationChange{
		APIVersion: NodeConfigurationChangeAPIVersion,
		Kind:       NodeConfigurationChangeKind,
		Metadata: renderedNodeConfigurationMetadata{
			SourceID:       sourceID,
			DesiredVersion: desiredVersion,
		},
		Apply: Apply{Mode: applyMode},
		Spec: renderedNodeConfigurationChangeSpec{
			KubeadmConfigs:          kubeadmConfigs,
			SystemExtensionPayloads: append([]SystemExtensionPayload(nil), request.SystemExtensionPayloads...),
			NodeOverrides: map[string]renderedNodeConfigurationOverlay{
				nodeName: overlay,
			},
		},
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("marshal node configuration change: %w", err)
	}
	return data, nil
}

func renderedControlPlaneEndpoint(config *controlplaneendpoint.Config) *controlPlaneEndpointOverlay {
	return &controlPlaneEndpointOverlay{Managed: config != nil, Config: config}
}

func renderKubeadmConfigs(ref string, configs map[string]kubeadmconfig.Plan) (map[string]inlineKubeadmConfig, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(configs) == 0 {
		return nil, nil
	}
	plan, ok := configs[ref]
	if !ok {
		return nil, fmt.Errorf("selected kubeadm config %q was not resolved", ref)
	}
	patches := make(map[string]string, len(plan.Patches))
	for _, patch := range plan.Patches {
		name := filepath.Base(patch.RenderPath)
		if _, exists := patches[name]; exists {
			return nil, fmt.Errorf("selected kubeadm config %q contains duplicate patch name %q", ref, name)
		}
		patches[name] = string(patch.Content)
	}
	if len(patches) == 0 {
		patches = nil
	}
	return map[string]inlineKubeadmConfig{
		ref: {Config: string(plan.Config.Content), Patches: patches},
	}, nil
}
