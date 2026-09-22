package configbundle

import (
	"fmt"

	"github.com/katl-dev/katl/internal/installer/clusterplan"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

// ConfigurationPlan renders host intent without selecting extension artifacts.
// The node resolves selections against its current or upgrade-target runtime.
type ConfigurationPlan struct {
	ClusterName               string
	Plan                      clusterplan.Plan
	KubeadmConfigs            map[string]kubeadmconfig.Plan
	SystemExtensionSelections map[string][]systemextensionbundle.Selection
}

func PlanConfiguration(request BuildRequest) (ConfigurationPlan, error) {
	compiled, err := compileSource(request, true)
	if err != nil {
		return ConfigurationPlan{}, err
	}
	return ConfigurationPlan{
		ClusterName:               compiled.source.Metadata.Name,
		Plan:                      compiled.plan,
		KubeadmConfigs:            compiled.kubeadmConfigs,
		SystemExtensionSelections: compiled.selections,
	}, nil
}

func configurationSelections(source SourceConfig) (SourceConfig, map[string][]systemextensionbundle.Selection, error) {
	selections := make(map[string][]systemextensionbundle.Selection, len(source.Spec.Nodes))
	for i := range source.Spec.Nodes {
		node := &source.Spec.Nodes[i]
		entries, err := mergeSourceSystemExtensions(source.Spec.Defaults.SystemExtensions, node.SystemExtensions)
		if err != nil {
			return SourceConfig{}, nil, fmt.Errorf("node %q: %w", node.Name, err)
		}
		desired := lowerSystemExtensions(entries)
		if err := manifest.ValidateSystemExtensions(desired, true); err != nil {
			return SourceConfig{}, nil, fmt.Errorf("node %q: %w", node.Name, err)
		}
		selections[node.Name] = make([]systemextensionbundle.Selection, 0, len(desired))
		for _, selection := range desired {
			selections[node.Name] = append(selections[node.Name], systemextensionbundle.Selection{
				Release:       selection.Release,
				Bundle:        selection.Bundle,
				State:         selection.State,
				Configuration: selection.Configuration,
				Units:         selection.Units,
			})
		}
		node.SystemExtensions = Optional[[]SourceSystemExtension]{}
	}
	source.Spec.Defaults.SystemExtensions = Optional[[]SourceSystemExtension]{}
	return source, selections, nil
}
