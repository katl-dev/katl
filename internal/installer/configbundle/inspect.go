package configbundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
)

const (
	NodeResolutionKind = "ClusterConfigNodeResolution"
	ConfigDiffKind     = "ClusterConfigDiff"
)

type NodeResolution struct {
	APIVersion     string                   `json:"apiVersion" yaml:"apiVersion"`
	Kind           string                   `json:"kind" yaml:"kind"`
	ClusterName    string                   `json:"clusterName" yaml:"clusterName"`
	Node           string                   `json:"node" yaml:"node"`
	Effective      SourceNode               `json:"effective" yaml:"effective"`
	Cluster        ResolvedCluster          `json:"cluster" yaml:"cluster"`
	Derived        ResolvedDerived          `json:"derived" yaml:"derived"`
	Provenance     []FieldProvenance        `json:"provenance" yaml:"provenance"`
	OwnedFiles     []OwnedFile              `json:"ownedFiles" yaml:"ownedFiles"`
	Warnings       []ResolutionWarning      `json:"warnings" yaml:"warnings"`
	Classification ResolutionClassification `json:"classification" yaml:"classification"`
}

type ResolvedCluster struct {
	ControlPlaneEndpoint *controlplaneendpoint.Config `json:"controlPlaneEndpoint,omitempty" yaml:"controlPlaneEndpoint,omitempty"`
	KubernetesVersion    string                       `json:"kubernetesVersion" yaml:"kubernetesVersion"`
}

type ResolvedDerived struct {
	Hostname       string          `json:"hostname" yaml:"hostname"`
	SystemRole     string          `json:"systemRole" yaml:"systemRole"`
	KubeadmConfig  string          `json:"kubeadmConfig,omitempty" yaml:"kubeadmConfig,omitempty"`
	StorageVolumes []DerivedVolume `json:"storageVolumes,omitempty" yaml:"storageVolumes,omitempty"`
}

type DerivedVolume struct {
	Name           string `json:"name" yaml:"name"`
	MountPath      string `json:"mountPath" yaml:"mountPath"`
	MountSource    string `json:"mountSource" yaml:"mountSource"`
	PartitionLabel string `json:"partitionLabel,omitempty" yaml:"partitionLabel,omitempty"`
}

type FieldProvenance struct {
	Path   string `json:"path" yaml:"path"`
	Source string `json:"source" yaml:"source"`
	Kind   string `json:"kind" yaml:"kind"`
}

type OwnedFile struct {
	Path  string `json:"path" yaml:"path"`
	Type  string `json:"type" yaml:"type"`
	Mode  string `json:"mode,omitempty" yaml:"mode,omitempty"`
	Owner string `json:"owner" yaml:"owner"`
}

type ResolutionWarning struct {
	Path    string `json:"path" yaml:"path"`
	Message string `json:"message" yaml:"message"`
}

type ResolutionClassification struct {
	Install   string `json:"install" yaml:"install"`
	Apply     string `json:"apply" yaml:"apply"`
	Lifecycle string `json:"lifecycle" yaml:"lifecycle"`
}

