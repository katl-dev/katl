package agent

import (
	"context"
	"errors"
	"os"
	"sort"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func nodeVolumeStatus(ctx context.Context, root, currentGeneration string, runner ToolRunner) ([]*agentapi.VolumeStatus, error) {
	installManifest, err := generationManifest(root, currentGeneration)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	volumes := append([]manifest.Volume(nil), installManifest.Install.Volumes...)
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	out := make([]*agentapi.VolumeStatus, 0, len(volumes))
	for _, volume := range volumes {
		unitName, err := generation.MountUnitName("/var/mnt/" + volume.Name)
		if err != nil {
			return nil, err
		}
		unit := systemExtensionUnitStatus(ctx, runner, manifest.SystemExtensionUnit{Name: unitName})
		targetKind := "partition"
		if volume.Selector.Disk != nil {
			targetKind = "disk"
		}
		out = append(out, &agentapi.VolumeStatus{
			Name:                 volume.Name,
			TargetKind:           targetKind,
			MountPath:            "/var/mnt/" + volume.Name,
			Filesystem:           volume.Filesystem,
			LoadState:            unit.LoadState,
			ActiveState:          unit.ActiveState,
			SubState:             unit.SubState,
			Result:               unit.Result,
			StateChangeTimestamp: unit.StateChangeTimestamp,
			FailureDiagnostic:    unit.FailureDiagnostic,
		})
	}
	return out, nil
}
