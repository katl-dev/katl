package configbundle

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func mergeSourceNodeLayer(base, next SourceNodeLayer) (SourceNodeLayer, error) {
	out := cloneSourceNodeLayer(base)
	if keys, ok := next.Access.SSH.AuthorizedKeys.Get(); ok {
		out.Access.SSH.AuthorizedKeys = supplied(slices.Clone(keys))
	}
	if next.Kernel != nil {
		out.Kernel = cloneSourceKernelConfig(next.Kernel)
	}
	out.HostConfiguration = mergeSourceHostConfiguration(out.HostConfiguration, next.HostConfiguration)
	var err error
	out.SystemExtensions, err = mergeSourceSystemExtensions(out.SystemExtensions, next.SystemExtensions)
	if err != nil {
		return SourceNodeLayer{}, err
	}
	out.Install.SystemDisk = mergeSourceDiskSelector(out.Install.SystemDisk, next.Install.SystemDisk)
	out.Storage.Disks, err = mergeSourceStorageDisks(out.Storage.Disks, next.Storage.Disks)
	if err != nil {
		return SourceNodeLayer{}, err
	}
	if strings.TrimSpace(next.Kubernetes.Address) != "" {
		out.Kubernetes.Address = strings.TrimSpace(next.Kubernetes.Address)
	}
	out.Kubernetes.Labels = mergeSourceLabels(out.Kubernetes.Labels, next.Kubernetes.Labels)
	if taints, ok := next.Kubernetes.Taints.Get(); ok {
		out.Kubernetes.Taints = supplied(slices.Clone(taints))
	}
	if next.Kubernetes.Kubelet != nil {
		out.Kubernetes.Kubelet = cloneSourceKubeletConfig(next.Kubernetes.Kubelet)
	}
	return out, nil
}

func cloneSourceNodeLayer(layer SourceNodeLayer) SourceNodeLayer {
	out := layer
	out.Access.SSH.AuthorizedKeys = cloneOptionalSlice(layer.Access.SSH.AuthorizedKeys)
	out.Kernel = cloneSourceKernelConfig(layer.Kernel)
	out.HostConfiguration = cloneSourceHostConfiguration(layer.HostConfiguration)
	out.SystemExtensions = cloneOptionalSourceSystemExtensions(layer.SystemExtensions)
	out.Install.SystemDisk = cloneSourceDiskSelector(layer.Install.SystemDisk)
	out.Storage.Disks = cloneOptionalStorageDisks(layer.Storage.Disks)
	out.Kubernetes.Labels = cloneOptionalMap(layer.Kubernetes.Labels)
	out.Kubernetes.Taints = cloneOptionalSlice(layer.Kubernetes.Taints)
	out.Kubernetes.Kubelet = cloneSourceKubeletConfig(layer.Kubernetes.Kubelet)
	return out
}

func cloneSourceKubeletConfig(config *SourceKubeletConfig) *SourceKubeletConfig {
	if config == nil {
		return nil
	}
	copy := *config
	return &copy
}

func mergeSourceHostConfiguration(base, next SourceHostConfiguration) SourceHostConfiguration {
	out := cloneSourceHostConfiguration(base)
	if sysfs, ok := next.Sysfs.Get(); ok {
		out.Sysfs = supplied(slices.Clone(sysfs))
	}
	nextSets, ok := next.FileSets.Get()
	if !ok {
		return out
	}
	if len(nextSets) == 0 {
		out.FileSets = supplied(map[string]SourceHostConfigurationFileSet{})
		return out
	}
	sets := map[string]SourceHostConfigurationFileSet{}
	if baseSets, present := out.FileSets.Get(); present {
		for name, set := range baseSets {
			if strings.TrimSpace(set.State) != manifest.HostConfigurationAbsent {
				sets[name] = cloneSourceHostConfigurationFileSet(set)
			}
		}
	}
	for name, set := range nextSets {
		if strings.TrimSpace(set.State) == manifest.HostConfigurationAbsent {
			delete(sets, name)
			continue
		}
		sets[name] = cloneSourceHostConfigurationFileSet(set)
	}
	out.FileSets = supplied(sets)
	return out
}

