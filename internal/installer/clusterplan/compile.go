package clusterplan

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/apiproxy"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/configdomain"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
)

const validationActivationPath = "/run/extensions/katl-kubernetes.raw"

type selectedKubernetes struct {
	version        string
	catalogRef     string
	bundleSource   string
	bundleRef      string
	sysext         *generation.ExtensionRef
	activationPath string
}

func Compile(request CompileRequest) (Plan, error) {
	config := request.Config
	if config.APIVersion != APIVersion {
		return Plan{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if config.Kind != Kind {
		return Plan{}, fmt.Errorf("kind must be %s", Kind)
	}
	if strings.TrimSpace(config.Metadata.Name) == "" {
		return Plan{}, fmt.Errorf("metadata.name is required")
	}
	if err := validateWipeTarget(config.Spec); err != nil {
		return Plan{}, err
	}
	if err := validateClusterImage(config.Spec.KatlosImage); err != nil {
		return Plan{}, err
	}
	if err := validateSharedLayer("spec.defaults", config.Spec.Defaults); err != nil {
		return Plan{}, err
	}
	kubernetes, err := selectKubernetes(config.Spec.Kubernetes, config.Spec.KatlosImage, request)
	if err != nil {
		return Plan{}, err
	}
	if len(config.Spec.Nodes) == 0 {
		return Plan{}, fmt.Errorf("spec.nodes must not be empty")
	}
	topology, err := LowerTopology(config, request.AddressOverrides)
	if err != nil {
		return Plan{}, err
	}
	endpointPlan := topology.Endpoint
	if endpointPlan != nil && endpointPlan.Config.Advertisement != nil {
		for _, plan := range request.KubeadmConfigs {
			if err := kubeadmconfig.ValidateManagedEndpoint(plan, endpointPlan.Config.Advertisement.VIP, endpointPlan.Config.Port); err != nil {
				return Plan{}, err
			}
		}
	}

	nodes := slices.Clone(config.Spec.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	materials := make([]NodeMaterial, 0, len(nodes))
	inventoryNodes := make([]inventory.Node, 0, len(nodes))
	for i, node := range nodes {
		located := topology.Inventory.Nodes[i]
		layer, err := mergedLayer(config.Spec.Defaults, node.Overrides)
		if err != nil {
			return Plan{}, fmt.Errorf("node %q: %w", located.Name, err)
		}
		layer.Bootstrap.Access = portableBootstrapAccess(layer.Bootstrap.Access)
		layer = applyTargetDiskDefaults(layer)
		material, invNode, err := compileNode(config, located, layer, kubernetes, request.KubeadmConfigs, endpointPlan, request.ManagementIdentities[located.Name])
		if err != nil {
			return Plan{}, err
		}
		materials = append(materials, material)
		inventoryNodes = append(inventoryNodes, invNode)
	}
	if config.Spec.Kubernetes.ExternalEndpoint == "" {
		if err := attachAPIProxyConfigs(materials, inventoryNodes, topology.Inventory.ControlPlaneEndpoint); err != nil {
			return Plan{}, err
		}
	}
	bootstrapInventory := topology.Inventory
	bootstrapInventory.KubernetesVersion = kubernetes.version
	bootstrapInventory.KubernetesBundleSource = kubernetes.bundleSource
	bootstrapInventory.KubernetesBundleRef = kubernetes.bundleRef
	bootstrapInventory.Nodes = inventoryNodes
	if err := validateBootstrapInventory(bootstrapInventory); err != nil {
		return Plan{}, err
	}
	return Plan{
		ControlPlaneEndpoint:   bootstrapInventory.ControlPlaneEndpoint.String(),
		EndpointPlan:           endpointPlan,
		KubernetesVersion:      kubernetes.version,
		KubernetesCatalogRef:   kubernetes.catalogRef,
		KubernetesBundleSource: kubernetes.bundleSource,
		KubernetesBundleRef:    kubernetes.bundleRef,
		KubernetesSysext:       kubernetes.sysext,
		KatlosImage:            config.Spec.KatlosImage,
		Nodes:                  materials,
		BootstrapInventory:     bootstrapInventory,
		AddressOverrides:       topology.AddressOverrides,
	}, nil
}

func attachAPIProxyConfigs(materials []NodeMaterial, nodes []inventory.Node, endpoint inventory.HostPort) error {
	if endpoint.IsZero() {
		return fmt.Errorf("control-plane endpoint is required for API proxy configuration")
	}
	if len(materials) != len(nodes) {
		return fmt.Errorf("API proxy node materials do not match bootstrap inventory")
	}
	for i := range materials {
		config, known, err := inventory.APIProxyConfig(nodes, nodes[i].Name, endpoint)
		if err != nil {
			return err
		}
		if !known {
			// When an address is unknown at install time, the bootstrap candidate
			// generation installs the complete proxy config.
			return nil
		}
		content, err := apiproxy.Render(config)
		if err != nil {
			return fmt.Errorf("node %q API proxy: %w", nodes[i].Name, err)
		}
		materials[i].APIProxy = config
		materials[i].NativeEtcFiles = append(materials[i].NativeEtcFiles, confext.NativeEtcFile{
			Path: apiproxy.ConfigPath, Content: content, Mode: 0o644, UID: 0, GID: 0,
		})
		if _, err := confext.ValidateNativeEtcBundle("", materials[i].NativeEtcFiles); err != nil {
			return fmt.Errorf("node %q native /etc files: %w", nodes[i].Name, err)
		}
	}
	return nil
}

func compileNode(config Config, located inventory.Node, layer NodeLayer, kubernetes selectedKubernetes, kubeadmConfigs map[string]kubeadmconfig.Plan, endpointPlan *controlplaneendpoint.Plan, management manifest.ManagementIdentity) (NodeMaterial, inventory.Node, error) {
	name, role := located.Name, located.SystemRole
	hostname := strings.TrimSpace(layer.Hostname)
	if hostname == "" {
		hostname = name
	}
	if layer.Install.TargetDisk == nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q install.targetDisk is required", name)
	}
	kubeadmRef := strings.TrimSpace(layer.Kubernetes.KubeadmConfigRef)
	bootstrapProfileResolvedID := ""
	if kubeadmRef != "" {
		bootstrapProfileResolvedID = "kubeadm:" + kubeadmRef
	}
	publicBundle := ""
	if _, err := kubernetesbundle.ParseImageReference(kubernetes.bundleRef); err == nil {
		publicBundle = kubernetes.bundleRef
	}
	controlPlaneEndpoint := ""
	var managedEndpoint *controlplaneendpoint.Config
	if endpointPlan != nil {
		controlPlaneEndpoint = endpointPlan.Endpoint
		if role == inventory.RoleControlPlane && endpointPlan.Config.Advertisement != nil {
			config := endpointPlan.Config
			managedEndpoint = &config
		}
	}
	installManifest := manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname:   hostname,
				SSH:        layer.SSH,
				Management: management,
			},
			SystemRole:           string(role),
			Kernel:               kernelConfig(layer.Kernel),
			HostConfiguration:    layer.HostConfiguration,
			GenerationRetention:  layer.GenerationRetention,
			SystemExtensions:     slices.Clone(layer.SystemExtensions),
			ControlPlaneEndpoint: managedEndpoint,
			Kubernetes: manifest.KubernetesConfig{
				External: config.Spec.Kubernetes.ExternalEndpoint != "",
				Address:  kubernetesAddressText(located.KubernetesAddress),
				Kubeadm:  manifest.KubeadmReference{ConfigRef: layer.Kubernetes.KubeadmConfigRef},
			},
			Bootstrap: &manifest.BootstrapIntent{
				ClusterName:          strings.TrimSpace(config.Metadata.Name),
				InventoryNodeName:    name,
				NodeAddress:          located.Address.String(),
				ControlPlaneEndpoint: controlPlaneEndpoint,
				BootstrapProfileRef:  kubeadmRef,
				ProfileResolvedID:    bootstrapProfileResolvedID,
				KubernetesCatalogRef: kubernetes.catalogRef,
				KubernetesBundle:     publicBundle,
				KubernetesVersion:    kubernetes.version,
				Access:               manifestAccess(layer.Bootstrap.Access),
				Labels:               copyLabels(layer.Kubernetes.NodeLabels),
				Taints:               slices.Clone(layer.Kubernetes.NodeTaints),
			},
		},
		Install: manifest.InstallConfig{
			WipeTarget: config.Spec.WipeTarget,
			TargetDisk: *layer.Install.TargetDisk,
			Volumes:    slices.Clone(layer.Install.Volumes),
		},
		KatlosImage: config.Spec.KatlosImage,
	}
	if err := manifest.ValidateWithOptions(installManifest, manifest.ValidateOptions{AllowMissingKatlosImage: true}); err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q manifest: %w", name, err)
	}
	kubeadmConfig := inventory.KubeadmConfig{}
	var kubeadmPlan *kubeadmconfig.Plan
	var kubeadmInput kubeadmconfig.NodeInput
	if kubeadmRef != "" {
		configPlan, ok := kubeadmConfigs[kubeadmRef]
		if !ok {
			return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q kubeadm config ref %q was not resolved", name, kubeadmRef)
		}
		kubeadmPlan = &configPlan
		var err error
		kubeadmInput, err = kubeadmconfig.ForNode(configPlan, installManifest.Node)
		if err != nil {
			return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q kubeadm config %q: %w", name, kubeadmRef, err)
		}
		kubeadmConfig = inventory.KubeadmConfig{
			Ref:              kubeadmRef,
			Path:             configPlan.Config.RenderPath,
			Intent:           inventory.KubeadmIntent(role),
			NodeLocalKubelet: configPlan.NodeLocalKubelet,
		}
	}
	nativeEtcFiles, err := configdomain.NativeEtcFiles(configdomain.RenderRequest{
		Manifest:                 installManifest,
		Kubeadm:                  kubeadmInput,
		KubernetesVersion:        kubernetes.version,
		KubernetesActivationPath: kubernetes.activationPath,
		DeferKubeadmInputs:       true,
	})
	if err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q config domains: %w", name, err)
	}
	if _, err := confext.ValidateNativeEtcBundle("", nativeEtcFiles); err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q native /etc files: %w", name, err)
	}
	if err := rejectHostSpecificMaterials(installManifest, nativeEtcFiles, kubeadmConfig, kubernetes.sysext); err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q: %w", name, err)
	}
	if err := rejectHostSpecificKubeadmPlan(kubeadmPlan); err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q: %w", name, err)
	}
	invNode := inventory.Node{
		Name:              name,
		Address:           located.Address,
		KubernetesAddress: located.KubernetesAddress,
		SystemRole:        role,
		Access:            layer.Bootstrap.Access,
		KubeadmConfig:     kubeadmConfig,
		KubernetesVersion: kubernetes.version,
	}
	return NodeMaterial{
		Name:                   name,
		SystemRole:             role,
		BootstrapAddress:       invNode.Address.String(),
		InstallManifest:        installManifest,
		NativeEtcFiles:         nativeEtcFiles,
		KubeadmConfig:          kubeadmConfig,
		NodeLabels:             copyLabels(layer.Kubernetes.NodeLabels),
		NodeTaints:             slices.Clone(layer.Kubernetes.NodeTaints),
		KubernetesVersion:      kubernetes.version,
		KubernetesCatalogRef:   kubernetes.catalogRef,
		KubernetesBundleSource: kubernetes.bundleSource,
		KubernetesBundleRef:    kubernetes.bundleRef,
		KubernetesSysext:       kubernetes.sysext,
	}, invNode, nil
}

