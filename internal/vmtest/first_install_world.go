package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer"
)

type FirstInstallWorldRun struct {
	Scenario *WorldScenario
	Node     Node
	Runner   Runner
	Config   FirstInstallConfig
	Repo     string
}

type FirstInstallWorldInput struct {
	Installer       InstallerBootConfig
	RuntimeArtifact string
	RuntimeESP      string
	NodeMetadata    string
	InstallManifest string
	Mode            FirstInstallWorldMode
	UseInstalledESP bool
	TargetDiskSize  string
}

type FirstInstallWorldMode string

const (
	FirstInstallWorldPreseed      FirstInstallWorldMode = "preseed"
	FirstInstallWorldGuestHandoff FirstInstallWorldMode = "guest-handoff"
)

type firstInstallWorldRun = FirstInstallWorldRun
type firstInstallWorldInput = FirstInstallWorldInput
type firstInstallWorldMode = FirstInstallWorldMode

const (
	firstInstallWorldPreseed      = FirstInstallWorldPreseed
	firstInstallWorldGuestHandoff = FirstInstallWorldGuestHandoff
)

func DefaultFirstInstallWorldInputFromEnv(mode FirstInstallWorldMode, useInstalledESP bool) FirstInstallWorldInput {
	return FirstInstallWorldInput{
		Installer:       FirstInstallInstallerBootFromEnv(),
		RuntimeArtifact: strings.TrimSpace(os.Getenv("KATL_RUNTIME_ARTIFACT")),
		InstallManifest: strings.TrimSpace(os.Getenv("KATL_INSTALL_MANIFEST")),
		Mode:            mode,
		UseInstalledESP: useInstalledESP,
		TargetDiskSize:  first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "32G"),
	}
}

func loadWorldManifestPath() string {
	return strings.TrimSpace(os.Getenv(WorldManifestEnv))
}

func firstKVM(value, fallback KVMPolicy) KVMPolicy {
	if value != "" {
		return value
	}
	return fallback
}

func planFirstInstallWorldRun(world World, name, repo string, spec NodeSpec, input firstInstallWorldInput, kvm KVMPolicy) (firstInstallWorldRun, error) {
	return PlanFirstInstallWorldRun(world, name, repo, spec, input, kvm)
}

func PlanFirstInstallWorldRun(world World, name, repo string, spec NodeSpec, input FirstInstallWorldInput, kvm KVMPolicy) (FirstInstallWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return FirstInstallWorldRun{}, err
	}
	run := FirstInstallWorldRun{Scenario: scenario, Repo: repo}
	node, err := scenario.AddNode(spec)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	run.Node = node
	factory := scenario.NodeFixtures(node)
	input, err = ResolveFirstInstallWorldInput(scenario, repo, spec, input)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	installer, err := factory.InstallerBoot(input.Installer)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	runtime, err := factory.RuntimeArtifact(input.RuntimeArtifact)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	installManifest, err := factory.InstallManifest(input.InstallManifest)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	runtimeESP := ""
	if !input.UseInstalledESP {
		esp, err := factory.ESPArtifacts(input.RuntimeESP)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		runtimeESP = esp.Path
	}
	nodeMetadata := ""
	if strings.TrimSpace(input.NodeMetadata) != "" {
		metadata, err := factory.NodeMetadata(input.NodeMetadata)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		nodeMetadata = metadata.Path
	}
	target, err := factory.FirstInstallTargetDisk("root", DiskQCOW2, input.TargetDiskSize)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	installer.RuntimeArtifact = runtime.Path
	mode := input.Mode
	if mode == "" {
		mode = FirstInstallWorldPreseed
	}
	run.Runner = NewRunner(Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      KeepAlways,
		KVM:       firstKVM(kvm, KVMAuto),
		Missing:   MissingFails,
	})
	run.Config = FirstInstallConfig{
		Installer: installer,
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: runtimeESP,
			NodeMetadata: nodeMetadata,
		},
		UseInstalledESP: input.UseInstalledESP,
		ManifestPath:    installManifest.Path,
		TargetDisk:      target,
	}
	switch mode {
	case FirstInstallWorldPreseed:
		run.Config.PreseedManifest = true
	case FirstInstallWorldGuestHandoff:
		run.Config.GuestHandoff = true
	default:
		err := errors.New("unsupported first-install world mode: " + string(mode))
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	return run, nil
}

