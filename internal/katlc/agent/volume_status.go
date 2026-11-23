package agent

import (
	"context"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/manifest"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func nodeVolumeStatus(ctx context.Context, root, currentGeneration string, runner ToolRunner, devices volumeDevices, discoveryRun ToolRunner) ([]*agentapi.VolumeStatus, error) {
	installManifest, err := generationManifest(root, currentGeneration)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	volumes := append([]manifest.Volume(nil), installManifest.Install.Volumes...)
	spec, _, err := generation.ReadGeneration(root, currentGeneration)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	bindings := volumeBindingsByName(spec.VolumeBindings)
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
		binding := bindings[volume.Name]
		mountSource, _ := disk.VolumeBindingMountSource(diskVolumeBinding(binding))
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
			MountSource:          mountSource,
		})
	}

	// Activation mounts no volume while any binding does not resolve. Devices
	// are discovered only when a volume is unmounted, so a healthy node pays
	// nothing for the explanation.
	if !slices.ContainsFunc(out, func(volume *agentapi.VolumeStatus) bool { return volume.ActiveState != "active" }) {
		return out, nil
	}
	names := make([]string, 0, len(out))
	for _, volume := range out {
		names = append(names, volume.Name)
	}
	faults := volumeFaultDiagnostics(ctx, devices, discoveryRun, root, currentGeneration, names)
	for _, volume := range out {
		if fault, ok := faults[volume.Name]; ok && volume.ActiveState != "active" {
			volume.FailureDiagnostic = strings.TrimSpace(fault + "\n" + volume.FailureDiagnostic)
		}
	}
	return out, nil
}
