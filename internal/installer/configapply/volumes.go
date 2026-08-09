package configapply

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func volumeMountNativeEtcFiles(volumes []manifest.Volume, bindings []generation.VolumeBinding) ([]confext.NativeEtcFile, error) {
	bound := make(map[string]generation.VolumeBinding, len(bindings))
	for _, binding := range bindings {
		if _, exists := bound[binding.Name]; exists {
			return nil, fmt.Errorf("duplicate volume binding %q", binding.Name)
		}
		bound[binding.Name] = binding
	}
	requests := make([]generation.ExtraMountRequest, 0, len(volumes))
	for _, volume := range volumes {
		binding, ok := bound[volume.Name]
		if !ok {
			return nil, fmt.Errorf("volume %q has no generation-owned device binding", volume.Name)
		}
		delete(bound, volume.Name)
		source, err := volumeBindingMountSource(binding)
		if err != nil {
			return nil, err
		}
		requests = append(requests, generation.ExtraMountRequest{
			Source:     source,
			Path:       "/var/mnt/" + volume.Name,
			Filesystem: volume.Filesystem,
		})
	}
	units, _, err := generation.RenderExtraMounts(requests)
	if err != nil {
		return nil, err
	}
	files := make([]confext.NativeEtcFile, 0, len(units)+1)
	unitNames := make([]string, 0, len(units))
	for _, unit := range units {
		files = append(files, confext.NativeEtcFile{
			Path: filepath.ToSlash(filepath.Join("/etc/systemd/system", unit.Name)), Content: unit.Content, Mode: 0o644,
		})
		unitNames = append(unitNames, unit.Name)
	}
	if len(unitNames) > 0 {
		files = append(files, confext.NativeEtcFile{
			Path: "/etc/systemd/system/katl-volumes.target.d/50-mounts.conf",
			Content: strings.Join([]string{
				"[Unit]",
				"Requires=" + strings.Join(unitNames, " "),
				"After=" + strings.Join(unitNames, " "),
				"",
			}, "\n"),
			Mode: 0o644,
		})
	}
	return files, nil
}

func volumeBindingMountSource(binding generation.VolumeBinding) (string, error) {
	switch {
	case strings.TrimSpace(binding.PartitionUUID) != "":
		return "PARTUUID=" + strings.TrimSpace(binding.PartitionUUID), nil
	case strings.TrimSpace(binding.FilesystemUUID) != "":
		return "UUID=" + strings.TrimSpace(binding.FilesystemUUID), nil
	default:
		return "", fmt.Errorf("volume binding %q has no partition or filesystem UUID", binding.Name)
	}
}
