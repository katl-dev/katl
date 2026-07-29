package configapply

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func volumeMountNativeEtcFiles(volumes []manifest.Volume) ([]confext.NativeEtcFile, error) {
	requests := make([]generation.ExtraMountRequest, 0, len(volumes))
	for _, volume := range volumes {
		source, err := configuredVolumeMountSource(volume)
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

func configuredVolumeMountSource(volume manifest.Volume) (string, error) {
	switch {
	case volume.Selector.Disk != nil:
		return "/dev/disk/by-partlabel/u-" + volume.Name, nil
	case volume.Selector.Partition == nil:
		return "", fmt.Errorf("volume %q has no selector", volume.Name)
	case volume.Selector.Partition.ByID != "":
		return volume.Selector.Partition.ByID, nil
	case volume.Selector.Partition.PartUUID != "":
		return "PARTUUID=" + volume.Selector.Partition.PartUUID, nil
	case volume.Selector.Partition.FilesystemUUID != "":
		return "UUID=" + volume.Selector.Partition.FilesystemUUID, nil
	default:
		return "/dev/disk/by-partlabel/u-" + volume.Name, nil
	}
}
