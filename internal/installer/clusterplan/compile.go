package clusterplan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/configdomain"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
)

const validationActivationPath = "/run/extensions/katl-kubernetes.raw"

const defaultNetworkdFile = "[Match]\nType=ether\n\n[Network]\nDHCP=yes\n\n[DHCPv4]\nClientIdentifier=mac\nUseHostname=no\n\n[DHCPv6]\nUseHostname=no\n"

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
	endpointPlan, err := resolveControlPlaneEndpoint(config, request.AddressOverrides)
	if err != nil {
		return Plan{}, err
	}
	controlPlaneEndpoint := ""
	if endpointPlan != nil {
		controlPlaneEndpoint = endpointPlan.Endpoint
	}
	if endpointPlan != nil && endpointPlan.Config.Advertisement != nil {
		for _, plan := range request.KubeadmConfigs {
			if err := kubeadmconfig.ValidateManagedEndpoint(plan, endpointPlan.Config.Advertisement.VIP, endpointPlan.Config.Port); err != nil {
				return Plan{}, err
			}
		}
	}

	nodes := append([]Node(nil), config.Spec.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	seen := make(map[string]struct{}, len(nodes))
	materials := make([]NodeMaterial, 0, len(nodes))
	inventoryNodes := make([]inventory.Node, 0, len(nodes))
	addressOverrides := make([]inventory.AddressOverride, 0, len(request.AddressOverrides))
	unusedAddressOverrides := make(map[string]string, len(request.AddressOverrides))
	for name, address := range request.AddressOverrides {
		unusedAddressOverrides[name] = address
	}
	for _, node := range nodes {
		name := strings.TrimSpace(node.Name)
		if name == "" {
			return Plan{}, fmt.Errorf("node name is required")
		}
		if _, ok := seen[name]; ok {
			return Plan{}, fmt.Errorf("duplicate node name %q", name)
		}
		seen[name] = struct{}{}
		role := inventory.SystemRole(strings.TrimSpace(string(node.SystemRole)))
		if role != inventory.RoleControlPlane && role != inventory.RoleWorker {
			return Plan{}, fmt.Errorf("node %q systemRole %q is unsupported", name, node.SystemRole)
		}
		layer, err := mergedLayer(config.Spec.Defaults, node.Overrides)
		if err != nil {
			return Plan{}, fmt.Errorf("node %q: %w", name, err)
		}
		layer.Bootstrap.Access = portableBootstrapAccess(layer.Bootstrap.Access)
		if len(layer.Networkd.Files) == 0 {
			layer.Networkd.Files = []manifest.NetworkdFile{{Name: "10-lan.network", Content: defaultNetworkdFile}}
		}
		layer = applyTargetDiskDefaults(layer)
		material, invNode, err := compileNode(config, name, role, layer, kubernetes, request.KubeadmConfigs, endpointPlan)
		if err != nil {
			return Plan{}, err
		}
		if override, ok := unusedAddressOverrides[name]; ok {
			delete(unusedAddressOverrides, name)
			override = strings.TrimSpace(override)
			if override == "" {
				return Plan{}, fmt.Errorf("address override for node %q is empty", name)
			}
			addressOverrides = append(addressOverrides, inventory.AddressOverride{
				Node:    name,
				Before:  invNode.Address,
				Address: override,
			})
			invNode.Address = override
			material.BootstrapAddress = override
			if material.InstallManifest.Node.Bootstrap != nil {
				material.InstallManifest.Node.Bootstrap.NodeAddress = override
			}
		}
		materials = append(materials, material)
		inventoryNodes = append(inventoryNodes, invNode)
	}
	if len(unusedAddressOverrides) > 0 {
		var names []string
		for name := range unusedAddressOverrides {
			names = append(names, name)
		}
		sort.Strings(names)
		return Plan{}, fmt.Errorf("address override references unknown node %q", names[0])
	}
	sort.Slice(addressOverrides, func(i, j int) bool {
		return addressOverrides[i].Node < addressOverrides[j].Node
	})
	bootstrapInventory := inventory.Inventory{
		ControlPlaneEndpoint:        controlPlaneEndpoint,
		ControlPlaneEndpointManaged: endpointPlan != nil && endpointPlan.Config.Advertisement != nil,
		KubernetesVersion:           kubernetes.version,
		KubernetesBundleSource:      kubernetes.bundleSource,
		KubernetesBundleRef:         kubernetes.bundleRef,
		Nodes:                       inventoryNodes,
	}
	if err := validateBootstrapInventory(bootstrapInventory); err != nil {
		return Plan{}, err
	}
	return Plan{
		ControlPlaneEndpoint:   bootstrapInventory.ControlPlaneEndpoint,
		EndpointPlan:           endpointPlan,
		KubernetesVersion:      kubernetes.version,
		KubernetesCatalogRef:   kubernetes.catalogRef,
		KubernetesBundleSource: kubernetes.bundleSource,
		KubernetesBundleRef:    kubernetes.bundleRef,
		KubernetesSysext:       kubernetes.sysext,
		KatlosImage:            config.Spec.KatlosImage,
		Nodes:                  materials,
		BootstrapInventory:     bootstrapInventory,
		AddressOverrides:       addressOverrides,
	}, nil
}