func mergeSourceSystemExtensions(base, next Optional[[]SourceSystemExtension]) (Optional[[]SourceSystemExtension], error) {
	nextExtensions, ok := next.Get()
	if !ok {
		return cloneOptionalSourceSystemExtensions(base), nil
	}
	if len(nextExtensions) == 0 {
		return supplied([]SourceSystemExtension{}), nil
	}
	entries := map[string]SourceSystemExtension{}
	if baseExtensions, present := base.Get(); present {
		for _, extension := range baseExtensions {
			if strings.TrimSpace(extension.State) != manifest.SystemExtensionAbsent {
				entries[extension.Name] = cloneSourceSystemExtension(extension)
			}
		}
	}
	seen := map[string]struct{}{}
	for _, extension := range nextExtensions {
		if _, exists := seen[extension.Name]; exists {
			return Optional[[]SourceSystemExtension]{}, fmt.Errorf("systemExtensions contains duplicate name %q", extension.Name)
		}
		seen[extension.Name] = struct{}{}
		if strings.TrimSpace(extension.State) == manifest.SystemExtensionAbsent {
			delete(entries, extension.Name)
			continue
		}
		entries[extension.Name] = cloneSourceSystemExtension(extension)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]SourceSystemExtension, 0, len(names))
	for _, name := range names {
		values = append(values, entries[name])
	}
	return supplied(values), nil
}

func mergeSourceStorageDisks(base, next Optional[[]SourceStorageDisk]) (Optional[[]SourceStorageDisk], error) {
	nextDisks, ok := next.Get()
	if !ok {
		return cloneOptionalStorageDisks(base), nil
	}
	if len(nextDisks) == 0 {
		return supplied([]SourceStorageDisk{}), nil
	}
	entries := map[string]SourceStorageDisk{}
	if baseDisks, present := base.Get(); present {
		for _, disk := range baseDisks {
			if strings.TrimSpace(disk.State) != manifest.SystemExtensionAbsent {
				entries[disk.Name] = cloneSourceStorageDisk(disk)
			}
		}
	}
	seen := map[string]struct{}{}
	for _, disk := range nextDisks {
		name := strings.TrimSpace(disk.Name)
		if _, exists := seen[name]; exists {
			return Optional[[]SourceStorageDisk]{}, fmt.Errorf("storage.disks contains duplicate name %q", name)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(disk.State) == manifest.SystemExtensionAbsent {
			delete(entries, name)
			continue
		}
		disk.Name = name
		if inherited, exists := entries[name]; exists {
			disk = mergeSourceStorageDisk(inherited, disk)
		}
		entries[name] = cloneSourceStorageDisk(disk)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]SourceStorageDisk, 0, len(names))
	for _, name := range names {
		values = append(values, entries[name])
	}
	return supplied(values), nil
}

func mergeSourceStorageDisk(base, next SourceStorageDisk) SourceStorageDisk {
	out := cloneSourceStorageDisk(base)
	out.Name = next.Name
	out.State = next.State
	out.Selector = mergeSourceVolumeSelector(out.Selector, next.Selector)
	if value, ok := next.Filesystem.Get(); ok {
		out.Filesystem = supplied(value)
	}
	if value, ok := next.Wipe.Get(); ok {
		out.Wipe = supplied(value)
	}
	return out
}

func mergeSourceVolumeSelector(base, next *SourceVolumeSelector) *SourceVolumeSelector {
	if next == nil {
		return cloneSourceVolumeSelector(base)
	}
	out := cloneSourceVolumeSelector(base)
	if out == nil {
		out = &SourceVolumeSelector{}
	}
	switch {
	case next.Disk != nil:
		if out.Partition != nil {
			out.Disk = nil
		}
		out.Disk = mergeSourceDiskSelector(out.Disk, next.Disk)
		out.Partition = nil
	case next.Partition != nil:
		if out.Disk != nil {
			out.Partition = nil
		}
		out.Partition = mergeSourcePartitionSelector(out.Partition, next.Partition)
		out.Disk = nil
	}
	return out
}

