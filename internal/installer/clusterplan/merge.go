package clusterplan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/manifest"
)

func mergedLayer(layers ...NodeLayer) (NodeLayer, error) {
	var out NodeLayer
	for _, layer := range layers {
		var err error
		out, err = mergeLayer(out, layer)
		if err != nil {
			return NodeLayer{}, err
		}
	}
	sortNetworkd(out.Networkd.Files)
	sortExtraDisks(out.Install.ExtraDisks)
	return out, nil
}

func mergeLayer(base, next NodeLayer) (NodeLayer, error) {
	out := base
	if strings.TrimSpace(next.Hostname) != "" {
		out.Hostname = strings.TrimSpace(next.Hostname)
	}
	out.SSH.AuthorizedKeys = appendUnique(out.SSH.AuthorizedKeys, next.SSH.AuthorizedKeys)
	networkd, err := mergeNetworkd(out.Networkd, next.Networkd)
	if err != nil {
		return NodeLayer{}, err
	}
	out.Networkd = networkd
	if next.Install.TargetDisk != nil {
		disk := *next.Install.TargetDisk
		out.Install.TargetDisk = &disk
	}
	defaults, err := mergeTargetDiskDefaults(out.Install.TargetDiskDefaults, next.Install.TargetDiskDefaults)
	if err != nil {
		return NodeLayer{}, err
	}
	out.Install.TargetDiskDefaults = defaults
	extra, err := mergeExtraDisks(out.Install.ExtraDisks, next.Install.ExtraDisks)
	if err != nil {
		return NodeLayer{}, err
	}
	out.Install.ExtraDisks = extra
	if strings.TrimSpace(next.Kubernetes.KubeadmConfigRef) != "" {
		out.Kubernetes.KubeadmConfigRef = strings.TrimSpace(next.Kubernetes.KubeadmConfigRef)
	}
	labels, err := mergeLabels(out.Kubernetes.NodeLabels, next.Kubernetes.NodeLabels)
	if err != nil {
		return NodeLayer{}, err
	}
	out.Kubernetes.NodeLabels = labels
	taints, err := mergeTaints(out.Kubernetes.NodeTaints, next.Kubernetes.NodeTaints)
	if err != nil {
		return NodeLayer{}, err
	}
	out.Kubernetes.NodeTaints = taints
	if strings.TrimSpace(next.Bootstrap.Address) != "" {
		out.Bootstrap.Address = strings.TrimSpace(next.Bootstrap.Address)
	}
	out.Bootstrap.Access = mergeAccess(out.Bootstrap.Access, next.Bootstrap.Access)
	return out, nil
}

func mergeTargetDiskDefaults(base, next *manifest.DiskSelector) (*manifest.DiskSelector, error) {
	if base == nil && next == nil {
		return nil, nil
	}
	out := manifest.DiskSelector{}
	if base != nil {
		if err := validateTargetDiskDefaults(*base); err != nil {
			return nil, err
		}
		out = *base
	}
	if next != nil {
		if err := validateTargetDiskDefaults(*next); err != nil {
			return nil, err
		}
		if next.MinSizeMiB != 0 {
			out.MinSizeMiB = next.MinSizeMiB
		}
	}
	if out == (manifest.DiskSelector{}) {
		return nil, nil
	}
	return &out, nil
}

func applyTargetDiskDefaults(layer NodeLayer) NodeLayer {
	if layer.Install.TargetDisk == nil || layer.Install.TargetDiskDefaults == nil {
		return layer
	}
	target := *layer.Install.TargetDisk
	if target.MinSizeMiB == 0 {
		target.MinSizeMiB = layer.Install.TargetDiskDefaults.MinSizeMiB
	}
	layer.Install.TargetDisk = &target
	return layer
}

func mergeLabels(base, next map[string]string) (map[string]string, error) {
	if len(base) == 0 && len(next) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(base)+len(next))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range next {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if previous, ok := out[key]; ok && previous != value {
			return nil, fmt.Errorf("node label %q has conflicting values", key)
		}
		out[key] = value
	}
	return out, nil
}

func mergeTaints(base, next []manifest.NodeTaint) ([]manifest.NodeTaint, error) {
	taints := append([]manifest.NodeTaint(nil), base...)
	index := make(map[string]int, len(taints))
	for i, taint := range taints {
		index[taintKey(taint)] = i
	}
	for _, taint := range next {
		taint.Key = strings.TrimSpace(taint.Key)
		taint.Value = strings.TrimSpace(taint.Value)
		taint.Effect = strings.TrimSpace(taint.Effect)
		if taint.Key == "" && taint.Effect == "" {
			continue
		}
		key := taintKey(taint)
		if i, ok := index[key]; ok {
			if taints[i].Value != taint.Value {
				return nil, fmt.Errorf("node taint %q has conflicting values", key)
			}
			continue
		}
		index[key] = len(taints)
		taints = append(taints, taint)
	}
	sortTaints(taints)
	return taints, nil
}

func taintKey(taint manifest.NodeTaint) string {
	return taint.Key + "\x00" + taint.Effect
}

func mergeNetworkd(base, next manifest.NetworkdConfig) (manifest.NetworkdConfig, error) {
	files := append([]manifest.NetworkdFile(nil), base.Files...)
	index := make(map[string]int, len(files))
	for i, file := range files {
		index[file.Name] = i
	}
	for _, file := range next.Files {
		if i, ok := index[file.Name]; ok {
			if files[i].Content != file.Content {
				return manifest.NetworkdConfig{}, fmt.Errorf("networkd file %q has conflicting content", file.Name)
			}
			continue
		}
		index[file.Name] = len(files)
		files = append(files, file)
	}
	sortNetworkd(files)
	return manifest.NetworkdConfig{Files: files}, nil
}

func mergeExtraDisks(base, next []manifest.ExtraDisk) ([]manifest.ExtraDisk, error) {
	disks := append([]manifest.ExtraDisk(nil), base...)
	index := make(map[string]int, len(disks))
	for i, disk := range disks {
		index[disk.Name] = i
	}
	for _, disk := range next {
		if i, ok := index[disk.Name]; ok {
			if !same(disks[i], disk) {
				return nil, fmt.Errorf("extra disk %q has conflicting settings", disk.Name)
			}
			continue
		}
		index[disk.Name] = len(disks)
		disks = append(disks, disk)
	}
	sortExtraDisks(disks)
	return disks, nil
}

func mergeAccess(base, next inventory.Access) inventory.Access {
	out := base
	if strings.TrimSpace(next.Method) != "" {
		out.Method = strings.TrimSpace(next.Method)
	}
	if strings.TrimSpace(next.User) != "" {
		out.User = strings.TrimSpace(next.User)
	}
	if strings.TrimSpace(next.CredentialRef) != "" {
		out.CredentialRef = strings.TrimSpace(next.CredentialRef)
	}
	return out
}

func appendUnique(base, next []string) []string {
	out := append([]string(nil), base...)
	seen := make(map[string]struct{}, len(out))
	for _, value := range out {
		seen[value] = struct{}{}
	}
	for _, value := range next {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortNetworkd(files []manifest.NetworkdFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
}

func sortExtraDisks(disks []manifest.ExtraDisk) {
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
}

func sortTaints(taints []manifest.NodeTaint) {
	sort.Slice(taints, func(i, j int) bool {
		if taints[i].Key != taints[j].Key {
			return taints[i].Key < taints[j].Key
		}
		if taints[i].Effect != taints[j].Effect {
			return taints[i].Effect < taints[j].Effect
		}
		return taints[i].Value < taints[j].Value
	})
}
