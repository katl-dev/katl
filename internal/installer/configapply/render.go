package configapply

import (
	"fmt"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/apiproxy"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"gopkg.in/yaml.v3"
)

type RenderNodeRequest struct {
	SystemExtensionSelections *[]systemextensionbundle.Selection
	NodeName                  string
	Manifest                  manifest.Manifest
	KubeadmConfigs            map[string]kubeadmconfig.Plan
	SourceID                  string
	DesiredVersion            string
	ApplyMode                 string
	RecreateNetdevs           bool
	KubeadmOnly               bool
	SystemExtensionPayloads   []SystemExtensionPayload
	APIProxy                  apiproxy.Config
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
	SystemExtensionSelections *[]systemextensionbundle.Selection          `yaml:"systemExtensionSelections,omitempty"`
	NodeOverrides             map[string]renderedNodeConfigurationOverlay `yaml:"nodeOverrides"`
	KubeadmConfigs            map[string]inlineKubeadmConfig              `yaml:"kubeadmConfigs,omitempty"`
	SystemExtensionPayloads   []SystemExtensionPayload                    `yaml:"systemExtensionPayloads,omitempty"`
}

type renderedNodeConfigurationOverlay struct {
	ExternalEndpoint     *string                      `yaml:"externalEndpoint,omitempty"`
	GenerationRetention  *generation.Retention        `yaml:"generationRetention"`
	Identity             *renderedNodeIdentity        `yaml:"identity,omitempty"`
	SystemRole           string                       `yaml:"systemRole,omitempty"`
	Kernel               *manifest.KernelConfig       `yaml:"kernel"`
	HostConfiguration    *manifest.HostConfiguration  `yaml:"hostConfiguration"`
	SystemExtensions     *[]manifest.SystemExtension  `yaml:"systemExtensions,omitempty"`
	Volumes              *[]manifest.Volume           `yaml:"volumes"`
	Kubernetes           *manifest.KubernetesConfig   `yaml:"kubernetes,omitempty"`
	ControlPlaneEndpoint *controlPlaneEndpointOverlay `yaml:"controlPlaneEndpoint,omitempty"`
	APIProxy             *apiproxy.Config             `yaml:"apiProxy,omitempty"`
	KubernetesMetadata   *kubernetesMetadata          `yaml:"kubernetesMetadata,omitempty"`
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
	if node.GenerationRetention == nil {
		node.GenerationRetention = &generation.Retention{}
	}
	systemExtensions := slices.Clone(node.SystemExtensions)
	volumes := slices.Clone(request.Manifest.Install.Volumes)
	kernel := manifest.KernelConfig{CommandLine: slices.Clone(node.Kernel.CommandLine)}
	kubeadmConfigs, err := renderKubeadmConfigs(node.Kubernetes.Kubeadm.ConfigRef, request.KubeadmConfigs)
	if err != nil {
		return nil, err
	}
	overlay := renderedNodeConfigurationOverlay{
		Identity: &renderedNodeIdentity{
			Hostname:       node.Identity.Hostname,
			AuthorizedKeys: slices.Clone(node.Identity.SSH.AuthorizedKeys),
		},
		SystemRole:           node.SystemRole,
		Kernel:               &kernel,
		HostConfiguration:    &node.HostConfiguration,
		GenerationRetention:  node.GenerationRetention,
		SystemExtensions:     &systemExtensions,
		Volumes:              &volumes,
		Kubernetes:           &node.Kubernetes,
		ControlPlaneEndpoint: renderedControlPlaneEndpoint(node.ControlPlaneEndpoint),
	}
	if !request.APIProxy.IsZero() {
		config, err := apiproxy.Normalize(request.APIProxy)
		if err != nil {
			return nil, fmt.Errorf("API proxy: %w", err)
		}
		overlay.APIProxy = &config
	}
	if request.KubeadmOnly {
		overlay = renderedNodeConfigurationOverlay{Kubernetes: &node.Kubernetes}
	}
	if node.Bootstrap != nil && (len(node.Bootstrap.Labels) > 0 || len(node.Bootstrap.Taints) > 0) {
		overlay.KubernetesMetadata = &kubernetesMetadata{Labels: node.Bootstrap.Labels, Taints: node.Bootstrap.Taints}
	}
	if node.Kubernetes.External && node.Bootstrap != nil {
		endpoint := node.Bootstrap.ControlPlaneEndpoint
		overlay.ExternalEndpoint = &endpoint
	}
	if request.SystemExtensionSelections != nil {
		if len(node.SystemExtensions) != 0 || len(request.SystemExtensionPayloads) != 0 {
			return nil, fmt.Errorf("extension selections cannot be rendered with resolved artifacts")
		}
		if len(*request.SystemExtensionSelections) == 0 {
			empty := []manifest.SystemExtension{}
			overlay.SystemExtensions = &empty
			request.SystemExtensionSelections = nil
		} else {
			overlay.SystemExtensions = nil
		}
	}
	document := renderedNodeConfigurationChange{
		APIVersion: NodeConfigurationChangeAPIVersion,
		Kind:       NodeConfigurationChangeKind,
		Metadata: renderedNodeConfigurationMetadata{
			SourceID:       sourceID,
			DesiredVersion: desiredVersion,
		},
		Apply: Apply{Mode: applyMode, RecreateNetdevs: request.RecreateNetdevs},
		Spec: renderedNodeConfigurationChangeSpec{
			SystemExtensionSelections: request.SystemExtensionSelections,
			KubeadmConfigs:            kubeadmConfigs,
			SystemExtensionPayloads:   slices.Clone(request.SystemExtensionPayloads),
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
		name, err := plan.PatchRelativePath(patch)
		if err != nil {
			return nil, err
		}
		if _, exists := patches[name]; exists {
			return nil, fmt.Errorf("selected kubeadm config %q contains duplicate patch name %q", ref, name)
		}
		patches[name] = string(patch.Content)
	}
	if len(patches) == 0 {
		patches = nil
	}
	return map[string]inlineKubeadmConfig{
		ref: {Config: string(plan.Config.Content), Patches: patches, NodeLocalKubelet: plan.NodeLocalKubelet, SharedKubeletBase: plan.SharedKubeletBase},
	}, nil
}

// SetRecreateNetdevs requests live netdev recreation in a rendered
// NodeConfigurationChange.
func SetRecreateNetdevs(document []byte) ([]byte, error) {
	var decoded nodeConfigurationChangeDocument
	if err := yaml.Unmarshal(document, &decoded); err != nil {
		return nil, fmt.Errorf("decode node configuration change: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("node configuration change must be a YAML mapping")
	}
	mapping := root.Content[0]
	var apply *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "apply" {
			apply = mapping.Content[i+1]
		}
	}
	if apply == nil {
		apply = &yaml.Node{Kind: yaml.MappingNode}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "apply"}, apply)
	}
	if apply.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("node configuration change apply must be a mapping")
	}
	for i := 0; i+1 < len(apply.Content); i += 2 {
		if apply.Content[i].Value == "recreateNetdevs" {
			apply.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
			return yaml.Marshal(&root)
		}
	}
	apply.Content = append(apply.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "recreateNetdevs"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	return yaml.Marshal(&root)
}