func InspectSelectedNode(selected SelectedNodeMaterial) (NodeResolution, error) {
	var node SourceNode
	nodeIndex := -1
	for i, candidate := range selected.Source.Spec.Nodes {
		if candidate.Name == selected.Node.Name {
			node = candidate
			nodeIndex = i
			break
		}
	}
	if nodeIndex < 0 {
		return NodeResolution{}, fmt.Errorf("normalized source does not contain selected node %q", selected.Node.Name)
	}
	resolved, err := mergeSourceNodeLayer(selected.Source.Spec.Defaults, sourceNodeLayer(node))
	if err != nil {
		return NodeResolution{}, fmt.Errorf("resolve node %q: %w", node.Name, err)
	}
	effective := SourceNode{
		Name:              node.Name,
		ControlPlane:      node.ControlPlane,
		Access:            resolved.Access,
		Kernel:            resolved.Kernel,
		HostConfiguration: resolved.HostConfiguration,
		SystemExtensions:  resolved.SystemExtensions,
		Install:           resolved.Install,
		Storage:           resolved.Storage,
		Kubernetes:        resolved.Kubernetes,
		Management:        node.Management,
	}
	base := sourceNodePath(node, nodeIndex)
	report := NodeResolution{
		APIVersion:  APIVersion,
		Kind:        NodeResolutionKind,
		ClusterName: selected.Source.Metadata.Name,
		Node:        node.Name,
		Effective:   effective,
		Cluster: ResolvedCluster{
			ControlPlaneEndpoint: selected.Source.Spec.ControlPlaneEndpoint,
			KubernetesVersion:    selected.NodeMaterial.KubernetesVersion,
		},
		Derived: ResolvedDerived{
			Hostname:      selected.InstallManifest.Node.Identity.Hostname,
			SystemRole:    selected.InstallManifest.Node.SystemRole,
			KubeadmConfig: selected.NodeMaterial.KubeadmConfig.Ref,
		},
		Classification: ResolutionClassification{
			Install:   "supported for unenrolled or explicitly reinstalled nodes",
			Apply:     "use config diff against the current desired config before applying",
			Lifecycle: "node role and system-disk changes require dedicated lifecycle operations",
		},
	}
	report.Provenance = nodeProvenance(node, resolved, base)
	report.Provenance = append(report.Provenance, FieldProvenance{
		Path: "spec.kubernetes.version", Source: "spec.kubernetes.version", Kind: "cluster",
	})
	if selected.Source.Spec.ControlPlaneEndpoint != nil {
		report.Provenance = append(report.Provenance, FieldProvenance{
			Path: "spec.controlPlaneEndpoint", Source: "spec.controlPlaneEndpoint", Kind: "cluster",
		})
	}
	report.Derived.StorageVolumes, report.Warnings = derivedVolumesAndWarnings(resolved.Storage, base)
	if extensions, ok := resolved.SystemExtensions.Get(); ok {
		installedByName := make(map[string]string, len(selected.InstallManifest.Node.SystemExtensions))
		for _, extension := range selected.InstallManifest.Node.SystemExtensions {
			installedByName[extension.Name] = extension.OCIManifestDigest
		}
		for _, extension := range extensions {
			digest := installedByName[extension.Name]
			if digest == "" || strings.Contains(extension.Bundle, "@") {
				continue
			}
			pinned := strings.SplitN(extension.Bundle, "@", 2)[0] + "@" + digest
			report.Warnings = append(report.Warnings, ResolutionWarning{
				Path:    fmt.Sprintf("%s.systemExtensions[name=%q].bundle", base, extension.Name),
				Message: fmt.Sprintf("mutable reference %q resolved to %s; pin it as %s for reproducible compilation", extension.Bundle, digest, pinned),
			})
		}
	}
	if node.Management.Address != "" {
		report.Warnings = append(report.Warnings, ResolutionWarning{
			Path:    base + ".management.address",
			Message: "management.address selects the workstation target and does not change generated node state",
		})
	}
	owned := append([]confext.NativeEtcFile(nil), selected.NodeMaterial.NativeEtcFiles...)
	if plan, ok := selected.KubeadmConfigs[selected.NodeMaterial.KubeadmConfig.Ref]; ok {
		owned = append(owned, plan.NativeEtcFiles()...)
	}
	report.OwnedFiles = ownedFiles(owned)
	sort.Slice(report.Provenance, func(i, j int) bool { return report.Provenance[i].Path < report.Provenance[j].Path })
	sort.Slice(report.Warnings, func(i, j int) bool { return report.Warnings[i].Path < report.Warnings[j].Path })
	return report, nil
}