func mergeSourceDiskSelector(base, next *SourceDiskSelector) *SourceDiskSelector {
	if next == nil {
		return cloneSourceDiskSelector(base)
	}
	out := cloneSourceDiskSelector(base)
	if out == nil {
		out = &SourceDiskSelector{}
	}
	if value, ok := next.ByID.Get(); ok {
		out.ByID = supplied(value)
	}
	if value, ok := next.WWN.Get(); ok {
		out.WWN = supplied(value)
	}
	if value, ok := next.Serial.Get(); ok {
		out.Serial = supplied(value)
	}
	if value, ok := next.MinSizeMiB.Get(); ok {
		out.MinSizeMiB = supplied(value)
	}
	return out
}

func mergeSourcePartitionSelector(base, next *SourcePartitionSelector) *SourcePartitionSelector {
	if next == nil {
		return cloneSourcePartitionSelector(base)
	}
	out := cloneSourcePartitionSelector(base)
	if out == nil {
		out = &SourcePartitionSelector{}
	}
	if value, ok := next.ByID.Get(); ok {
		out.ByID = supplied(value)
	}
	if value, ok := next.PartUUID.Get(); ok {
		out.PartUUID = supplied(value)
	}
	if value, ok := next.FilesystemUUID.Get(); ok {
		out.FilesystemUUID = supplied(value)
	}
	return out
}

func mergeSourceLabels(base, next Optional[map[string]string]) Optional[map[string]string] {
	nextLabels, ok := next.Get()
	if !ok {
		return cloneOptionalMap(base)
	}
	if len(nextLabels) == 0 {
		return supplied(map[string]string{})
	}
	values := map[string]string{}
	if baseLabels, present := base.Get(); present {
		values = maps.Clone(baseLabels)
	}
	maps.Copy(values, nextLabels)
	return supplied(values)
}

func lowerSSHAccess(access SourceSSHAccess) manifest.SSHIdentity {
	keys, ok := access.AuthorizedKeys.Get()
	if !ok {
		return manifest.SSHIdentity{}
	}
	return manifest.SSHIdentity{AuthorizedKeys: slices.Clone(keys)}
}

func lowerKernelConfig(config *SourceKernelConfig) *manifest.KernelConfig {
	if config == nil {
		return nil
	}
	commandLine, _ := config.CommandLine.Get()
	return &manifest.KernelConfig{CommandLine: slices.Clone(commandLine)}
}

func lowerHostConfiguration(config SourceHostConfiguration) manifest.HostConfiguration {
	var sysfs []manifest.HostConfigurationSysfsSetting
	if settings, ok := config.Sysfs.Get(); ok {
		sysfs = make([]manifest.HostConfigurationSysfsSetting, 0, len(settings))
		for _, setting := range settings {
			sysfs = append(sysfs, manifest.HostConfigurationSysfsSetting{Name: setting.Path, Value: setting.Value})
		}
	}
	var sets map[string]manifest.HostConfigurationSet
	if fileSets, ok := config.FileSets.Get(); ok {
		sets = make(map[string]manifest.HostConfigurationSet, len(fileSets))
		for name, set := range fileSets {
			sets[name] = manifest.HostConfigurationSet{
				State: set.State,
				Files: slices.Clone(set.Files),
				Notify: manifest.HostConfigurationNotifications{
					Systemd: slices.Clone(set.OnChange.Systemd),
				},
			}
		}
	}
	return manifest.HostConfiguration{Sysfs: sysfs, Sets: sets}
}

func lowerDiskSelector(selector *SourceDiskSelector) *manifest.DiskSelector {
	if selector == nil {
		return nil
	}
	return &manifest.DiskSelector{
		ByID:       selector.ByID.Value(),
		WWN:        selector.WWN.Value(),
		Serial:     selector.Serial.Value(),
		MinSizeMiB: selector.MinSizeMiB.Value(),
	}
}

func lowerStorageDisks(disks Optional[[]SourceStorageDisk]) []manifest.Volume {
	values, ok := disks.Get()
	if !ok {
		return nil
	}
	out := make([]manifest.Volume, 0, len(values))
	for _, disk := range values {
		if strings.TrimSpace(disk.State) == manifest.SystemExtensionAbsent {
			continue
		}
		out = append(out, manifest.Volume{
			Name:       strings.TrimSpace(disk.Name),
			Selector:   lowerVolumeSelector(disk.Selector),
			Filesystem: disk.Filesystem.Value(),
			Wipe:       disk.Wipe.Value(),
		})
	}
	return out
}