func kernelConfig(config *manifest.KernelConfig) manifest.KernelConfig {
	if config == nil {
		return manifest.KernelConfig{}
	}
	return manifest.KernelConfig{CommandLine: slices.Clone(config.CommandLine)}
}

func manifestAccess(access inventory.Access) manifest.BootstrapAccess {
	return manifest.BootstrapAccess{
		Method:        strings.TrimSpace(access.Method),
		User:          strings.TrimSpace(access.User),
		CredentialRef: strings.TrimSpace(access.CredentialRef),
	}
}

func portableBootstrapAccess(access inventory.Access) inventory.Access {
	access.Method = strings.TrimSpace(access.Method)
	access.User = strings.TrimSpace(access.User)
	access.CredentialRef = strings.TrimSpace(access.CredentialRef)
	if access.Method == "" && access.User == "" && access.CredentialRef == "" {
		return inventory.Access{Method: "agent"}
	}
	if access.Method == "agent" && !strings.HasPrefix(access.CredentialRef, "vsock:") {
		access.CredentialRef = ""
	}
	return access
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	return maps.Clone(labels)
}

func validateSharedLayer(path string, layer NodeLayer) error {
	if layer.Install.TargetDisk != nil {
		return fmt.Errorf("%s.install.targetDisk is not allowed; target disk identity must be set per node", path)
	}
	if strings.TrimSpace(layer.Kubernetes.Address) != "" {
		return fmt.Errorf("%s.kubernetes.address is not allowed; Kubernetes address must be set per node", path)
	}
	if layer.Install.TargetDiskDefaults != nil {
		if err := validateTargetDiskDefaults(*layer.Install.TargetDiskDefaults); err != nil {
			return fmt.Errorf("%s.install.%w", path, err)
		}
	}
	return nil
}