func nodeProvenance(node SourceNode, resolved SourceNodeLayer, base string) []FieldProvenance {
	entries := []FieldProvenance{
		{Path: base + ".name", Source: base + ".name", Kind: "node"},
		{Path: base + ".controlPlane", Source: base + ".controlPlane", Kind: "node"},
	}
	choose := func(path string, nodeSet bool) {
		source := "spec.defaults." + path
		kind := "default"
		if nodeSet {
			source = base + "." + path
			kind = "node"
		}
		entries = append(entries, FieldProvenance{Path: base + "." + path, Source: source, Kind: kind})
	}
	_, keysSet := node.Access.SSH.AuthorizedKeys.Get()
	if _, set := resolved.Access.SSH.AuthorizedKeys.Get(); set {
		choose("access.ssh.authorizedKeys", keysSet)
	}
	if resolved.Kernel != nil {
		choose("kernel", node.Kernel != nil)
	}
	_, sysfsSet := node.HostConfiguration.Sysfs.Get()
	if _, set := resolved.HostConfiguration.Sysfs.Get(); set {
		choose("hostConfiguration.sysfs", sysfsSet)
	}
	resolvedSets, _ := resolved.HostConfiguration.FileSets.Get()
	nodeSets, nodeSetsSet := node.HostConfiguration.FileSets.Get()
	for _, name := range sortedMapKeys(resolvedSets) {
		_, fromNode := nodeSets[name]
		choose(fmt.Sprintf("hostConfiguration.fileSets[%q]", name), nodeSetsSet && fromNode)
	}
	resolvedExtensions, _ := resolved.SystemExtensions.Get()
	nodeExtensions, nodeExtensionsSet := node.SystemExtensions.Get()
	for _, extension := range resolvedExtensions {
		choose(fmt.Sprintf("systemExtensions[name=%q]", extension.Name), nodeExtensionsSet && hasExtension(nodeExtensions, extension.Name))
	}
	resolvedSystemDisk := resolved.Install.SystemDisk
	diskFields := []struct {
		name string
		used bool
		set  bool
	}{
		{"byID", optionalStringSet(resolvedSystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.ByID }), optionalStringSet(node.Install.SystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.ByID })},
		{"wwn", optionalStringSet(resolvedSystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.WWN }), optionalStringSet(node.Install.SystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.WWN })},
		{"serial", optionalStringSet(resolvedSystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.Serial }), optionalStringSet(node.Install.SystemDisk, func(v *SourceDiskSelector) Optional[string] { return v.Serial })},
		{"minSizeMiB", optionalUintSet(resolvedSystemDisk), optionalUintSet(node.Install.SystemDisk)},
	}
	for _, field := range diskFields {
		if field.used {
			choose("install.systemDisk."+field.name, field.set)
		}
	}
	resolvedDisks, _ := resolved.Storage.Volumes.Get()
	nodeDisks, nodeDisksSet := node.Storage.Volumes.Get()
	for _, disk := range resolvedDisks {
		nodeDisk, found := storageVolume(nodeDisks, disk.Name)
		prefix := fmt.Sprintf("storage.volumes[name=%q]", disk.Name)
		var nodeSelector *SourceVolumeSelector
		if nodeDisksSet && found {
			nodeSelector = nodeDisk.Selector
		}
		if disk.Selector != nil && disk.Selector.Disk != nil {
			var nodeDiskSelector *SourceDiskSelector
			if nodeSelector != nil {
				nodeDiskSelector = nodeSelector.Disk
			}
			selectorFields := []struct {
				name string
				used bool
				set  bool
			}{
				{"byID", optionalStringSet(disk.Selector.Disk, func(v *SourceDiskSelector) Optional[string] { return v.ByID }), optionalStringSet(nodeDiskSelector, func(v *SourceDiskSelector) Optional[string] { return v.ByID })},
				{"wwn", optionalStringSet(disk.Selector.Disk, func(v *SourceDiskSelector) Optional[string] { return v.WWN }), optionalStringSet(nodeDiskSelector, func(v *SourceDiskSelector) Optional[string] { return v.WWN })},
				{"serial", optionalStringSet(disk.Selector.Disk, func(v *SourceDiskSelector) Optional[string] { return v.Serial }), optionalStringSet(nodeDiskSelector, func(v *SourceDiskSelector) Optional[string] { return v.Serial })},
				{"minSizeMiB", optionalUintSet(disk.Selector.Disk), optionalUintSet(nodeDiskSelector)},
			}
			for _, field := range selectorFields {
				if field.used {
					choose(prefix+".selector.disk."+field.name, field.set)
				}
			}
		}
		if disk.Selector != nil && disk.Selector.Partition != nil {
			var nodePartition *SourcePartitionSelector
			if nodeSelector != nil {
				nodePartition = nodeSelector.Partition
			}
			partitionFields := []struct {
				name string
				used bool
				set  bool
			}{
				{"byID", optionalPartitionStringSet(disk.Selector.Partition, func(v *SourcePartitionSelector) Optional[string] { return v.ByID }), optionalPartitionStringSet(nodePartition, func(v *SourcePartitionSelector) Optional[string] { return v.ByID })},
				{"partUUID", optionalPartitionStringSet(disk.Selector.Partition, func(v *SourcePartitionSelector) Optional[string] { return v.PartUUID }), optionalPartitionStringSet(nodePartition, func(v *SourcePartitionSelector) Optional[string] { return v.PartUUID })},
				{"filesystemUUID", optionalPartitionStringSet(disk.Selector.Partition, func(v *SourcePartitionSelector) Optional[string] { return v.FilesystemUUID }), optionalPartitionStringSet(nodePartition, func(v *SourcePartitionSelector) Optional[string] { return v.FilesystemUUID })},
				{"byVolumeName", optionalPartitionBoolSet(disk.Selector.Partition), optionalPartitionBoolSet(nodePartition)},
			}
			selected := false
			for _, field := range partitionFields {
				if field.used {
					selected = true
					choose(prefix+".selector.partition."+field.name, field.set)
				}
			}
			if !selected {
				choose(prefix+".selector.partition", nodePartition != nil)
			}
		}
		_, filesystemSet := nodeDisk.Filesystem.Get()
		if _, set := disk.Filesystem.Get(); set {
			choose(prefix+".filesystem", nodeDisksSet && found && filesystemSet)
		}
		_, wipeSet := nodeDisk.Wipe.Get()
		if _, set := disk.Wipe.Get(); set {
			choose(prefix+".wipe", nodeDisksSet && found && wipeSet)
		}
	}
	resolvedLabels, _ := resolved.Kubernetes.Labels.Get()
	nodeLabels, nodeLabelsSet := node.Kubernetes.Labels.Get()
	for _, key := range sortedMapKeys(resolvedLabels) {
		_, found := nodeLabels[key]
		choose(fmt.Sprintf("kubernetes.labels[%q]", key), nodeLabelsSet && found)
	}
	if strings.TrimSpace(resolved.Kubernetes.Address) != "" {
		choose("kubernetes.address", strings.TrimSpace(node.Kubernetes.Address) != "")
	}
	if resolved.Kubernetes.Kubelet != nil {
		choose("kubernetes.kubelet.configFile", node.Kubernetes.Kubelet != nil)
	}
	_, taintsSet := node.Kubernetes.Taints.Get()
	if _, set := resolved.Kubernetes.Taints.Get(); set {
		choose("kubernetes.taints", taintsSet)
	}
	if node.Management.Address != "" {
		entries = append(entries, FieldProvenance{Path: base + ".management.address", Source: base + ".management.address", Kind: "node"})
	}
	return entries
}