type mkosiArtifactIndex struct {
	Artifacts []mkosiArtifact `json:"artifacts"`
}

type mkosiArtifact struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	MetadataPath string `json:"metadataPath"`
	SHA256       string `json:"sha256"`
	SizeBytes    uint64 `json:"sizeBytes"`
}

type katlosImageMetadata struct {
	Version          string `json:"version"`
	Architecture     string `json:"architecture"`
	RuntimeInterface string `json:"runtimeInterface"`
	Role             string `json:"role"`
	ImageRole        string `json:"imageRole"`
	SHA256           string `json:"sha256"`
	SizeBytes        uint64 `json:"sizeBytes"`
	Components       []struct {
		Name           string `json:"name"`
		Role           string `json:"role"`
		PayloadVersion string `json:"payloadVersion"`
	} `json:"components"`
}

func resolveFirstInstallWorldInput(scenario *WorldScenario, repo string, spec NodeSpec, input firstInstallWorldInput) (firstInstallWorldInput, error) {
	return ResolveFirstInstallWorldInput(scenario, repo, spec, input)
}

func ResolveFirstInstallWorldInput(scenario *WorldScenario, repo string, spec NodeSpec, input FirstInstallWorldInput) (FirstInstallWorldInput, error) {
	index := defaultLocalMkosiArtifacts(repo)
	if indexPath := explicitMkosiArtifactIndexPath(); indexPath != "" {
		var err error
		index, err = readMkosiArtifactIndex(indexPath, repo)
		if err != nil {
			return input, err
		}
	}
	if input.Installer.InstallerUKI == "" && input.Installer.InstallerKernel == "" && input.Installer.InstallerInitrd == "" {
		if artifact, ok := index.artifact("installer-uki"); ok {
			input.Installer.InstallerUKI = artifact.Path
		}
	}
	if input.RuntimeArtifact == "" {
		if artifact, ok := index.artifact("runtime-root"); ok {
			input.RuntimeArtifact = artifact.Path
		}
	}
	if !input.UseInstalledESP && strings.TrimSpace(input.RuntimeESP) == "" {
		input.UseInstalledESP = true
	}
	if input.InstallManifest == "" {
		manifestPath, err := writeFirstInstallWorldManifestSource(scenario, repo, spec, index)
		if err != nil {
			return input, err
		}
		input.InstallManifest = manifestPath
	}
	if strings.TrimSpace(input.NodeMetadata) == "" {
		metadataPath, err := writeFirstInstallWorldNodeMetadataSource(scenario, spec)
		if err != nil {
			return input, err
		}
		input.NodeMetadata = metadataPath
	}
	return input, nil
}

func explicitMkosiArtifactIndexPath() string {
	return strings.TrimSpace(os.Getenv("KATL_MKOSI_ARTIFACT_INDEX"))
}

func defaultLocalMkosiArtifacts(repo string) mkosiArtifactIndex {
	var index mkosiArtifactIndex
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	for _, artifact := range []mkosiArtifact{
		{Kind: "installer-uki", Path: filepath.Join(mkosiDir, "katl-installer.efi")},
		{Kind: "runtime-root", Path: filepath.Join(mkosiDir, "katl-runtime-root.squashfs")},
		{Kind: "kubernetes-sysext", Path: filepath.Join(mkosiDir, "katl-kubernetes.raw"), MetadataPath: filepath.Join(mkosiDir, "katl-kubernetes.raw.json")},
	} {
		if _, err := os.Stat(artifact.Path); err == nil {
			index.Artifacts = append(index.Artifacts, artifact)
		}
	}
	if artifact, err := discoverKatlOSInstallImage(repo); err == nil {
		index.Artifacts = append(index.Artifacts, artifact)
	}
	return index
}

func readMkosiArtifactIndex(path, repo string) (mkosiArtifactIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mkosiArtifactIndex{}, err
	}
	var index mkosiArtifactIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return mkosiArtifactIndex{}, err
	}
	for i := range index.Artifacts {
		index.Artifacts[i].Path = repoAbs(repo, index.Artifacts[i].Path)
		index.Artifacts[i].MetadataPath = repoAbs(repo, index.Artifacts[i].MetadataPath)
	}
	return index, nil
}