func lowerVolumeSelector(selector *SourceVolumeSelector) manifest.VolumeSelector {
	if selector == nil {
		return manifest.VolumeSelector{}
	}
	out := manifest.VolumeSelector{Disk: lowerDiskSelector(selector.Disk)}
	if selector.Partition != nil {
		out.Partition = &manifest.PartitionSelector{
			ByID:           selector.Partition.ByID.Value(),
			PartUUID:       selector.Partition.PartUUID.Value(),
			FilesystemUUID: selector.Partition.FilesystemUUID.Value(),
		}
	}
	return out
}

func lowerLabels(labels Optional[map[string]string]) map[string]string {
	values, ok := labels.Get()
	if !ok {
		return nil
	}
	return maps.Clone(values)
}

func lowerTaints(taints Optional[[]manifest.NodeTaint]) []manifest.NodeTaint {
	values, ok := taints.Get()
	if !ok {
		return nil
	}
	return slices.Clone(values)
}

func validateDefaultSystemDisk(selector *SourceDiskSelector) error {
	if selector == nil {
		return nil
	}
	for _, value := range []Optional[string]{selector.ByID, selector.WWN, selector.Serial} {
		if configured, ok := value.Get(); ok && strings.TrimSpace(configured) != "" {
			return fmt.Errorf("identifying selectors byID, wwn, and serial must be set per node")
		}
	}
	return nil
}

func validateDefaultStorageDisks(disks Optional[[]SourceStorageDisk]) error {
	values, ok := disks.Get()
	if !ok {
		return nil
	}
	for i, volume := range values {
		field := fmt.Sprintf("spec.defaults.storage.disks[%d]", i)
		if wipe, supplied := volume.Wipe.Get(); supplied && wipe {
			return fmt.Errorf("%s.wipe must not be true in defaults; set destructive authority on a concrete node volume", field)
		}
		if volume.Selector == nil {
			continue
		}
		if volume.Selector.Partition != nil {
			return fmt.Errorf("%s.selector.partition identifies a target and must be set on a concrete node volume", field)
		}
		if volume.Selector.Disk == nil {
			continue
		}
		for _, identity := range []struct {
			name  string
			value Optional[string]
		}{
			{name: "byID", value: volume.Selector.Disk.ByID},
			{name: "wwn", value: volume.Selector.Disk.WWN},
			{name: "serial", value: volume.Selector.Disk.Serial},
		} {
			if configured, supplied := identity.value.Get(); supplied && strings.TrimSpace(configured) != "" {
				return fmt.Errorf("%s.selector.disk.%s identifies a target and must be set on a concrete node volume", field, identity.name)
			}
		}
	}
	return nil
}

func validateSourceStorageDisks(field string, disks Optional[[]SourceStorageDisk], requireComplete bool) error {
	values, ok := disks.Get()
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	for i, disk := range values {
		name := strings.TrimSpace(disk.Name)
		if name == "" {
			return fmt.Errorf("%s[%d].name is required", field, i)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%s contains duplicate name %q", field, name)
		}
		seen[name] = struct{}{}
		switch state := strings.TrimSpace(disk.State); state {
		case "", manifest.SystemExtensionPresent, manifest.SystemExtensionAbsent:
		default:
			return fmt.Errorf("%s[%d].state must be %q or %q", field, i, manifest.SystemExtensionPresent, manifest.SystemExtensionAbsent)
		}
		if strings.TrimSpace(disk.State) == manifest.SystemExtensionAbsent {
			continue
		}
		if disk.Selector != nil && disk.Selector.Disk != nil && disk.Selector.Partition != nil {
			return fmt.Errorf("%s[%d].selector must set exactly one of disk or partition", field, i)
		}
		if !requireComplete {
			continue
		}
		volume := lowerStorageDisks(supplied([]SourceStorageDisk{disk}))
		if err := manifest.ValidateVolumes(volume); err != nil {
			message := strings.Replace(err.Error(), "install.volumes[0]", fmt.Sprintf("%s[%d]", field, i), 1)
			return fmt.Errorf("%s", message)
		}
	}
	return nil
}