func derivedVolumesAndWarnings(storage SourceStorageLayer, base string) ([]DerivedVolume, []ResolutionWarning) {
	disks, _ := storage.Volumes.Get()
	volumes := make([]DerivedVolume, 0, len(disks))
	var warnings []ResolutionWarning
	for _, disk := range disks {
		derived := DerivedVolume{Name: disk.Name, MountPath: "/var/mnt/" + disk.Name}
		path := fmt.Sprintf("%s.storage.volumes[name=%q]", base, disk.Name)
		switch {
		case disk.Selector != nil && disk.Selector.Disk != nil:
			derived.PartitionLabel = "u-" + disk.Name
			derived.MountSource = "/dev/disk/by-partlabel/" + derived.PartitionLabel
		case disk.Selector != nil && disk.Selector.Partition != nil:
			selector := disk.Selector.Partition
			switch {
			case selector.ByID.Value() != "":
				derived.MountSource = selector.ByID.Value()
			case selector.PartUUID.Value() != "":
				derived.MountSource = "PARTUUID=" + selector.PartUUID.Value()
			case selector.FilesystemUUID.Value() != "":
				derived.MountSource = "UUID=" + selector.FilesystemUUID.Value()
			case selector.ByVolumeName.Value():
				derived.PartitionLabel = "u-" + disk.Name
				derived.MountSource = "/dev/disk/by-partlabel/" + derived.PartitionLabel
			}
		}
		if wipe, set := disk.Wipe.Get(); set && wipe {
			warnings = append(warnings, ResolutionWarning{Path: path + ".wipe", Message: "overwriting existing data requires operation-level acknowledgement naming this node and volume"})
		}
		volumes = append(volumes, derived)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, warnings
}

func ownedFiles(files []confext.NativeEtcFile) []OwnedFile {
	out := make([]OwnedFile, 0, len(files))
	for _, file := range files {
		kind := string(file.Type)
		if kind == "" {
			kind = string(confext.NativeEtcRegularFile)
		}
		mode := file.Mode
		if mode == 0 && file.Type != confext.NativeEtcSymlink {
			mode = 0o644
		}
		entry := OwnedFile{Path: file.Path, Type: kind, Owner: fmt.Sprintf("%d:%d", file.UID, file.GID)}
		if file.Type != confext.NativeEtcSymlink {
			entry.Mode = formatMode(mode)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func formatMode(mode fs.FileMode) string { return fmt.Sprintf("%04o", mode.Perm()) }

func hasExtension(values []SourceSystemExtension, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}

func storageVolume(values []SourceStorageVolume, name string) (SourceStorageVolume, bool) {
	for _, value := range values {
		if value.Name == name {
			return value, true
		}
	}
	return SourceStorageVolume{}, false
}

func optionalStringSet(selector *SourceDiskSelector, get func(*SourceDiskSelector) Optional[string]) bool {
	if selector == nil {
		return false
	}
	_, ok := get(selector).Get()
	return ok
}

func optionalUintSet(selector *SourceDiskSelector) bool {
	if selector == nil {
		return false
	}
	_, ok := selector.MinSizeMiB.Get()
	return ok
}

func optionalPartitionStringSet(selector *SourcePartitionSelector, get func(*SourcePartitionSelector) Optional[string]) bool {
	if selector == nil {
		return false
	}
	_, ok := get(selector).Get()
	return ok
}

func optionalPartitionBoolSet(selector *SourcePartitionSelector) bool {
	if selector == nil {
		return false
	}
	_, ok := selector.ByVolumeName.Get()
	return ok
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type ConfigDiff struct {
	APIVersion     string              `json:"apiVersion" yaml:"apiVersion"`
	Kind           string              `json:"kind" yaml:"kind"`
	Node           string              `json:"node" yaml:"node"`
	BeforeCluster  string              `json:"beforeCluster" yaml:"beforeCluster"`
	AfterCluster   string              `json:"afterCluster" yaml:"afterCluster"`
	Changes        []ConfigFieldChange `json:"changes" yaml:"changes"`
	Classification DiffClassification  `json:"classification" yaml:"classification"`
}

type ConfigFieldChange struct {
	Path              string `json:"path" yaml:"path"`
	Before            any    `json:"before,omitempty" yaml:"before,omitempty"`
	After             any    `json:"after,omitempty" yaml:"after,omitempty"`
	Classification    string `json:"classification" yaml:"classification"`
	RequiredOperation string `json:"requiredOperation,omitempty" yaml:"requiredOperation,omitempty"`
	Message           string `json:"message" yaml:"message"`
}

type DiffClassification struct {
	Overall            string   `json:"overall" yaml:"overall"`
	RequiredOperations []string `json:"requiredOperations,omitempty" yaml:"requiredOperations,omitempty"`
}

func DiffNodeResolutions(before, after NodeResolution) (ConfigDiff, error) {
	if before.Node != after.Node {
		return ConfigDiff{}, fmt.Errorf("config diff resolved different nodes %q and %q; use --node to compare one stable node identity; node renames require an explicit lifecycle operation", before.Node, after.Node)
	}
	base := fmt.Sprintf("spec.nodes[%q]", after.Node)
	if after.Node == "" {
		base = fmt.Sprintf("spec.nodes[%q]", before.Node)
	}
	diff := ConfigDiff{
		APIVersion:    APIVersion,
		Kind:          ConfigDiffKind,
		Node:          after.Node,
		BeforeCluster: before.ClusterName,
		AfterCluster:  after.ClusterName,
	}
	if diff.Node == "" {
		diff.Node = before.Node
	}
	add := func(path string, left, right any) {
		if publicEqual(left, right) {
			return
		}
		classification, operation, message := classifyDiffPath(path)
		diff.Changes = append(diff.Changes, ConfigFieldChange{
			Path: path, Before: left, After: right, Classification: classification,
			RequiredOperation: operation, Message: message,
		})
	}
	add("spec.controlPlaneEndpoint", before.Cluster.ControlPlaneEndpoint, after.Cluster.ControlPlaneEndpoint)
	add("spec.kubernetes.version", before.Cluster.KubernetesVersion, after.Cluster.KubernetesVersion)
	add(base+".controlPlane", before.Effective.ControlPlane, after.Effective.ControlPlane)
	add(base+".access.ssh.authorizedKeys", before.Effective.Access.SSH.AuthorizedKeys.Value(), after.Effective.Access.SSH.AuthorizedKeys.Value())
	add(base+".kernel", before.Effective.Kernel, after.Effective.Kernel)
	add(base+".hostConfiguration.sysfs", before.Effective.HostConfiguration.Sysfs.Value(), after.Effective.HostConfiguration.Sysfs.Value())
	diffNamedMaps(&diff, base+".hostConfiguration.fileSets", before.Effective.HostConfiguration.FileSets.Value(), after.Effective.HostConfiguration.FileSets.Value())
	diffNamedExtensions(&diff, base+".systemExtensions", before.Effective.SystemExtensions.Value(), after.Effective.SystemExtensions.Value())
	add(base+".install.systemDisk", before.Effective.Install.SystemDisk, after.Effective.Install.SystemDisk)
	diffNamedVolumes(&diff, base+".storage.volumes", before.Effective.Storage.Volumes.Value(), after.Effective.Storage.Volumes.Value())
	add(base+".kubernetes.address", before.Effective.Kubernetes.Address, after.Effective.Kubernetes.Address)
	add(base+".kubernetes.kubelet", before.Effective.Kubernetes.Kubelet, after.Effective.Kubernetes.Kubelet)
	diffStringMaps(&diff, base+".kubernetes.labels", before.Effective.Kubernetes.Labels.Value(), after.Effective.Kubernetes.Labels.Value())
	add(base+".kubernetes.taints", before.Effective.Kubernetes.Taints.Value(), after.Effective.Kubernetes.Taints.Value())
	add(base+".management.address", before.Effective.Management.Address, after.Effective.Management.Address)
	sort.Slice(diff.Changes, func(i, j int) bool { return diff.Changes[i].Path < diff.Changes[j].Path })
	diff.Classification = summarizeDiff(diff.Changes)
	return diff, nil
}

func appendNamedChange(diff *ConfigDiff, path string, before, after any) {
	if publicEqual(before, after) {
		return
	}
	classification, operation, message := classifyDiffPath(path)
	if strings.Contains(path, ".storage.volumes") && after == nil {
		message = "removal unmounts the volume and stops Katl management; the partition, filesystem, and data are preserved"
	}
	diff.Changes = append(diff.Changes, ConfigFieldChange{Path: path, Before: before, After: after, Classification: classification, RequiredOperation: operation, Message: message})
}

func diffNamedMaps[V any](diff *ConfigDiff, prefix string, before, after map[string]V) {
	for _, name := range sortedUnionKeys(before, after) {
		left, leftOK := before[name]
		right, rightOK := after[name]
		appendNamedChange(diff, fmt.Sprintf("%s[%q]", prefix, name), optionalAny(left, leftOK), optionalAny(right, rightOK))
	}
}

func diffNamedExtensions(diff *ConfigDiff, prefix string, before, after []SourceSystemExtension) {
	left := make(map[string]SourceSystemExtension, len(before))
	right := make(map[string]SourceSystemExtension, len(after))
	for _, value := range before {
		left[value.Name] = value
	}
	for _, value := range after {
		right[value.Name] = value
	}
	diffNamedMaps(diff, prefix, left, right)
}

func diffNamedVolumes(diff *ConfigDiff, prefix string, before, after []SourceStorageVolume) {
	left := make(map[string]SourceStorageVolume, len(before))
	right := make(map[string]SourceStorageVolume, len(after))
	for _, value := range before {
		left[value.Name] = value
	}
	for _, value := range after {
		right[value.Name] = value
	}
	diffNamedMaps(diff, prefix, left, right)
}

func diffStringMaps(diff *ConfigDiff, prefix string, before, after map[string]string) {
	for _, name := range sortedUnionKeys(before, after) {
		left, leftOK := before[name]
		right, rightOK := after[name]
		appendNamedChange(diff, fmt.Sprintf("%s[%q]", prefix, name), optionalAny(left, leftOK), optionalAny(right, rightOK))
	}
}

func classifyDiffPath(path string) (string, string, string) {
	switch {
	case path == "spec.kubernetes.version":
		return "operation-only", "kubernetes-upgrade", "Kubernetes payload changes use the Kubernetes upgrade operation"
	case path == "spec.controlPlaneEndpoint":
		return "operation-only", "control-plane-endpoint-migration", "an initialized control-plane endpoint needs a dedicated migration; this operation is not yet supported"
	case strings.HasSuffix(path, ".controlPlane"):
		return "operation-only", "wipe-reinstall", "changing a node role requires an explicit lifecycle operation"
	case strings.Contains(path, ".install.systemDisk"):
		return "operation-only", "wipe-reinstall", "changing the system disk requires an explicit reinstall and never wipes implicitly"
	case strings.Contains(path, ".kubernetes.address"):
		return "operation-only", "kubeadm-aware operation", "changing kubelet node identity requires a kubeadm-aware operation"
	case strings.Contains(path, ".kubernetes.kubelet"):
		return "operation-only", "kubeadm-aware operation", "per-node kubelet configuration changes require kubeadm-aware reconciliation on this node"
	case strings.Contains(path, ".kubernetes.labels"), strings.Contains(path, ".kubernetes.taints"):
		return "operation-only", "kubeadm-aware operation", "Kubernetes node metadata changes require Kubernetes-aware reconciliation"
	case strings.Contains(path, ".management.address"):
		return "target-only", "", "changes only the workstation management target; no node generation or mutation is planned"
	case strings.Contains(path, ".storage.volumes"):
		return "online-applicable", "", "volume changes require live target discovery; existing data is preserved unless explicitly authorized"
	case strings.Contains(path, ".hostConfiguration"):
		return "online-or-next-boot", "", "the concrete file or sysfs change determines whether live preflight succeeds"
	case strings.Contains(path, ".kernel"), strings.Contains(path, ".systemExtensions"), strings.Contains(path, ".authorizedKeys"):
		return "staged-only", "", "change activates through a staged node generation"
	default:
		return "online-or-next-boot", "", "change is handled by normal configuration apply"
	}
}

func summarizeDiff(changes []ConfigFieldChange) DiffClassification {
	if len(changes) == 0 {
		return DiffClassification{Overall: "no-change"}
	}
	rank := map[string]int{"target-only": 1, "online-applicable": 2, "online-or-next-boot": 3, "staged-only": 4, "operation-only": 5}
	overall := "target-only"
	operations := map[string]struct{}{}
	for _, change := range changes {
		if rank[change.Classification] > rank[overall] {
			overall = change.Classification
		}
		if change.RequiredOperation != "" {
			operations[change.RequiredOperation] = struct{}{}
		}
	}
	return DiffClassification{Overall: overall, RequiredOperations: sortedMapKeys(operations)}
}

func publicEqual(left, right any) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func optionalAny[V any](value V, ok bool) any {
	if !ok {
		return nil
	}
	return value
}

func sortedUnionKeys[V any](left, right map[string]V) []string {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return sortedMapKeys(keys)
}