func resolveControlPlaneEndpoint(config Config, addressOverrides map[string]string) (*controlplaneendpoint.Plan, error) {
	controlPlanes := 0
	controlPlaneName := ""
	controlPlaneAddress := ""
	for _, node := range config.Spec.Nodes {
		if inventory.SystemRole(strings.TrimSpace(string(node.SystemRole))) != inventory.RoleControlPlane {
			continue
		}
		controlPlanes++
		controlPlaneName = strings.TrimSpace(node.Name)
		layer, err := mergedLayer(config.Spec.Defaults, node.Overrides)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", controlPlaneName, err)
		}
		controlPlaneAddress = strings.TrimSpace(layer.Bootstrap.Address)
		if override, ok := addressOverrides[controlPlaneName]; ok {
			controlPlaneAddress = strings.TrimSpace(override)
		}
	}
	if controlPlanes == 0 {
		if config.Spec.ControlPlaneEndpoint != nil && controlplaneendpoint.Managed(*config.Spec.ControlPlaneEndpoint) {
			return nil, fmt.Errorf("managed controlPlaneEndpoint requires at least one control-plane node")
		}
		return nil, nil
	}
	endpoint := config.Spec.ControlPlaneEndpoint
	if endpoint == nil {
		if controlPlanes > 1 {
			return nil, fmt.Errorf("spec.controlPlaneEndpoint is required for a cluster with more than one control-plane node")
		}
		if controlPlaneAddress == "" {
			return nil, fmt.Errorf("spec.controlPlaneEndpoint is required when control-plane node %q has no bootstrap address", controlPlaneName)
		}
		endpoint = &controlplaneendpoint.Config{Host: controlPlaneAddress}
	}
	plan, err := controlplaneendpoint.Normalize(*endpoint)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func compileNode(config Config, name string, role inventory.SystemRole, layer NodeLayer, kubernetes selectedKubernetes, kubeadmConfigs map[string]kubeadmconfig.Plan, endpointPlan *controlplaneendpoint.Plan) (NodeMaterial, inventory.Node, error) {
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
				Hostname: hostname,
				SSH:      layer.SSH,
			},
			SystemRole:           string(role),
			Networkd:             layer.Networkd,
			ControlPlaneEndpoint: managedEndpoint,
			Kubernetes: manifest.KubernetesConfig{
				Kubeadm: manifest.KubeadmReference{ConfigRef: layer.Kubernetes.KubeadmConfigRef},
			},
			Bootstrap: &manifest.BootstrapIntent{
				ClusterName:          strings.TrimSpace(config.Metadata.Name),
				InventoryNodeName:    name,
				NodeAddress:          strings.TrimSpace(layer.Bootstrap.Address),
				ControlPlaneEndpoint: controlPlaneEndpoint,
				BootstrapProfileRef:  kubeadmRef,
				ProfileResolvedID:    bootstrapProfileResolvedID,
				KubernetesCatalogRef: kubernetes.catalogRef,
				KubernetesBundle:     publicBundle,
				Access:               manifestAccess(layer.Bootstrap.Access),
				Labels:               copyLabels(layer.Kubernetes.NodeLabels),
				Taints:               append([]manifest.NodeTaint(nil), layer.Kubernetes.NodeTaints...),
			},
		},
		Install: manifest.InstallConfig{
			WipeTarget: config.Spec.WipeTarget,
			TargetDisk: *layer.Install.TargetDisk,
			ExtraDisks: append([]manifest.ExtraDisk(nil), layer.Install.ExtraDisks...),
		},
		KatlosImage: config.Spec.KatlosImage,
	}
	if err := manifest.ValidateWithOptions(installManifest, manifest.ValidateOptions{AllowMissingKatlosImage: true}); err != nil {
		return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q manifest: %w", name, err)
	}
	kubeadmConfig := inventory.KubeadmConfig{}
	var kubeadmPlan *kubeadmconfig.Plan
	if kubeadmRef != "" {
		configPlan, ok := kubeadmConfigs[kubeadmRef]
		if !ok {
			return NodeMaterial{}, inventory.Node{}, fmt.Errorf("node %q kubeadm config ref %q was not resolved", name, kubeadmRef)
		}
		kubeadmPlan = &configPlan
		kubeadmConfig = inventory.KubeadmConfig{
			Ref:    kubeadmRef,
			Path:   configPlan.Config.RenderPath,
			Intent: inventory.KubeadmIntent(role),
		}
	}
	nativeEtcFiles, err := configdomain.NativeEtcFiles(configdomain.RenderRequest{
		Manifest:                 installManifest,
		KubeadmConfigs:           kubeadmConfigs,
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
		Address:           strings.TrimSpace(layer.Bootstrap.Address),
		SystemRole:        role,
		Access:            layer.Bootstrap.Access,
		KubeadmConfig:     kubeadmConfig,
		KubernetesVersion: kubernetes.version,
		Labels:            copyLabels(layer.Kubernetes.NodeLabels),
	}
	return NodeMaterial{
		Name:                   name,
		SystemRole:             role,
		BootstrapAddress:       invNode.Address,
		InstallManifest:        installManifest,
		NativeEtcFiles:         nativeEtcFiles,
		KubeadmConfig:          kubeadmConfig,
		NodeLabels:             copyLabels(layer.Kubernetes.NodeLabels),
		NodeTaints:             append([]manifest.NodeTaint(nil), layer.Kubernetes.NodeTaints...),
		KubernetesVersion:      kubernetes.version,
		KubernetesCatalogRef:   kubernetes.catalogRef,
		KubernetesBundleSource: kubernetes.bundleSource,
		KubernetesBundleRef:    kubernetes.bundleRef,
		KubernetesSysext:       kubernetes.sysext,
	}, invNode, nil
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
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func validateSharedLayer(path string, layer NodeLayer) error {
	if layer.Install.TargetDisk != nil {
		return fmt.Errorf("%s.install.targetDisk is not allowed; target disk identity must be set per node", path)
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
	for _, value := range []string{version, catalogRef, bundleRef} {
		if value != "" {
			selectors++
		}
	}
	if selectors == 0 {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.payloadVersion, catalogRef, or bundleRef is required")
	}
	if selectors > 1 {
		return selectedKubernetes{}, fmt.Errorf("spec.kubernetes must set exactly one of payloadVersion, catalogRef, or bundleRef")
	}
	if version != "" {
		if sysextcatalog.KubernetesMinor(version) == "" {
			return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.payloadVersion %q must be vMAJOR.MINOR.PATCH", version)
		}
		return selectedKubernetes{
			version:        version,
			activationPath: validationActivationPath,
		}, nil
	}
	if bundleRef != "" {
		payloadVersion, err := kubernetesbundle.PayloadVersionFromRef(bundleRef)
		if err != nil {
			return selectedKubernetes{}, fmt.Errorf("spec.kubernetes.bundleRef %q: %w", bundleRef, err)
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
	validationInventory.Nodes = append([]inventory.Node(nil), bootstrapInventory.Nodes...)
	for i := range validationInventory.Nodes {
		if strings.TrimSpace(validationInventory.Nodes[i].Address) == "" {
			validationInventory.Nodes[i].Address = "127.0.0.1"
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
