package bootstrapplan

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configdomain"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/operation"
)

type InputSelection struct {
	Configuration configapply.GenerationConfiguration
	Input         KubeadmInput
}

// SelectInput binds bootstrap to the desired configuration of its recorded
// source generation. Installed intent supplies identity and legacy aliases.
func SelectInput(root, generationID string, installed installer.ClusterIntent, request operation.BootstrapRequest) (InputSelection, error) {
	if err := validateIntent(installed); err != nil {
		return InputSelection{}, err
	}
	configuration, err := configapply.ReadGenerationConfiguration(root, generationID)
	if err != nil {
		return InputSelection{}, fmt.Errorf("read bootstrap source generation: %w", err)
	}
	if request.InventoryNodeName != installed.Inventory.NodeName || request.SystemRole != installed.SystemRole {
		return InputSelection{}, fmt.Errorf("bootstrap node identity does not match installed intent")
	}
	if version := strings.TrimSpace(installed.Kubernetes.PayloadVersion); version != "" && request.KubernetesPayloadVersion != version {
		return InputSelection{}, fmt.Errorf("bootstrapRequest kubernetesPayloadVersion %q does not match stored intent %q", request.KubernetesPayloadVersion, version)
	}
	if configuration.Manifest.Node.SystemRole != request.SystemRole {
		return InputSelection{}, fmt.Errorf("selected generation systemRole does not match bootstrap request")
	}
	ref := strings.TrimSpace(configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	plan, ok := configuration.KubeadmConfigs[ref]
	if !ok || ref == "" || plan.Name != ref || plan.Config.RenderPath != filepath.Join("/etc/katl/kubeadm", ref, "config.yaml") {
		return InputSelection{}, fmt.Errorf("selected generation does not own kubeadm config %q", ref)
	}
	role, err := configdomain.KubeadmIntent(plan)
	if err != nil {
		return InputSelection{}, fmt.Errorf("read selected kubeadm intent: %w", err)
	}
	if role != request.SystemRole {
		return InputSelection{}, fmt.Errorf("selected kubeadm intent %q does not match bootstrap role %q", role, request.SystemRole)
	}
	profile := strings.TrimSpace(request.BootstrapProfileRef)
	allowed := profile == ref
	if alias := installed.BootstrapProfile; alias != nil && profile == alias.Ref && alias.KubeadmConfigRef == ref {
		allowed = true
	}
	if alias := configuration.Manifest.Node.Bootstrap; alias != nil && profile == alias.BootstrapProfileRef && alias.ProfileResolvedID == "kubeadm:"+ref {
		allowed = true
	}
	if !allowed {
		return InputSelection{}, fmt.Errorf("bootstrapProfileRef %q does not select generation kubeadm config %q", profile, ref)
	}
	plan, err = kubeadmconfig.WithNodeAddress(plan, configuration.Manifest.Node.Kubernetes.Address)
	if err != nil {
		return InputSelection{}, fmt.Errorf("prepare selected bootstrap node address: %w", err)
	}
	digest, err := plan.InputDigest()
	if err != nil {
		return InputSelection{}, err
	}
	if expected := strings.TrimSpace(request.KubeadmInputDigest); expected != "" && expected != digest {
		return InputSelection{}, fmt.Errorf("bootstrapRequest kubeadmInputDigest does not match selected generation input")
	}
	configuration.KubeadmConfigs[ref] = plan
	return InputSelection{Configuration: configuration, Input: KubeadmInput{
		ConfigRef: ref, Intent: role, Path: plan.Config.RenderPath, Digest: digest,
	}}, nil
}