func validateTargetDiskDefaults(selector manifest.DiskSelector) error {
	for _, value := range []string{selector.ByID, selector.WWN, selector.Serial} {
		if strings.TrimSpace(value) != "" {
			return fmt.Errorf("targetDiskDefaults must not set byID, wwn, or serial")
		}
	}
	return nil
}

func selectKubernetes(selection KubernetesSelection, image manifest.KatlosImage, request CompileRequest) (selectedKubernetes, error) {
	version := strings.TrimSpace(selection.PayloadVersion)
	catalogRef := strings.TrimSpace(selection.CatalogRef)
	bundleSource := strings.TrimSpace(selection.BundleSource)
	bundleRef := strings.TrimSpace(selection.BundleRef)
	if (bundleSource == "") != (bundleRef == "") {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.bundleSource and bundleRef must be set together")
	}
	selectors := 0
	for _, value := range []string{catalogRef, bundleRef} {
		if value != "" {
			selectors++
		}
	}
	if version != "" && bundleRef == "" {
		selectors++
	}
	if selectors == 0 {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.payloadVersion, catalogRef, or bundleRef is required")
	}
	if selectors > 1 {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes must set exactly one of payloadVersion, catalogRef, or bundleRef")
	}
	if version != "" && bundleRef == "" {
		if sysextcatalog.KubernetesMinor(version) == "" {
			return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.payloadVersion %q must be vMAJOR.MINOR.PATCH", version)
		}
		return selectedKubernetes{
			version:        version,
			activationPath: validationActivationPath,
		}, nil
	}
	if bundleRef != "" {
		payloadVersion := version
		if payloadVersion == "" {
			var err error
			payloadVersion, err = kubernetesbundle.PayloadVersionFromRef(bundleRef)
			if err != nil {
				return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.bundleRef %q: %w", bundleRef, err)
			}
		}
		if sysextcatalog.KubernetesMinor(payloadVersion) == "" {
			return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.payloadVersion is required with a bundle reference")
		}
		return selectedKubernetes{
			version:        payloadVersion,
			bundleSource:   bundleSource,
			bundleRef:      bundleRef,
			activationPath: validationActivationPath,
		}, nil
	}
	if manifest.KatlosImageEmpty(image) {
		return selectedKubernetes{}, fmt.Errorf("spec.katlosImage is required when spec.kubernetes.catalogRef selects an architecture-specific payload")
	}

	ref, err := sysextcatalog.Select(sysextcatalog.SelectionRequest{
		Catalog: request.KubernetesCatalog,
		Version: catalogRef,
		Runtime: sysextcatalog.Runtime{
			Interface:    strings.TrimSpace(image.RuntimeInterface),
			Architecture: strings.TrimSpace(image.Architecture),
		},
		ArtifactBasePath: strings.TrimSpace(request.KubernetesArtifactBasePath),
		ActivationPath:   strings.TrimSpace(request.KubernetesActivationPath),
	})
	if err != nil {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.catalogRef %q: %w", catalogRef, err)
	}
	return selectedKubernetes{
		version:        ref.PayloadVersion,
		catalogRef:     catalogRef,
		sysext:         &ref,
		activationPath: ref.ActivationPath,
	}, nil
}

