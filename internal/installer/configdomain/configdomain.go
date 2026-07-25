package configdomain

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
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
	files = append(files, confext.NativeEtcFile{
		Path:    "/etc/hostname",
		Content: request.Manifest.Node.Identity.Hostname + "\n",
		Mode:    0o644,
		UID:     0,
		GID:     0,
	})
	if endpoint := request.Manifest.Node.ControlPlaneEndpoint; endpoint != nil {
		endpointPlan, err := controlplaneendpoint.Normalize(*endpoint)
		if err != nil {
			return nil, fmt.Errorf("node.controlPlaneEndpoint: %w", err)
		}
		config, err := bgpapivip.FromControlPlaneEndpoint(endpointPlan)
		if err != nil {
			return nil, err
		}
		app, err := bgpapivip.RenderNativeEtcFiles(bgpapivip.RenderRequest{
			Config:   config,
			NodeRole: request.Manifest.Node.SystemRole,
		})
		if err != nil {
			return nil, err
		}
		files = append(files, app.NativeEtcFiles()...)
	}
	identity, err := generation.RenderSSH(request.Manifest.Node.Identity.SSH.AuthorizedKeys)
	if err != nil {
		return nil, err
	}
	for _, account := range []string{"katl", "root"} {
		files = append(files, confext.NativeEtcFile{
			Path:    "/etc/ssh/authorized_keys/" + account,
			Content: identity.AuthorizedKeys,
			// Public keys are not secrets. Keep the Katl-owned files immutable
			// to both accounts while allowing sshd to read them after dropping
			// privileges for the unprivileged operator account.
			Mode: 0o644,
			UID:  0,
			GID:  0,
		})
	}
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
	hostFiles, err := hostConfigurationFiles(request.Manifest.Node.HostConfiguration)
	if err != nil {
		return nil, err
	}
	files = append(files, hostFiles...)
	extensionFiles, err := systemExtensionFiles(request.Manifest.Node.SystemExtensions)
	if err != nil {
		return nil, err
	}
	files = append(files, extensionFiles...)
	plans, err := confext.ValidateNativeEtcBundle("", files)
	if err != nil {
		return nil, err
	}

	fileByPath := make(map[string]confext.NativeEtcFile, len(files))
	for _, file := range files {
		fileByPath[filepath.Clean(file.Path)] = file
	}
	normalizedFiles := make([]confext.NativeEtcFile, 0, len(plans))
	for _, plan := range plans {
		source := fileByPath[plan.Path]
		normalizedFiles = append(normalizedFiles, confext.NativeEtcFile{
			Path:    plan.Path,
			Content: source.Content,
			Type:    source.Type,
			Mode:    plan.Mode,
			UID:     plan.UID,
			GID:     plan.GID,
		})
	}
	return normalizedFiles, nil
}

func systemExtensionFiles(extensions []manifest.SystemExtension) ([]confext.NativeEtcFile, error) {
	if err := manifest.ValidateSystemExtensions(extensions, false); err != nil {
		return nil, fmt.Errorf("node.systemExtensions: %w", err)
	}
	var files []confext.NativeEtcFile
	var required []string
	unitActivation := make(map[string]bool)
	for _, extension := range extensions {
		for _, file := range extension.Configuration.Files {
			if file.Content == nil {
				return nil, fmt.Errorf("node.systemExtensions %q path %q has no embedded content", extension.Name, file.Path)
			}
			mode := file.Mode
			if mode == 0 {
				mode = 0o644
			}
			files = append(files, confext.NativeEtcFile{
				Path: file.Path, Content: *file.Content, Mode: fs.FileMode(mode), UID: 0, GID: 0,
			})
		}
		for _, unit := range extension.Units {
			if unit.Enable {
				files = append(files, confext.NativeEtcFile{
					Path:    filepath.ToSlash(filepath.Join("/etc/systemd/system/multi-user.target.wants", unit.Name)),
					Content: "/usr/lib/systemd/system/" + unit.Name,
					Type:    confext.NativeEtcSymlink,
				})
				if _, exists := unitActivation[unit.Name]; !exists {
					unitActivation[unit.Name] = false
				}
			}
			if unit.RequiredForBootHealth {
				required = append(required, unit.Name)
				unitActivation[unit.Name] = true
			}
			for _, dropIn := range unit.DropIns {
				if dropIn.Content == nil {
					return nil, fmt.Errorf("node.systemExtensions %q unit %q drop-in %q has no embedded content", extension.Name, unit.Name, dropIn.Name)
				}
				files = append(files, confext.NativeEtcFile{
					Path:    filepath.ToSlash(filepath.Join("/etc/systemd/system", unit.Name+".d", dropIn.Name)),
					Content: *dropIn.Content,
					Mode:    0o644,
					UID:     0,
					GID:     0,
				})
			}
		}
	}
	sort.Strings(required)
	unitNames := make([]string, 0, len(unitActivation))
	for name := range unitActivation {
		unitNames = append(unitNames, name)
	}
	sort.Strings(unitNames)
	if len(unitNames) > 0 {
		var activation strings.Builder
		activation.WriteString("[Service]\n")
		for _, name := range unitNames {
			activation.WriteString("ExecStart=")
			if !unitActivation[name] {
				activation.WriteByte('-')
			}
			activation.WriteString("/usr/bin/systemctl start ")
			activation.WriteString(name)
			activation.WriteByte('\n')
		}
		files = append(files, confext.NativeEtcFile{
			Path:    "/etc/systemd/system/katl-system-extensions-activate.service.d/50-units.conf",
			Content: activation.String(),
			Mode:    0o644,
		})
	}
	if len(required) > 0 {
		files = append(files, confext.NativeEtcFile{
			Path:    "/etc/systemd/system/katl-boot-health.service.d/50-system-extensions.conf",
			Content: "[Unit]\nRequires=" + strings.Join(required, " ") + "\nAfter=" + strings.Join(required, " ") + "\n",
			Mode:    0o644,
		})
	}
	return files, nil
}

func hostConfigurationFiles(config manifest.HostConfiguration) ([]confext.NativeEtcFile, error) {
	if err := manifest.ValidateHostConfiguration(config, false); err != nil {
		return nil, fmt.Errorf("node.hostConfiguration: %w", err)
	}
	setNames := make([]string, 0, len(config.Sets))
	for name := range config.Sets {
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	var files []confext.NativeEtcFile
	for _, setName := range setNames {
		set := config.Sets[setName]
		if set.State == manifest.HostConfigurationAbsent {
			continue
		}
		for _, file := range set.Files {
			if file.Content == nil {
				return nil, fmt.Errorf("node.hostConfiguration set %q path %q has no embedded content", setName, file.Path)
			}
			mode := file.Mode
			if mode == 0 {
				mode = 0o644
			}
			files = append(files, confext.NativeEtcFile{
				Path:    file.Path,
				Content: *file.Content,
				Mode:    fs.FileMode(mode),
				UID:     0,
				GID:     0,
			})
		}
	}
	return files, nil
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
