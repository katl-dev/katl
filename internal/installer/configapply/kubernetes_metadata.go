package configapply

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

// kubernetesMetadata is a node's desired Kubernetes labels and taints in a
// NodeConfigurationChange. Rendering omits it when both are empty, so an
// absent value means the node has no labels or taints.
type kubernetesMetadata struct {
	Labels map[string]string    `json:"labels,omitempty" yaml:"labels,omitempty"`
	Taints []manifest.NodeTaint `json:"taints,omitempty" yaml:"taints,omitempty"`
}

// applyKubernetesMetadata records a node's desired labels and taints. The
// node's kubeadm input carries them, so they reach Kubernetes when the node
// registers. Katl does not change a registered Node; the change is recorded
// so configuration converges, and the warning names the kubectl commands that
// bring the Node to the same state.
func applyKubernetesMetadata(installManifest *manifest.Manifest, overlay NodeOverlay, registered bool, domains *domainAccumulator) {
	node := &installManifest.Node
	var currentLabels map[string]string
	var currentTaints []manifest.NodeTaint
	if node.Bootstrap != nil {
		currentLabels = node.Bootstrap.Labels
		currentTaints = node.Bootstrap.Taints
	}
	if maps.Equal(currentLabels, overlay.NodeLabels) && slices.Equal(currentTaints, overlay.NodeTaints) {
		return
	}
	// The merged manifest shares the current manifest's intent until replaced.
	var bootstrap manifest.BootstrapIntent
	if node.Bootstrap != nil {
		bootstrap = *node.Bootstrap
	}
	bootstrap.Labels = maps.Clone(overlay.NodeLabels)
	bootstrap.Taints = slices.Clone(overlay.NodeTaints)
	node.Bootstrap = &bootstrap
	domains.add(DomainKubernetesNodeMetadata)
	if registered {
		domains.kubernetesMetadataGuidance = kubectlMetadataGuidance(node.Identity.Hostname, currentLabels, overlay.NodeLabels, currentTaints, overlay.NodeTaints)
	}
}

func kubectlMetadataGuidance(nodeName string, currentLabels, desiredLabels map[string]string, currentTaints, desiredTaints []manifest.NodeTaint) string {
	var commands []string
	var labelArgs []string
	for _, key := range sortedKeys(desiredLabels) {
		if value, ok := currentLabels[key]; !ok || value != desiredLabels[key] {
			labelArgs = append(labelArgs, key+"="+desiredLabels[key])
		}
	}
	for _, key := range sortedKeys(currentLabels) {
		if _, ok := desiredLabels[key]; !ok {
			labelArgs = append(labelArgs, key+"-")
		}
	}
	if len(labelArgs) > 0 {
		commands = append(commands, fmt.Sprintf("kubectl label node %s --overwrite %s", nodeName, strings.Join(labelArgs, " ")))
	}

	// Kubernetes identifies a taint by key and effect.
	var taintArgs []string
	for _, taint := range desiredTaints {
		if !slices.Contains(currentTaints, taint) {
			taintArgs = append(taintArgs, kubectlTaint(taint))
		}
	}
	for _, taint := range currentTaints {
		if !slices.ContainsFunc(desiredTaints, func(desired manifest.NodeTaint) bool {
			return desired.Key == taint.Key && desired.Effect == taint.Effect
		}) {
			taintArgs = append(taintArgs, taint.Key+":"+taint.Effect+"-")
		}
	}
	if len(taintArgs) > 0 {
		commands = append(commands, fmt.Sprintf("kubectl taint node %s --overwrite %s", nodeName, strings.Join(taintArgs, " ")))
	}
	if len(commands) == 0 {
		return ""
	}
	return fmt.Sprintf("Kubernetes node labels and taints apply when a node joins; %s has joined, so Katl recorded the change without updating its Node. To update the Node, run: %s", nodeName, strings.Join(commands, "; "))
}

func kubectlTaint(taint manifest.NodeTaint) string {
	if taint.Value == "" {
		return taint.Key + ":" + taint.Effect
	}
	return taint.Key + "=" + taint.Value + ":" + taint.Effect
}