func validateClusterImage(image manifest.KatlosImage) error {
	if manifest.KatlosImageEmpty(image) {
		return nil
	}
	testManifest := manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname: "image-validation",
				SSH: manifest.SSHIdentity{AuthorizedKeys: []string{
					"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example",
				}},
			},
			SystemRole: string(inventory.RoleControlPlane),
		},
		Install: manifest.InstallConfig{
			WipeTarget: true,
			TargetDisk: manifest.DiskSelector{ByID: "/dev/disk/by-id/katl-image-validation"},
		},
		KatlosImage: image,
	}
	if err := manifest.Validate(testManifest); err != nil {
		return fmt.Errorf("spec.katlosImage: %w", err)
	}
	return nil
}

func validateWipeTarget(spec Spec) error {
	if !spec.WipeTarget {
		return fmt.Errorf("spec.wipeTarget must be true")
	}
	return nil
}

func validateBootstrapInventory(bootstrapInventory inventory.Inventory) error {
	validationInventory := bootstrapInventory
	validationInventory.Nodes = slices.Clone(bootstrapInventory.Nodes)
	for i := range validationInventory.Nodes {
		if validationInventory.Nodes[i].Address.IsZero() {
			validationInventory.Nodes[i].Address = inventory.ManagementAddress{Host: inventory.HostFromAddr(netip.MustParseAddr("127.0.0.1"))}
		}
	}
	if err := inventory.ValidateInventory(validationInventory); err != nil {
		return err
	}
	return nil
}

func rejectHostSpecificMaterials(values ...any) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	text := string(data)
	return rejectHostSpecificText(text)
}

func rejectHostSpecificKubeadmPlan(plan *kubeadmconfig.Plan) error {
	if plan == nil {
		return nil
	}
	if err := rejectHostSpecificText(string(plan.Config.Content)); err != nil {
		return err
	}
	for _, patch := range plan.Patches {
		if err := rejectHostSpecificText(string(patch.Content)); err != nil {
			return err
		}
	}
	return nil
}

func rejectHostSpecificText(text string) error {
	for _, denied := range []string{"/run/current-system", "/nix/store", "/etc/profiles", "/home/"} {
		if strings.Contains(text, denied) {
			return fmt.Errorf("generated materials contain host-specific path %s", denied)
		}
	}
	return nil
}

func same(a, b any) bool {
	return reflect.DeepEqual(a, b)
}
