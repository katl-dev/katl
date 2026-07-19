package configapply

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
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
	KubeadmConfigs      map[string]inlineKubeadmConfig      `json:"kubeadmConfigs,omitempty" yaml:"kubeadmConfigs,omitempty"`
}

type inlineKubeadmConfig struct {
	Config  string            `json:"config" yaml:"config"`
	Patches map[string]string `json:"patches,omitempty" yaml:"patches,omitempty"`
}

type nodeConfigurationOverlay struct {
	Identity             *IdentityOverlay             `json:"identity,omitempty" yaml:"identity,omitempty"`
	SystemRole           string                       `json:"systemRole,omitempty" yaml:"systemRole,omitempty"`
	Networkd             *manifest.NetworkdConfig     `json:"networkd,omitempty" yaml:"networkd,omitempty"`
	Sysctl               *manifest.SysctlConfig       `json:"sysctl,omitempty" yaml:"sysctl,omitempty"`
	Kubernetes           *manifest.KubernetesConfig   `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	ControlPlaneEndpoint *controlPlaneEndpointOverlay `json:"controlPlaneEndpoint,omitempty" yaml:"controlPlaneEndpoint,omitempty"`
	LivePreflight        map[string]bool              `json:"livePreflight,omitempty" yaml:"livePreflight,omitempty"`
}

type controlPlaneEndpointOverlay struct {
	Managed bool                         `json:"managed" yaml:"managed"`
	Config  *controlplaneendpoint.Config `json:"config,omitempty" yaml:"config,omitempty"`
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
	if err := validateEndpointOverlays(document.Spec); err != nil {
		return TrustedBundleRequest{}, err
	}

	request := base
	request.SourceID = document.Metadata.SourceID
	request.DesiredVersion = document.Metadata.DesiredVersion
	request.ApplyMode = document.Apply.Mode
	kubeadmConfigs, changedKubeadmConfigs, err := mergeInlineKubeadmConfigs(base.KubeadmConfigs, document.Spec.KubeadmConfigs)
	if err != nil {
		return TrustedBundleRequest{}, err
	}
	request.KubeadmConfigs = kubeadmConfigs
	request.ClusterDefaults = document.Spec.ClusterDefaults.nodeOverlay(changedKubeadmConfigs)
	request.SystemRoleOverrides = nodeOverlayMap(document.Spec.SystemRoleOverrides, changedKubeadmConfigs)
	request.NodeOverrides = nodeOverlayMap(document.Spec.NodeOverrides, changedKubeadmConfigs)
	return request, nil
}

func validateEndpointOverlays(spec nodeConfigurationChangeSpec) error {
	validate := func(path string, overlay nodeConfigurationOverlay) error {
		endpoint := overlay.ControlPlaneEndpoint
		if endpoint == nil {
			return nil
		}
		if endpoint.Managed && endpoint.Config == nil {
			return fmt.Errorf("%s.controlPlaneEndpoint.config is required when managed is true", path)
		}
		if !endpoint.Managed && endpoint.Config != nil {
			return fmt.Errorf("%s.controlPlaneEndpoint.config must be omitted when managed is false", path)
		}
		return nil
	}
	if err := validate("spec.clusterDefaults", spec.ClusterDefaults); err != nil {
		return err
	}
	for name, overlay := range spec.SystemRoleOverrides {
		if err := validate("spec.systemRoleOverrides."+name, overlay); err != nil {
			return err
		}
	}
	for name, overlay := range spec.NodeOverrides {
		if err := validate("spec.nodeOverrides."+name, overlay); err != nil {
			return err
		}
	}
	return nil
}

func ApplyNodeConfigurationChange(ctx context.Context, reader io.Reader, base TrustedBundleRequest) (TrustedBundleResult, error) {
	request, err := DecodeNodeConfigurationChange(reader, base)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	return ApplyTrustedBundle(ctx, request)
}

func nodeOverlayMap(overlays map[string]nodeConfigurationOverlay, changedConfigs map[string]struct{}) map[string]NodeOverlay {
	if len(overlays) == 0 {
		return nil
	}
	out := make(map[string]NodeOverlay, len(overlays))
	for name, overlay := range overlays {
		out[name] = overlay.nodeOverlay(changedConfigs)
	}
	return out
}

func (overlay nodeConfigurationOverlay) nodeOverlay(changedConfigs map[string]struct{}) NodeOverlay {
	kubeadmChanged := false
	if overlay.Kubernetes != nil {
		_, kubeadmChanged = changedConfigs[strings.TrimSpace(overlay.Kubernetes.Kubeadm.ConfigRef)]
	}
	nodeOverlay := NodeOverlay{
		Identity:       overlay.Identity,
		SystemRole:     overlay.SystemRole,
		Networkd:       overlay.Networkd,
		Sysctl:         overlay.Sysctl,
		Kubernetes:     overlay.Kubernetes,
		KubeadmChanged: kubeadmChanged,
		LivePreflight:  overlay.LivePreflight,
	}
	if overlay.ControlPlaneEndpoint != nil {
		nodeOverlay.ControlPlaneEndpointSet = true
		nodeOverlay.ControlPlaneEndpoint = overlay.ControlPlaneEndpoint.Config
	}
	return nodeOverlay
}

func mergeInlineKubeadmConfigs(installed map[string]kubeadmconfig.Plan, inline map[string]inlineKubeadmConfig) (map[string]kubeadmconfig.Plan, map[string]struct{}, error) {
	if len(inline) == 0 {
		return installed, nil, nil
	}
	configs := make(map[string]kubeadmconfig.Plan, len(installed)+len(inline))
	changed := make(map[string]struct{}, len(inline))
	for name, plan := range installed {
		configs[name] = plan
	}
	names := make([]string, 0, len(inline))
	for name := range inline {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input := inline[name]
		files := []kubeadmconfig.File{{
			RenderPath: "/etc/katl/kubeadm/" + name + "/config.yaml",
			Content:    []byte(input.Config),
			Mode:       0o644,
		}}
		patchNames := make([]string, 0, len(input.Patches))
		for patchName := range input.Patches {
			patchNames = append(patchNames, patchName)
		}
		sort.Strings(patchNames)
		for _, patchName := range patchNames {
			files = append(files, kubeadmconfig.File{
				RenderPath: filepath.ToSlash(filepath.Join("/etc/katl/kubeadm", name, "patches", patchName)),
				Content:    []byte(input.Patches[patchName]),
				Mode:       0o644,
			})
		}
		plan, err := kubeadmconfig.PlanFromRenderedFiles(name, files)
		if err != nil {
			return nil, nil, fmt.Errorf("spec.kubeadmConfigs.%s: %w", name, err)
		}
		previous, exists := installed[name]
		if !exists || !reflect.DeepEqual(previous.NativeEtcFiles(), plan.NativeEtcFiles()) {
			changed[name] = struct{}{}
		}
		configs[name] = plan
	}
	return configs, changed, nil
}
