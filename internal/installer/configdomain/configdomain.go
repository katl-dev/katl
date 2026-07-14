package configdomain

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type RenderRequest struct {
	Manifest                 manifest.Manifest
	KubeadmConfigs           map[string]kubeadmconfig.Plan
	KubernetesVersion        string
	KubernetesActivationPath string
	DeferKubeadmInputs       bool
}

func NativeEtcFiles(request RenderRequest) ([]confext.NativeEtcFile, error) {
	files := networkdFiles(request.Manifest.Node.Networkd)
	files = append(files, sysctlFiles(request.Manifest.Node.Sysctl)...)
	files = append(files, confext.NativeEtcFile{
		Path:    "/etc/hostname",
		Content: request.Manifest.Node.Identity.Hostname + "\n",
		Mode:    0o644,
		UID:     0,
		GID:     0,
	})
	identity, err := generation.RenderSSH(request.Manifest.Node.Identity.SSH.AuthorizedKeys)
	if err != nil {
		return nil, err
	}
	files = append(files, confext.NativeEtcFile{
		Path:    "/etc/ssh/authorized_keys/katl",
		Content: identity.AuthorizedKeys,
		Mode:    0o600,
		UID:     0,
		GID:     0,
	})
	ref := request.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
	var kubeadm *kubeadmconfig.Plan
	if ref != "" {
		config, ok := request.KubeadmConfigs[ref]
		if !ok {
			return nil, fmt.Errorf("node.kubernetes.kubeadm.configRef %q was not resolved", ref)
		}
		if config.Name != ref {
			return nil, fmt.Errorf("node.kubernetes.kubeadm.configRef %q resolved to KubeadmConfig %q", ref, config.Name)
		}
		if err := validateKubeadmIntent(request.Manifest.Node.SystemRole, config); err != nil {
			return nil, err
		}
		if request.KubernetesVersion == "" && !request.DeferKubeadmInputs {
			return nil, fmt.Errorf("node.kubernetes.kubeadm.configRef %q requires selected Kubernetes payload version", ref)
		}
		if request.KubernetesActivationPath == "" && !request.DeferKubeadmInputs {
			return nil, fmt.Errorf("node.kubernetes.kubeadm.configRef %q requires selected Kubernetes activation path", ref)
		}
		if err := validateKubeadmVersion(request.KubernetesVersion, config); err != nil {
			return nil, err
		}
		kubeadm = &config
		kubeadmFiles := config.NativeEtcFiles()
		if _, err := confext.ValidateNativeEtcBundle("", kubeadmFiles); err != nil {
			return nil, err
		}
		if !request.DeferKubeadmInputs {
			files = append(files, kubeadmFiles...)
		}
	}
	nodeMetadata, err := nodeMetadataFile(request.Manifest, kubeadm, request.KubernetesVersion, request.KubernetesActivationPath)
	if err != nil {
		return nil, err
	}
	files = append(files, nodeMetadata)
	plans, err := confext.ValidateNativeEtcBundle("", files)
	if err != nil {
		return nil, err
	}

	contentByPath := make(map[string]string, len(files))
	for _, file := range files {
		contentByPath[filepath.Clean(file.Path)] = file.Content
	}
	normalizedFiles := make([]confext.NativeEtcFile, 0, len(plans))
	for _, plan := range plans {
		normalizedFiles = append(normalizedFiles, confext.NativeEtcFile{
			Path:    plan.Path,
			Content: contentByPath[plan.Path],
			Mode:    plan.Mode,
			UID:     plan.UID,
			GID:     plan.GID,
		})
	}
	return normalizedFiles, nil
}

type nodeMetadata struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Identity   nodeMetadataIdentity   `json:"identity"`
	SystemRole string                 `json:"systemRole"`
	Kubeadm    *nodeMetadataKubeadm   `json:"kubeadm,omitempty"`
	Kubernetes nodeMetadataKubernetes `json:"kubernetes,omitempty"`
}

type nodeMetadataIdentity struct {
	Hostname string `json:"hostname"`
}