func validateResolvedSourceNode(path string, layer SourceNodeLayer) error {
	if keys, ok := layer.Access.SSH.AuthorizedKeys.Get(); !ok || len(keys) == 0 {
		return fmt.Errorf("%s.access.ssh.authorizedKeys must not be empty", path)
	} else {
		for i, key := range keys {
			if !manifest.ValidAuthorizedKey(key) {
				return fmt.Errorf("%s.access.ssh.authorizedKeys[%d] must be an SSH public key", path, i)
			}
		}
	}
	if layer.Install.SystemDisk == nil {
		return fmt.Errorf("%s.install.systemDisk is required", path)
	}
	selected := 0
	for _, value := range []Optional[string]{layer.Install.SystemDisk.ByID, layer.Install.SystemDisk.WWN, layer.Install.SystemDisk.Serial} {
		if configured, ok := value.Get(); ok && strings.TrimSpace(configured) != "" {
			selected++
		}
	}
	if selected != 1 {
		return fmt.Errorf("%s.install.systemDisk must set exactly one of byID, wwn, or serial", path)
	}
	if err := validateSourceStorageDisks(path+".storage.disks", layer.Storage.Disks, true); err != nil {
		return err
	}
	if err := validateSourceHostConfiguration(path+".hostConfiguration", layer.HostConfiguration); err != nil {
		return err
	}
	if err := validateSourceSystemExtensions(path+".systemExtensions", layer.SystemExtensions); err != nil {
		return err
	}
	return nil
}

func validateSourceHostConfiguration(field string, config SourceHostConfiguration) error {
	if err := manifest.ValidateHostConfiguration(lowerHostConfiguration(config), true); err != nil {
		return fmt.Errorf("%s.%s", field, publicHostConfigurationMessage(err.Error()))
	}
	return nil
}

func publicHostConfigurationMessage(message string) string {
	if strings.HasPrefix(message, "sysfs[") {
		message = strings.Replace(message, "].name", "].path", 1)
	}
	if strings.HasPrefix(message, "sets[") {
		message = "fileSets[" + strings.TrimPrefix(message, "sets[")
	}
	message = strings.ReplaceAll(message, ".notify.", ".onChange.")
	message = strings.ReplaceAll(message, "host configuration set name", "fileSets key")
	return message
}

func validateSourceSystemExtensions(field string, extensions Optional[[]SourceSystemExtension]) error {
	if err := manifest.ValidateSystemExtensions(lowerSystemExtensions(extensions), true); err != nil {
		return fmt.Errorf("%s%s", field, publicSystemExtensionsMessage(err.Error()))
	}
	return nil
}

func publicSystemExtensionsMessage(message string) string {
	if index := strings.Index(message, ".configuration: sets["); index >= 0 {
		detail := message[index+len(".configuration: sets["):]
		if end := strings.Index(detail, "].files"); end >= 0 {
			message = message[:index] + ".configuration.files" + detail[end+len("].files"):]
		}
	}
	message = strings.ReplaceAll(message, "host configuration set name", "system extension name")
	return message
}

func lowerSystemExtensions(extensions Optional[[]SourceSystemExtension]) []manifest.SystemExtension {
	values, ok := extensions.Get()
	if !ok {
		return nil
	}
	out := make([]manifest.SystemExtension, 0, len(values))
	for _, extension := range values {
		out = append(out, lowerSystemExtension(extension))
	}
	return out
}

func lowerSystemExtension(extension SourceSystemExtension) manifest.SystemExtension {
	extension = cloneSourceSystemExtension(extension)
	if extension.resolved != nil {
		return *extension.resolved
	}
	return manifest.SystemExtension{
		Name:          extension.Name,
		State:         extension.State,
		Bundle:        extension.Bundle,
		Configuration: extension.Configuration,
		Units:         slices.Clone(extension.Units),
	}
}

func cloneSourceKernelConfig(config *SourceKernelConfig) *SourceKernelConfig {
	if config == nil {
		return nil
	}
	clone := *config
	clone.CommandLine = cloneOptionalSlice(config.CommandLine)
	return &clone
}