func (index mkosiArtifactIndex) artifact(kind string) (mkosiArtifact, bool) {
	for _, artifact := range index.Artifacts {
		if artifact.Kind == kind {
			return artifact, true
		}
	}
	return mkosiArtifact{}, false
}

func writeFirstInstallWorldManifestSource(scenario *WorldScenario, repo string, spec NodeSpec, index mkosiArtifactIndex) (string, error) {
	image, ok := index.artifact("katlos-install-image")
	if !ok {
		var err error
		image, err = discoverKatlOSInstallImage(repo)
		if err != nil {
			return "", err
		}
	}
	metadata, err := readKatlOSImageMetadata(image)
	if err != nil {
		return "", err
	}
	if metadata.Role == "" {
		metadata.Role = metadata.ImageRole
	}
	if metadata.Role == "" {
		metadata.Role = "install"
	}
	if metadata.SHA256 == "" {
		metadata.SHA256 = image.SHA256
	}
	if metadata.SizeBytes == 0 {
		metadata.SizeBytes = image.SizeBytes
	}
	sourceDir := filepath.Join(scenario.Dir, "inputs", "install-manifest-source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return "", err
	}
	localRef := filepath.Base(image.Path)
	localImage := filepath.Join(sourceDir, localRef)
	if _, err := os.Lstat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(image.Path, localImage); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	kubernetesVersion := metadata.KubernetesPayloadVersion()
	if kubernetesVersion == "" {
		if artifact, ok := index.artifact("kubernetes-sysext"); ok {
			var err error
			kubernetesVersion, err = readKubernetesSysextPayloadVersion(artifact)
			if err != nil {
				return "", err
			}
		}
	}
	kubeadmRef, err := writeFirstInstallWorldKubeadmSource(sourceDir, spec, kubernetesVersion)
	if err != nil {
		return "", err
	}
	node := map[string]any{
		"identity": map[string]any{
			"hostname": spec.Name,
			"ssh": map[string]any{
				"authorizedKeys": []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"},
			},
		},
		"networkd": map[string]any{
			"files": []map[string]any{
				{
					"name":    "80-katl-vmtest-dhcp.network",
					"content": "[Match]\nName=*\n\n[Network]\nDHCP=yes\n",
				},
			},
		},
		"systemRole": string(spec.Role),
	}
	if kubeadmRef != "" {
		node["kubernetes"] = map[string]any{
			"kubeadm": map[string]any{
				"configRef": kubeadmRef,
			},
		}
	}
	manifest := map[string]any{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind":       "InstallManifest",
		"node":       node,
		"install": map[string]any{
			"allowDestructiveInstall": true,
			"targetDisk": map[string]any{
				"byID":       "/dev/disk/by-id/virtio-katl-root",
				"minSizeMiB": 32,
			},
		},
		"katlosImage": map[string]any{
			"localRef":         localRef,
			"sha256":           metadata.SHA256,
			"sizeBytes":        metadata.SizeBytes,
			"version":          metadata.Version,
			"architecture":     metadata.Architecture,
			"runtimeInterface": metadata.RuntimeInterface,
			"role":             metadata.Role,
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(sourceDir, "install-manifest.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return manifestPath, nil
}

func (metadata katlosImageMetadata) KubernetesPayloadVersion() string {
	for _, component := range metadata.Components {
		if component.Name == "kubernetes" || component.Role == "kubernetes-sysext" {
			return strings.TrimSpace(component.PayloadVersion)
		}
	}
	return ""
}

func readKubernetesSysextPayloadVersion(artifact mkosiArtifact) (string, error) {
	metadataPath := strings.TrimSpace(artifact.MetadataPath)
	if metadataPath == "" {
		metadataPath = artifact.Path + ".json"
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes sysext metadata: %w", err)
	}
	var metadata struct {
		PayloadVersion string `json:"payloadVersion"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode Kubernetes sysext metadata: %w", err)
	}
	return strings.TrimSpace(metadata.PayloadVersion), nil
}

func writeFirstInstallWorldKubeadmSource(sourceDir string, spec NodeSpec, kubernetesVersion string) (string, error) {
	switch spec.Role {
	case ControlPlane:
		return writeFirstInstallWorldKubeadmPlan(sourceDir, "control-plane", controlPlaneKubeadmConfig(spec.Name, kubernetesVersion))
	case Worker:
		return writeFirstInstallWorldKubeadmPlan(sourceDir, "worker", workerKubeadmConfig(spec.Name))
	default:
		return "", nil
	}
}

func writeFirstInstallWorldKubeadmPlan(sourceDir, name, config string) (string, error) {
	configRel := filepath.ToSlash(filepath.Join(installer.KubeadmConfigFilesDir, name+".yaml"))
	objectDir := filepath.Join(sourceDir, installer.KubeadmConfigObjectsDir)
	if err := os.MkdirAll(objectDir, 0o755); err != nil {
		return "", err
	}
	configPath := filepath.Join(sourceDir, filepath.FromSlash(configRel))
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return "", err
	}
	object := fmt.Sprintf(`apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: %s
spec:
  configFile: %s
`, name, configRel)
	if err := os.WriteFile(filepath.Join(objectDir, name+".yaml"), []byte(object), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func controlPlaneKubeadmConfig(nodeName string, kubernetesVersion string) string {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		nodeName = "cp-1"
	}
	kubernetesVersion = strings.TrimSpace(kubernetesVersion)
	if kubernetesVersion == "" {
		kubernetesVersion = "v1.36.1"
	}
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  name: ` + nodeName + `
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl-smoke
kubernetesVersion: ` + kubernetesVersion + `
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`
}

func workerKubeadmConfig(nodeName string) string {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		nodeName = "worker-1"
	}
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  name: ` + nodeName + `
  criSocket: unix:///run/containerd/containerd.sock
`
}

func writeFirstInstallWorldNodeMetadataSource(scenario *WorldScenario, spec NodeSpec) (string, error) {
	sourceDir := filepath.Join(scenario.Dir, "inputs", "node-metadata-source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return "", err
	}
	metadata := map[string]any{
		"apiVersion": "katl.dev/v1alpha1",
		"kind":       "NodeMetadata",
		"identity": map[string]any{
			"hostname": spec.Name,
		},
		"systemRole": string(spec.Role),
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", err
	}
	metadataPath := filepath.Join(sourceDir, "node.json")
	if err := os.WriteFile(metadataPath, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return metadataPath, nil
}

func discoverKatlOSInstallImage(repo string) (mkosiArtifact, error) {
	matches, err := filepath.Glob(filepath.Join(repo, "_build", "mkosi", "katlos-install-*.squashfs"))
	if err != nil {
		return mkosiArtifact{}, err
	}
	if len(matches) != 1 {
		return mkosiArtifact{}, fmt.Errorf("install manifest source is required: expected one local KatlOS install image, found %d", len(matches))
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		return mkosiArtifact{}, err
	}
	return mkosiArtifact{Kind: "katlos-install-image", Path: matches[0], MetadataPath: matches[0] + ".json", SizeBytes: uint64(info.Size())}, nil
}

func readKatlOSImageMetadata(image mkosiArtifact) (katlosImageMetadata, error) {
	if strings.TrimSpace(image.MetadataPath) == "" {
		return katlosImageMetadata{}, errors.New("KatlOS install image metadata is required")
	}
	data, err := os.ReadFile(image.MetadataPath)
	if err != nil {
		return katlosImageMetadata{}, err
	}
	var metadata katlosImageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return katlosImageMetadata{}, err
	}
	return metadata, nil
}

func repoAbs(repo, path string) string {
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repo, path)
}

func firstInstallInstallerBootFromEnv() InstallerBootConfig {
	return FirstInstallInstallerBootFromEnv()
}

func FirstInstallInstallerBootFromEnv() InstallerBootConfig {
	kernel := strings.TrimSpace(os.Getenv("KATL_INSTALLER_KERNEL"))
	initrd := strings.TrimSpace(os.Getenv("KATL_INSTALLER_INITRD"))
	if kernel != "" || initrd != "" {
		return InstallerBootConfig{
			InstallerKernel: kernel,
			InstallerInitrd: initrd,
			CommandLine: []string{
				"console=ttyS0,115200n8",
				"systemd.log_target=console",
				"loglevel=6",
			},
		}
	}
	return InstallerBootConfig{InstallerUKI: strings.TrimSpace(os.Getenv("KATL_INSTALLER_UKI"))}
}