type nodeMetadataKubeadm struct {
	ConfigRef  string `json:"configRef,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
	Intent     string `json:"intent,omitempty"`
}

type nodeMetadataKubernetes struct {
	PayloadVersion string `json:"payloadVersion,omitempty"`
	ActivationPath string `json:"activationPath,omitempty"`
}

func nodeMetadataFile(installManifest manifest.Manifest, config *kubeadmconfig.Plan, kubernetesVersion string, kubernetesActivationPath string) (confext.NativeEtcFile, error) {
	metadata := nodeMetadata{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "NodeMetadata",
		Identity: nodeMetadataIdentity{
			Hostname: installManifest.Node.Identity.Hostname,
		},
		SystemRole: installManifest.Node.SystemRole,
		Kubernetes: nodeMetadataKubernetes{
			PayloadVersion: kubernetesVersion,
			ActivationPath: kubernetesActivationPath,
		},
	}
	if config != nil {
		intent, err := kubeadmIntent(*config)
		if err != nil {
			return confext.NativeEtcFile{}, err
		}
		metadata.Kubeadm = &nodeMetadataKubeadm{
			ConfigRef:  config.Name,
			ConfigPath: config.Config.RenderPath,
			Intent:     intent,
		}
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return confext.NativeEtcFile{}, fmt.Errorf("marshal node metadata: %w", err)
	}
	return confext.NativeEtcFile{
		Path:    "/etc/katl/node.json",
		Content: string(append(data, '\n')),
		Mode:    0o644,
		UID:     0,
		GID:     0,
	}, nil
}

func validateKubeadmIntent(systemRole string, config kubeadmconfig.Plan) error {
	intent, err := kubeadmIntent(config)
	if err != nil {
		return err
	}
	if systemRole != intent {
		return fmt.Errorf("node.systemRole %q requires kubeadm intent %q, got %q from KubeadmConfig %q", systemRole, systemRole, intent, config.Name)
	}
	return nil
}

func validateKubeadmVersion(kubernetesVersion string, config kubeadmconfig.Plan) error {
	if kubernetesVersion == "" {
		return nil
	}
	for _, document := range config.Documents {
		if document.KubernetesVersion != "" && document.KubernetesVersion != kubernetesVersion {
			return fmt.Errorf("KubeadmConfig %q kubernetesVersion %q does not match selected Kubernetes payload version %q", config.Name, document.KubernetesVersion, kubernetesVersion)
		}
	}
	return nil
}

func KubeadmIntent(config kubeadmconfig.Plan) (string, error) {
	return kubeadmIntent(config)
}

func kubeadmIntent(config kubeadmconfig.Plan) (string, error) {
	var intent string
	for _, document := range config.Documents {
		next := ""
		switch document.Kind {
		case "InitConfiguration", "ClusterConfiguration":
			next = "control-plane"
		case "JoinConfiguration":
			if document.ControlPlane {
				next = "control-plane"
			} else {
				next = "worker"
			}
		}
		if next == "" {
			continue
		}
		if intent != "" && intent != next {
			return "", fmt.Errorf("KubeadmConfig %q mixes %s and %s intents", config.Name, intent, next)
		}
		intent = next
	}
	if intent == "" {
		return "", fmt.Errorf("KubeadmConfig %q does not contain init or join intent", config.Name)
	}
	return intent, nil
}

func sysctlFiles(config manifest.SysctlConfig) []confext.NativeEtcFile {
	if len(config.Settings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(config.Settings))
	for key := range config.Settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Generated by Katl. Do not edit in place.\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "%s = %s\n", key, config.Settings[key])
	}
	return []confext.NativeEtcFile{{
		Path:    "/etc/sysctl.d/90-katl.conf",
		Content: b.String(),
		Mode:    0o644,
		UID:     0,
		GID:     0,
	}}
}

func networkdFiles(config manifest.NetworkdConfig) []confext.NativeEtcFile {
	files := make([]confext.NativeEtcFile, 0, len(config.Files))
	for _, file := range config.Files {
		files = append(files, confext.NativeEtcFile{
			Path:    filepath.Join("/etc/systemd/network", file.Name),
			Content: file.Content,
			Mode:    0o644,
			UID:     0,
			GID:     0,
		})
	}
	return files
}