func cloneSourceHostConfiguration(config SourceHostConfiguration) SourceHostConfiguration {
	out := config
	out.Sysfs = cloneOptionalSlice(config.Sysfs)
	if fileSets, ok := config.FileSets.Get(); ok {
		sets := maps.Clone(fileSets)
		for name, set := range sets {
			sets[name] = cloneSourceHostConfigurationFileSet(set)
		}
		out.FileSets = supplied(sets)
	}
	return out
}

func cloneSourceHostConfigurationFileSet(set SourceHostConfigurationFileSet) SourceHostConfigurationFileSet {
	out := set
	out.Files = slices.Clone(set.Files)
	out.OnChange.Systemd = slices.Clone(set.OnChange.Systemd)
	return out
}

func cloneOptionalStorageDisks(optional Optional[[]SourceStorageDisk]) Optional[[]SourceStorageDisk] {
	disks, ok := optional.Get()
	if !ok {
		return Optional[[]SourceStorageDisk]{}
	}
	values := make([]SourceStorageDisk, 0, len(disks))
	for _, disk := range disks {
		values = append(values, cloneSourceStorageDisk(disk))
	}
	return supplied(values)
}

func cloneSourceStorageDisk(disk SourceStorageDisk) SourceStorageDisk {
	out := disk
	out.Selector = cloneSourceVolumeSelector(disk.Selector)
	return out
}

func cloneOptionalSourceSystemExtensions(optional Optional[[]SourceSystemExtension]) Optional[[]SourceSystemExtension] {
	extensions, ok := optional.Get()
	if !ok {
		return Optional[[]SourceSystemExtension]{}
	}
	return supplied(cloneSourceSystemExtensions(extensions))
}

func cloneSourceSystemExtensions(extensions []SourceSystemExtension) []SourceSystemExtension {
	values := make([]SourceSystemExtension, 0, len(extensions))
	for _, extension := range extensions {
		values = append(values, cloneSourceSystemExtension(extension))
	}
	return values
}

func cloneSourceSystemExtension(extension SourceSystemExtension) SourceSystemExtension {
	out := extension
	out.Configuration.Files = slices.Clone(extension.Configuration.Files)
	out.Units = slices.Clone(extension.Units)
	for i := range out.Units {
		out.Units[i].DropIns = slices.Clone(extension.Units[i].DropIns)
	}
	if extension.resolved != nil {
		resolved := *extension.resolved
		resolved.SupportedRuntimeInterfaces = slices.Clone(extension.resolved.SupportedRuntimeInterfaces)
		resolved.Configuration.Files = slices.Clone(extension.resolved.Configuration.Files)
		resolved.Units = slices.Clone(extension.resolved.Units)
		for i := range resolved.Units {
			resolved.Units[i].DropIns = slices.Clone(extension.resolved.Units[i].DropIns)
		}
		resolved.Payloads = slices.Clone(extension.resolved.Payloads)
		out.resolved = &resolved
	}
	return out
}

func cloneSourceDiskSelector(selector *SourceDiskSelector) *SourceDiskSelector {
	if selector == nil {
		return nil
	}
	out := *selector
	return &out
}

func cloneSourceVolumeSelector(selector *SourceVolumeSelector) *SourceVolumeSelector {
	if selector == nil {
		return nil
	}
	out := *selector
	out.Disk = cloneSourceDiskSelector(selector.Disk)
	out.Partition = cloneSourcePartitionSelector(selector.Partition)
	return &out
}

func cloneSourcePartitionSelector(selector *SourcePartitionSelector) *SourcePartitionSelector {
	if selector == nil {
		return nil
	}
	out := *selector
	return &out
}

func cloneOptionalSlice[T any](optional Optional[[]T]) Optional[[]T] {
	values, ok := optional.Get()
	if !ok {
		return Optional[[]T]{}
	}
	return supplied(slices.Clone(values))
}

func cloneOptionalMap[K comparable, V any](optional Optional[map[K]V]) Optional[map[K]V] {
	values, ok := optional.Get()
	if !ok {
		return Optional[map[K]V]{}
	}
	return supplied(maps.Clone(values))
}
